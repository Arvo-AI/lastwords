package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Arvo-AI/lastwords/internal/store"
)

var (
	capturesTotal     = promauto.NewCounterVec(prometheus.CounterOpts{Name: "lastwords_captures_total", Help: "Captures saved, by kind."}, []string{"kind"})
	captureErrors     = promauto.NewCounter(prometheus.CounterOpts{Name: "lastwords_capture_errors_total", Help: "Captures that failed to save."})
	trackedContainers = promauto.NewGauge(prometheus.GaugeOpts{Name: "lastwords_tracked_containers", Help: "Containers with a cgroup on this node."})
	crashesSuppressed = promauto.NewCounter(prometheus.CounterOpts{Name: "lastwords_crashes_suppressed_total", Help: "Crashes of crash-looping containers that were counted but not captured."})
	storeBytes        = promauto.NewGauge(prometheus.GaugeOpts{Name: "lastwords_store_bytes", Help: "Bytes used by captures on this node."})
)

// maxPartialRead caps ?max= reads, which are held in memory.
const maxPartialRead = 16 << 20

// TokenHeader carries the shared API token. It is not Authorization because
// the Kubernetes API server proxy does not forward that header.
const TokenHeader = "X-Lastwords-Token"

// NodeInfo is returned by /v1/node.
type NodeInfo struct {
	Node       string    `json:"node"`
	Version    string    `json:"version"`
	Started    time.Time `json:"started"`
	Tracked    int       `json:"trackedContainers"`
	StoreBytes int64     `json:"storeBytes"`
	CoreDumps  bool      `json:"coreDumps"`
	Threshold  float64   `json:"threshold"`
}

func (a *Agent) serveAPI() *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if a.pods == nil || !a.pods.Synced() {
			http.Error(w, "pod watcher not synced", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/node", a.auth(func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		n := len(a.conts)
		a.mu.Unlock()
		writeJSON(w, NodeInfo{Node: a.cfg.NodeName, Version: Version, Started: a.started, Tracked: n, StoreBytes: a.Store.TotalBytes(), CoreDumps: a.cfg.CoreDumps, Threshold: a.cfg.Threshold})
	}))
	mux.HandleFunc("GET /v1/watchlist", a.auth(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, a.Watchlist()) }))
	mux.HandleFunc("GET /v1/captures", a.auth(a.handleList))
	mux.HandleFunc("GET /v1/captures/{id}", a.auth(func(w http.ResponseWriter, r *http.Request) {
		c, err := a.Store.Get(r.PathValue("id"))
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, c)
	}))
	mux.HandleFunc("GET /v1/captures/{id}/files/{name}", a.auth(a.handleFile))
	srv := &http.Server{Addr: a.cfg.ListenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("api server", "err", err)
		}
	}()
	return srv
}

func (a *Agent) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Captures can hold heap dumps and logs: without a token the API stays shut.
		if a.cfg.APIToken == "" {
			http.Error(w, "LASTWORDS_API_TOKEN is not set on the agent", http.StatusForbidden)
			return
		}
		got := r.Header.Get(TokenHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(a.cfg.APIToken)) != 1 {
			http.Error(w, "missing or wrong "+TokenHeader, http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// ParseSince accepts a duration ("2h", "3d") or an RFC3339 time.
func ParseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return time.Now().Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Parse(time.RFC3339, s)
}

func (a *Agent) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := ParseSince(q.Get("since"))
	if err != nil {
		http.Error(w, "bad since: "+err.Error(), http.StatusBadRequest)
		return
	}
	list, err := a.Store.List(store.Filter{
		Namespace: q.Get("namespace"), Pod: q.Get("pod"), Container: q.Get("container"),
		ContainerID: q.Get("containerID"), Kind: store.Kind(q.Get("kind")), Since: since,
	})
	if err != nil {
		httpErr(w, err)
		return
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && len(list) > n {
		list = list[:n]
	}
	if list == nil {
		list = []*store.Capture{}
	}
	writeJSON(w, list)
}

func (a *Agent) handleFile(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	q := r.URL.Query()
	if maxB, err := strconv.ParseInt(q.Get("max"), 10, 64); err == nil && maxB > 0 {
		// Partial reads are buffered; whole files stream below.
		maxB = min(maxB, maxPartialRead)
		b, size, err := a.Store.ReadFileTail(id, name, maxB, q.Get("tail") == "1")
		if err != nil {
			httpErr(w, err)
			return
		}
		w.Header().Set("X-Lastwords-File-Size", strconv.FormatInt(size, 10))
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(b)
		return
	}
	f, err := a.Store.Open(id, name)
	if err != nil {
		httpErr(w, err)
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		w.Header().Set("X-Lastwords-File-Size", strconv.FormatInt(st.Size(), 10))
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func httpErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
