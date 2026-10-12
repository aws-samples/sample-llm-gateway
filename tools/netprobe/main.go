// Command netprobe is a development aid for private-networking labs: from inside a pod it
// resolves a list of hostnames and attempts HTTPS connections, printing what each one reached.
// It exists because the gateway image has no shell and public images cannot be pulled into a
// VPC with no internet egress.
//
//	netprobe -resolve host1,host2 -get https://a/,https://b/ -timeout 6s -wait 20s
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	resolve := flag.String("resolve", "", "comma-separated hostnames to resolve")
	get := flag.String("get", "", "comma-separated URLs to GET (status code or error is printed)")
	timeout := flag.Duration("timeout", 6*time.Second, "per-attempt timeout")
	wait := flag.Duration("wait", 0, "sleep before starting (lets NetworkPolicy land on a fresh pod)")
	flag.Parse()
	if *wait > 0 {
		fmt.Printf("waiting %s for network policy\n", *wait)
		time.Sleep(*wait)
	}
	for _, h := range split(*resolve) {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		addrs, err := net.DefaultResolver.LookupHost(ctx, h)
		cancel()
		if err != nil {
			fmt.Printf("resolve %-70s ERR %v\n", h, err)
			continue
		}
		fmt.Printf("resolve %-70s %s\n", h, strings.Join(addrs, " "))
	}
	// Do not follow redirects: STS answers "/" with a 302 to a public marketing page, which would
	// make a reachable VPC endpoint look like a timeout.
	client := &http.Client{Timeout: *timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // probe only cares about reachability
		DialContext:     (&net.Dialer{Timeout: *timeout}).DialContext,
	}}
	for _, u := range split(*get) {
		start := time.Now()
		resp, err := client.Get(u)
		if err != nil {
			fmt.Printf("get     %-70s ERR after %s: %v\n", u, time.Since(start).Round(time.Millisecond), shortErr(err))
			continue
		}
		resp.Body.Close()
		fmt.Printf("get     %-70s HTTP %d in %s (remote %s)\n", u, resp.StatusCode, time.Since(start).Round(time.Millisecond), resp.Request.URL.Host)
	}
	os.Exit(0)
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i > 0 && len(s)-i < 60 {
		return s[i+2:]
	}
	return s
}
