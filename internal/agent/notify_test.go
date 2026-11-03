package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Arvo-AI/lastwords/internal/store"
)

func TestWebhookAndSlackPayloads(t *testing.T) {
	var gotHook WebhookPayload
	var gotSlack map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/slack" {
			_ = json.Unmarshal(b, &gotSlack)
		} else {
			_ = json.Unmarshal(b, &gotHook)
		}
	}))
	defer srv.Close()
	cfg := Defaults()
	cfg.WebhookURL, cfg.SlackWebhookURL, cfg.Cluster = srv.URL+"/hook", srv.URL+"/slack", "prod"
	n := newNotifier(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := &store.Capture{ID: "20260101T000000Z-abc", Kind: store.KindOOMKill, Namespace: "shop", Pod: "checkout-1", Headline: "checkout-1/app was OOMKilled", Findings: []string{"Memory rose"}}
	n.Send(context.Background(), c, "# summary")
	if gotHook.Event != "oomkill" || gotHook.Capture.ID != c.ID || gotHook.Cluster != "prod" || gotHook.Summary != "# summary" {
		t.Fatalf("webhook payload: %+v", gotHook)
	}
	if !strings.Contains(gotSlack["text"], "kubectl lastwords show 20260101T000000Z-abc") || !strings.Contains(gotSlack["text"], "prod") {
		t.Fatalf("slack text: %q", gotSlack["text"])
	}
}

func TestNotifyKindsFilter(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	cfg := Defaults()
	cfg.WebhookURL, cfg.NotifyKinds = srv.URL, []string{"oomkill"}
	n := newNotifier(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.Send(context.Background(), &store.Capture{Kind: store.KindPressure}, "")
	if called {
		t.Fatal("pressure captures should not notify when only oomkill is selected")
	}
}
