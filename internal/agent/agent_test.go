package agent

import (
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
