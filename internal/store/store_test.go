package store

import (
	"strings"
	"testing"
	"time"
)

func commit(t *testing.T, s *Store, kind Kind, pod string, at time.Time, payload int) string {
	t.Helper()
	w, err := s.Begin(&Capture{Kind: kind, Time: at, Namespace: "shop", Pod: pod, ContainerID: "cid-" + pod})
	if err != nil {
		t.Fatal(err)
	}
	w.WriteFile("logs.txt", "logs", []byte(strings.Repeat("x", payload)))
	if err := w.Commit("# summary\n"); err != nil {
		t.Fatal(err)
	}
	return w.C.ID
}

func TestCommitListGetFiles(t *testing.T) {
	s, _ := New(t.TempDir(), 0, 0)
	t0 := time.Now().Add(-time.Hour)
	a := commit(t, s, KindPressure, "checkout-1", t0, 10)
	b := commit(t, s, KindOOMKill, "checkout-1", t0.Add(time.Minute), 10)
	commit(t, s, KindCrash, "billing-1", t0.Add(2*time.Minute), 10)

	all, _ := s.List(Filter{})
	if len(all) != 3 || all[0].Pod != "billing-1" {
		t.Fatalf("list newest first: %+v", all)
	}
	got, _ := s.List(Filter{Pod: "checkout", Kind: KindOOMKill})
	if len(got) != 1 || got[0].ID != b {
		t.Fatalf("filter: %+v", got)
	}
	if err := s.Update(a, func(c *Capture) { c.Related = []string{b} }); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Get(a); len(c.Related) != 1 {
		t.Fatal("update lost")
	}
	data, size, err := s.ReadFileTail(a, "logs.txt", 4, true)
	if err != nil || string(data) != "xxxx" || size != 10 {
		t.Fatalf("tail read: %q %d %v", data, size, err)
	}
}

func TestRejectsPathTraversal(t *testing.T) {
	s, _ := New(t.TempDir(), 0, 0)
	id := commit(t, s, KindCrash, "p", time.Now(), 1)
	for _, name := range []string{"../capture.json", "..", ".hidden", "a/b"} {
		if _, err := s.Open(id, name); err == nil {
			t.Errorf("Open(%q) should fail", name)
		}
	}
	if _, err := s.Get("../" + id); err == nil {
		t.Error("Get with traversal should fail")
	}
}

func TestPruneBySize(t *testing.T) {
	s, _ := New(t.TempDir(), 0, 0)
	t0 := time.Now().Add(-time.Hour)
	oldest := commit(t, s, KindCrash, "a", t0, 4000)
	commit(t, s, KindCrash, "b", t0.Add(time.Minute), 4000)
	newest := commit(t, s, KindCrash, "c", t0.Add(2*time.Minute), 4000)
	s.MaxBytes = 9000
	s.Prune()
	if _, err := s.Get(oldest); err == nil {
		t.Fatal("oldest capture should be pruned first")
	}
	if _, err := s.Get(newest); err != nil {
		t.Fatal("newest capture must survive")
	}
}

func TestPruneKeepsNewestEvenIfOverBudget(t *testing.T) {
	s, _ := New(t.TempDir(), 0, 0)
	t0 := time.Now().Add(-time.Hour)
	old := commit(t, s, KindCrash, "a", t0, 100)
	big := commit(t, s, KindCoreDump, "b", t0.Add(time.Minute), 20000)
	s.MaxBytes = 10000
	s.Prune()
	if _, err := s.Get(big); err != nil {
		t.Fatal("the capture just taken was deleted for being large")
	}
	if _, err := s.Get(old); err == nil {
		t.Fatal("older captures should still make room")
	}
}

func TestPruneByAgeUsesCaptureTimeNotUpdates(t *testing.T) {
	s, _ := New(t.TempDir(), 0, 0)
	old := commit(t, s, KindPressure, "a", time.Now().Add(-10*24*time.Hour), 10)
	recent := commit(t, s, KindCrash, "b", time.Now(), 10)
	// Linking a later capture rewrites the old one's manifest (and its mtime).
	if err := s.Update(old, func(c *Capture) { c.Related = append(c.Related, recent) }); err != nil {
		t.Fatal(err)
	}
	s.MaxAge = 7 * 24 * time.Hour
	s.Prune()
	if _, err := s.Get(old); err == nil {
		t.Fatal("a 10-day-old capture survived MaxAge because it was updated")
	}
	if _, err := s.Get(recent); err != nil {
		t.Fatal(err)
	}
}
