package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Arvo-AI/lastwords/internal/client"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connectHTTP(ctx context.Context, url, token string) (*mcp.ClientSession, error) {
	tr := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token}}}
	return mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, tr, nil)
}

// In-cluster agents reach lastwords over HTTP. Captures can hold logs and
// heap contents, so the endpoint must refuse callers without the token.
func TestHTTPRequiresToken(t *testing.T) {
	ctx := context.Background()
	noCluster := func() (*client.Client, error) { return nil, errors.New("no cluster") }
	srv := httptest.NewServer(Handler("s3cret", noCluster, "test"))
	defer srv.Close()

	if _, err := connectHTTP(ctx, srv.URL, "wrong"); err == nil {
		t.Fatal("connected with a wrong token")
	}

	cs, err := connectHTTP(ctx, srv.URL, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 4 {
		t.Fatalf("tools/list over HTTP: %v, err %v", tools, err)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "lastwords_watchlist", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "no cluster") {
		t.Fatalf("want the connection error in the result, got %+v", res.Content)
	}
}
