// Package gopprof fetches heap and goroutine profiles from a Go program's
// net/http/pprof endpoint and summarizes where memory is held.
package gopprof

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/pprof/profile"
)

var client = &http.Client{Timeout: 20 * time.Second}

func get(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

// decompress gunzips data if it is gzipped, refusing output over max bytes
// (profile.Parse would inflate a gzip bomb without limit).
func decompress(data []byte, max int64) ([]byte, error) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, fmt.Errorf("heap profile inflates past %d MB", max>>20)
	}
	return raw, nil
}

// Profiles is what we pulled from one Go process.
type Profiles struct {
	Heap       []byte // gzipped protobuf, readable with `go tool pprof`
	Goroutines []byte // debug=1 text
	Top        []Site // top in-use allocation sites
	TotalInUse int64
}

// Site is one allocation location and the bytes still held from it.
type Site struct {
	Function string
	Location string
	InUse    int64
}

// Fetch pulls profiles from http://ip:port/debug/pprof/.
func Fetch(ctx context.Context, ip string, port int) (*Profiles, error) {
	base := "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/debug/pprof/"
	// The endpoint is served by the (untrusted) workload: every read is capped
	// so a bogus response cannot run the agent out of memory.
	heap, err := get(ctx, base+"heap", maxHeapProfile)
	if err != nil {
		return nil, err
	}
	p := &Profiles{Heap: heap}
	if g, err := get(ctx, base+"goroutine?debug=1", maxGoroutineDump); err == nil {
		p.Goroutines = g
	}
	p.Top, p.TotalInUse, _ = TopInUse(heap, 15)
	return p, nil
}

// Size limits for what a workload's pprof endpoint may send. Real heap
// profiles are a few MB even for large programs.
const (
	maxHeapProfile   = 32 << 20
	maxGoroutineDump = 8 << 20
	maxProfileRaw    = 64 << 20 // decompressed, before parsing
)

// TopInUse parses a heap profile and returns the top n sites by inuse_space,
// attributing each sample to its first non-runtime frame.
func TopInUse(data []byte, n int) ([]Site, int64, error) {
	raw, err := decompress(data, maxProfileRaw)
	if err != nil {
		return nil, 0, err
	}
	prof, err := profile.ParseData(raw)
	if err != nil {
		return nil, 0, err
	}
	idx := -1
	for i, st := range prof.SampleType {
		if st.Type == "inuse_space" {
			idx = i
		}
	}
	if idx < 0 {
		return nil, 0, fmt.Errorf("no inuse_space in profile")
	}
	agg := map[string]*Site{}
	var total int64
	for _, s := range prof.Sample {
		v := s.Value[idx]
		if v == 0 {
			continue
		}
		total += v
		fn, loc := frame(s)
		k := fn + "|" + loc
		if agg[k] == nil {
			agg[k] = &Site{Function: fn, Location: loc}
		}
		agg[k].InUse += v
	}
	out := make([]Site, 0, len(agg))
	for _, s := range agg {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InUse > out[j].InUse })
	if len(out) > n {
		out = out[:n]
	}
	return out, total, nil
}

func frame(s *profile.Sample) (string, string) {
	first := ""
	firstLoc := ""
	for _, l := range s.Location {
		for _, ln := range l.Line {
			if ln.Function == nil {
				continue
			}
			name := ln.Function.Name
			loc := fmt.Sprintf("%s:%d", ln.Function.Filename, ln.Line)
			if first == "" {
				first, firstLoc = name, loc
			}
			if strings.HasPrefix(name, "runtime.") || strings.HasPrefix(name, "internal/") ||
				strings.HasPrefix(name, "bytes.") || strings.HasPrefix(name, "strings.") ||
				strings.HasPrefix(name, "encoding/") || strings.HasPrefix(name, "bufio.") || strings.HasPrefix(name, "reflect.") {
				continue
			}
			return name, loc
		}
	}
	if first == "" {
		return "(unknown)", ""
	}
	return first, firstLoc
}

// GoroutineCount extracts the total from a debug=1 goroutine dump header
// ("goroutine profile: total 1234").
func GoroutineCount(text []byte) int {
	line, _, _ := bytes.Cut(text, []byte("\n"))
	f := strings.Fields(string(line))
	if len(f) > 0 {
		n, _ := strconv.Atoi(f[len(f)-1])
		return n
	}
	return 0
}
