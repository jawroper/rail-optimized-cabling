package main

import "fmt"

type Row struct {
	Role                                     string
	Speed                                    string
	E1Name, E1Type, E1IfaceID, E1Iface, E1IP string
	E2Name, E2Type, E2IfaceID, E2Iface, E2IP string
	Rack                                     string
}

func ifaceName(prefix string, port int, leg int, hasLeg bool) string {
	base := fmt.Sprintf("%s%d", prefix, port)
	if hasLeg {
		return fmt.Sprintf("%s:%d", base, leg)
	}
	return base
}

// buildServerStripes splits server indices [0, serverCount) into stripes.
// stripeMode="packed" fills each stripe to nodesPerStripe capacity before
// starting the next (the last stripe may be smaller) -- this was the Go
// port's only behavior prior to v6.
// stripeMode="even" (matching Python v5's default) first computes the
// number of stripes needed (ceil(serverCount/nodesPerStripe)), then splits
// serverCount as evenly as possible across exactly that many stripes.
func buildServerStripes(serverCount, nodesPerStripe int, stripeMode string) [][]int {
	if stripeMode == "even" {
		numStripes := (serverCount + nodesPerStripe - 1) / nodesPerStripe
		if numStripes < 1 {
			numStripes = 1
		}
		base := serverCount / numStripes
		rem := serverCount % numStripes
		var stripes [][]int
		start := 0
		for g := 0; g < numStripes; g++ {
			size := base
			if g < rem {
				size++
			}
			grp := make([]int, size)
			for k := 0; k < size; k++ {
				grp[k] = start + k
			}
			stripes = append(stripes, grp)
			start += size
		}
		return stripes
	}

	// packed (default/fallback)
	var stripes [][]int
	start := 0
	for start < serverCount {
		size := nodesPerStripe
		if start+size > serverCount {
			size = serverCount - start
		}
		grp := make([]int, size)
		for k := 0; k < size; k++ {
			grp[k] = start + k
		}
		stripes = append(stripes, grp)
		start += size
	}
	return stripes
}

// baseSubs returns the full superset of name_format fields, with neutral
// defaults for whichever ones don't apply in a given context. A format
// string only references the subset it actually needs.
func baseSubs(pod, plane int) map[string]int {
	return map[string]int{
		"pod": pod, "plane": plane, "plane1": plane,
		"stripe": 0, "stripe1": 1, "rail": 0, "rail1": 1,
		"local": 0, "local1": 1, "global": 0, "global1": 1,
		"idx": 0, "idx1": 1,
	}
}

func findServerLayer(cfg Config) (Layer, error) {
	for _, l := range cfg.Layers {
		if l.Rails > 0 {
			return l, nil
		}
	}
	return Layer{}, fmt.Errorf("no layer with 'rails' set found (expected exactly one base/server layer)")
}

// GenerateRows walks the connections in the order given in the config
// (which must be dependency-ordered, base layer first) and produces the
// full set of cabling rows for one PoD.
func GenerateRows(cfg Config, pod int, prefix string, stripeMode string) ([]Row, error) {
	layerByName := map[string]Layer{}
	for _, l := range cfg.Layers {
		layerByName[l.Name] = l
	}

	serverLayer, err := findServerLayer(cfg)
	if err != nil {
		return nil, err
	}

	instances := map[string][]string{} // layer name -> ordered instance names (switch-like layers)
	nextFreePort := map[string]int{}   // instance name -> next unused physical port

	var rows []Row

	for _, conn := range cfg.Connections {
		fromLayer, ok1 := layerByName[conn.From]
		toLayer, ok2 := layerByName[conn.To]
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("connection references unknown layer: %s -> %s", conn.From, conn.To)
		}
		hasLeg := conn.Breakout > 1

		if conn.From == serverLayer.Name && toLayer.NodesPerStripe > 0 {
			// --- striped connection: server NICs -> per-rail-stripe switches ---
			stripes := buildServerStripes(serverLayer.Count, toLayer.NodesPerStripe, stripeMode)
			var toInstances []string
			for rail := 0; rail < serverLayer.Rails; rail++ {
				for g, grp := range stripes {
					subs := baseSubs(pod, toLayer.Plane)
					subs["stripe"], subs["stripe1"] = g, g+1
					subs["rail"], subs["rail1"] = rail, rail+1

					var instName string
					if IsNamedFormat(toLayer.NameFormat) {
						var err error
						instName, err = FormatName(toLayer.NameFormat, subs)
						if err != nil {
							return nil, err
						}
					} else {
						instName = fmt.Sprintf(toLayer.NameFormat, rail, g, pod) // legacy positional order
					}
					toInstances = append(toInstances, instName)

					downSize := (len(grp) + conn.Breakout - 1) / conn.Breakout
					base := nextFreePort[instName]
					if base+downSize > cfg.PortsPerSwitch {
						return nil, fmt.Errorf("layer %s instance %s exceeds port capacity (%d > %d)",
							toLayer.Name, instName, base+downSize, cfg.PortsPerSwitch)
					}
					for localIdx, srvIdx := range grp {
						portNum := base + localIdx/conn.Breakout
						leg := localIdx % conn.Breakout

						var srvName string
						if serverLayer.NameFormat != "" {
							srvSubs := map[string]int{}
							for k, v := range subs {
								srvSubs[k] = v
							}
							srvSubs["local"], srvSubs["local1"] = localIdx, localIdx+1
							srvSubs["global"], srvSubs["global1"] = srvIdx, srvIdx+1
							if IsNamedFormat(serverLayer.NameFormat) {
								var err error
								srvName, err = FormatName(serverLayer.NameFormat, srvSubs)
								if err != nil {
									return nil, err
								}
							} else {
								srvName = fmt.Sprintf(serverLayer.NameFormat, srvIdx, pod) // legacy positional order
							}
						} else {
							srvName = fmt.Sprintf("%s_%03d_PoD_%02d", prefix, srvIdx, pod)
						}

						row := Row{
							Role:    fmt.Sprintf("%s to %s", toLayer.DisplayName, serverLayer.DisplayName),
							Speed:   conn.Speed,
							E1Name:  instName,
							E1Type:  toLayer.DisplayName,
							E1Iface: ifaceName(cfg.InterfacePrefix, portNum, leg, hasLeg),
							E2Name:  srvName,
							E2Type:  serverLayer.DisplayName,
							E2Iface: fmt.Sprintf("nic-%02d", rail),
						}
						if serverLayer.RackSize > 0 {
							rackNum := srvIdx / serverLayer.RackSize
							row.Rack = fmt.Sprintf("Rack_%02d_PoD_%02d", rackNum, pod)
						}
						rows = append(rows, row)
					}
					nextFreePort[instName] = base + downSize
				}
			}
			instances[toLayer.Name] = toInstances
			continue
		}

		// --- mesh connection: full 1-link-per-pair mesh between two counted layers ---
		fromInstances, ok := instances[fromLayer.Name]
		if !ok {
			return nil, fmt.Errorf(
				"layer %s has no instances yet when processing %s -> %s; "+
					"connections must be listed base-layer-first in the config",
				fromLayer.Name, conn.From, conn.To)
		}

		toInstances, ok := instances[toLayer.Name]
		if !ok {
			for idx := 0; idx < toLayer.Count; idx++ {
				var name string
				if IsNamedFormat(toLayer.NameFormat) {
					subs := baseSubs(pod, toLayer.Plane)
					subs["idx"], subs["idx1"] = idx, idx+1
					var err error
					name, err = FormatName(toLayer.NameFormat, subs)
					if err != nil {
						return nil, err
					}
				} else if toLayer.Shared {
					name = fmt.Sprintf(toLayer.NameFormat, idx) // legacy positional order
				} else {
					name = fmt.Sprintf(toLayer.NameFormat, idx, pod) // legacy positional order
				}
				toInstances = append(toInstances, name)
			}
			instances[toLayer.Name] = toInstances
		}

		fromBases := make([]int, len(fromInstances))
		for fi, name := range fromInstances {
			fromBases[fi] = nextFreePort[name]
			if fromBases[fi]+len(toInstances) > cfg.PortsPerSwitch {
				return nil, fmt.Errorf("layer %s instance %s exceeds port capacity (%d > %d)",
					fromLayer.Name, name, fromBases[fi]+len(toInstances), cfg.PortsPerSwitch)
			}
		}
		toBases := make([]int, len(toInstances))
		for ti, name := range toInstances {
			if toLayer.Shared {
				toBases[ti] = (pod - 1) * len(fromInstances)
			} else {
				toBases[ti] = nextFreePort[name]
			}
			if toBases[ti]+len(fromInstances) > cfg.PortsPerSwitch {
				return nil, fmt.Errorf(
					"PoD %d exceeds port capacity on layer %s instance %s (port %d >= %d); "+
						"add more %s instances before onboarding this PoD",
					pod, toLayer.Name, name, toBases[ti]+len(fromInstances), cfg.PortsPerSwitch, toLayer.Name)
			}
		}

		legs := 1
		if hasLeg {
			legs = conn.Breakout
		}
		for fi, fromName := range fromInstances {
			for ti, toName := range toInstances {
				fromPort := fromBases[fi] + ti
				toPort := toBases[ti] + fi
				for leg := 0; leg < legs; leg++ {
					row := Row{
						Role:    fmt.Sprintf("%s to %s", toLayer.DisplayName, fromLayer.DisplayName),
						Speed:   conn.Speed,
						E1Name:  fromName,
						E1Type:  fromLayer.DisplayName,
						E1Iface: ifaceName(cfg.InterfacePrefix, fromPort, leg, hasLeg),
						E2Name:  toName,
						E2Type:  toLayer.DisplayName,
						E2Iface: ifaceName(cfg.InterfacePrefix, toPort, leg, hasLeg),
					}
					rows = append(rows, row)
				}
			}
		}

		for fi, name := range fromInstances {
			nextFreePort[name] = fromBases[fi] + len(toInstances)
		}
		if !toLayer.Shared {
			for ti, name := range toInstances {
				nextFreePort[name] = toBases[ti] + len(fromInstances)
			}
		}
	}

	return rows, nil
}
