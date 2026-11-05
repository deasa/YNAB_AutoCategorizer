package mcp

import (
	"context"
	"errors"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolDef is one entry of the registry.
type toolDef struct {
	name  string
	scope string // the token scope that permits this tool
	add   func(*sdkmcp.Server, *Options)
}

// registry is every tool the server offers. Each tools file adds itself from
// init, so a new tool only adds a file.
var registry []toolDef

// handler is a tool's body. It returns only its data; registering it adds the
// data freshness every tool response must carry.
type handler[In, Out any] func(ctx context.Context, o *Options, req *sdkmcp.CallToolRequest, in In) (Out, error)

// Response is the shape of every tool's result.
type Response[T any] struct {
	Data          T         `json:"data"`
	DataFreshness Freshness `json:"data_freshness" jsonschema:"how current the local mirror behind this answer is"`
}

// register adds a tool to the registry.
func register[In, Out any](scope string, tool *sdkmcp.Tool, h handler[In, Out]) {
	registry = append(registry, define(scope, tool, h))
}

func define[In, Out any](scope string, tool *sdkmcp.Tool, h handler[In, Out]) toolDef {
	if scope == "" {
		panic("mcp: tool " + tool.Name + " has no scope")
	}
	return toolDef{name: tool.Name, scope: scope, add: func(s *sdkmcp.Server, o *Options) {
		sdkmcp.AddTool(s, tool, func(ctx context.Context, req *sdkmcp.CallToolRequest, in In) (*sdkmcp.CallToolResult, Response[Out], error) {
			res, err := respond(ctx, o, tool.Name, func() (Out, error) { return h(ctx, o, req, in) })
			return nil, res, err
		})
	}}
}

// errTool is all a client sees of a failure; the detail goes to the log.
var errTool = errors.New("this tool failed; see the finbot logs")

// respond runs a tool's body and wraps its data with freshness. A failure to
// read freshness fails the call, because no answer goes out without it.
func respond[T any](ctx context.Context, o *Options, name string, body func() (T, error)) (Response[T], error) {
	data, err := body()
	if err == nil {
		var f Freshness
		if f, err = readFreshness(ctx, o); err == nil {
			return Response[T]{Data: data, DataFreshness: f}, nil
		}
	}
	o.Log.Error("mcp tool failed", "tool", name, "err", o.Redact(fmt.Sprint(err)))
	return Response[T]{}, errTool
}
