// Command edgectl issues tunnel certificates by hand, until connectors enroll
// themselves.
//
//	edgectl ca -name acme -out pki/
//	edgectl issue -ca pki/ -name hub -dns acme.connect.instalily.ai -server -out pki/
//	edgectl issue -ca pki/ -name connector-1 -client -out pki/
//
// ca writes ca.crt and ca.key; issue writes <name>.crt and <name>.key.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Instalily/edge-connector/internal/pki"
)

const day = 24 * time.Hour

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "ca":
		err = ca(os.Args[2:])
	case "issue":
		err = issue(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "edgectl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: edgectl ca -name NAME -out DIR | edgectl issue -ca DIR -name NAME (-server|-client) -out DIR")
	os.Exit(2)
}

func ca(args []string) error {
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	name := fs.String("name", "", "customer name, used as the CA's common name")
	out := fs.String("out", ".", "output directory")
	days := fs.Int("days", 730, "validity in days")
	fs.Parse(args)

	p, err := pki.NewCA(*name+" edge-connector CA", time.Duration(*days)*day)
	if err != nil {
		return err
	}
	return write(p, *out, "ca")
}

func issue(args []string) error {
	fs := flag.NewFlagSet("issue", flag.ExitOnError)
	caDir := fs.String("ca", ".", "directory holding ca.crt and ca.key")
	name := fs.String("name", "", "common name, also the output file name")
	dns := fs.String("dns", "", "comma-separated DNS names")
	ips := fs.String("ip", "", "comma-separated IP addresses")
	server := fs.Bool("server", false, "a hub certificate")
	client := fs.Bool("client", false, "a connector certificate")
	out := fs.String("out", ".", "output directory")
	days := fs.Int("days", 90, "validity in days")
	fs.Parse(args)

	ca, err := pki.Load(filepath.Join(*caDir, "ca.crt"), filepath.Join(*caDir, "ca.key"))
	if err != nil {
		return err
	}
	opts := pki.LeafOptions{
		CommonName: *name,
		DNSNames:   split(*dns),
		Server:     *server,
		Client:     *client,
		Validity:   time.Duration(*days) * day,
	}
	for _, s := range split(*ips) {
		ip := net.ParseIP(s)
		if ip == nil {
			return fmt.Errorf("invalid IP %q", s)
		}
		opts.IPs = append(opts.IPs, ip)
	}
	p, err := ca.Issue(opts)
	if err != nil {
		return err
	}
	return write(p, *out, *name)
}

func write(p *pki.Pair, dir, name string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cert, key := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := p.Write(cert, key); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s (expires %s)\n", cert, key, p.Cert.NotAfter.Format(time.DateOnly))
	return nil
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
