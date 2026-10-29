package cmd

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// probeTimeout bounds `finbot health`.
const probeTimeout = 5 * time.Second

// probe GETs /healthz on the local server at addr (HTTP_ADDR, default :8080)
// and returns 0 on 200, else 1. It is the container healthcheck, so the image
// needs no curl.
func probe(addr string, stderr io.Writer) int {
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(stderr, "finbot health: HTTP_ADDR must be host:port\n")
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: probeTimeout}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintf(stderr, "finbot health: %v\n", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "finbot health: %s\n", resp.Status)
		return 1
	}
	return 0
}
