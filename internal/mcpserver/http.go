package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Arvo-AI/lastwords/internal/client"
)

// Handler serves MCP over streamable HTTP at any path, for agents that run
// inside the cluster (HolmesGPT, kagent, Aurora) and cannot start a local
// stdio process. Every request must carry "Authorization: Bearer <token>".
func Handler(token string, connect func() (*client.Client, error), version string) http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "lastwords", Title: "lastwords", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	Register(s, lazy(connect))
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(strings.TrimSpace(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="lastwords"`)
			http.Error(w, "missing or wrong bearer token (the token is in the lastwords-api Secret)", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// RunHTTP serves Handler on addr at /mcp, plus an unauthenticated /healthz,
// until ctx is done.
func RunHTTP(ctx context.Context, addr, token string, connect func() (*client.Client, error), version string) error {
	if token == "" {
		return errors.New("no API token: set LASTWORDS_MCP_TOKEN (the chart passes the lastwords-api Secret)")
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", Handler(token, connect, version))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shut)
	}
}
