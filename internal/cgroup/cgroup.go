// Package cgroup reads container memory accounting from the host cgroup tree.
//
// It supports cgroup v2 (unified) and falls back to the cgroup v1 memory
// controller. All paths are absolute paths on the agent's view of the host
// cgroup mount (usually /host/sys/fs/cgroup).
package cgroup

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Version is the cgroup hierarchy version of a directory.
type Version int

const (
	V1 Version = 1
	V2 Version = 2
)

// Stats is one sample of a container's memory accounting.
type Stats struct {
	Version Version
	// Usage is memory.current (v2) or memory.usage_in_bytes (v1), in bytes.
	Usage uint64
	// Limit is memory.max / memory.limit_in_bytes. 0 means unlimited.
	Limit uint64
	// Stat is the parsed memory.stat file.
	Stat map[string]uint64
	// Events is the parsed memory.events file (v2 only; v1 exposes oom_kill via memory.oom_control).
	Events map[string]uint64
	// PSI holds memory pressure (v2 only, when the kernel has PSI enabled).
	PSI *Pressure
}

// Pressure is the parsed memory.pressure file.
type Pressure struct {
	SomeAvg10, SomeAvg60, FullAvg10, FullAvg60 float64
}

// WorkingSet is the number the kubelet and the OOM killer care about:
// usage minus inactive page cache, which the kernel can reclaim before killing.
func (s Stats) WorkingSet() uint64 {
	inactive := s.Stat["inactive_file"]
	if s.Version == V1 {
		inactive = s.Stat["total_inactive_file"]
	}
	if inactive > s.Usage {
		return 0
	}
	return s.Usage - inactive
}

// Ratio is working set divided by limit, or 0 when unlimited.
func (s Stats) Ratio() float64 {
	if s.Limit == 0 {
		return 0
	}
	return float64(s.WorkingSet()) / float64(s.Limit)
}

// OOMKills is the cumulative number of OOM kills inside this cgroup.
func (s Stats) OOMKills() uint64 {
	return s.Events["oom_kill"]
}

// DetectVersion reports whether dir is a cgroup v2 or v1 memory directory.
func DetectVersion(dir string) Version {
	if _, err := os.Stat(filepath.Join(dir, "memory.current")); err == nil {
		return V2
	}
	return V1
}

// Read samples memory accounting for the cgroup directory dir.
func Read(dir string) (Stats, error) {
	if DetectVersion(dir) == V2 {
		return readV2(dir)
	}
	return readV1(dir)
}

func readV2(dir string) (Stats, error) {
	s := Stats{Version: V2}
	var err error
	if s.Usage, err = readUint(filepath.Join(dir, "memory.current")); err != nil {
		return s, err
	}
	s.Limit, _ = readLimit(filepath.Join(dir, "memory.max"))
	s.Stat, _ = readKV(filepath.Join(dir, "memory.stat"))
	s.Events, _ = readKV(filepath.Join(dir, "memory.events"))
	if p, err := ReadPressure(filepath.Join(dir, "memory.pressure")); err == nil {
		s.PSI = &p
	}
	return s, nil
}

func readV1(dir string) (Stats, error) {
	s := Stats{Version: V1}
	var err error
	if s.Usage, err = readUint(filepath.Join(dir, "memory.usage_in_bytes")); err != nil {
		return s, err
	}
	s.Limit, _ = readLimit(filepath.Join(dir, "memory.limit_in_bytes"))
	s.Stat, _ = readKV(filepath.Join(dir, "memory.stat"))
	if oc, err := readKV(filepath.Join(dir, "memory.oom_control")); err == nil {
		s.Events = map[string]uint64{"oom_kill": oc["oom_kill"]}
	}
	return s, nil
}

// v1 reports "unlimited" as a huge page-aligned number.
const v1Unlimited = uint64(1) << 62

func readLimit(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v := strings.TrimSpace(string(b))
	if v == "max" {
		return 0, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, err
	}
	if n >= v1Unlimited {
		return 0, nil
	}
	return n, nil
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func readKV(path string) (map[string]uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := make(map[string]uint64)
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		if n, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			m[f[0]] = n
		}
	}
	return m, nil
}

// ReadPressure parses a PSI file such as memory.pressure.
func ReadPressure(path string) (Pressure, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Pressure{}, err
	}
	return ParsePressure(string(b))
}

// ParsePressure parses PSI text:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
func ParsePressure(text string) (Pressure, error) {
	var p Pressure
	found := false
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		vals := map[string]float64{}
		for _, kv := range f[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			if x, err := strconv.ParseFloat(v, 64); err == nil {
				vals[k] = x
			}
		}
		switch f[0] {
		case "some":
			p.SomeAvg10, p.SomeAvg60 = vals["avg10"], vals["avg60"]
			found = true
		case "full":
			p.FullAvg10, p.FullAvg60 = vals["avg10"], vals["avg60"]
			found = true
		}
	}
	if !found {
		return p, errors.New("no PSI lines")
	}
	return p, nil
}

// Procs lists the PIDs (in the agent's PID namespace, i.e. host PIDs when
// the agent runs with hostPID) that belong to the cgroup directory.
func Procs(dir string) ([]int, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, l := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(l); err == nil {
			pids = append(pids, n)
		}
	}
	return pids, nil
}
