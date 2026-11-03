package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Arvo-AI/lastwords/internal/client"
)

// An MCP client (or a registry probing the server) starts it before any
// kubeconfig works. The tools must still be listed, and calls must explain
// the problem instead of the process exiting.
func TestServesToolsWithoutACluster(t *testing.T) {
	ctx := context.Background()
	s := mcp.NewServer(&mcp.Implementation{Name: "lastwords", Version: "test"}, nil)
	Register(s, lazy(func() (*client.Client, error) { return nil, errors.New("no kubeconfig") }))
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 4 {
		t.Fatalf("tools/list without a cluster: %v tools, err %v", len(tools.Tools), err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "lastwords_list_captures", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "no kubeconfig") {
		t.Fatalf("want an error result naming the cause, got %+v", res.Content)
	}
}
