package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Arvo-AI/lastwords/internal/store"
)

type notifier struct {
	webhook string
	slack   string
	kinds   map[string]bool
	cluster string
	log     *slog.Logger
	http    *http.Client
}

func newNotifier(cfg Config, log *slog.Logger) *notifier {
	k := map[string]bool{}
	for _, x := range cfg.NotifyKinds {
		k[strings.TrimSpace(x)] = true
	}
	return &notifier{webhook: cfg.WebhookURL, slack: cfg.SlackWebhookURL, kinds: k, cluster: cfg.Cluster, log: log, http: &http.Client{Timeout: 10 * time.Second}}
}

// WebhookPayload is the JSON body POSTed to the generic webhook.
type WebhookPayload struct {
	Source   string         `json:"source"`
	Version  string         `json:"version"`
	Cluster  string         `json:"cluster,omitempty"`
	Event    string         `json:"event"`
	Headline string         `json:"headline"`
	Capture  *store.Capture `json:"capture"`
	Summary  string         `json:"summary"`
}

func (n *notifier) Send(ctx context.Context, c *store.Capture, summary string) {
	if !n.kinds[string(c.Kind)] {
		return
	}
	if n.webhook != "" {
		p := WebhookPayload{Source: "lastwords", Version: Version, Cluster: n.cluster, Event: string(c.Kind), Headline: c.Headline, Capture: c, Summary: summary}
		n.post(ctx, n.webhook, p)
	}
	if n.slack != "" {
		n.post(ctx, n.slack, slackMessage(c, n.cluster))
	}
}

func slackMessage(c *store.Capture, cluster string) map[string]any {
	icon := map[store.Kind]string{store.KindPressure: ":warning:", store.KindOOMKill: ":skull:", store.KindCrash: ":boom:", store.KindCoreDump: ":boom:", store.KindProcessOOM: ":skull:", store.KindEvicted: ":outbox_tray:"}[c.Kind]
	var b strings.Builder
	fmt.Fprintf(&b, "%s *%s*\n", icon, c.Headline)
	where := c.Namespace + "/" + c.Pod
	if cluster != "" {
		where = cluster + " · " + where
	}
	fmt.Fprintf(&b, "%s · node %s\n", where, c.Node)
	for i, f := range c.Findings {
		if i == 4 {
			break
		}
		fmt.Fprintf(&b, "• %s\n", f)
	}
	fmt.Fprintf(&b, "`kubectl lastwords show %s`", c.ID)
	return map[string]any{"text": b.String()}
}

func (n *notifier) post(ctx context.Context, url string, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "lastwords/"+Version)
		resp, err := n.http.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return
			}
			if resp.StatusCode < 500 && resp.StatusCode != 429 {
				n.log.Warn("webhook rejected", "status", resp.StatusCode)
				return
			}
		}
		select {
		case <-ctx.Done():
			n.log.Warn("webhook gave up", "err", ctx.Err())
			return
		case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
		}
	}
	n.log.Warn("webhook failed after retries")
}
