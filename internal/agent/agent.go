package agent

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Arvo-AI/lastwords/internal/cgroup"
	"github.com/Arvo-AI/lastwords/internal/core"
	"github.com/Arvo-AI/lastwords/internal/k8s"
	"github.com/Arvo-AI/lastwords/internal/procfs"
	"github.com/Arvo-AI/lastwords/internal/store"
)

// Version is set at build time.
var Version = "dev"

// tracked is a container the monitor samples.
type tracked struct {
	id        string
	dir       string
	ref       k8s.ContainerRef
	timeline  Timeline
	last      cgroup.Stats
	oomKills  uint64
	tiers     map[string]bool // capture tiers already taken for this container instance
	runtime   procfs.Runtime  // main process runtime, detected once memory gets interesting
	captureMu sync.Mutex
	goneAt    time.Time // when the cgroup disappeared (kept for death timelines)
}

// Agent is the node daemon.
type Agent struct {
	cfg     Config
	log     *slog.Logger
	kube    kubernetes.Interface
	pods    *k8s.Watcher
	Store   *store.Store
	notify  *notifier
	upload  *uploader
	sem     chan struct{}
	mu      sync.Mutex
	conts   map[string]*tracked
	started time.Time
	crashMu sync.Mutex
	crashes map[string][]time.Time // pod/container -> recent crash capture times
}

// New builds an agent.
func New(cfg Config, kube kubernetes.Interface, log *slog.Logger) (*Agent, error) {
	st, err := store.New(cfg.DataDir+"/captures", cfg.MaxStoreBytes, cfg.MaxAge)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		cfg:     cfg,
		log:     log,
		kube:    kube,
		Store:   st,
		sem:     make(chan struct{}, max(1, cfg.MaxConcurrent)),
		conts:   map[string]*tracked{},
		crashes: map[string][]time.Time{},
		started: time.Now(),
	}
	a.notify = newNotifier(cfg, log)
	if cfg.S3.Bucket != "" {
		up, err := newUploader(cfg)
		if err != nil {
			return nil, err
		}
		a.upload = up
	}
	return a, nil
}

// Run starts all loops and blocks until ctx is done.
func (a *Agent) Run(ctx context.Context) error {
	a.pods = k8s.NewWatcher(a.kube, a.cfg.NodeName)
	a.pods.OnDeath = func(ev k8s.TerminationEvent) {
		if a.ignored(ev.Pod) {
			return
		}
		go a.captureDeath(ctx, ev)
	}
	a.pods.OnEvict = func(p *corev1.Pod) {
		if a.ignored(p) {
			return
		}
		go a.captureEviction(ctx, p)
	}
	// The API server can be down when the agent starts. Everything that does
	// not need it (health, sampling, core dumps) starts anyway; pods get
	// attached to containers once the watcher syncs, and /readyz waits for it.
	srv := a.serveAPI()
	defer srv.Close()
	go func() {
		a.pods.Start(ctx)
		if ctx.Err() == nil {
			a.log.Info("pod watcher synced", "node", a.cfg.NodeName)
		}
	}()

	if a.cfg.CoreDumps {
		hostDir := a.cfg.CoreHostDir
		if hostDir == "" {
			hostDir = a.cfg.DataDir
		}
		prev, err := core.Install(a.cfg.DataDir, hostDir, core.Config{MaxBytes: a.cfg.CoreMaxBytes, OnlyContainers: true})
		if err != nil {
			a.log.Error("core dump capture disabled", "err", err)
		} else {
			a.log.Info("core dump capture enabled", "pattern", core.Pattern(hostDir), "previous", prev)
			if a.cfg.RestoreCoreOnExit {
				defer func() {
					if err := core.Restore(a.cfg.DataDir, hostDir); err != nil {
						a.log.Warn("restore core_pattern", "err", err)
					}
				}()
			}
			go a.coreLoop(ctx)
		}
	}

	go a.pruneLoop(ctx)

	a.monitorLoop(ctx)
	return nil
}

// pruneLoop enforces the store's age limit even when no new captures arrive.
func (a *Agent) pruneLoop(ctx context.Context) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		a.Store.Prune()
		storeBytes.Set(float64(a.Store.TotalBytes()))
		if a.cfg.CoreDumps {
			core.Sweep(a.cfg.DataDir, time.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *Agent) ignored(p *corev1.Pod) bool {
	if p == nil {
		return true
	}
	if p.Annotations[k8s.AnnoIgnore] == "true" {
		return true
	}
	for _, ns := range a.cfg.IgnoreNamespaces {
		if p.Namespace == ns {
			return true
		}
	}
	if len(a.cfg.Namespaces) > 0 {
		for _, ns := range a.cfg.Namespaces {
			if p.Namespace == ns {
				return false
			}
		}
		return true
	}
	return false
}

// monitorLoop samples every tracked container on a fixed interval and
// rediscovers containers every few seconds.
func (a *Agent) monitorLoop(ctx context.Context) {
	tick := time.NewTicker(a.cfg.Interval)
	defer tick.Stop()
	var lastDiscover time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if now.Sub(lastDiscover) > 5*time.Second {
				a.discover()
				lastDiscover = now
			}
			a.sampleAll(ctx, now)
		}
	}
}

func (a *Agent) discover() {
	found, err := cgroup.Discover(a.cfg.CgroupDir)
	if err != nil {
		a.log.Debug("cgroup discover", "err", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for id, dir := range found {
		t, ok := a.conts[id]
		if !ok {
			t = &tracked{id: id, dir: dir, tiers: map[string]bool{}, timeline: Timeline{keep: a.cfg.History}}
			a.conts[id] = t
		}
		t.dir = dir
		t.goneAt = time.Time{}
		if ref, ok := a.pods.Lookup(id); ok {
			t.ref = ref
		}
	}
	for id, t := range a.conts {
		if _, ok := found[id]; ok {
			continue
		}
		if t.goneAt.IsZero() {
			t.goneAt = now
		} else if now.Sub(t.goneAt) > 15*time.Minute {
			delete(a.conts, id)
		}
	}
	trackedContainers.Set(float64(len(found)))
}

// lookupTracked returns a tracked container (alive or recently gone).
func (a *Agent) lookupTracked(id string) *tracked {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conts[id]
}

func (a *Agent) sampleAll(ctx context.Context, now time.Time) {
	a.mu.Lock()
	list := make([]*tracked, 0, len(a.conts))
	for _, t := range a.conts {
		if t.goneAt.IsZero() && t.ref.Pod != nil {
			list = append(list, t)
		}
	}
	a.mu.Unlock()
	for _, t := range list {
		st, err := cgroup.Read(t.dir)
		if err != nil {
			continue
		}
		a.mu.Lock()
		prevKills := t.oomKills
		t.last = st
		t.oomKills = st.OOMKills()
		t.timeline.Add(Sample{T: now, WorkingSet: st.WorkingSet(), Usage: st.Usage})
		n := t.timeline.Len()
		growth := t.timeline.Growth(10 * time.Second)
		pod := t.ref.Pod // discover() replaces t.ref under a.mu
		a.mu.Unlock()

		if a.ignored(pod) {
			continue
		}
		// The kernel OOM-killed something in this cgroup. If the container
		// survives, that was a child process; captureProcessOOM checks.
		if n > 1 && st.OOMKills() > prevKills {
			go a.captureProcessOOM(ctx, t, st)
		}
		if st.Limit == 0 {
			continue
		}
		// Detect the runtime once per container instance, and only when it
		// starts to matter: it reads /proc of every process in the cgroup.
		if t.runtime == "" && st.Ratio() >= runtimeProbeRatio {
			rt := procfs.Runtime("unknown")
			if main := procfs.Largest(a.processes(t.dir)); main != nil {
				rt = main.Runtime
			}
			a.mu.Lock()
			t.runtime = rt
			a.mu.Unlock()
		}
		tier := a.evaluate(pod, st, growth, t.runtime == procfs.RuntimeJVM)
		if tier == "" {
			continue
		}
		a.mu.Lock()
		taken := t.tiers[tier]
		if !taken {
			t.tiers[tier] = true
		}
		a.mu.Unlock()
		if taken {
			continue
		}
		go a.capturePressure(ctx, t, tier)
	}
}

// runtimeProbeRatio is the share of the limit at which a container's runtime
// is detected. It must be low enough for the JVM tiers to still be ahead.
const runtimeProbeRatio = 0.40

// evaluate decides whether this sample should trigger a capture and returns
// the tier name ("threshold" or "last-gasp"), or "". For JVMs the tiers are
// measured against the point where the next GC would hit the limit.
func (a *Agent) evaluate(pod *corev1.Pod, st cgroup.Stats, growth float64, jvm bool) string {
	threshold := a.cfg.Threshold
	if pod != nil {
		if v, err := strconv.ParseFloat(pod.Annotations[k8s.AnnoThreshold], 64); err == nil && v > 0 && v < 1 {
			threshold = v
		}
	}
	limit, ws := float64(st.Limit), float64(st.WorkingSet())
	if jvm && a.cfg.JVMGCHeadroom > 0 && a.cfg.JVMGCHeadroom < 1 {
		limit *= 1 - a.cfg.JVMGCHeadroom
	}
	ratio := ws / limit
	if a.cfg.LastGasp > 0 && ratio >= a.cfg.LastGasp {
		return "last-gasp"
	}
	if ratio >= threshold {
		return "threshold"
	}
	if a.cfg.Predictive && ratio >= a.cfg.PredictMin {
		if growth > 0 && limit > ws {
			if secs := (limit - ws) / growth; secs < a.cfg.PredictSecs {
				return "threshold"
			}
		}
	}
	return ""
}

// WatchItem is a live view of one container for the API.
type WatchItem struct {
	Namespace       string  `json:"namespace"`
	Pod             string  `json:"pod"`
	Container       string  `json:"container"`
	ContainerID     string  `json:"containerID"`
	WorkingSetBytes uint64  `json:"workingSetBytes"`
	LimitBytes      uint64  `json:"limitBytes"`
	Ratio           float64 `json:"ratio"`
	GrowthBytesPerS float64 `json:"growthBytesPerSecond"`
	SecondsToLimit  float64 `json:"secondsToLimit,omitempty"`
	OOMKills        uint64  `json:"oomKills"`
}

// Watchlist returns the live memory state of all tracked containers.
func (a *Agent) Watchlist() []WatchItem {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []WatchItem
	for _, t := range a.conts {
		if !t.goneAt.IsZero() || t.ref.Pod == nil || a.ignored(t.ref.Pod) {
			continue
		}
		g := t.timeline.Growth(30 * time.Second)
		w := WatchItem{
			Namespace: t.ref.Pod.Namespace, Pod: t.ref.Pod.Name, Container: t.ref.Container, ContainerID: t.id,
			WorkingSetBytes: t.last.WorkingSet(), LimitBytes: t.last.Limit, Ratio: t.last.Ratio(),
			GrowthBytesPerS: g, OOMKills: t.oomKills,
		}
		if g > 0 && t.last.Limit > t.last.WorkingSet() {
			w.SecondsToLimit = float64(t.last.Limit-t.last.WorkingSet()) / g
		}
		out = append(out, w)
	}
	return out
}

// The capture budget limits captures for crash-looping containers: per kind
// of capture and container, the first maxCrashCaptures in crashWindow are
// captured and later ones are only counted.
const (
	maxCrashCaptures = 3
	crashWindow      = 30 * time.Minute
)

// allowCapture reports whether to take a capture of this kind for the
// container key, and how many were taken in the window before it.
func (a *Agent) allowCapture(kind, key string, now time.Time) (bool, int) {
	a.crashMu.Lock()
	defer a.crashMu.Unlock()
	k := kind + "|" + key
	var recent []time.Time
	for _, t := range a.crashes[k] {
		if now.Sub(t) < crashWindow {
			recent = append(recent, t)
		}
	}
	// Forget containers that have been quiet for a whole window, so the map
	// does not grow with every pod that ever crashed.
	for other, ts := range a.crashes {
		if other != k && (len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= crashWindow) {
			delete(a.crashes, other)
		}
	}
	if len(recent) >= maxCrashCaptures {
		a.crashes[k] = recent
		return false, len(recent)
	}
	a.crashes[k] = append(recent, now)
	return true, len(recent)
}

// key identifies a container across restarts (pod/container), falling back
// to the container ID when the pod is unknown.
func refKey(r k8s.ContainerRef, id string) string {
	if r.Pod == nil {
		return id
	}
	return r.Pod.Namespace + "/" + r.Pod.Name + "/" + r.Container
}
