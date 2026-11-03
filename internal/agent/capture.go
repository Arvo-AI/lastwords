package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Arvo-AI/lastwords/internal/cgroup"
	"github.com/Arvo-AI/lastwords/internal/core"
	"github.com/Arvo-AI/lastwords/internal/gopprof"
	"github.com/Arvo-AI/lastwords/internal/jvm"
	"github.com/Arvo-AI/lastwords/internal/k8s"
	"github.com/Arvo-AI/lastwords/internal/logs"
	"github.com/Arvo-AI/lastwords/internal/procfs"
	"github.com/Arvo-AI/lastwords/internal/store"
)

func (a *Agent) base(kind store.Kind, p *corev1.Pod, container, containerID string, restarts int32) *store.Capture {
	c := &store.Capture{
		Kind:         kind,
		Time:         time.Now().UTC(),
		Node:         a.cfg.NodeName,
		Container:    container,
		ContainerID:  containerID,
		RestartCount: restarts,
	}
	if p != nil {
		c.Namespace, c.Pod, c.PodUID = p.Namespace, p.Name, string(p.UID)
		c.Owner = k8s.OwnerName(p)
		for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			if cs.Name == container {
				c.Image = cs.Image
			}
		}
		if c.Image == "" {
			for _, ct := range p.Spec.Containers {
				if ct.Name == container {
					c.Image = ct.Image
				}
			}
		}
	}
	return c
}

func (a *Agent) acquire(ctx context.Context) bool {
	select {
	case a.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (a *Agent) release() { <-a.sem }

// capturePressure runs when a container nears its memory limit.
func (a *Agent) capturePressure(ctx context.Context, t *tracked, tier string) {
	if !a.acquire(ctx) {
		return
	}
	defer a.release()
	t.captureMu.Lock()
	defer t.captureMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, a.cfg.CaptureTimeout)
	defer cancel()

	a.mu.Lock()
	st := t.last
	samples := t.timeline.Samples()
	ref := t.ref
	dir := t.dir
	a.mu.Unlock()

	// A container that OOMs on every start would otherwise get two snapshots
	// per restart forever.
	if ok, _ := a.allowCapture("pressure", refKey(ref, t.id), time.Now()); !ok {
		crashesSuppressed.Inc()
		return
	}
	c := a.base(store.KindPressure, ref.Pod, ref.Container, t.id, ref.Restarts)
	c.Trigger = tier
	c.Memory = memoryOf(st, samples)
	w, err := a.Store.Begin(c)
	if err != nil {
		a.log.Error("begin capture", "err", err)
		return
	}
	a.log.Info("memory pressure capture", "pod", c.Namespace+"/"+c.Pod, "container", c.Container, "ratio", fmt.Sprintf("%.2f", c.Memory.Ratio), "tier", tier)

	// Runtime evidence first: it is the most time-critical.
	procs := a.processes(dir)
	main := procfs.Largest(procs)
	var sections []section
	if main != nil {
		c.Runtime = string(main.Runtime)
		switch main.Runtime {
		case procfs.RuntimeJVM:
			sections = append(sections, a.collectJVM(ctx, w, main.Pid, ref.Pod)...)
		case procfs.RuntimeGo:
			sections = append(sections, a.collectGo(ctx, w, ref.Pod)...)
		case procfs.RuntimePython:
			c.Findings = append(c.Findings, "Python process: lastwords captured the memory breakdown. For allocation sites, run the app with tracemalloc or memray enabled.")
		case procfs.RuntimeNode:
			c.Findings = append(c.Findings, "Node.js process: lastwords captured the memory breakdown. Start node with --heapsnapshot-near-heap-limit=2 to get V8 heap snapshots too.")
		}
	}
	a.writeCgroup(w, dir, st)
	if len(procs) > 0 {
		w.WriteFile("processes.txt", "Processes in the container with RSS, peak RSS, threads and smaps_rollup", []byte(procfs.Format(procs)))
	}
	w.WriteFile("timeline.csv", "Working set over the last minutes, sampled every "+a.cfg.Interval.String(), []byte(CSV(samples, st.Limit)))
	c.Findings = append(breakdownFindings(st, main), c.Findings...)
	c.Headline = headlinePressure(c)
	a.finish(ctx, w, render(c, samples, sections))
}

// collectJVM uses the attach API to pull class histogram, heap info, native
// memory summary, thread dump, and (opt-in) a full heap dump.
func (a *Agent) collectJVM(ctx context.Context, w *store.Writer, pid int, pod *corev1.Pod) []section {
	cl, err := jvm.New(pid)
	if err != nil {
		w.Errorf("jvm attach: %v", err)
		return nil
	}
	defer cl.Close()
	var secs []section
	gone := false
	alive := func(cmd string, cause error) bool {
		if !gone {
			if _, err := os.Stat(filepath.Join(a.cfg.ProcDir, strconv.Itoa(pid))); err != nil {
				gone = true
				w.Errorf("the JVM exited during the capture (most likely the OOM kill itself); jcmd %s failed with %v and the remaining JVM commands were skipped", cmd, cause)
			}
		}
		return !gone
	}
	run := func(cmd, file, desc string) string {
		if gone {
			return ""
		}
		if ctx.Err() != nil {
			w.Errorf("jcmd %s: skipped, capture timed out", cmd)
			return ""
		}
		out, err := cl.Jcmd(ctx, cmd)
		if err != nil || out == "" {
			if err == nil {
				err = errors.New("empty response")
			}
			if !alive(cmd, err) {
				return ""
			}
			w.Errorf("jcmd %s: %v", cmd, err)
			if out == "" {
				return ""
			}
		}
		w.WriteFile(file, desc, []byte(out))
		return out
	}
	// The histogram is the most useful output, so it goes first, while the JVM
	// is most likely still alive. -all skips the full GC the default form
	// forces: on a JVM near its container limit that GC needs extra memory
	// (live young objects are copied into the old generation) and the capture
	// itself would trigger the OOM kill. Leaked objects are reachable, so
	// counting unreachable ones too does not hide them.
	if a.cfg.JVMHistogram {
		out := run("GC.class_histogram -all", "jvm-class-histogram.txt", "Objects on the heap by class, without forcing a GC (jcmd GC.class_histogram -all)")
		entries, total := jvm.ParseHistogram(out)
		entries, filler := jvm.DropFillers(entries)
		if filler <= total {
			total -= filler
		}
		if len(entries) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "Java heap: %s across %d classes (counted without forcing a GC, so it can include garbage not yet collected; collector filler space is left out).\n\n", procfs.Bytes(total), len(entries))
			b.WriteString("| Class | Bytes | Share | Instances |\n|---|---:|---:|---:|\n")
			for i, e := range entries {
				if i == 10 {
					break
				}
				share := 0.0
				if total > 0 {
					share = 100 * float64(e.Bytes) / float64(total)
				}
				fmt.Fprintf(&b, "| `%s` | %s | %.0f%% | %d |\n", e.Class, procfs.Bytes(e.Bytes), share, e.Instances)
			}
			secs = append(secs, section{"Java heap: top classes", b.String()})
			if app := jvm.AppClasses(entries, 3); len(app) > 0 {
				var parts []string
				for _, e := range app {
					parts = append(parts, fmt.Sprintf("%s: %s, %s in total", e.Class, plural(int(e.Instances), "instance"), procfs.Bytes(e.Bytes)))
				}
				w.C.Findings = append(w.C.Findings, "Largest application classes on the heap: "+strings.Join(parts, "; ")+". Sizes are shallow: arrays and strings these objects point to are counted under byte[], String, etc. A large instance count here usually names the leaking cache or collection.")
			}
			top := entries[0]
			if total > 0 && float64(top.Bytes)/float64(total) > 0.4 {
				f := fmt.Sprintf("One class dominates the Java heap: %s holds %s (%.0f%%) in %d instances.", top.Class, procfs.Bytes(top.Bytes), 100*float64(top.Bytes)/float64(total), top.Instances)
				if h, ok := jvm.Holder(entries, top); ok && h.Class != top.Class {
					f += fmt.Sprintf(" %s has almost the same number of instances (%d), so each one most likely holds one %s: %s is what keeps this memory alive.", h.Class, h.Instances, top.Class, h.Class)
				}
				w.C.Findings = append(w.C.Findings, f)
			}
		}
	}
	run("GC.heap_info", "jvm-heap-info.txt", "JVM heap regions and usage (jcmd GC.heap_info)")
	if a.cfg.JVMNativeMemory && ctx.Err() == nil && !gone {
		out, err := cl.Jcmd(ctx, "VM.native_memory summary")
		if err == nil && !strings.Contains(out, "not enabled") {
			w.WriteFile("jvm-native-memory.txt", "JVM native memory by category (jcmd VM.native_memory summary)", []byte(out))
		} else {
			w.C.Findings = append(w.C.Findings, "JVM native memory tracking is off. Add -XX:NativeMemoryTracking=summary to see off-heap usage (metaspace, threads, direct buffers) in future captures.")
		}
	}
	if a.cfg.JVMThreadDump {
		run("Thread.print", "jvm-threads.txt", "All Java thread stacks at capture time (jcmd Thread.print)")
	}
	if ctx.Err() == nil && !gone && (a.cfg.AllowHeapDumps || (pod != nil && pod.Annotations[k8s.AnnoHeapDump] == "true")) {
		a.jvmHeapDump(ctx, w, cl)
	}
	return secs
}

func (a *Agent) jvmHeapDump(ctx context.Context, w *store.Writer, cl *jvm.Client) {
	name := "lastwords-" + w.C.ID + ".hprof"
	defer cl.RemoveTmpFile(name)
	if _, err := cl.Jcmd(ctx, "GC.heap_dump -gz=1 /tmp/"+name); err != nil {
		w.Errorf("heap dump: %v", err)
		return
	}
	// The file sits in a directory the container controls: open it without
	// following symlinks and copy at most HeapDumpMaxBytes.
	in, err := cl.OpenTmpFile(name)
	if err != nil {
		w.Errorf("read heap dump: %v", err)
		return
	}
	defer in.Close()
	out, err := os.Create(w.Path("heap.hprof.gz"))
	if err != nil {
		w.Errorf("write heap dump: %v", err)
		return
	}
	n, err := io.Copy(out, ctxReader{ctx, io.LimitReader(in, a.cfg.HeapDumpMaxBytes+1)})
	out.Close()
	if err == nil && n > a.cfg.HeapDumpMaxBytes {
		err = fmt.Errorf("larger than heapDumpMaxBytes (%s)", procfs.Bytes(uint64(a.cfg.HeapDumpMaxBytes)))
	}
	if err != nil {
		_ = os.Remove(w.Path("heap.hprof.gz"))
		w.Errorf("copy heap dump: %v", err)
		return
	}
	w.AddFile("heap.hprof.gz", "Full Java heap dump (gzipped HPROF). Open with Eclipse MAT or VisualVM. Contains in-memory data, handle as sensitive")
}

// collectGo pulls the heap profile from net/http/pprof on the pod IP.
func (a *Agent) collectGo(ctx context.Context, w *store.Writer, pod *corev1.Pod) []section {
	if pod == nil || pod.Status.PodIP == "" {
		return nil
	}
	port := a.cfg.GoPprofPort
	if v, err := strconv.Atoi(pod.Annotations[k8s.AnnoPprofPort]); err == nil && v > 0 {
		port = v
	}
	prof, err := gopprof.Fetch(ctx, pod.Status.PodIP, port)
	if err != nil {
		w.C.Findings = append(w.C.Findings, fmt.Sprintf("Go program: no pprof endpoint on port %d, so no heap profile. Import net/http/pprof and serve it, then annotate the pod with %s=<port>.", port, k8s.AnnoPprofPort))
		return nil
	}
	w.WriteFile("go-heap.pb.gz", "Go heap profile (inuse and alloc). Run: go tool pprof -top go-heap.pb.gz", prof.Heap)
	if len(prof.Goroutines) > 0 {
		w.WriteFile("go-goroutines.txt", "Goroutine stacks grouped by count (debug=1)", prof.Goroutines)
		if n := gopprof.GoroutineCount(prof.Goroutines); n > 10000 {
			w.C.Findings = append(w.C.Findings, fmt.Sprintf("%d goroutines are running; a goroutine leak can hold memory.", n))
		}
	}
	if len(prof.Top) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "In-use heap: %s.\n\n| Allocated by | Where | In use | Share |\n|---|---|---:|---:|\n", procfs.Bytes(uint64(prof.TotalInUse)))
	for i, s := range prof.Top {
		if i == 10 {
			break
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %.0f%% |\n", s.Function, s.Location, procfs.Bytes(uint64(s.InUse)), 100*float64(s.InUse)/float64(max(1, prof.TotalInUse)))
	}
	if ws := w.C.Memory; ws != nil && ws.WorkingSetBytes > 0 && float64(prof.TotalInUse) < 0.6*float64(ws.WorkingSetBytes) {
		w.C.Findings = append(w.C.Findings, fmt.Sprintf("Live Go heap is %s but the container uses %s: the garbage collector lets the heap grow to about twice the live data (GOGC=100) before collecting. Set GOMEMLIMIT to about 90%% of the container limit so the GC works harder before the kernel kills the process. That buys time but does not fix a leak.", procfs.Bytes(uint64(prof.TotalInUse)), procfs.Bytes(ws.WorkingSetBytes)))
	}
	top := prof.Top[0]
	if prof.TotalInUse > 0 && float64(top.InUse)/float64(prof.TotalInUse) > 0.4 {
		w.C.Findings = append(w.C.Findings, fmt.Sprintf("One allocation site holds most of the Go heap: %s (%s) keeps %s in use.", top.Function, top.Location, procfs.Bytes(uint64(top.InUse))))
	}
	return []section{{"Go heap: top allocation sites", b.String()}}
}

func (a *Agent) processes(dir string) []*procfs.Process {
	pids, _ := cgroup.Procs(dir)
	var out []*procfs.Process
	for i, pid := range pids {
		if i >= 64 {
			break
		}
		if pr, err := procfs.Read(pid); err == nil {
			out = append(out, pr)
		}
	}
	return out
}

func (a *Agent) writeCgroup(w *store.Writer, dir string, st cgroup.Stats) {
	var b strings.Builder
	fmt.Fprintf(&b, "# cgroup v%d %s\n\n", st.Version, dir)
	for _, f := range []string{"memory.current", "memory.max", "memory.high", "memory.peak", "memory.events", "memory.pressure", "memory.stat", "memory.swap.current", "memory.usage_in_bytes", "memory.limit_in_bytes", "memory.max_usage_in_bytes", "memory.failcnt", "memory.oom_control"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "## %s\n%s\n", f, strings.TrimRight(string(data), "\n"))
	}
	w.WriteFile("cgroup-memory.txt", "Raw cgroup memory accounting (memory.stat, events, pressure)", []byte(b.String()))
}

func memoryOf(st cgroup.Stats, samples []Sample) *store.Memory {
	m := &store.Memory{
		WorkingSetBytes: st.WorkingSet(),
		UsageBytes:      st.Usage,
		LimitBytes:      st.Limit,
		Ratio:           st.Ratio(),
		GrowthBytesPerS: Growth(samples, 10*time.Second),
		Breakdown:       map[string]uint64{},
	}
	if st.PSI != nil {
		m.PSIFullAvg10 = st.PSI.FullAvg10
	}
	if m.GrowthBytesPerS > 0 && st.Limit > st.WorkingSet() {
		m.SecondsToLimit = float64(st.Limit-st.WorkingSet()) / m.GrowthBytesPerS
	}
	for _, k := range []string{"anon", "file", "kernel", "kernel_stack", "pagetables", "sock", "shmem", "slab", "percpu", "rss", "cache", "total_rss", "total_cache", "mapped_file"} {
		if v, ok := st.Stat[k]; ok {
			m.Breakdown[k] = v
		}
	}
	return m
}

// breakdownFindings explains where the container's memory is, in plain words.
func breakdownFindings(st cgroup.Stats, main *procfs.Process) []string {
	var out []string
	ws := st.WorkingSet()
	if ws == 0 {
		return nil
	}
	anon := st.Stat["anon"]
	if st.Version == cgroup.V1 {
		anon = st.Stat["total_rss"]
	}
	share := func(v uint64) float64 { return 100 * float64(v) / float64(ws) }
	switch {
	case anon > 0 && share(anon) >= 70:
		out = append(out, fmt.Sprintf("%.0f%% of the working set is anonymous memory (heap, malloc, stacks), so the growth is inside the program, not page cache.", share(anon)))
	case st.Stat["shmem"] > 0 && share(st.Stat["shmem"]) >= 30:
		out = append(out, fmt.Sprintf("%.0f%% of the working set is shared memory (tmpfs or emptyDir with medium: Memory counts against the limit).", share(st.Stat["shmem"])))
	case st.Stat["file"] > 0 && share(st.Stat["file"]) >= 50:
		out = append(out, fmt.Sprintf("%.0f%% of usage is file cache that the kernel could not reclaim (active file pages).", share(st.Stat["file"])))
	}
	if k := st.Stat["kernel"]; k > 0 && share(k) >= 20 {
		out = append(out, fmt.Sprintf("Kernel memory is %s (%.0f%%): look at sockets, many threads, or slab usage.", procfs.Bytes(k), share(k)))
	}
	if s := st.Stat["sock"]; s > 0 && share(s) >= 10 {
		out = append(out, fmt.Sprintf("Socket buffers hold %s: slow consumers or many open connections.", procfs.Bytes(s)))
	}
	if main != nil && main.Threads > 1000 {
		out = append(out, fmt.Sprintf("%s runs %d threads; each thread stack costs memory.", main.Comm, main.Threads))
	}
	return out
}

// captureProcessOOM records an OOM kill of a process inside a container that
// kept running (a worker, a child process, a sidecar shell).
func (a *Agent) captureProcessOOM(ctx context.Context, t *tracked, st cgroup.Stats) {
	// Give the kubelet and cgroup time to show whether the whole container died.
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Second):
	}
	a.mu.Lock()
	gone := !t.goneAt.IsZero()
	ref := t.ref
	dir := t.dir
	samples := t.timeline.Samples()
	a.mu.Unlock()
	if gone {
		return // the container died; captureDeath handles it
	}
	if pids, err := cgroup.Procs(dir); err != nil || len(pids) == 0 {
		return
	}
	// A worker pool whose children keep getting killed would otherwise
	// produce a capture per kill.
	if ok, _ := a.allowCapture("process-oom", refKey(ref, t.id), time.Now()); !ok {
		crashesSuppressed.Inc()
		return
	}
	if !a.acquire(ctx) {
		return
	}
	defer a.release()
	c := a.base(store.KindProcessOOM, ref.Pod, ref.Container, t.id, ref.Restarts)
	c.Trigger = "memory.events oom_kill"
	c.Memory = memoryOf(st, samples)
	w, err := a.Store.Begin(c)
	if err != nil {
		return
	}
	procs := a.processes(dir)
	a.writeCgroup(w, dir, st)
	if len(procs) > 0 {
		w.WriteFile("processes.txt", "Processes still running in the container", []byte(procfs.Format(procs)))
	}
	w.WriteFile("timeline.csv", "Working set before the kill", []byte(CSV(samples, st.Limit)))
	c.Findings = append(c.Findings, "The kernel killed a process inside this container, but the container kept running. Kubernetes does not report this as OOMKilled. Look for a crashed worker or child process in the logs.")
	c.Findings = append(c.Findings, breakdownFindings(st, procfs.Largest(procs))...)
	c.Headline = fmt.Sprintf("A process in %s/%s was OOM-killed inside the container (limit %s); the container survived", c.Pod, c.Container, procfs.Bytes(st.Limit))
	a.finish(ctx, w, render(c, samples, nil))
}

// captureDeath runs when the kubelet reports a container terminated with an
// error: OOMKilled, non-zero exit, or killed by a signal.
func (a *Agent) captureDeath(ctx context.Context, ev k8s.TerminationEvent) {
	if !a.acquire(ctx) {
		return
	}
	defer a.release()
	kind := store.KindCrash
	if ev.State.Reason == "OOMKilled" {
		kind = store.KindOOMKill
	}
	ok, prior := a.allowCapture("death", ev.Pod.Namespace+"/"+ev.Pod.Name+"/"+ev.Container, time.Now())
	if !ok {
		crashesSuppressed.Inc()
		a.log.Info("crash loop: not capturing again", "pod", ev.Pod.Namespace+"/"+ev.Pod.Name, "container", ev.Container, "captured", prior)
		return
	}
	c := a.base(kind, ev.Pod, ev.Container, ev.ContainerID, ev.Restarts)
	c.Trigger = "container terminated: " + ev.State.Reason
	c.Termination = &store.Termination{
		Reason: ev.State.Reason, ExitCode: ev.State.ExitCode, Signal: ev.State.Signal,
		StartedAt: ev.State.StartedAt.Time, FinishedAt: ev.State.FinishedAt.Time, Message: ev.State.Message,
	}
	w, err := a.Store.Begin(c)
	if err != nil {
		return
	}
	a.log.Info("container death capture", "pod", c.Namespace+"/"+c.Pod, "container", c.Container, "reason", ev.State.Reason, "exit", ev.State.ExitCode)

	// The memory curve leading up to the death, if we were watching.
	var samples []Sample
	limit := uint64(k8s.LimitFor(ev.Pod, ev.Container))
	if t := a.lookupTracked(ev.ContainerID); t != nil {
		a.mu.Lock()
		samples = t.timeline.Samples()
		if t.last.Limit > 0 {
			limit = t.last.Limit
		}
		last := t.last
		a.mu.Unlock()
		if len(samples) > 0 {
			c.Memory = memoryOf(last, samples)
			c.Memory.LimitBytes = limit
			if kind != store.KindOOMKill {
				// A projection to the limit means nothing for a crash.
				c.Memory.GrowthBytesPerS, c.Memory.SecondsToLimit = 0, 0
			}
			w.WriteFile("timeline.csv", "Working set up to the moment the container died", []byte(CSV(samples, limit)))
		}
	}

	// The last words: the dead instance's log, before the next restart rotates it.
	var logText string
	if a.cfg.CaptureLogs {
		dir := logs.ContainerDir(ev.Pod.Namespace, ev.Pod.Name, string(ev.Pod.UID), ev.Container)
		if path, err := logs.FindInstanceLog(dir, ev.Attempt, ev.State.FinishedAt.Time); err == nil {
			if logText, err = logs.Tail(path, a.cfg.LogLines, 4<<20); err == nil {
				w.WriteFile("logs.txt", fmt.Sprintf("Last %d log lines of the container instance that died", a.cfg.LogLines), []byte(logText))
			} else {
				w.Errorf("read logs: %v", err)
			}
		} else {
			w.Errorf("find logs: %v", err)
		}
	}
	if ev := a.podEvents(ctx, ev.Pod); ev != "" {
		w.WriteFile("events.txt", "Kubernetes events for the pod (they expire after 1 hour by default)", []byte(ev))
	}
	w.WriteFile("termination.txt", "Container termination state reported by the kubelet", []byte(fmt.Sprintf(
		"reason: %s\nexitCode: %d\nsignal: %d\nstartedAt: %s\nfinishedAt: %s\nrestartCount: %d\nmessage: %s\n",
		ev.State.Reason, ev.State.ExitCode, ev.State.Signal, ev.State.StartedAt.UTC().Format(time.RFC3339), ev.State.FinishedAt.UTC().Format(time.RFC3339), ev.Restarts, ev.State.Message)))

	// Link captures of the same container instance taken before the death.
	snapshots := 0
	if prior, err := a.Store.List(store.Filter{ContainerID: ev.ContainerID}); err == nil {
		for _, p := range prior {
			c.Related = append(c.Related, p.ID)
			if p.Kind == store.KindPressure {
				snapshots++
			}
		}
	}
	if ev.Init {
		c.Findings = append(c.Findings, "This is an init container: the pod's main containers do not start until it exits 0, so the whole pod is down while it fails.")
	}
	c.Findings = append(c.Findings, deathFindings(c, samples, limit, logText, snapshots)...)
	if kind == store.KindOOMKill {
		c.Findings = append(c.Findings, a.snapshotFindings(c.Related)...)
	}
	if prior == maxCrashCaptures-1 {
		c.Findings = append(c.Findings, fmt.Sprintf("This container is crash-looping: this is crash capture %d in %s. Further crashes in that window are counted but not captured.", maxCrashCaptures, crashWindow))
	}
	c.Headline = headlineDeath(c, limit, snapshots)
	id := c.ID
	a.finish(ctx, w, render(c, samples, nil))
	for _, r := range c.Related {
		_ = a.Store.Update(r, func(p *store.Capture) { p.Related = appendUnique(p.Related, id) })
	}
}

func deathFindings(c *store.Capture, samples []Sample, limit uint64, logText string, snapshots int) []string {
	var out []string
	t := c.Termination
	if t.Reason == "OOMKilled" {
		if len(samples) >= 2 {
			first, last := samples[0], samples[len(samples)-1]
			dur := last.T.Sub(first.T)
			if dur > 0 && last.WorkingSet > first.WorkingSet {
				rate := float64(last.WorkingSet-first.WorkingSet) / dur.Seconds()
				out = append(out, fmt.Sprintf("Memory rose from %s to %s over %s (%s/s) before the kill.", procfs.Bytes(first.WorkingSet), procfs.Bytes(last.WorkingSet), dur.Round(time.Second), procfs.Bytes(uint64(rate))))
				switch shape, d := ClassifyGrowth(samples); shape {
				case ShapeSteady:
					out = append(out, fmt.Sprintf("Memory climbed steadily for %s, the pattern of a leak or an unbounded cache. Raising the limit would only delay the next kill.", d.Round(time.Second)))
				case ShapeSpike:
					out = append(out, fmt.Sprintf("Most of the rise came in the last %s: a sudden allocation, such as a large request, file or query result, rather than a slow leak. If that load is legitimate the limit may be too low; otherwise cap the input size.", max(d, time.Second).Round(time.Second)))
				}
			}
		}
		if snapshots > 0 {
			verb := "were"
			if snapshots == 1 {
				verb = "was"
			}
			out = append(out, fmt.Sprintf("%s of what was using the memory %s taken before the kill; see Related captures.", plural(snapshots, "snapshot"), verb))
		} else {
			out = append(out, "No snapshot was taken before the kill: memory went from below the threshold to the limit between samples, or the agent was not watching yet. The timeline and logs are still saved.")
		}
	} else {
		switch {
		case t.Signal != 0:
			out = append(out, fmt.Sprintf("The process was killed by %s.", core.SignalName(int(t.Signal))))
		case t.ExitCode > 128:
			out = append(out, fmt.Sprintf("Exit code %d means the process died from %s.", t.ExitCode, core.SignalName(int(t.ExitCode-128))))
		default:
			out = append(out, fmt.Sprintf("The process exited with code %d.", t.ExitCode))
		}
	}
	if f := runtimeOOM(logText, c.Memory, limit); f != "" {
		out = append(out, f)
	}
	if site := logs.CrashSite(logText); site != "" {
		out = append(out, "Crashed in "+site+".")
	}
	hits := logs.Interesting(logText, 5)
	for _, l := range hits {
		out = append(out, "Log: "+l)
	}
	if len(hits) == 0 && t.Reason != "OOMKilled" {
		if last := lastLines(logText, 3); len(last) > 0 {
			out = append(out, "Last log lines before it died: "+strings.Join(last, " | "))
		}
	}
	return out
}

// runtimeOOM explains an out-of-memory error raised by the language runtime
// itself. These kill the process at the runtime's own limit (-Xmx, V8's old
// space), usually well below the container limit, so raising the container
// limit alone does not help.
func runtimeOOM(logText string, m *store.Memory, limit uint64) string {
	usage := ""
	if m != nil && limit > 0 {
		usage = fmt.Sprintf(" The container was using %s of its %s limit, so this is the runtime's own cap, not the container's.", procfs.Bytes(m.WorkingSetBytes), procfs.Bytes(limit))
	}
	switch {
	case strings.Contains(logText, "java.lang.OutOfMemoryError: Java heap space"),
		strings.Contains(logText, "java.lang.OutOfMemoryError: GC overhead limit exceeded"):
		return "The JVM ran out of Java heap (its -Xmx or MaxRAMPercentage), not container memory." + usage + " Raise the heap only if the load is real; to see what filled it, add -XX:+HeapDumpOnOutOfMemoryError -XX:HeapDumpPath= pointing at a volume that survives the restart."
	case strings.Contains(logText, "java.lang.OutOfMemoryError: Metaspace"):
		return "The JVM ran out of Metaspace (class metadata): usually classes generated or loaded in a loop (proxies, scripting, redeploys), or -XX:MaxMetaspaceSize set too low."
	case strings.Contains(logText, "java.lang.OutOfMemoryError: Direct buffer memory"):
		return "The JVM ran out of direct (off-heap) buffer memory, set by -XX:MaxDirectMemorySize: typically Netty or NIO buffers that are not released."
	case strings.Contains(logText, "unable to create native thread"):
		return "The JVM could not start another thread: a thread leak, or the pod's pids limit was reached. The thread count in earlier captures shows which."
	case strings.Contains(logText, "JavaScript heap out of memory"):
		return "Node.js hit V8's old-space limit (--max-old-space-size, which defaults from the container limit)." + usage + " Retained objects are the cause; start node with --heapsnapshot-near-heap-limit=2 to get a heap snapshot next time."
	case strings.Contains(logText, "fatal error: runtime: out of memory"):
		return "The Go runtime could not get more memory from the OS (GOMEMLIMIT, ulimit or overcommit), before the container limit was reached."
	case strings.Contains(logText, "MemoryError"):
		return "Python raised MemoryError: an allocation failed (ulimit, or a single huge allocation) before the kernel OOM killer stepped in."
	}
	return ""
}

func lastLines(text string, n int) []string {
	var out []string
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 200 {
				l = l[:200] + "..."
			}
			out = append([]string{l}, out...)
		}
	}
	return out
}

// snapshotFindings carries what the pre-kill snapshots found into the OOM
// kill capture, so one capture tells the whole story.
func (a *Agent) snapshotFindings(ids []string) []string {
	var out []string
	for _, id := range ids {
		p, err := a.Store.Get(id)
		if err != nil || p.Kind != store.KindPressure {
			continue
		}
		for _, f := range p.Findings {
			if strings.HasPrefix(f, "JVM native memory tracking is off") {
				continue
			}
			out = append(out, fmt.Sprintf("Before the kill (%s, %.0f%% of limit): %s", id, 100*p.Memory.Ratio, f))
		}
		break // the newest snapshot is enough; List returns newest first
	}
	return out
}

// captureEviction records a pod evicted by the kubelet (node pressure).
func (a *Agent) captureEviction(ctx context.Context, p *corev1.Pod) {
	if !a.acquire(ctx) {
		return
	}
	defer a.release()
	c := a.base(store.KindEvicted, p, "", "", 0)
	c.Trigger = "pod evicted"
	w, err := a.Store.Begin(c)
	if err != nil {
		return
	}
	w.WriteFile("eviction.txt", "Eviction reason from the pod status", []byte(fmt.Sprintf("reason: %s\nmessage: %s\nphase: %s\n", p.Status.Reason, p.Status.Message, p.Status.Phase)))
	if a.cfg.CaptureLogs {
		for _, cs := range p.Status.ContainerStatuses {
			dir := logs.ContainerDir(p.Namespace, p.Name, string(p.UID), cs.Name)
			if path, err := logs.FindInstanceLog(dir, -1, time.Time{}); err == nil {
				if text, err := logs.Tail(path, a.cfg.LogLines, 4<<20); err == nil {
					w.WriteFile("logs-"+cs.Name+".txt", "Last log lines of container "+cs.Name, []byte(text))
				}
			}
		}
	}
	if ev := a.podEvents(ctx, p); ev != "" {
		w.WriteFile("events.txt", "Kubernetes events for the pod", []byte(ev))
	}
	c.Findings = append(c.Findings, "The kubelet evicted this pod: "+p.Status.Message)
	if strings.Contains(p.Status.Message, "memory") {
		c.Findings = append(c.Findings, "Node memory pressure evicts pods whose usage most exceeds their memory request first. Pods with requests below their real usage are the first to go.")
	}
	c.Headline = fmt.Sprintf("%s/%s was evicted from node %s", p.Namespace, p.Name, a.cfg.NodeName)
	a.finish(ctx, w, render(c, nil, nil))
}

// coreLoop picks up core dumps written by the core-handler helper.
func (a *Agent) coreLoop(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !a.pods.Synced() {
			continue // cores wait on disk until they can be matched to pods
		}
		files, _ := core.Pending(a.cfg.DataDir)
		for _, f := range files {
			a.captureCore(ctx, f)
		}
	}
}

func (a *Agent) captureCore(ctx context.Context, metaPath string) {
	m, err := core.ReadMeta(metaPath)
	if err != nil {
		_ = os.Rename(metaPath, metaPath+".bad")
		return
	}
	dir := filepath.Dir(metaPath)
	var pod *corev1.Pod
	container := ""
	var restarts int32
	if ref, ok := a.pods.Lookup(m.ContainerID); ok {
		pod, container, restarts = ref.Pod, ref.Container, ref.Restarts
	}
	if pod != nil && a.ignored(pod) {
		a.discardCore(dir, m, metaPath)
		return
	}
	key := m.ContainerID
	if pod != nil {
		key = pod.Namespace + "/" + pod.Name + "/" + container
	}
	if ok, _ := a.allowCapture("core", key, time.Now()); !ok {
		crashesSuppressed.Inc()
		a.discardCore(dir, m, metaPath)
		return
	}
	c := a.base(store.KindCoreDump, pod, container, m.ContainerID, restarts)
	c.Trigger = core.SignalName(m.Signal)
	c.Time = m.Time
	c.Runtime = "native"
	w, err := a.Store.Begin(c)
	if err != nil {
		return
	}
	if err := os.Rename(filepath.Join(dir, m.CoreFile), w.Path("core.gz")); err == nil {
		w.AddFile("core.gz", fmt.Sprintf("Gzipped core dump of %s. Debug with: gunzip core.gz && gdb <binary> core", m.Exe))
	} else {
		w.Errorf("move core: %v", err)
	}
	if m.MapsFile != "" {
		if err := os.Rename(filepath.Join(dir, m.MapsFile), w.Path("maps.txt")); err == nil {
			w.AddFile("maps.txt", "Memory map of the process at crash time (for symbolizing addresses)")
		}
	}
	w.WriteFile("crash.txt", "Crash metadata from the kernel", []byte(fmt.Sprintf("signal: %s (%d)\ncomm: %s\nexe: %s\ncmdline: %s\nhostPid: %d\ncoreBytes: %d\ntruncated: %v\ncgroup: %s\n",
		core.SignalName(m.Signal), m.Signal, m.Comm, m.Exe, procfs.RedactCmdline(m.Cmdline), m.HostPid, m.CoreBytes, m.Truncated, m.Cgroup)))
	if m.Truncated {
		c.Findings = append(c.Findings, fmt.Sprintf("The core was cut at %s (coreDumps.maxBytes). Stack traces usually still work; full memory inspection may not.", procfs.Bytes(uint64(m.CoreBytes))))
	}
	c.Findings = append(c.Findings, fmt.Sprintf("%s (%s) crashed with %s. Open the core with gdb or lldb using the same binary from image %s.", m.Comm, m.Exe, core.SignalName(m.Signal), c.Image))
	if prior, err := a.Store.List(store.Filter{ContainerID: m.ContainerID}); err == nil {
		for _, p := range prior {
			c.Related = append(c.Related, p.ID)
		}
	}
	if pod == nil {
		c.Pod = "(unknown pod)"
	}
	c.Headline = fmt.Sprintf("%s/%s dumped core: %s in %s", c.Pod, nz(c.Container, m.Comm), core.SignalName(m.Signal), m.Exe)
	id := c.ID
	a.finish(ctx, w, render(c, nil, nil))
	for _, r := range c.Related {
		_ = a.Store.Update(r, func(p *store.Capture) { p.Related = appendUnique(p.Related, id) })
	}
	_ = os.Remove(metaPath)
}

func (a *Agent) discardCore(dir string, m *core.Meta, metaPath string) {
	_ = os.Remove(filepath.Join(dir, m.CoreFile))
	if m.MapsFile != "" {
		_ = os.Remove(filepath.Join(dir, m.MapsFile))
	}
	_ = os.Remove(metaPath)
}

func (a *Agent) podEvents(ctx context.Context, p *corev1.Pod) string {
	if p == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	evs, err := a.kube.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.uid=" + string(p.UID)})
	if err != nil || len(evs.Items) == 0 {
		return ""
	}
	items := evs.Items
	sort.Slice(items, func(i, j int) bool { return eventTime(items[i]).Before(eventTime(items[j])) })
	var b bytes.Buffer
	for _, e := range items {
		fmt.Fprintf(&b, "%s %-7s %-22s x%-3d %s\n", eventTime(e).UTC().Format(time.RFC3339), e.Type, e.Reason, max(1, e.Count), e.Message)
	}
	return b.String()
}

func eventTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}

// finish commits the capture and fans out: Kubernetes Event, webhook, upload.
func (a *Agent) finish(ctx context.Context, w *store.Writer, summary string) {
	c := w.C
	if err := w.Commit(summary); err != nil {
		a.log.Error("commit capture", "id", c.ID, "err", err)
		w.Abort()
		captureErrors.Inc()
		return
	}
	capturesTotal.WithLabelValues(string(c.Kind)).Inc()
	storeBytes.Set(float64(a.Store.TotalBytes()))
	a.log.Info("capture saved", "id", c.ID, "kind", c.Kind, "pod", c.Namespace+"/"+c.Pod, "headline", c.Headline)
	if a.pods != nil && a.pods.Recorder != nil && c.Namespace != "" && c.PodUID != "" {
		ref := &corev1.ObjectReference{Kind: "Pod", Namespace: c.Namespace, Name: c.Pod, UID: typesUID(c.PodUID), APIVersion: "v1"}
		a.pods.Recorder.Eventf(ref, corev1.EventTypeWarning, eventReason(c.Kind), "%s. Details: kubectl lastwords show %s", c.Headline, c.ID)
	}
	// Uploads and notifications run on their own: a slow webhook or bucket
	// must not hold the capture slot that the next OOM needs, and must still
	// run when the capture itself used up its time budget.
	if a.upload != nil {
		go func() {
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
			defer cancel()
			loc, err := a.upload.Upload(uctx, a.Store.CaptureDir(c.ID), c)
			if err != nil {
				a.log.Warn("upload capture", "id", c.ID, "err", err)
				return
			}
			_ = a.Store.Update(c.ID, func(p *store.Capture) { p.UploadedTo = loc })
		}()
	}
	go func() {
		nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		a.notify.Send(nctx, c, summary)
	}()
}

func eventReason(k store.Kind) string {
	switch k {
	case store.KindPressure:
		return "LastWordsMemoryPressure"
	case store.KindOOMKill:
		return "LastWordsOOMKill"
	case store.KindProcessOOM:
		return "LastWordsProcessOOMKill"
	case store.KindCoreDump:
		return "LastWordsCoreDump"
	case store.KindEvicted:
		return "LastWordsEvicted"
	}
	return "LastWordsCrash"
}

// ctxReader stops a long copy when the capture's context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func nz(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
