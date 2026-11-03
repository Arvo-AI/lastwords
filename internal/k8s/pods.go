// Package k8s watches the pods on this node and maps container IDs to pods.
package k8s

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

// Annotation keys users can put on pods.
const (
	AnnoPrefix = "lastwords.dev/"
	// AnnoIgnore: "true" disables lastwords for the pod.
	AnnoIgnore = AnnoPrefix + "ignore"
	// AnnoHeapDump: "true" allows full heap dumps (JVM .hprof). Off by default
	// because heap dumps contain whatever was in memory, including secrets.
	AnnoHeapDump = AnnoPrefix + "heap-dump"
	// AnnoPprofPort: port serving net/http/pprof for Go programs (default 6060).
	AnnoPprofPort = AnnoPrefix + "pprof-port"
	// AnnoThreshold: per-pod trigger threshold, e.g. "0.85".
	AnnoThreshold = AnnoPrefix + "threshold"
)

// ContainerRef identifies a container in a pod.
type ContainerRef struct {
	Pod       *corev1.Pod
	Container string
	Image     string
	Restarts  int32
}

// TerminationEvent is emitted when a container on this node terminates.
type TerminationEvent struct {
	Pod         *corev1.Pod
	Container   string
	ContainerID string // ID of the container that died
	Restarts    int32
	Attempt     int32 // kubelet attempt number of the dead instance (its log is <attempt>.log)
	State       corev1.ContainerStateTerminated
}

// Watcher keeps an index of pods on one node.
type Watcher struct {
	client   kubernetes.Interface
	node     string
	mu       sync.RWMutex
	byID     map[string]ContainerRef
	seen     map[string]types.UID // container IDs already reported as terminated -> pod
	OnDeath  func(TerminationEvent)
	OnEvict  func(*corev1.Pod)
	Recorder record.EventRecorder
	synced   atomic.Bool
}

// NewWatcher creates a watcher for pods scheduled on node.
func NewWatcher(client kubernetes.Interface, node string) *Watcher {
	b := record.NewBroadcaster()
	b.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: client.CoreV1().Events("")})
	rec := b.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "lastwords", Host: node})
	return &Watcher{client: client, node: node, byID: map[string]ContainerRef{}, seen: map[string]types.UID{}, Recorder: rec}
}

// Start runs the informer until ctx is done.
func (w *Watcher) Start(ctx context.Context) {
	f := informers.NewSharedInformerFactoryWithOptions(w.client, 10*time.Minute,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", w.node).String()
		}))
	inf := f.Core().V1().Pods().Informer()
	_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { w.upsert(nil, o.(*corev1.Pod)) },
		UpdateFunc: func(old, cur any) { w.upsert(old.(*corev1.Pod), cur.(*corev1.Pod)) },
		DeleteFunc: func(o any) {
			if p, ok := o.(*corev1.Pod); ok {
				w.remove(p)
			} else if t, ok := o.(cache.DeletedFinalStateUnknown); ok {
				if p, ok := t.Obj.(*corev1.Pod); ok {
					w.remove(p)
				}
			}
		},
	})
	f.Start(ctx.Done())
	if cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		w.synced.Store(true)
	}
}

// Synced reports whether the initial list finished.
func (w *Watcher) Synced() bool { return w.synced.Load() }

// TrimID strips the runtime prefix from a containerID ("containerd://abc" -> "abc").
func TrimID(id string) string {
	if i := strings.Index(id, "://"); i >= 0 {
		return id[i+3:]
	}
	return id
}

func allStatuses(p *corev1.Pod) []corev1.ContainerStatus {
	out := append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...)
	return append(out, p.Status.ContainerStatuses...)
}

func (w *Watcher) upsert(old, p *corev1.Pod) {
	w.mu.Lock()
	// Index the running container and the one that died before it: a core
	// dump or late death event can name either. Older instances of a
	// restarting container are dropped, so a crash loop does not grow the
	// index (or keep old pod objects alive).
	w.forget(p.UID)
	for _, cs := range allStatuses(p) {
		ref := ContainerRef{Pod: p, Container: cs.Name, Image: cs.Image, Restarts: cs.RestartCount}
		if id := TrimID(cs.ContainerID); id != "" {
			w.byID[id] = ref
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			if id := TrimID(t.ContainerID); id != "" {
				w.byID[id] = ref
			}
		}
	}
	current := map[string]bool{}
	for _, cs := range allStatuses(p) {
		for _, t := range []*corev1.ContainerStateTerminated{cs.LastTerminationState.Terminated, cs.State.Terminated} {
			if t != nil {
				current[TrimID(t.ContainerID)] = true
			}
		}
	}
	for id, uid := range w.seen {
		if uid == p.UID && !current[id] {
			delete(w.seen, id) // gone from the status; it cannot be reported again
		}
	}
	var deaths []TerminationEvent
	initial := old == nil
	for _, cs := range allStatuses(p) {
		// A restarted container keeps its previous death in LastTerminationState.
		// A container that stopped for good (restartPolicy Never/OnFailure) shows it in State.
		for i, t := range []*corev1.ContainerStateTerminated{cs.LastTerminationState.Terminated, cs.State.Terminated} {
			if t == nil {
				continue
			}
			// The kubelet numbers instances by restart count; the previous
			// instance of a restarted container is one behind.
			attempt := cs.RestartCount
			if i == 0 {
				attempt--
			}
			id := TrimID(t.ContainerID)
			if _, done := w.seen[id]; id == "" || done {
				continue
			}
			w.seen[id] = p.UID
			// On startup we mark existing terminations as seen without reporting
			// deaths that happened before the agent was running, unless recent.
			if initial && time.Since(t.FinishedAt.Time) > 2*time.Minute {
				continue
			}
			if t.ExitCode == 0 && t.Reason == "Completed" {
				continue
			}
			// A pod being deleted (rollout, scale-down, drain) was asked to
			// stop: its exit is a shutdown, not a crash. An OOM kill while
			// shutting down is still worth reporting.
			if i == 1 && p.DeletionTimestamp != nil && t.Reason != "OOMKilled" {
				continue
			}
			deaths = append(deaths, TerminationEvent{Pod: p, Container: cs.Name, ContainerID: id, Restarts: cs.RestartCount, Attempt: attempt, State: *t})
		}
	}
	evicted := p.Status.Reason == "Evicted" && (old == nil || old.Status.Reason != "Evicted")
	if initial && evicted && p.Status.StartTime != nil && time.Since(lastTransition(p)) > 2*time.Minute {
		evicted = false
	}
	w.mu.Unlock()
	if w.OnDeath != nil {
		for _, d := range deaths {
			w.OnDeath(d)
		}
	}
	if evicted && w.OnEvict != nil {
		w.OnEvict(p)
	}
}

func lastTransition(p *corev1.Pod) time.Time {
	var t time.Time
	for _, c := range p.Status.Conditions {
		if c.LastTransitionTime.After(t) {
			t = c.LastTransitionTime.Time
		}
	}
	return t
}

func (w *Watcher) remove(p *corev1.Pod) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.forget(p.UID)
	for id, uid := range w.seen {
		if uid == p.UID {
			delete(w.seen, id)
		}
	}
}

// forget drops every indexed container of the pod. Callers hold w.mu.
func (w *Watcher) forget(uid types.UID) {
	for id, ref := range w.byID {
		if ref.Pod != nil && ref.Pod.UID == uid {
			delete(w.byID, id)
		}
	}
}

// Lookup finds the pod and container for a container ID.
func (w *Watcher) Lookup(id string) (ContainerRef, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	r, ok := w.byID[id]
	return r, ok
}

// LimitFor returns the container's memory limit from the pod spec (0 = none).
func LimitFor(p *corev1.Pod, container string) int64 {
	for _, c := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
		if c.Name == container {
			if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				return q.Value()
			}
		}
	}
	return 0
}

// OwnerName returns "Kind/name" of the pod's controller, resolving
// ReplicaSet names back to their Deployment by trimming the hash suffix.
func OwnerName(p *corev1.Pod) string {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			if o.Kind == "ReplicaSet" {
				if i := strings.LastIndex(o.Name, "-"); i > 0 {
					return "Deployment/" + o.Name[:i]
				}
			}
			return o.Kind + "/" + o.Name
		}
	}
	return ""
}
