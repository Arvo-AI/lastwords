package agent

import (
	"github.com/Arvo-AI/lastwords/internal/cgroup"
	"testing"
	"time"
)

func TestCaptureBudget(t *testing.T) {
	a := &Agent{crashes: map[string][]time.Time{}}
	now := time.Unix(10000, 0)
	for i := 0; i < maxCrashCaptures; i++ {
		if ok, _ := a.allowCapture("death", "ns/p/c", now.Add(time.Duration(i)*time.Minute)); !ok {
			t.Fatalf("crash %d should be captured", i)
		}
	}
	if ok, _ := a.allowCapture("death", "ns/p/c", now.Add(5*time.Minute)); ok {
		t.Fatal("crash loop should be suppressed")
	}
	// Another kind and another container have their own budgets.
	if ok, _ := a.allowCapture("core", "ns/p/c", now.Add(5*time.Minute)); !ok {
		t.Fatal("core budget should be separate from the death budget")
	}
	if ok, _ := a.allowCapture("death", "ns/q/c", now.Add(5*time.Minute)); !ok {
		t.Fatal("another container should not be suppressed")
	}
	if ok, _ := a.allowCapture("death", "ns/p/c", now.Add(crashWindow+3*time.Minute)); !ok {
		t.Fatal("budget should refill after the window")
	}
}

// Replays the leaky JVM measured on EKS: 384 MiB limit, 5.5 MiB/s, and a
// young GC at ~80% that jumps past the limit. The first capture has to come
// before that point for JVMs, while other runtimes keep the plain tiers.
func TestJVMTiersFireBeforeTheGCKill(t *testing.T) {
	a := &Agent{cfg: Defaults()}
	const limit = 384 << 20
	const growth = 5.5 * (1 << 20)
	first := func(jvm bool) float64 {
		for ws := uint64(limit / 10); ws < limit; ws += 1 << 20 {
			st := cgroup.Stats{Version: cgroup.V2, Usage: ws, Limit: limit, Stat: map[string]uint64{}}
			if a.evaluate(nil, st, growth, jvm) != "" {
				return float64(ws) / limit
			}
		}
		return 1
	}
	if r := first(true); r >= 0.75 {
		t.Fatalf("JVM first capture at %.0f%% of the limit; its GC kills it at ~80%%", 100*r)
	}
	if r := first(false); r < 0.70 {
		t.Fatalf("non-JVM captured at %.0f%%, earlier than the plain predictive floor", 100*r)
	}
}
