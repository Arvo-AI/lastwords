// Package procfs reads per-process facts from host /proc for evidence capture.
package procfs

import (
	"bufio"
	"bytes"
	"debug/buildinfo"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Root is where host /proc is visible (the agent runs with hostPID).
var Root = "/proc"

// Runtime is a detected language runtime.
type Runtime string

const (
	RuntimeJVM     Runtime = "jvm"
	RuntimeGo      Runtime = "go"
	RuntimePython  Runtime = "python"
	RuntimeNode    Runtime = "node"
	RuntimeDotnet  Runtime = "dotnet"
	RuntimeRuby    Runtime = "ruby"
	RuntimeNative  Runtime = "native"
	RuntimeUnknown Runtime = ""
)

// Process is a snapshot of one process.
type Process struct {
	Pid       int
	Comm      string
	Cmdline   string
	RSSBytes  uint64
	HWMBytes  uint64
	Threads   int
	Runtime   Runtime
	GoVersion string
	Rollup    map[string]uint64 // smaps_rollup in bytes
}

func p(pid int, name string) string { return filepath.Join(Root, strconv.Itoa(pid), name) }

// Read snapshots a process. Missing files are tolerated (the process may be exiting).
func Read(pid int) (*Process, error) {
	st, err := os.ReadFile(p(pid, "status"))
	if err != nil {
		return nil, err
	}
	pr := &Process{Pid: pid}
	for _, line := range strings.Split(string(st), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Name":
			pr.Comm = v
		case "VmRSS":
			pr.RSSBytes = kb(v)
		case "VmHWM":
			pr.HWMBytes = kb(v)
		case "Threads":
			pr.Threads, _ = strconv.Atoi(v)
		}
	}
	if b, err := os.ReadFile(p(pid, "cmdline")); err == nil {
		pr.Cmdline = RedactCmdline(strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " ")))
	}
	if b, err := os.ReadFile(p(pid, "smaps_rollup")); err == nil {
		pr.Rollup = parseKB(b)
	}
	pr.Runtime, pr.GoVersion = DetectRuntime(pid, pr.Comm)
	return pr, nil
}

func kb(v string) uint64 {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseUint(f[0], 10, 64)
	return n * 1024
}

func parseKB(b []byte) map[string]uint64 {
	m := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || !strings.HasSuffix(strings.TrimSpace(v), "kB") {
			continue
		}
		m[k] = kb(v)
	}
	return m
}

// DetectRuntime identifies the language runtime of a process from its memory
// maps, executable and command name.
func DetectRuntime(pid int, comm string) (Runtime, string) {
	maps, _ := os.ReadFile(p(pid, "maps"))
	s := string(maps)
	switch {
	case strings.Contains(s, "/libjvm.so"):
		return RuntimeJVM, ""
	case strings.Contains(s, "libcoreclr.so"):
		return RuntimeDotnet, ""
	case strings.Contains(s, "libpython") || strings.HasPrefix(comm, "python"):
		return RuntimePython, ""
	case strings.Contains(s, "libnode.so") || comm == "node" || strings.HasPrefix(comm, "node"):
		return RuntimeNode, ""
	case strings.Contains(s, "libruby") || strings.HasPrefix(comm, "ruby"):
		return RuntimeRuby, ""
	}
	if bi, err := buildinfo.ReadFile(p(pid, "exe")); err == nil {
		return RuntimeGo, bi.GoVersion
	}
	if strings.HasPrefix(comm, "python") {
		return RuntimePython, ""
	}
	if len(maps) > 0 {
		return RuntimeNative, ""
	}
	return RuntimeUnknown, ""
}

// secretFlag matches command-line arguments that commonly carry secrets.
var secretWords = []string{"password", "passwd", "secret", "token", "apikey", "api-key", "api_key", "credential", "private-key", "auth"}

// urlCreds matches the password in scheme://user:password@host.
var urlCreds = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^/@\s:]*):[^/@\s]+@`)

// RedactCmdline masks values of arguments that look like secrets
// (--password=x, -Dapi.token=x, --token x, postgres://u:x@h, mysql -px).
func RedactCmdline(cmd string) string {
	f := strings.Fields(cmd)
	mysql := len(f) > 0 && (strings.HasPrefix(filepath.Base(f[0]), "mysql") || strings.HasPrefix(filepath.Base(f[0]), "mariadb"))
	for i := 0; i < len(f); i++ {
		f[i] = urlCreds.ReplaceAllString(f[i], "$1:<redacted>@")
		if mysql && strings.HasPrefix(f[i], "-p") && len(f[i]) > 2 {
			f[i] = "-p<redacted>"
			continue
		}
		low := strings.ToLower(f[i])
		hit := false
		for _, w := range secretWords {
			if strings.Contains(low, w) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if k, _, ok := strings.Cut(f[i], "="); ok {
			f[i] = k + "=<redacted>"
		} else if strings.HasPrefix(f[i], "-") && i+1 < len(f) && !strings.HasPrefix(f[i+1], "-") {
			f[i+1] = "<redacted>"
			i++
		}
	}
	return strings.Join(f, " ")
}

// Format renders processes as a readable table plus each smaps_rollup.
func Format(ps []*Process) string {
	sort.Slice(ps, func(i, j int) bool { return ps[i].RSSBytes > ps[j].RSSBytes })
	var b strings.Builder
	fmt.Fprintf(&b, "%-8s %-16s %-8s %10s %10s %7s  %s\n", "PID", "COMM", "RUNTIME", "RSS", "PEAK_RSS", "THREADS", "CMDLINE")
	for _, pr := range ps {
		rt := string(pr.Runtime)
		if pr.GoVersion != "" {
			rt += " " + pr.GoVersion
		}
		cmd := pr.Cmdline
		if len(cmd) > 300 {
			cmd = cmd[:300] + "..."
		}
		fmt.Fprintf(&b, "%-8d %-16s %-8s %10s %10s %7d  %s\n", pr.Pid, pr.Comm, rt, Bytes(pr.RSSBytes), Bytes(pr.HWMBytes), pr.Threads, cmd)
	}
	for _, pr := range ps {
		if len(pr.Rollup) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n# smaps_rollup pid %d (%s)\n", pr.Pid, pr.Comm)
		keys := make([]string, 0, len(pr.Rollup))
		for k := range pr.Rollup {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%-18s %10s\n", k, Bytes(pr.Rollup[k]))
		}
	}
	return b.String()
}

// Bytes formats a byte count as a short human string (MiB/GiB).
func Bytes(n uint64) string {
	const (
		ki = 1024
		mi = ki * 1024
		gi = mi * 1024
	)
	switch {
	case n >= gi:
		return fmt.Sprintf("%.2f GiB", float64(n)/gi)
	case n >= mi:
		return fmt.Sprintf("%.1f MiB", float64(n)/mi)
	case n >= ki:
		return fmt.Sprintf("%.0f KiB", float64(n)/ki)
	}
	return fmt.Sprintf("%d B", n)
}

// ProcessTree helps find the "main" process: the one with the largest RSS.
func Largest(ps []*Process) *Process {
	var best *Process
	for _, pr := range ps {
		if best == nil || pr.RSSBytes > best.RSSBytes {
			best = pr
		}
	}
	return best
}
