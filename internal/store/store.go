// Package store keeps captures on the node's disk: one directory per capture
// holding capture.json, summary.md and evidence files.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind is why a capture was taken.
type Kind string

const (
	// KindPressure: memory close to the container limit, captured before the kill.
	KindPressure Kind = "pressure"
	// KindOOMKill: the container was OOM-killed (the main process died).
	KindOOMKill Kind = "oomkill"
	// KindProcessOOM: a process inside the container was OOM-killed but the container survived.
	KindProcessOOM Kind = "process-oomkill"
	// KindCrash: the container exited with an error (non-zero exit, signal).
	KindCrash Kind = "crash"
	// KindCoreDump: the kernel produced a core dump for a process in the container.
	KindCoreDump Kind = "coredump"
	// KindEvicted: the kubelet evicted the pod.
	KindEvicted Kind = "evicted"
)

// File describes one evidence file in a capture.
type File struct {
	Name        string `json:"name"`
	Bytes       int64  `json:"bytes"`
	Description string `json:"description"`
}

// Memory is the memory picture at capture time.
type Memory struct {
	WorkingSetBytes uint64            `json:"workingSetBytes"`
	UsageBytes      uint64            `json:"usageBytes"`
	LimitBytes      uint64            `json:"limitBytes"`
	Ratio           float64           `json:"ratio"`
	GrowthBytesPerS float64           `json:"growthBytesPerSecond"`
	SecondsToLimit  float64           `json:"secondsToLimit,omitempty"`
	PSIFullAvg10    float64           `json:"psiFullAvg10,omitempty"`
	Breakdown       map[string]uint64 `json:"breakdown,omitempty"`
}

// Termination is the container's last state when it died.
type Termination struct {
	Reason     string    `json:"reason"`
	ExitCode   int32     `json:"exitCode"`
	Signal     int32     `json:"signal,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Message    string    `json:"message,omitempty"`
}

// Capture is the manifest stored as capture.json.
type Capture struct {
	ID            string       `json:"id"`
	Kind          Kind         `json:"kind"`
	Time          time.Time    `json:"time"`
	Node          string       `json:"node"`
	Namespace     string       `json:"namespace"`
	Pod           string       `json:"pod"`
	PodUID        string       `json:"podUID,omitempty"`
	Container     string       `json:"container"`
	ContainerID   string       `json:"containerID,omitempty"`
	Image         string       `json:"image,omitempty"`
	Owner         string       `json:"owner,omitempty"`
	RestartCount  int32        `json:"restartCount"`
	Runtime       string       `json:"runtime,omitempty"`
	Trigger       string       `json:"trigger"`
	Headline      string       `json:"headline"`
	Memory        *Memory      `json:"memory,omitempty"`
	Termination   *Termination `json:"termination,omitempty"`
	Findings      []string     `json:"findings,omitempty"`
	Files         []File       `json:"files"`
	Related       []string     `json:"related,omitempty"`
	Errors        []string     `json:"errors,omitempty"`
	UploadedTo    string       `json:"uploadedTo,omitempty"`
	SchemaVersion int          `json:"schemaVersion"`
}

// Store is a directory of captures.
type Store struct {
	Dir      string
	MaxBytes int64
	MaxAge   time.Duration
	mu       sync.Mutex
}

// New opens (and creates) a store at dir.
func New(dir string, maxBytes int64, maxAge time.Duration) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Store{Dir: dir, MaxBytes: maxBytes, MaxAge: maxAge}, nil
}

// NewID returns a sortable, unique capture ID: 20260102T150405Z-<6 hex>.
func NewID(t time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// Writer builds one capture in a temp directory and commits it atomically.
type Writer struct {
	s   *Store
	tmp string
	C   *Capture
}

// Begin starts a new capture.
func (s *Store) Begin(c *Capture) (*Writer, error) {
	if c.ID == "" {
		c.ID = NewID(c.Time)
	}
	c.SchemaVersion = 1
	tmp := filepath.Join(s.Dir, ".tmp-"+c.ID)
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return nil, err
	}
	return &Writer{s: s, tmp: tmp, C: c}, nil
}

// Path returns where a file with this name will live inside the capture.
func (w *Writer) Path(name string) string { return filepath.Join(w.tmp, name) }

// WriteFile adds an evidence file.
func (w *Writer) WriteFile(name, description string, data []byte) {
	if err := os.WriteFile(w.Path(name), data, 0o640); err != nil {
		w.Errorf("write %s: %v", name, err)
		return
	}
	w.AddFile(name, description)
}

// AddFile registers a file already written to Path(name).
func (w *Writer) AddFile(name, description string) {
	st, err := os.Stat(w.Path(name))
	if err != nil {
		w.Errorf("stat %s: %v", name, err)
		return
	}
	w.C.Files = append(w.C.Files, File{Name: name, Bytes: st.Size(), Description: description})
}

// Errorf records a non-fatal problem in the capture itself, so the reader
// knows what could not be collected and why.
func (w *Writer) Errorf(format string, args ...any) {
	w.C.Errors = append(w.C.Errors, fmt.Sprintf(format, args...))
}

// Commit writes capture.json and summary.md and moves the capture into place.
func (w *Writer) Commit(summary string) error {
	if summary != "" {
		w.WriteFile("summary.md", "Human and agent readable summary", []byte(summary))
	}
	b, err := json.MarshalIndent(w.C, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(w.Path("capture.json"), b, 0o640); err != nil {
		return err
	}
	final := filepath.Join(w.s.Dir, w.C.ID)
	if err := os.Rename(w.tmp, final); err != nil {
		return err
	}
	w.s.Prune()
	return nil
}

// Abort discards an uncommitted capture.
func (w *Writer) Abort() { _ = os.RemoveAll(w.tmp) }

// Update rewrites capture.json for an existing capture (used to add related IDs
// or the upload location after the fact).
func (s *Store) Update(id string, fn func(*Capture)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.Get(id)
	if err != nil {
		return err
	}
	fn(c)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(s.Dir, id, "capture.json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ErrNotFound is returned for unknown capture IDs.
var ErrNotFound = errors.New("capture not found")

func validID(id string) bool {
	return id != "" && !strings.ContainsAny(id, "/\\") && !strings.HasPrefix(id, ".")
}

// Get reads one capture manifest.
func (s *Store) Get(id string) (*Capture, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, id, "capture.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var c Capture
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Filter selects captures for List.
type Filter struct {
	Namespace   string
	Pod         string // prefix match
	Container   string
	ContainerID string
	Kind        Kind
	Since       time.Time
}

func (f Filter) match(c *Capture) bool {
	if f.Namespace != "" && c.Namespace != f.Namespace {
		return false
	}
	if f.Pod != "" && !strings.HasPrefix(c.Pod, f.Pod) {
		return false
	}
	if f.Container != "" && c.Container != f.Container {
		return false
	}
	if f.ContainerID != "" && c.ContainerID != f.ContainerID {
		return false
	}
	if f.Kind != "" && c.Kind != f.Kind {
		return false
	}
	if !f.Since.IsZero() && c.Time.Before(f.Since) {
		return false
	}
	return true
}

// List returns matching captures, newest first.
func (s *Store) List(f Filter) ([]*Capture, error) {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	var out []*Capture
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		c, err := s.Get(e.Name())
		if err != nil {
			continue
		}
		if f.match(c) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// Open opens an evidence file of a capture for reading.
func (s *Store) Open(id, name string) (*os.File, error) {
	if !validID(id) || name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		return nil, ErrNotFound
	}
	f, err := os.Open(filepath.Join(s.Dir, id, name))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	return f, err
}

// ReadFileTail returns up to max bytes of a file; when tail is true it returns
// the last max bytes instead of the first.
func (s *Store) ReadFileTail(id, name string, max int64, tail bool) ([]byte, int64, error) {
	f, err := s.Open(id, name)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	if max <= 0 || max > size {
		max = size
	}
	if tail && size > max {
		if _, err := f.Seek(size-max, io.SeekStart); err != nil {
			return nil, size, err
		}
	}
	b := make([]byte, max)
	n, err := io.ReadFull(f, b)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, size, err
	}
	return b[:n], size, nil
}

// Dir returns the on-disk directory of a capture.
func (s *Store) CaptureDir(id string) string { return filepath.Join(s.Dir, id) }

// Prune enforces MaxAge and MaxBytes, deleting the oldest captures first.
func (s *Store) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	type item struct {
		name string
		size int64
		mod  time.Time
	}
	var items []item
	var total int64
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(s.Dir, e.Name())
		if strings.HasPrefix(e.Name(), ".tmp-") {
			// Leftover from a crash mid-capture.
			if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > time.Hour {
				_ = os.RemoveAll(p)
			}
			continue
		}
		size := dirSize(p)
		items = append(items, item{e.Name(), size, captureTime(p, e.Name())})
		total += size
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	for i, it := range items {
		tooOld := s.MaxAge > 0 && time.Since(it.mod) > s.MaxAge
		// The newest capture is kept even if it alone is over the size budget:
		// deleting the evidence just collected would defeat the point.
		tooBig := s.MaxBytes > 0 && total > s.MaxBytes && i < len(items)-1
		if !tooOld && !tooBig {
			continue
		}
		_ = os.RemoveAll(filepath.Join(s.Dir, it.name))
		total -= it.size
	}
}

// captureTime is when a capture was taken, from its ID (which starts with a
// UTC timestamp) rather than the directory mtime, which updates bump.
func captureTime(dir, id string) time.Time {
	if len(id) >= 16 {
		if t, err := time.Parse("20060102T150405Z", id[:16]); err == nil {
			return t
		}
	}
	if st, err := os.Stat(dir); err == nil {
		return st.ModTime()
	}
	return time.Now()
}

func dirSize(p string) int64 {
	var n int64
	_ = filepath.Walk(p, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// TotalBytes reports the store's current size.
func (s *Store) TotalBytes() int64 { return dirSize(s.Dir) }
