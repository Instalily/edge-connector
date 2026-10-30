// Command edgectl issues the certificates, and the enrollment tokens, a
// customer's hub and connectors use.
//
// Once per customer, keeping every .key file in Instalily's secret store:
//
//	edgectl ca -name acme -prefix hub-ca -out pki/
//	edgectl ca -name acme -prefix connector-ca -out pki/
//	edgectl issue -ca pki/ -ca-prefix hub-ca -name hub -dns acme.connect.instalily.ai -server -out pki/
//
// Once for all customers:
//
//	edgectl token-key -out pki/
//
// For each connector, a token to hand the customer:
//
//	edgectl token -key pki/token.key -hub-ca pki/hub-ca.crt -identity connector-1 -expires 24h
//
// Connectors can still get certificates by hand instead of enrolling:
//
//	edgectl issue -ca pki/ -ca-prefix connector-ca -name connector-1 -client -out pki/
//
// ca writes <prefix>.crt and <prefix>.key; issue writes <name>.crt and
// <name>.key. A server certificate's .crt also holds its CA, which
// connectors pin when they enroll.
package main

import (
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Instalily/edge-connector/internal/enroll"
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
	case "token-key":
		err = tokenKey(os.Args[2:])
	case "token":
		err = token(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "edgectl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  edgectl ca -name NAME [-prefix ca] -out DIR
  edgectl issue -ca DIR [-ca-prefix ca] -name NAME (-server|-client) -out DIR
  edgectl token-key -out DIR
  edgectl token -key FILE -hub-ca FILE -identity NAME [-expires 24h]`)
	os.Exit(2)
}

func ca(args []string) error {
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	name := fs.String("name", "", "customer name, used in the CA's common name")
	prefix := fs.String("prefix", "ca", "output file name: hub-ca or connector-ca")
	out := fs.String("out", ".", "output directory")
	days := fs.Int("days", 730, "validity in days")
	fs.Parse(args)

	// "hub-ca" names "acme edge-connector hub CA".
	cn := *name + " edge-connector CA"
	if kind := strings.TrimSuffix(*prefix, "-ca"); kind != *prefix {
		cn = fmt.Sprintf("%s edge-connector %s CA", *name, kind)
	}
	p, err := pki.NewCA(cn, time.Duration(*days)*day)
	if err != nil {
		return err
	}
	return write(p, *out, *prefix)
}

func issue(args []string) error {
	fs := flag.NewFlagSet("issue", flag.ExitOnError)
	caDir := fs.String("ca", ".", "directory holding the CA")
	caPrefix := fs.String("ca-prefix", "ca", "the CA's file name in that directory")
	name := fs.String("name", "", "common name, also the output file name")
	dns := fs.String("dns", "", "comma-separated DNS names")
	ips := fs.String("ip", "", "comma-separated IP addresses")
	server := fs.Bool("server", false, "a hub certificate")
	client := fs.Bool("client", false, "a connector certificate")
	out := fs.String("out", ".", "output directory")
	days := fs.Int("days", 90, "validity in days")
	fs.Parse(args)

	ca, err := pki.Load(filepath.Join(*caDir, *caPrefix+".crt"), filepath.Join(*caDir, *caPrefix+".key"))
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
	if *server {
		return write(p, *out, *name, ca.Cert)
	}
	return write(p, *out, *name)
}

func tokenKey(args []string) error {
	fs := flag.NewFlagSet("token-key", flag.ExitOnError)
	out := fs.String("out", ".", "output directory")
	fs.Parse(args)

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	priv, pub := filepath.Join(*out, "token.key"), filepath.Join(*out, "token.pub")
	if err := enroll.GenerateTokenKey(priv, pub); err != nil {
		return err
	}
	fmt.Printf("wrote %s (signs tokens: keep it in the secret store) and %s (give it to hubs)\n", priv, pub)
	return nil
}

func token(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	keyFile := fs.String("key", "", "token signing key, from token-key")
	hubCAFile := fs.String("hub-ca", "", "the CA that issued the customer's hub certificate")
	identity := fs.String("identity", "", "the connector's name, e.g. connector-1")
	expires := fs.Duration("expires", 24*time.Hour, "how long the token can be used, at most 168h")
	fs.Parse(args)

	key, err := enroll.LoadTokenSigner(*keyFile)
	if err != nil {
		return err
	}
	hubCA, err := pki.LoadCert(*hubCAFile)
	if err != nil {
		return err
	}
	claims, err := enroll.NewClaims(*identity, hubCA, time.Now(), *expires)
	if err != nil {
		return err
	}
	tok, err := enroll.Sign(key, claims)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "token %s for %s, valid until %s\n", claims.ID, claims.Identity, time.Unix(claims.Expires, 0).UTC().Format(time.RFC3339))
	fmt.Println(tok)
	return nil
}

func write(p *pki.Pair, dir, name string, chain ...*x509.Certificate) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cert, key := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := p.Write(cert, key, chain...); err != nil {
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
