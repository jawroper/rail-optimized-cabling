# cabling-go

Generates a cabling assignment CSV file for a 3-stage and/or 5-stage topology with the server to leaf connections being rail-optimized. If a 5-stage, use the tool
to generate a PoD at a time.

The tool can create cabling that complies with Apstra cabling format and is
flexible enough to use user defined naming for the switches and servers.

It is YAML driven.

## Build

```
make            # cross-compile all 5 platform binaries into dist/
make native     # build just for the machine you're on -> ./cabling-go
make clean      # remove build output
```

Platforms produced by `make`:
- `dist/cabling-go-linux-amd64`
- `dist/cabling-go-linux-arm64`
- `dist/cabling-go-darwin-amd64`   (macOS Intel)
- `dist/cabling-go-darwin-arm64`   (macOS Apple Silicon)
- `dist/cabling-go-windows-amd64.exe`

No external Go modules are required -- the YAML parsing is a small
dependency-free parser purpose-built for the config shape below, so `go
build` works offline with just the standard library.

## Run

```
./cabling-go -config cluster_800g.yaml -pod 1 -prefix Srvr -output pod01_cabling.csv
./cabling-go -config cluster_800g.yaml -pod 2 -prefix Srvr -output pod02_cabling.csv -base-ip 10.2.0.0
```

Flags:
- `-config` (required) -- path to the YAML topology file
- `-pod` -- PoD number, default 1
- `-prefix` -- server name prefix, default "Srvr"
- `-output` -- output CSV path, default `podNN_cabling.csv`
- `-base-ip` -- which /24 block to start allocating /31 links from. The last
  octet you pass is ignored; allocation always starts at that block's
  x.x.x.2. Within each /24 block, only x.x.x.2 through x.x.x.253 are used
  (skipping .0, .1, .254, .255), giving 126 usable /31 pairs per block. A
  pair never straddles two /24s -- once a block's 126 pairs are used up,
  allocation rolls forward to the next /24 automatically.
- `-stripe-mode` -- `even` (default) splits nodes as equally as possible across
  leaf stripes (e.g. 62,62,62,62,62,62); `packed` fills each leaf stripe to full
  switch capacity (`nodes_per_stripe` in the config) before starting the next
  (e.g. 64,64,64,64,64,52)

Two ready-to-use configs are included:
- `cluster_800g.yaml` -- native 800G links T1<->T2 and T2<->T3
- `cluster_400g.yaml` -- same topology, but those links use 2x400G breakout
  legs to the same neighbor instead (same aggregate bandwidth, 2x the rows)

## Config file format

```yaml
ports_per_switch: 64        # physical ports per switch (optional, default 64)
interface_prefix: "et-0/0/" # interface naming prefix   (optional, default "et-0/0/")

layers:
  - name: server             # internal key, referenced by connections below
    display_name: Server     # optional -- label used in Role/Type columns; defaults to Title(name)
    count: 372                # total servers in one PoD
    rails: 8                  # NICs per server -- marks this as THE base/server layer (exactly one required)
    rack_size: 12              # optional -- if set, adds a Rack_NN_PoD_NN column
    name_format: "sys%(pod)03d_%(stripe1)03d_%(local1)03d"  # optional -- see naming fields below

  - name: t1
    display_name: Leaf
    name_format: "T1_Rail_%(rail)02d_Leaf_%(stripe)02d_PoD_%(pod)02d"
    striping:
      nodes_per_stripe: 62      # marks this as a per-rail-stripe layer fed by the server layer

  - name: t2
    display_name: Spine
    name_format: "T2_Spine_%(idx)02d_PoD_%(pod)02d"
    count: 32                   # per-PoD switch count
    plane: 1                    # optional -- naming label only, defaults to 1

  - name: t3
    display_name: Super-Spine
    name_format: "T3_Super_Spine_%(idx)02d"  # shared layers aren't PoD-numbered
    count: 16
    shared: true                 # one shared pool across all PoDs; each PoD gets its own port block
    plane: 1

connections:
  - from: server
    to: t1
    speed: 400G
    breakout: 2       # 2 = split each physical port into 2 logical legs (:0 / :1) to the SAME neighbor
  - from: t1
    to: t2
    speed: 800G       # omit breakout (or set 1) for a native, unbroken link
  - from: t2
    to: t3
    speed: 800G
```

### Naming fields (`name_format`)

`name_format` uses named placeholders, `%(field)0Nd` -- `0N` zero-pads to
width N; a bare `%(field)d` is unpadded. Any subset of these fields can be
used, in any order:

| Field | Meaning |
|---|---|
| `pod` | PoD number (1-based, as passed to `-pod`) |
| `plane`, `plane1` | this layer's configured `plane` value (both identical -- `plane1` exists only for naming symmetry with the pairs below) |
| `stripe`, `stripe1` | stripe index within the PoD (0-based / 1-based) -- striped layers (e.g. leaf) |
| `rail`, `rail1` | rail index (0-based / 1-based) -- striped layers only |
| `local`, `local1` | server's index within its own stripe (0-based / 1-based) -- server layer only |
| `global`, `global1` | server's index across the whole PoD (0-based / 1-based) -- server layer only |
| `idx`, `idx1` | instance index for a plain counted layer, e.g. spine or superspine (0-based / 1-based) |

Example matching `leaf001_001_1` / `spine001_001_01` / `sspine001_01` / `sys001_001_001`:

```yaml
t1 name_format:     "leaf%(pod)03d_%(stripe1)03d_%(rail1)d"
t2 name_format:     "spine%(pod)03d_%(plane)03d_%(idx1)02d"
t3 name_format:     "sspine%(plane)03d_%(idx1)02d"
server name_format: "sys%(pod)03d_%(stripe1)03d_%(local1)03d"
```

The server layer's `name_format` is optional -- if omitted, server naming
falls back to `-prefix`-based flat numbering (`Srvr_000_PoD_01`, ...), the
same as earlier versions. Old-style positional formats (`%02d`, `%02d`,
`%02d` with no field names) still work unchanged for backward compatibility
-- a format string is auto-detected as named only if it contains at least
one `%(field)` token.

Rules the config must follow:
- Exactly one layer must set `rails` -- that's the base/server layer.
- `connections` must be listed base-layer-first (server -> t1 -> t2 -> t3, ...);
  each layer must already have been produced by an earlier connection before
  it's used as a `from`.
- The `server -> <striped layer>` connection is special: it fans each rail's
  NICs out across per-rail-stripe switches sized by `striping.nodes_per_stripe`.
  All other connections are a full 1-link-per-pair mesh between every
  instance of `from` and every instance of `to`.
- `shared: true` layers (like T3) keep one instance pool across all PoD runs.
  Each PoD's link block is offset automatically so re-running with a
  different `-pod` value never collides with a previous PoD's ports -- up to
  `ports_per_switch / count(from layer)` PoDs before you need more instances.
- If `name_format` is omitted, a default like `T2_%02d_PoD_%02d` is used --
  fine for testing, but you'll usually want to set it explicitly.

## What's NOT generalized

This is a config-driven version of one specific 3-tier pattern (rail-striped
leaf layer, then a plain full-mesh up through however many more tiers you
list), not an arbitrary graph engine. It will not handle: multiple base
layers, non-mesh fan-out patterns between switch tiers, or layers with more
than one `striping` dimension. For anything outside that shape, the
generator will need extending.
