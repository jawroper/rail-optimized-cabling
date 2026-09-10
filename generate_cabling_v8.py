#!/usr/bin/env python3
"""
Generate a cabling assignment CSV for one PoD of a rail-optimized AI cluster,
driven by a YAML topology config (same schema as the Go port, cabling-go).

v8 changes from v7:
  - name_format substitution switched from POSITIONAL (%d, %d, %d in a fixed
    order) to NAMED (%(pod)03d, %(rail1)d, etc.) -- order no longer matters,
    and any subset of available fields can be used in any order.
  - New "plane" layer field (YAML: `plane: 1`, default 1 if omitted). Purely
    a naming label for now -- it does not affect port math or link counts.
    Exists so multi-plane superspine fabrics (not used in any config we've
    built so far, which are all single-plane) can be named correctly later
    without another engine change.
  - The base/server layer can now also set its own `name_format`, using
    per-stripe local numbering (sys{pod}_{stripe}_{local}) instead of the
    flat pod-wide numbering the --prefix fallback uses. If the server layer
    has no name_format, behavior is unchanged from v7 (prefix + flat index).

Available name_format fields (use whichever subset your format needs):
    pod                 PoD number (1-based, as passed to --pod)
    plane, plane1       this layer's configured `plane` value (identical --
                         plane1 exists only for naming-symmetry with the
                         0-based/1-based pairs below)
    stripe, stripe1     stripe index within the PoD (0-based / 1-based)
    rail, rail1         rail index (0-based / 1-based) -- leaf layer only
    local, local1       server's index within its own stripe (0-based / 1-based)
    global, global1     server's index across the whole PoD (0-based / 1-based)
    idx, idx1           instance index for a plain counted layer, e.g. spine
                         or superspine (0-based / 1-based)

Example: to match leafXXX_WWW_Z / spineXXX_YYY_ZZ / sspineYYY_ZZ / sysXXX_WWW_ZZZ:
    t1 name_format:  "leaf%(pod)03d_%(stripe1)03d_%(rail1)d"
    t2 name_format:  "spine%(pod)03d_%(plane)03d_%(idx1)02d"
    t3 name_format:  "sspine%(plane)03d_%(idx1)02d"
    server name_format: "sys%(pod)03d_%(stripe1)03d_%(local1)03d"
See cluster_apstra.yaml for a full working example.

Usage:
    python3 generate_cabling_v8.py --config cluster_800g.yaml --pod 1 --prefix Srvr --output pod01_cabling.csv
    python3 generate_cabling_v8.py --config cluster_apstra.yaml --pod 2 --output pod02_cabling.csv --base-ip 10.2.0.0

Requires PyYAML (pip install pyyaml --break-system-packages).
"""

import argparse
import csv
import math
import re
import yaml

_NAMED_TOKEN_RE = re.compile(r"%\(\w+\)")


def is_named_format(template):
    """True if the format string uses %(name)d-style named tokens rather
    than plain positional %d tokens. Lets old v7-and-earlier configs (which
    used positional formats) keep working unchanged under the new engine."""
    return bool(_NAMED_TOKEN_RE.search(template))


# --------------------------------------------------------------------------
# Config loading
# --------------------------------------------------------------------------

class Layer:
    def __init__(self, raw):
        self.name = raw["name"]
        self.count = int(raw.get("count", 0) or 0)
        self.rails = int(raw.get("rails", 0) or 0)
        self.name_format = raw.get("name_format", "")
        self.shared = bool(raw.get("shared", False))
        self.rack_size = int(raw.get("rack_size", 0) or 0)
        self.plane = int(raw.get("plane", 1) or 1)
        striping = raw.get("striping") or {}
        self.nodes_per_stripe = int(striping.get("nodes_per_stripe", 0) or 0)
        self.display_name = raw.get("display_name") or (self.name[:1].upper() + self.name[1:] if self.name else self.name)

        if not self.name:
            raise ValueError("layer missing required 'name' field")
        if not self.name_format and self.rails == 0:
            # Only non-base layers get an auto-generated default. The base
            # (server) layer is intentionally left with an empty
            # name_format here -- it falls back to --prefix-based naming
            # in generate_rows unless the config explicitly sets one.
            if self.nodes_per_stripe > 0:
                self.name_format = self.name.upper() + "_Rail_%(rail)02d_stripe_%(stripe)02d_PoD_%(pod)02d"
            elif self.shared:
                self.name_format = self.name.upper() + "_%(idx)02d"
            else:
                self.name_format = self.name.upper() + "_%(idx)02d_PoD_%(pod)02d"


class Connection:
    def __init__(self, raw):
        self.frm = raw["from"]
        self.to = raw["to"]
        self.speed = raw.get("speed", "")
        self.breakout = int(raw.get("breakout", 1) or 1)
        if not self.frm or not self.to:
            raise ValueError("connection missing 'from' or 'to'")


class Config:
    def __init__(self, path):
        with open(path) as f:
            data = yaml.safe_load(f)
        self.interface_prefix = data.get("interface_prefix", "et-0/0/")
        self.ports_per_switch = int(data.get("ports_per_switch", 64) or 64)
        self.layers = [Layer(l) for l in data.get("layers", [])]
        self.connections = [Connection(c) for c in data.get("connections", [])]
        if not self.layers:
            raise ValueError("config has no layers")
        if not self.connections:
            raise ValueError("config has no connections")


# --------------------------------------------------------------------------
# Topology engine (mirrors cabling-go/topology.go)
# --------------------------------------------------------------------------

def iface_name(prefix, port, leg, has_leg):
    base = f"{prefix}{port}"
    return f"{base}:{leg}" if has_leg else base


def base_subs(pod, plane):
    """Full superset of name_format fields, with neutral defaults for
    whichever ones don't apply in a given context. A format string only
    references the subset it actually needs."""
    return {
        "pod": pod, "plane": plane, "plane1": plane,
        "stripe": 0, "stripe1": 1, "rail": 0, "rail1": 1,
        "local": 0, "local1": 1, "global": 0, "global1": 1,
        "idx": 0, "idx1": 1,
    }


def find_server_layer(cfg):
    for l in cfg.layers:
        if l.rails > 0:
            return l
    raise ValueError("no layer with 'rails' set found (expected exactly one base/server layer)")


def build_server_stripes(server_count, nodes_per_stripe, stripe_mode):
    """stripe_mode='even' splits server_count as evenly as possible across
    ceil(server_count/nodes_per_stripe) stripes. 'packed' fills each stripe to
    nodes_per_stripe capacity before starting the next (last may be smaller)."""
    if stripe_mode == "even":
        num_stripes = max(1, math.ceil(server_count / nodes_per_stripe))
        base, rem = divmod(server_count, num_stripes)
        stripes, start = [], 0
        for g in range(num_stripes):
            size = base + (1 if g < rem else 0)
            stripes.append(list(range(start, start + size)))
            start += size
        return stripes

    # packed
    stripes, start = [], 0
    while start < server_count:
        size = min(nodes_per_stripe, server_count - start)
        stripes.append(list(range(start, start + size)))
        start += size
    return stripes


def generate_rows(cfg, pod, prefix, stripe_mode):
    layer_by_name = {l.name: l for l in cfg.layers}
    server_layer = find_server_layer(cfg)

    instances = {}       # layer name -> ordered list of instance names
    next_free_port = {}  # instance name -> next unused physical port

    rows = []

    for conn in cfg.connections:
        from_layer = layer_by_name.get(conn.frm)
        to_layer = layer_by_name.get(conn.to)
        if from_layer is None or to_layer is None:
            raise ValueError(f"connection references unknown layer: {conn.frm} -> {conn.to}")
        has_leg = conn.breakout > 1

        if conn.frm == server_layer.name and to_layer.nodes_per_stripe > 0:
            # --- striped connection: server NICs -> per-rail-stripe switches ---
            stripes = build_server_stripes(server_layer.count, to_layer.nodes_per_stripe, stripe_mode)
            to_instances = []
            for rail in range(server_layer.rails):
                for g, grp in enumerate(stripes):
                    subs = base_subs(pod, to_layer.plane)
                    subs["stripe"], subs["stripe1"] = g, g + 1
                    subs["rail"], subs["rail1"] = rail, rail + 1
                    if is_named_format(to_layer.name_format):
                        inst_name = to_layer.name_format % subs
                    else:
                        inst_name = to_layer.name_format % (rail, g, pod)  # legacy positional order
                    to_instances.append(inst_name)

                    down_size = math.ceil(len(grp) / conn.breakout)
                    base = next_free_port.get(inst_name, 0)
                    if base + down_size > cfg.ports_per_switch:
                        raise ValueError(
                            f"layer {to_layer.name} instance {inst_name} exceeds port capacity "
                            f"({base + down_size} > {cfg.ports_per_switch})")

                    for local_idx, srv_idx in enumerate(grp):
                        port_num = base + local_idx // conn.breakout
                        leg = local_idx % conn.breakout

                        srv_subs = dict(subs)
                        srv_subs["local"], srv_subs["local1"] = local_idx, local_idx + 1
                        srv_subs["global"], srv_subs["global1"] = srv_idx, srv_idx + 1
                        if server_layer.name_format:
                            if is_named_format(server_layer.name_format):
                                srv_name = server_layer.name_format % srv_subs
                            else:
                                srv_name = server_layer.name_format % (srv_idx, pod)  # legacy positional order
                        else:
                            srv_name = f"{prefix}_{srv_idx:03d}_PoD_{pod:02d}"

                        row = {
                            "Role": f"{to_layer.display_name} to {server_layer.display_name}",
                            "Speed": conn.speed,
                            "Endpoint 1 Name": inst_name,
                            "Endpoint 1 Type": to_layer.display_name,
                            "Endpoint 1 Interface Name": iface_name(cfg.interface_prefix, port_num, leg, has_leg),
                            "Endpoint 2 Name": srv_name,
                            "Endpoint 2 Type": server_layer.display_name,
                            "Endpoint 2 Interface Name": f"nic-{rail:02d}",
                        }
                        if server_layer.rack_size > 0:
                            rack_num = srv_idx // server_layer.rack_size
                            row["Rack"] = f"Rack_{rack_num:02d}_PoD_{pod:02d}"
                        rows.append(row)
                    next_free_port[inst_name] = base + down_size
            instances[to_layer.name] = to_instances
            continue

        # --- mesh connection: full 1-link-per-pair mesh between two counted layers ---
        from_instances = instances.get(from_layer.name)
        if from_instances is None:
            raise ValueError(
                f"layer {from_layer.name} has no instances yet when processing "
                f"{conn.frm} -> {conn.to}; connections must be listed base-layer-first")

        to_instances = instances.get(to_layer.name)
        if to_instances is None:
            to_instances = []
            for idx in range(to_layer.count):
                if is_named_format(to_layer.name_format):
                    subs = base_subs(pod, to_layer.plane)
                    subs["idx"], subs["idx1"] = idx, idx + 1
                    to_instances.append(to_layer.name_format % subs)
                else:
                    if to_layer.shared:
                        to_instances.append(to_layer.name_format % (idx,))  # legacy positional order
                    else:
                        to_instances.append(to_layer.name_format % (idx, pod))  # legacy positional order
            instances[to_layer.name] = to_instances

        from_bases = []
        for name in from_instances:
            b = next_free_port.get(name, 0)
            if b + len(to_instances) > cfg.ports_per_switch:
                raise ValueError(
                    f"layer {from_layer.name} instance {name} exceeds port capacity "
                    f"({b + len(to_instances)} > {cfg.ports_per_switch})")
            from_bases.append(b)

        to_bases = []
        for name in to_instances:
            if to_layer.shared:
                b = (pod - 1) * len(from_instances)
            else:
                b = next_free_port.get(name, 0)
            if b + len(from_instances) > cfg.ports_per_switch:
                raise ValueError(
                    f"PoD {pod} exceeds port capacity on layer {to_layer.name} instance {name} "
                    f"(port {b + len(from_instances)} >= {cfg.ports_per_switch}); "
                    f"add more {to_layer.name} instances before onboarding this PoD")
            to_bases.append(b)

        legs = conn.breakout if has_leg else 1
        for fi, from_name in enumerate(from_instances):
            for ti, to_name in enumerate(to_instances):
                from_port = from_bases[fi] + ti
                to_port = to_bases[ti] + fi
                for leg in range(legs):
                    rows.append({
                        "Role": f"{to_layer.display_name} to {from_layer.display_name}",
                        "Speed": conn.speed,
                        "Endpoint 1 Name": from_name,
                        "Endpoint 1 Type": from_layer.display_name,
                        "Endpoint 1 Interface Name": iface_name(cfg.interface_prefix, from_port, leg, has_leg),
                        "Endpoint 2 Name": to_name,
                        "Endpoint 2 Type": to_layer.display_name,
                        "Endpoint 2 Interface Name": iface_name(cfg.interface_prefix, to_port, leg, has_leg),
                    })

        for fi, name in enumerate(from_instances):
            next_free_port[name] = from_bases[fi] + len(to_instances)
        if not to_layer.shared:
            for ti, name in enumerate(to_instances):
                next_free_port[name] = to_bases[ti] + len(from_instances)

    return rows


# --------------------------------------------------------------------------
# IP assignment + CSV output
# --------------------------------------------------------------------------

CSV_FIELDS = [
    "Role", "Speed",
    "Endpoint 1 Name", "Endpoint 1 Type", "Endpoint 1 Interface ID",
    "Endpoint 1 Interface Name", "Endpoint 1 IP",
    "Endpoint 2 Name", "Endpoint 2 Type", "Endpoint 2 Interface ID",
    "Endpoint 2 Interface Name", "Endpoint 2 IP",
    "Rack",
]

PAIRS_PER_BLOCK = 126  # boundary-safe /31 allocation (see v7 notes): x.x.x.2-253 per /24


def ip_to_int(ip_str):
    parts = [int(p) for p in ip_str.split(".")]
    return (parts[0] << 24) | (parts[1] << 16) | (parts[2] << 8) | parts[3]


def int_to_ip(n):
    return f"{(n >> 24) & 255}.{(n >> 16) & 255}.{(n >> 8) & 255}.{n & 255}"


def assign_ips(rows, base_ip):
    base_block = ip_to_int(base_ip) & 0xFFFFFF00
    for i, row in enumerate(rows):
        block_offset, local_idx = divmod(i, PAIRS_PER_BLOCK)
        net1 = base_block + block_offset * 256 + 2 + local_idx * 2
        net2 = net1 + 1
        row["Endpoint 1 IP"] = f"{int_to_ip(net1)}/31"
        row["Endpoint 2 IP"] = f"{int_to_ip(net2)}/31"
        row.setdefault("Endpoint 1 Interface ID", "")
        row.setdefault("Endpoint 2 Interface ID", "")
        row.setdefault("Rack", "")


def main():
    parser = argparse.ArgumentParser(description="Generate PoD cabling assignment CSV from a YAML topology config")
    parser.add_argument("--config", required=True, help="path to the YAML topology config")
    parser.add_argument("--pod", type=int, default=1, help="PoD number (1, 2, ...)")
    parser.add_argument("--prefix", type=str, default="Srvr",
                         help="server name prefix (only used if the server layer has no name_format)")
    parser.add_argument("--output", type=str, default=None, help="output CSV path")
    parser.add_argument("--base-ip", type=str, default="192.168.0.2",
                         help="which /24 block to start allocating /31 links from "
                              "(last octet is ignored; allocation always starts at x.x.x.2)")
    parser.add_argument("--stripe-mode", type=str, default="even", choices=["even", "packed"],
                         help="even = split nodes as equally as possible across leaf stripes. "
                              "packed = fill each leaf stripe to full switch capacity first.")
    args = parser.parse_args()

    output_path = args.output or f"pod{args.pod:02d}_cabling.csv"

    cfg = Config(args.config)
    rows = generate_rows(cfg, args.pod, args.prefix, args.stripe_mode)
    assign_ips(rows, args.base_ip)

    with open(output_path, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=CSV_FIELDS)
        writer.writeheader()
        writer.writerows(rows)

    print(f"Wrote {len(rows)} cabling rows to {output_path}")


if __name__ == "__main__":
    main()
