package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const cid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func plentyOfDisk(t *testing.T) {
	t.Helper()
	orig := statDisk
	statDisk = func(string) (uint64, uint64, error) { return 100 << 30, 200 << 30, nil }
	t.Cleanup(func() { statDisk = orig })
}

func TestPodKeyIsStableAcrossRestarts(t *testing.T) {
	a := "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podAAA.slice/cri-containerd-" + cid + ".scope\n"
	other := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	b := "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podAAA.slice/cri-containerd-" + other + ".scope\n"
	if podKey(a, cid) != podKey(b, other) {
		t.Fatalf("restarted container should map to the same pod: %q vs %q", podKey(a, cid), podKey(b, other))
	}
	if got := podKey(a, cid); got != "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podAAA.slice" {
		t.Fatalf("podKey = %q", got)
	}
}

func TestAdmitLimitsCoresPerPod(t *testing.T) {
	plentyOfDisk(t)
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	for i := 0; i < maxCoresPerPod; i++ {
		if r := admit(dir, "podA", 1, now.Add(time.Duration(i)*time.Minute)); r != "" {
			t.Fatalf("core %d rejected: %s", i, r)
		}
	}
	if r := admit(dir, "podA", 1, now.Add(5*time.Minute)); r == "" {
		t.Fatal("crash-looping pod should be limited")
	}
	if r := admit(dir, "podB", 1, now.Add(5*time.Minute)); r != "" {
		t.Fatalf("other pod rejected: %s", r)
	}
	if r := admit(dir, "podA", 1, now.Add(ledgerWindow+5*time.Minute)); r != "" {
		t.Fatalf("limit should reset after the window: %s", r)
	}
}

func TestAdmitStopsWhenAgentIsBehind(t *testing.T) {
	plentyOfDisk(t)
	dir := t.TempDir()
	for i := 0; i < maxPendingCores; i++ {
		if err := os.WriteFile(filepath.Join(dir, string(rune('a'+i))+".core.gz"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if r := admit(dir, "podA", 1, time.Now()); r == "" {
		t.Fatal("should refuse while cores are piling up")
	}
}

func TestAdmitRefusesWhenDiskIsShort(t *testing.T) {
	orig := statDisk
	t.Cleanup(func() { statDisk = orig })
	statDisk = func(string) (uint64, uint64, error) { return 3 << 30, 200 << 30, nil }
	if r := admit(t.TempDir(), "podA", 1<<30, time.Now()); r == "" {
		t.Fatal("should keep 10% of the filesystem free")
	}
	statDisk = func(string) (uint64, uint64, error) { return 50 << 30, 60 << 30, nil }
	if r := admit(t.TempDir(), "podA", 30<<30, time.Now()); r == "" {
		t.Fatal("should refuse a core that could take more than half the free space")
	}
	if r := admit(t.TempDir(), "podA", 1<<30, time.Now()); r != "" {
		t.Fatalf("enough space, but refused: %s", r)
	}
}

func TestSweepRemovesOnlyStaleLeftovers(t *testing.T) {
	dir := t.TempDir()
	cores := filepath.Join(dir, "cores")
	if err := os.MkdirAll(cores, 0o750); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	files := map[string]bool{ // name -> should survive
		"a.core.gz.part":     false,
		"b.json.bad":         false,
		"c.core.gz":          false, // no c.json
		"d.core.gz":          true,
		"d.json":             true,
		"fresh.core.gz.part": true,
	}
	for name := range files {
		p := filepath.Join(cores, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name != "fresh.core.gz.part" {
			_ = os.Chtimes(p, old, old)
		}
	}
	Sweep(dir, time.Now())
	for name, keep := range files {
		_, err := os.Stat(filepath.Join(cores, name))
		if exists := err == nil; exists != keep {
			t.Errorf("%s: exists=%v, want %v", name, exists, keep)
		}
	}
}
