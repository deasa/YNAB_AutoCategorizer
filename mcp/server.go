// Package mcp is finbot's MCP endpoint: a Streamable HTTP resource server behind
// bearer-token auth. Tools read SQLite only and never call YNAB, so they can't
// touch the rate budget. Nothing logged here carries a token, email or id.
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	finauth "github.com/deasa/YNAB_AutoCategorizer/auth"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	// Path is the MCP endpoint.
	Path = "/mcp"

	sessionTimeout = 30 * time.Minute
)

// Options is what NewHandler needs.
type Options struct {
	Verify              sdkauth.TokenVerifier
	ResourceMetadataURL string
	Version             string

	DB     *store.DB
	PlanID string // the active plan
	// StaleAfter is the sync age past which data_freshness reports stale.
	StaleAfter time.Duration
	Log        *slog.Logger
	// Redact scrubs secrets from an error before it's logged.
	Redact func(string) string
	Now    func() time.Time // defaults to time.Now
}

// NewHandler returns the MCP endpoint. A request without a valid token gets 401
// with a WWW-Authenticate header pointing at the protected-resource metadata; a
// token without the MCP scope gets 403.
//
// The go-sdk's DNS-rebinding guard stays on. It refuses a public Host only on a
// connection that arrived at a loopback address, so finbot must listen on the
// container interface (see MCP_LISTEN_ADDR).
func NewHandler(o Options) http.Handler {
	return newHandler(o, registry)
}

func newHandler(o Options, defs []toolDef) http.Handler {
	srv := newServer(o, defs)
	// One server for all sessions; the SDK binds each session to the
	// TokenInfo.UserID that created it. The handler calls this on every request.
	h := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv },
		&sdkmcp.StreamableHTTPOptions{SessionTimeout: sessionTimeout})
	return sdkauth.RequireBearerToken(o.Verify, &sdkauth.RequireBearerTokenOptions{
		ResourceMetadataURL: o.ResourceMetadataURL,
		Scopes:              []string{finauth.ScopeMCP},
	})(h)
}

func newServer(o Options, defs []toolDef) *sdkmcp.Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Redact == nil {
		o.Redact = func(s string) string { return s }
	}
	s := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "finbot", Version: o.Version}, nil)
	scopes := make(map[string]string, len(defs))
	for _, d := range defs {
		d.add(s, &o)
		scopes[d.name] = d.scope
	}
	s.AddReceivingMiddleware(limitToToken(scopes))
	return s
}

// limitToToken shows and runs only the tools the caller's token permits. It
// checks each request's own token, not the one that opened the session, so a
// narrower token in a live session gets the narrower set. No token, no tools.
func limitToToken(scopes map[string]string) sdkmcp.Middleware {
	return func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, req sdkmcp.Request) (sdkmcp.Result, error) {
			var info *sdkauth.TokenInfo
			if extra := req.GetExtra(); extra != nil {
				info = extra.TokenInfo
			}
			permitted := func(tool string) bool {
				return info != nil && slices.Contains(info.Scopes, scopes[tool])
			}
			switch method {
			case "tools/call":
				if call, ok := req.(*sdkmcp.CallToolRequest); ok {
					if _, known := scopes[call.Params.Name]; known && !permitted(call.Params.Name) {
						// The same error as a tool that doesn't exist.
						return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams,
							Message: fmt.Sprintf("unknown tool %q", call.Params.Name)}
					}
				}
			case "tools/list":
				res, err := next(ctx, method, req)
				if err != nil {
					return res, err
				}
				if list, ok := res.(*sdkmcp.ListToolsResult); ok {
					list.Tools = slices.DeleteFunc(slices.Clone(list.Tools), func(t *sdkmcp.Tool) bool {
						return !permitted(t.Name)
					})
				}
				return res, nil
			}
			return next(ctx, method, req)
		}
	}
}
