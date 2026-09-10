package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// This file implements a small, dependency-free parser for the restricted
// YAML subset used by cluster topology config files. It supports exactly
// this shape (no flow-style {}/[], no multi-document files, no anchors):
//
//   top_level_key: value
//   layers:
//     - name: server
//       count: 372
//       rails: 8
//     - name: t1
//       name_format: "T1_Rail_%02d_Leaf_%02d_PoD_%02d"
//       striping:
//         nodes_per_stripe: 62
//   connections:
//     - from: server
//       to: t1
//       speed: 400G
//       breakout: 2
//
// Each list item may contain at most one level of nested map (e.g. "striping:").
// If your config needs more than this, use a real YAML library instead.

type Layer struct {
	Name           string
	Count          int    // 0 if unset (derived layers don't need it)
	Rails          int    // only meaningful on the base/server layer
	NameFormat     string // %(name)0Nd named-placeholder pattern, or legacy positional %d
	DisplayName    string // human label for Role/Type columns; defaults to Title(Name)
	NodesPerStripe int    // >0 marks this as a "striped" layer (fed by a striping connection)
	Shared         bool   // true = one shared pool across all PoDs (e.g. T3)
	RackSize       int    // only meaningful on the base/server layer; 0 = no Rack column
	Plane          int    // naming label only (no port-math effect); defaults to 1
}

type Connection struct {
	From     string
	To       string
	Speed    string
	Breakout int // 1 = native, no breakout. 2 = split each port into 2 legs.
}

type Config struct {
	Layers          []Layer
	Connections     []Connection
	InterfacePrefix string
	PortsPerSwitch  int
}

func defaultConfig() Config {
	return Config{
		InterfacePrefix: "et-0/0/",
		PortsPerSwitch:  64,
	}
}

// rawMap is a flat map of a YAML list item's keys to string values, with
// nested-map keys flattened as "parent.child".
type rawMap map[string]string

func indentOf(s string) int {
	n := 0
	for _, c := range s {
		if c != ' ' {
			break
		}
		n++
	}
	return n
}

func stripInlineComment(s string) string {
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
			continue
		}
		if c == '#' {
			return s[:i]
		}
	}
	return s
}

func stripQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func splitKV(s string) (string, string) {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return strings.TrimSpace(s), ""
	}
	k := strings.TrimSpace(s[:idx])
	v := stripQuotes(strings.TrimSpace(s[idx+1:]))
	return k, v
}

// parseList reads a top-level "section:" block made of "- key: value" items.
// lines[i] is the line right after the "section:" header.
func parseList(lines []string, i int) ([]rawMap, int) {
	var items []rawMap
	for i < len(lines) {
		l := lines[i]
		ind := indentOf(l)
		t := strings.TrimSpace(l)
		if ind == 0 {
			break
		}
		if !strings.HasPrefix(t, "- ") {
			i++
			continue
		}
		item := rawMap{}
		itemIndent := ind
		k, v := splitKV(strings.TrimPrefix(t, "- "))
		if k != "" {
			item[k] = v
		}
		i++
		for i < len(lines) {
			l2 := lines[i]
			ind2 := indentOf(l2)
			t2 := strings.TrimSpace(l2)
			if ind2 <= itemIndent {
				break
			}
			if strings.HasPrefix(t2, "- ") {
				break
			}
			k2, v2 := splitKV(t2)
			if v2 == "" {
				// nested map: absorb its children as "k2.child"
				nestIndent := ind2
				i++
				for i < len(lines) {
					l3 := lines[i]
					ind3 := indentOf(l3)
					if ind3 <= nestIndent {
						break
					}
					k3, v3 := splitKV(strings.TrimSpace(l3))
					item[k2+"."+k3] = v3
					i++
				}
			} else {
				item[k2] = v2
				i++
			}
		}
		items = append(items, item)
	}
	return items, i
}

func parseYAMLLite(path string) ([]rawMap, []rawMap, map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := stripInlineComment(sc.Text())
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, nil, err
	}

	top := map[string]string{}
	var layersRaw, connsRaw []rawMap

	i := 0
	for i < len(lines) {
		line := lines[i]
		indent := indentOf(line)
		trimmed := strings.TrimSpace(line)
		if indent != 0 {
			i++
			continue
		}
		if strings.HasSuffix(trimmed, ":") {
			section := strings.TrimSuffix(trimmed, ":")
			var items []rawMap
			items, i = parseList(lines, i+1)
			switch section {
			case "layers":
				layersRaw = items
			case "connections":
				connsRaw = items
			}
			continue
		}
		k, v := splitKV(trimmed)
		if k != "" {
			top[k] = v
		}
		i++
	}
	return layersRaw, connsRaw, top, nil
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// IsNamedFormat reports whether template uses %(name)0Nd-style named
// tokens rather than plain positional %d tokens -- lets older configs
// (which used positional formats) keep working unchanged.
var namedTokenRe = regexp.MustCompile(`%\((\w+)\)`)

func IsNamedFormat(template string) bool {
	return namedTokenRe.MatchString(template)
}

var namedFmtTokenRe = regexp.MustCompile(`%\((\w+)\)(0?)(\d*)d`)

// FormatName substitutes %(key)0Nd-style tokens using subs, matching
// Python's "%(key)0Nd" % dict formatting semantics: 0N pads with zeros to
// width N; a bare %(key)d has no padding.
func FormatName(template string, subs map[string]int) (string, error) {
	var missing error
	result := namedFmtTokenRe.ReplaceAllStringFunc(template, func(match string) string {
		m := namedFmtTokenRe.FindStringSubmatch(match)
		key := m[1]
		zeroPad := m[2] == "0"
		widthStr := m[3]
		val, ok := subs[key]
		if !ok {
			missing = fmt.Errorf("name_format references unknown field %q", key)
			return match
		}
		s := strconv.Itoa(val)
		if zeroPad && widthStr != "" {
			width, _ := strconv.Atoi(widthStr)
			for len(s) < width {
				s = "0" + s
			}
		}
		return s
	})
	if missing != nil {
		return "", missing
	}
	return result, nil
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func LoadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	layersRaw, connsRaw, top, err := parseYAMLLite(path)
	if err != nil {
		return cfg, err
	}
	if v, ok := top["interface_prefix"]; ok && v != "" {
		cfg.InterfacePrefix = v
	}
	if v, ok := top["ports_per_switch"]; ok && v != "" {
		cfg.PortsPerSwitch = atoiDefault(v, cfg.PortsPerSwitch)
	}

	for _, m := range layersRaw {
		l := Layer{
			Name:       m["name"],
			Count:      atoiDefault(m["count"], 0),
			Rails:      atoiDefault(m["rails"], 0),
			NameFormat: m["name_format"],
			Shared:     m["shared"] == "true",
			RackSize:   atoiDefault(m["rack_size"], 0),
			Plane:      atoiDefault(m["plane"], 1),
		}
		if v, ok := m["striping.nodes_per_stripe"]; ok {
			l.NodesPerStripe = atoiDefault(v, 0)
		}
		if v, ok := m["display_name"]; ok && v != "" {
			l.DisplayName = v
		} else {
			l.DisplayName = titleCase(l.Name)
		}
		if l.Name == "" {
			return cfg, fmt.Errorf("layer missing required 'name' field")
		}
		if l.NameFormat == "" && l.Rails == 0 {
			// Only non-base layers get an auto-generated default. The base
			// (server) layer is intentionally left with an empty
			// NameFormat here -- it falls back to -prefix-based naming in
			// GenerateRows unless the config explicitly sets one.
			switch {
			case l.NodesPerStripe > 0:
				l.NameFormat = strings.ToUpper(l.Name) + "_Rail_%(rail)02d_stripe_%(stripe)02d_PoD_%(pod)02d"
			case l.Shared:
				l.NameFormat = strings.ToUpper(l.Name) + "_%(idx)02d"
			default:
				l.NameFormat = strings.ToUpper(l.Name) + "_%(idx)02d_PoD_%(pod)02d"
			}
		}
		cfg.Layers = append(cfg.Layers, l)
	}

	for _, m := range connsRaw {
		c := Connection{
			From:     m["from"],
			To:       m["to"],
			Speed:    m["speed"],
			Breakout: atoiDefault(m["breakout"], 1),
		}
		if c.From == "" || c.To == "" {
			return cfg, fmt.Errorf("connection missing 'from' or 'to'")
		}
		cfg.Connections = append(cfg.Connections, c)
	}

	if len(cfg.Layers) == 0 {
		return cfg, fmt.Errorf("config has no layers")
	}
	if len(cfg.Connections) == 0 {
		return cfg, fmt.Errorf("config has no connections")
	}
	return cfg, nil
}
