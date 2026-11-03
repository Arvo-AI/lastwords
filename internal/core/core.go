// Package core captures core dumps from crashing containers.
//
// The kernel's core_pattern is node-wide and not namespaced. We point it at a
// small helper (this same binary, copied onto the host) that the kernel runs
// in the host's root namespaces with the core on stdin:
//
//	|/var/lib/lastwords/bin/lastwords core-handler %P %s %t %e
//
// The helper writes the gzipped core and metadata into <hostDir>/cores, and
// the agent picks them up, joins them to the pod, and turns them into captures.
package core

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Arvo-AI/lastwords/internal/cgroup"
)

// Meta is written next to each core by the helper.
type Meta struct {
	HostPid     int       `json:"hostPid"`
	Signal      int       `json:"signal"`
	Time        time.Time `json:"time"`
	Comm        string    `json:"comm"`
	Exe         string    `json:"exe"`
	Cmdline     string    `json:"cmdline"`
	ContainerID string    `json:"containerID"`
	Cgroup      string    `json:"cgroup"`
	CoreFile    string    `json:"coreFile"`
	CoreBytes   int64     `json:"coreBytes"`
	Truncated   bool      `json:"truncated"`
	MapsFile    string    `json:"mapsFile,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// Config is written by the agent to <hostDir>/core.conf for the helper.
type Config struct {
	MaxBytes int64 `json:"maxBytes"`
	// OnlyContainers: when true, ignore crashes of processes outside containers
	// (node daemons), and let them dump nowhere.
	OnlyContainers bool `json:"onlyContainers"`
}

const patternFile = "/proc/sys/kernel/core_pattern"

// Pattern returns the core_pattern line for a helper at hostDir/bin/lastwords.
func Pattern(hostDir string) string {
	return "|" + filepath.Join(hostDir, "bin", "lastwords") + " core-handler %P %s %t %e"
}

// Install copies the running binary to agentDir/bin/lastwords, writes the
// helper config, and points the kernel at hostDir (the same directory as the
// kernel sees it). It saves the previous pattern to agentDir/core_pattern.orig.
func Install(agentDir, hostDir string, cfg Config) (previous string, err error) {
	if err := os.MkdirAll(filepath.Join(agentDir, "bin"), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(agentDir, "cores"), 0o750); err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if err := copyFile(self, filepath.Join(agentDir, "bin", "lastwords"), 0o755); err != nil {
		return "", fmt.Errorf("install helper: %w", err)
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(agentDir, "core.conf"), b, 0o644); err != nil {
		return "", err
	}
	prevB, err := os.ReadFile(patternFile)
	if err != nil {
		return "", err
	}
	previous = strings.TrimSpace(string(prevB))
	want := Pattern(hostDir)
	if len(want) >= 128 {
		return previous, fmt.Errorf("core_pattern %q is %d chars; the kernel limit is 127. Use a shorter host directory", want, len(want))
	}
	if previous != want {
		_ = os.WriteFile(filepath.Join(agentDir, "core_pattern.orig"), []byte(previous+"\n"), 0o644)
	}
	if err := os.WriteFile(patternFile, []byte(want), 0o644); err != nil {
		return previous, fmt.Errorf("set core_pattern (needs a privileged agent): %w", err)
	}
	return previous, nil
}

// Restore puts back the core_pattern saved by Install, if ours is still set.
func Restore(agentDir, hostDir string) error {
	cur, err := os.ReadFile(patternFile)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(cur)) != Pattern(hostDir) {
		return nil
	}
	orig, err := os.ReadFile(filepath.Join(agentDir, "core_pattern.orig"))
	if err != nil {
		orig = []byte("core")
	}
	return os.WriteFile(patternFile, []byte(strings.TrimSpace(string(orig))), 0o644)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Handle is the helper entry point. args are the expanded %P %s %t %e.
// It must never block the crashing process for long and never panic.
func Handle(args []string, stdin io.Reader) error {
	if len(args) < 3 {
		return errors.New("usage: core-handler <pid> <signal> <time> [comm]")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	hostDir := filepath.Dir(filepath.Dir(self))
	cfg := Config{MaxBytes: 1 << 30, OnlyContainers: true}
	if b, err := os.ReadFile(filepath.Join(hostDir, "core.conf")); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	pid, _ := strconv.Atoi(args[0])
	sig, _ := strconv.Atoi(args[1])
	ts, _ := strconv.ParseInt(args[2], 10, 64)
	m := Meta{HostPid: pid, Signal: sig, Time: time.Unix(ts, 0).UTC(), Comm: strings.Join(args[3:], " ")}

	// Read /proc first: the process stays around until we finish reading the core.
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	if b, err := os.ReadFile(filepath.Join(procDir, "cgroup")); err == nil {
		m.Cgroup = strings.TrimSpace(string(b))
		m.ContainerID = cgroup.ContainerIDFromProcCgroup(m.Cgroup)
	}
	if cfg.OnlyContainers && m.ContainerID == "" {
		return nil // returning without reading drops the core
	}
	dir := filepath.Join(hostDir, "cores")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	// This runs before the agent sees anything, so it guards the node itself:
	// a crash-looping container must not fill the disk with cores.
	if reason := admit(dir, podKey(m.Cgroup, m.ContainerID), cfg.MaxBytes, m.Time); reason != "" {
		return errors.New("core dropped: " + reason)
	}
	m.Exe, _ = os.Readlink(filepath.Join(procDir, "exe"))
	if b, err := os.ReadFile(filepath.Join(procDir, "cmdline")); err == nil {
		m.Cmdline = strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
	}
	base := fmt.Sprintf("%s-%d", m.Time.Format("20060102T150405Z"), pid)
	if maps, err := os.ReadFile(filepath.Join(procDir, "maps")); err == nil {
		m.MapsFile = base + ".maps"
		_ = os.WriteFile(filepath.Join(dir, m.MapsFile), maps, 0o640)
	}

	m.CoreFile = base + ".core.gz"
	f, err := os.OpenFile(filepath.Join(dir, m.CoreFile+".part"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	zw, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	n, err := io.Copy(zw, io.LimitReader(bufio.NewReaderSize(stdin, 1<<20), cfg.MaxBytes))
	m.CoreBytes = n
	if err != nil {
		m.Error = err.Error()
	}
	if n >= cfg.MaxBytes {
		m.Truncated = true
	}
	_ = zw.Close()
	_ = f.Close()
	_ = os.Rename(filepath.Join(dir, m.CoreFile+".part"), filepath.Join(dir, m.CoreFile))
	b, _ := json.MarshalIndent(m, "", "  ")
	// The .json appears last; the agent only picks up cores whose .json exists.
	tmp := filepath.Join(dir, base+".json.part")
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, base+".json"))
}

// Limits enforced by the helper before it writes anything.
const (
	maxPendingCores = 4 // cores written but not yet picked up by the agent
	maxCoresPerPod  = 3 // per pod in ledgerWindow, like the agent's crash budget
	ledgerWindow    = 30 * time.Minute
	minFreeFraction = 0.10 // always leave this share of the filesystem free
)

// admit decides whether a core may be written to dir. It returns "" to accept,
// or why not. It records accepted cores per pod in a small ledger file, under
// an flock because the kernel runs one helper per crash, possibly in parallel.
func admit(dir, key string, maxBytes int64, now time.Time) string {
	if pending, _ := filepath.Glob(filepath.Join(dir, "*.core.gz*")); len(pending) >= maxPendingCores {
		return fmt.Sprintf("%d cores already waiting for the agent", len(pending))
	}
	if free, total, err := statDisk(dir); err == nil {
		if free < uint64(maxBytes)*2 || float64(free) < minFreeFraction*float64(total) {
			return fmt.Sprintf("only %d MiB free on the capture filesystem", free>>20)
		}
	}
	lf, err := os.OpenFile(filepath.Join(dir, ".ledger"), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return "" // no ledger: still bounded by the pending and disk checks
	}
	defer lf.Close()
	if err := lockFile(lf); err != nil {
		return ""
	}
	ledger := map[string][]int64{}
	if b, err := io.ReadAll(io.LimitReader(lf, 1<<20)); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &ledger)
	}
	cutoff := now.Add(-ledgerWindow).Unix()
	for k, ts := range ledger {
		kept := ts[:0]
		for _, t := range ts {
			if t > cutoff {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(ledger, k)
		} else {
			ledger[k] = kept
		}
	}
	reason := ""
	if len(ledger[key]) >= maxCoresPerPod {
		reason = fmt.Sprintf("%d cores from this pod in the last %s", len(ledger[key]), ledgerWindow)
	} else {
		ledger[key] = append(ledger[key], now.Unix())
	}
	b, _ := json.Marshal(ledger)
	if err := lf.Truncate(0); err == nil {
		_, _ = lf.WriteAt(b, 0)
	}
	return reason
}

// podKey identifies the pod a crashing process belongs to: the parent of its
// container cgroup (the pod cgroup), which stays the same across restarts.
func podKey(procCgroup, containerID string) string {
	for _, line := range strings.Split(procCgroup, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && containerID != "" && strings.Contains(parts[2], containerID) {
			return filepath.Dir(strings.TrimRight(parts[2], "/"))
		}
	}
	return containerID
}

// Pending lists finished core metadata files in agentDir/cores.
func Pending(agentDir string) ([]string, error) {
	return filepath.Glob(filepath.Join(agentDir, "cores", "*.json"))
}

// Sweep removes leftovers in agentDir/cores that will never become captures:
// partial files from a helper killed mid-write, cores whose metadata never
// arrived, and metadata the agent could not parse.
func Sweep(agentDir string, now time.Time) {
	ents, err := os.ReadDir(filepath.Join(agentDir, "cores"))
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || name == ".ledger" || strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < time.Hour {
			continue
		}
		p := filepath.Join(agentDir, "cores", name)
		if strings.HasSuffix(name, ".bad") || strings.HasSuffix(name, ".part") {
			_ = os.Remove(p)
			continue
		}
		// A core or maps file whose .json is missing after an hour is orphaned.
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".core.gz"), ".maps")
		if _, err := os.Stat(filepath.Join(agentDir, "cores", base+".json")); os.IsNotExist(err) {
			_ = os.Remove(p)
		}
	}
}

// ReadMeta loads one metadata file.
func ReadMeta(path string) (*Meta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Meta
	return &m, json.Unmarshal(b, &m)
}

// SignalName names common fatal signals.
func SignalName(sig int) string {
	names := map[int]string{4: "SIGILL", 5: "SIGTRAP", 6: "SIGABRT", 7: "SIGBUS", 8: "SIGFPE", 11: "SIGSEGV", 3: "SIGQUIT", 31: "SIGSYS"}
	if n, ok := names[sig]; ok {
		return n
	}
	return "signal " + strconv.Itoa(sig)
}
