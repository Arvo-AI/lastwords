// Package logs copies the last lines a container wrote before it died, from
// the kubelet's CRI log files, before the next restart rotates them away.
package logs

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Root is where host /var/log/pods is visible.
var Root = "/var/log/pods"

// ContainerDir returns the kubelet log directory of a container.
func ContainerDir(namespace, pod, podUID, container string) string {
	return filepath.Join(Root, fmt.Sprintf("%s_%s_%s", namespace, pod, podUID), container)
}

// FindInstanceLog picks the log file of one container instance. The kubelet
// names files <attempt>.log, so with a known attempt (>= 0) the file is exact.
// Otherwise it is the newest file last written no later than shortly after
// finishedAt, or simply the newest file.
func FindInstanceLog(dir string, attempt int32, finishedAt time.Time) (string, error) {
	if attempt >= 0 {
		p := filepath.Join(dir, strconv.Itoa(int(attempt))+".log")
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, nil
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	type cand struct {
		n    int
		path string
		mod  time.Time
	}
	var cs []cand
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".log") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(name, ".log"))
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cs = append(cs, cand{n, filepath.Join(dir, name), info.ModTime()})
	}
	if len(cs) == 0 {
		return "", fmt.Errorf("no log files in %s", dir)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].n > cs[j].n })
	if !finishedAt.IsZero() {
		for _, c := range cs {
			if !c.mod.After(finishedAt.Add(5 * time.Second)) {
				return c.path, nil
			}
		}
	}
	return cs[0].path, nil
}

// Tail returns the last maxLines lines (at most maxBytes) of a CRI log file,
// converted to "TIMESTAMP STREAM message" with partial lines joined.
func Tail(path string, maxLines int, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := st.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	if start > 0 {
		// Drop the first, probably cut, line.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	lines := ParseCRI(data)
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// ParseCRI converts CRI log format lines
//
//	2026-01-02T03:04:05.123456789Z stdout F hello
//
// into "2026-01-02T03:04:05.123Z stdout hello", joining partial (P) chunks.
// Lines that are not CRI formatted (docker json-file etc.) are kept as is.
func ParseCRI(data []byte) []string {
	var out []string
	var partial strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		parts := strings.SplitN(line, " ", 4)
		if len(parts) < 3 {
			out = append(out, line)
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, parts[0])
		if err != nil || (parts[1] != "stdout" && parts[1] != "stderr") {
			out = append(out, line)
			continue
		}
		msg := ""
		if len(parts) == 4 {
			msg = parts[3]
		}
		if parts[2] == "P" {
			partial.WriteString(msg)
			continue
		}
		if partial.Len() > 0 {
			msg = partial.String() + msg
			partial.Reset()
		}
		out = append(out, ts.UTC().Format("2006-01-02T15:04:05.000Z")+" "+parts[1]+" "+msg)
	}
	if partial.Len() > 0 {
		out = append(out, partial.String())
	}
	return out
}

// Interesting returns log lines that look like the cause of death: panics,
// fatal errors, OOM messages, stack trace heads. At most max lines.
func Interesting(text string, max int) []string {
	keys := []string{"panic:", "fatal error:", "Exception", "OutOfMemoryError", "Segmentation fault", "SIGSEGV", "SIGABRT", "Traceback (most recent call last)", "FATAL", "Fatal", "out of memory", "Killed", "core dumped", "unhandled", "Unhandled"}
	var out []string
	for _, l := range strings.Split(text, "\n") {
		for _, k := range keys {
			if strings.Contains(l, k) {
				if len(l) > 300 {
					l = l[:300] + "..."
				}
				out = append(out, l)
				break
			}
		}
	}
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// CrashSite finds where a program crashed from a stack trace in its logs:
// the first application frame of a Go panic, a Java exception, or the last
// frame of a Python traceback. It returns "" when there is no trace.
func CrashSite(text string) string {
	lines := strings.Split(text, "\n")
	msg := func(l string) string {
		// Strip the "TIMESTAMP stream " prefix added by ParseCRI.
		if f := strings.SplitN(l, " ", 3); len(f) == 3 && (f[1] == "stdout" || f[1] == "stderr") {
			return f[2]
		}
		return l
	}
	// Go: "panic: ..." then "goroutine N [running]:" then pairs of "func(...)" / "\tfile:line +0x..".
	for i := len(lines) - 1; i >= 0; i-- {
		m := msg(lines[i])
		if !strings.HasPrefix(m, "panic: ") && !strings.HasPrefix(m, "fatal error: ") {
			continue
		}
		for j := i + 1; j+1 < len(lines); j++ {
			fn := strings.TrimSpace(msg(lines[j]))
			loc := strings.TrimSpace(msg(lines[j+1]))
			if fn == "" || strings.HasPrefix(fn, "goroutine ") || !strings.HasPrefix(loc, "/") {
				continue
			}
			if strings.HasPrefix(fn, "runtime.") || strings.HasPrefix(fn, "panic(") || strings.HasPrefix(fn, "internal/") {
				j++
				continue
			}
			if k := strings.Index(fn, "("); k > 0 {
				fn = fn[:k]
			}
			if k := strings.Index(loc, " +0x"); k > 0 {
				loc = loc[:k]
			}
			return fmt.Sprintf("%s at %s (%s)", fn, loc, strings.TrimPrefix(m, "panic: "))
		}
	}
	// Java: "Exception in thread ..." or "...Exception: msg" followed by "\tat pkg.Class.method(File.java:N)".
	for i := len(lines) - 1; i >= 0; i-- {
		m := strings.TrimSpace(msg(lines[i]))
		if !(strings.Contains(m, "Exception") || strings.Contains(m, "Error:")) || strings.HasPrefix(m, "at ") {
			continue
		}
		for j := i + 1; j < len(lines) && j < i+40; j++ {
			f := strings.TrimSpace(msg(lines[j]))
			if !strings.HasPrefix(f, "at ") {
				if strings.HasPrefix(f, "...") || strings.HasPrefix(f, "Caused by") {
					continue
				}
				break
			}
			fr := strings.TrimPrefix(f, "at ")
			if strings.HasPrefix(fr, "java.") || strings.HasPrefix(fr, "jdk.") || strings.HasPrefix(fr, "sun.") || strings.HasPrefix(fr, "javax.") {
				continue
			}
			return fmt.Sprintf("%s (%s)", fr, m)
		}
	}
	// Python: last 'File "x.py", line N, in fn' before the exception line.
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.Contains(msg(lines[i]), "Traceback (most recent call last)") {
			continue
		}
		var site, exc string
		for j := i + 1; j < len(lines); j++ {
			m := strings.TrimSpace(msg(lines[j]))
			if strings.HasPrefix(m, "File \"") {
				site = m
			} else if site != "" && !strings.HasPrefix(msg(lines[j]), " ") && m != "" && !strings.HasPrefix(m, "File") {
				exc = m
				if !strings.HasPrefix(lines[j], " ") && strings.Contains(m, ":") {
					break
				}
			}
		}
		if site != "" {
			return fmt.Sprintf("%s (%s)", site, exc)
		}
	}
	return ""
}
