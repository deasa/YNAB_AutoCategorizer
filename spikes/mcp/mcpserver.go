package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type helloArgs struct{}

type helloResult struct {
	UserID      string `json:"user_id" jsonschema:"hashed identifier of the signed-in user"`
	DisplayName string `json:"display_name,omitempty" jsonschema:"Google display name"`
}

// hello returns the caller's identity from the auth context, proving that the
// token's user reaches tool handlers. It never returns the raw email.
func hello(ctx context.Context, req *mcp.CallToolRequest, _ helloArgs) (*mcp.CallToolResult, helloResult, error) {
	ti := req.Extra.TokenInfo
	if ti == nil {
		return nil, helloResult{}, fmt.Errorf("no auth context")
	}
	name, _ := ti.Extra["name"].(string)
	return nil, helloResult{UserID: ti.UserID, DisplayName: name}, nil
}

func newMCPServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "finbot-spike", Version: "0.0.1"}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "hello",
		Description: "Say hello and report who you are signed in as.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, hello)
	return s
}

// mcpHandler is the Streamable HTTP endpoint behind bearer-token auth. A
// request without a valid token gets 401 with a WWW-Authenticate header that
// points at the protected-resource metadata; a token without the mcp scope
// gets 403. One server is shared by all sessions; the SDK binds each session
// to the TokenInfo.UserID that created it.
func mcpHandler(verify auth.TokenVerifier, resourceMetadataURL string) http.Handler {
	srv := newMCPServer()
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute})
	return auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: resourceMetadataURL,
		Scopes:              []string{scopeMCP},
	})(h)
}
