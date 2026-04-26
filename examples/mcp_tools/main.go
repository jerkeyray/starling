package main

import (
	"context"
	"encoding/json"
	"fmt"

	toolmcp "github.com/jerkeyray/starling/tool/mcp"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx := context.Background()

	server := gomcp.NewServer(&gomcp.Implementation{Name: "demo-mcp", Version: "v0.0.1"}, nil)
	server.AddTool(&gomcp.Tool{
		Name:        "greet",
		Description: "Return a greeting for a user.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`),
	}, func(_ context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		var args struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &gomcp.CallToolResult{
			StructuredContent: map[string]any{"greeting": "hello " + args.Name},
		}, nil
	})

	clientTransport, serverTransport := gomcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	client, err := toolmcp.New(ctx, clientTransport, toolmcp.WithToolNamePrefix("mcp_"))
	if err != nil {
		panic(err)
	}
	defer client.Close()

	tools, err := client.Tools(ctx)
	if err != nil {
		panic(err)
	}
	out, err := tools[0].Execute(ctx, json.RawMessage(`{"name":"starling"}`))
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
}
