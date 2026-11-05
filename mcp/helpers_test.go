package mcp

import (
	"encoding/json"
	"net/netip"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func decode[T any](t *testing.T, res *sdkmcp.CallToolResult) Response[T] {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out Response[T]
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return out
}

func mustAddrPort(t *testing.T, s string) netip.AddrPort {
	t.Helper()
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		t.Fatal(err)
	}
	return ap
}
