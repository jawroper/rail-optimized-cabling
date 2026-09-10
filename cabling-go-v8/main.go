// Command cabling-go generates a cabling assignment CSV for one PoD of a
// rail-optimized AI cluster, driven entirely by a YAML topology config.
//
// Usage:
//
//	cabling-go -config cluster.yaml -pod 1 -prefix Srvr -output pod01_cabling.csv
//	cabling-go -config cluster.yaml -pod 2 -prefix Srvr -output pod02_cabling.csv -base-ip 10.2.0.0
//
// See cluster_800g.yaml / cluster_400g.yaml for example configs and the
// README for the config file format.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"net"
	"os"
)

var version = "v8"

var csvHeader = []string{
	"Role", "Speed",
	"Endpoint 1 Name", "Endpoint 1 Type", "Endpoint 1 Interface ID",
	"Endpoint 1 Interface Name", "Endpoint 1 IP",
	"Endpoint 2 Name", "Endpoint 2 Type", "Endpoint 2 Interface ID",
	"Endpoint 2 Interface Name", "Endpoint 2 IP",
	"Rack",
}

func rowToRecord(r Row) []string {
	return []string{
		r.Role, r.Speed,
		r.E1Name, r.E1Type, r.E1IfaceID, r.E1Iface, r.E1IP,
		r.E2Name, r.E2Type, r.E2IfaceID, r.E2Iface, r.E2IP,
		r.Rack,
	}
}

// Boundary-safe /31 allocation: within each /24 block, only x.x.x.2 through
// x.x.x.253 are used (skipping .0, .1, .254, .255), giving 126 usable /31
// pairs per block. A pair is always fully contained in one block -- it never
// straddles two /24s. Once a block's 126 pairs are exhausted, allocation
// rolls forward to the next /24.
const pairsPerBlock = 126

// assignIPs allocates sequential /31 point-to-point subnets, two addresses
// per link, starting from the /24 block containing baseIP (baseIP's last
// octet is ignored -- it only marks which block to start from).
func assignIPs(rows []Row, baseIP string) error {
	ip := net.ParseIP(baseIP).To4()
	if ip == nil {
		return fmt.Errorf("invalid --base-ip: %q", baseIP)
	}
	val := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	baseBlock := val &^ 0xFF
	for i := range rows {
		blockOffset := uint32(i / pairsPerBlock)
		localIdx := uint32(i % pairsPerBlock)
		net1 := baseBlock + blockOffset*256 + 2 + localIdx*2
		net2 := net1 + 1
		rows[i].E1IP = fmt.Sprintf("%d.%d.%d.%d/31", byte(net1>>24), byte(net1>>16), byte(net1>>8), byte(net1))
		rows[i].E2IP = fmt.Sprintf("%d.%d.%d.%d/31", byte(net2>>24), byte(net2>>16), byte(net2>>8), byte(net2))
	}
	return nil
}

func main() {
	configPath := flag.String("config", "", "path to the YAML topology config (required)")
	pod := flag.Int("pod", 1, "PoD number (1, 2, ...)")
	prefix := flag.String("prefix", "Srvr", "server name prefix")
	output := flag.String("output", "", "output CSV path (default: podNN_cabling.csv)")
	baseIP := flag.String("base-ip", "192.168.0.2", "which /24 block to start allocating /31 links from (last octet is ignored; allocation always starts at x.x.x.2)")
	stripeMode := flag.String("stripe-mode", "even", "server striping: 'even' (as equal as possible, e.g. 62,62,62,62,62,62) or 'packed' (fill each stripe to capacity first, e.g. 64,64,64,64,64,52)")
	flag.Parse()

	if *stripeMode != "even" && *stripeMode != "packed" {
		fmt.Fprintf(os.Stderr, "error: -stripe-mode must be 'even' or 'packed', got %q\n", *stripeMode)
		os.Exit(2)
	}

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "error: -config is required")
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	rows, err := GenerateRows(cfg, *pod, *prefix, *stripeMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error generating topology: %v\n", err)
		os.Exit(1)
	}

	if err := assignIPs(rows, *baseIP); err != nil {
		fmt.Fprintf(os.Stderr, "error assigning IPs: %v\n", err)
		os.Exit(1)
	}

	outPath := *output
	if outPath == "" {
		outPath = fmt.Sprintf("pod%02d_cabling.csv", *pod)
	}

	f, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating output file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	w.UseCRLF = true
	if err := w.Write(csvHeader); err != nil {
		fmt.Fprintf(os.Stderr, "error writing CSV header: %v\n", err)
		os.Exit(1)
	}
	for _, r := range rows {
		if err := w.Write(rowToRecord(r)); err != nil {
			fmt.Fprintf(os.Stderr, "error writing CSV row: %v\n", err)
			os.Exit(1)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fmt.Fprintf(os.Stderr, "error flushing CSV: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Wrote %d cabling rows to %s\n", len(rows), outPath)
}
