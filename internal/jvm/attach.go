// Package jvm talks to a running HotSpot JVM through the Dynamic Attach
// mechanism (the same protocol `jcmd` and `jattach` use), from outside the
// target container.
//
// Protocol (Linux):
//  1. Create the trigger file /tmp/.attach_pid<nspid> in the target's mount
//     namespace and send SIGQUIT to the target.
//  2. The JVM's attach listener creates the UNIX socket /tmp/.java_pid<nspid>.
//  3. Write "1\0<cmd>\0<arg1>\0<arg2>\0<arg3>\0" to the socket.
//  4. Read "<rc>\n<output>" until EOF.
//
// We reach the target's /tmp through /proc/<hostpid>/root, which works from an
// agent running with hostPID. Everything under that /tmp is controlled by the
// target, so the Linux implementation pins a directory fd and never follows
// a symlink there: see attach_linux.go.
package jvm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ProcRoot is where host /proc is visible. Overridable for tests.
var ProcRoot = "/proc"

// IsJVM reports whether the process maps libjvm.so. We must be certain before
// sending SIGQUIT: for a non-JVM process SIGQUIT's default action is to
// terminate (and core dump).
func IsJVM(pid int) bool {
	f, err := os.Open(filepath.Join(ProcRoot, strconv.Itoa(pid), "maps"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if strings.HasSuffix(sc.Text(), "/libjvm.so") {
			return true
		}
	}
	return false
}

// NSPid returns the PID of the process inside its own PID namespace (the
// innermost value of NSpid in /proc/<pid>/status), plus its uid and gid.
func NSPid(pid int) (nspid, uid, gid int, err error) {
	b, err := os.ReadFile(filepath.Join(ProcRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, 0, 0, err
	}
	nspid = pid
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "NSpid:":
			if n, err := strconv.Atoi(f[len(f)-1]); err == nil {
				nspid = n
			}
		case "Uid:":
			uid, _ = strconv.Atoi(f[2]) // effective uid
		case "Gid:":
			gid, _ = strconv.Atoi(f[2])
		}
	}
	return nspid, uid, gid, nil
}

// Client is a connection target for one JVM process.
type Client struct {
	Pid     int // host PID
	NSPid   int
	UID     int
	GID     int
	Timeout time.Duration // per command, on top of the caller's context
	h       handle        // platform state: fds pinned to the target
}

// MaxResponse caps what one attach command may return. Class histograms and
// thread dumps of very large JVMs stay well below it; a hostile process
// pretending to be a JVM cannot make the agent buffer more.
const MaxResponse = 16 << 20

// Exec runs an attach command, e.g. Exec(ctx, "jcmd", "GC.class_histogram").
// It returns the command output. A non-zero JVM return code is an error that
// still carries the output text.
func (c *Client) Exec(ctx context.Context, cmd string, args ...string) (string, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	if err := c.ensureListener(ctx); err != nil {
		return "", err
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	// A JVM stuck in GC can sit on a request forever: the context, not just
	// a socket deadline, bounds the whole exchange.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	var req bytes.Buffer
	req.WriteString("1\x00")
	req.WriteString(cmd + "\x00")
	for i := 0; i < 3; i++ {
		if i < len(args) {
			req.WriteString(args[i])
		}
		req.WriteByte(0)
	}
	if _, err := conn.Write(req.Bytes()); err != nil {
		return "", fmt.Errorf("write attach request: %w", err)
	}
	out, err := io.ReadAll(io.LimitReader(conn, MaxResponse))
	if err != nil && len(out) == 0 {
		if ctx.Err() != nil {
			return "", fmt.Errorf("attach %s: %w", cmd, ctx.Err())
		}
		return "", fmt.Errorf("read attach response: %w", err)
	}
	rc, body, _ := strings.Cut(string(out), "\n")
	if code, perr := strconv.Atoi(strings.TrimSpace(rc)); perr == nil && code != 0 {
		return body, fmt.Errorf("jvm returned %d: %s", code, firstLine(body))
	}
	return body, nil
}

// Jcmd runs a diagnostic command line such as "GC.class_histogram" or
// "GC.heap_dump /tmp/x.hprof".
func (c *Client) Jcmd(ctx context.Context, line string) (string, error) {
	return c.Exec(ctx, "jcmd", line)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// HistogramEntry is one row of GC.class_histogram.
type HistogramEntry struct {
	Instances uint64
	Bytes     uint64
	Class     string
}

// ParseHistogram parses GC.class_histogram output:
//
//	 num     #instances         #bytes  class name (module)
//	-------------------------------------------------------
//	   1:          1234       98765432  [B (java.base@21)
func ParseHistogram(text string) (entries []HistogramEntry, totalBytes uint64) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "Total" {
			totalBytes, _ = strconv.ParseUint(f[2], 10, 64)
			continue
		}
		if len(f) < 4 || !strings.HasSuffix(f[0], ":") {
			continue
		}
		inst, err1 := strconv.ParseUint(f[1], 10, 64)
		by, err2 := strconv.ParseUint(f[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		entries = append(entries, HistogramEntry{Instances: inst, Bytes: by, Class: PrettyClass(f[3])})
	}
	return entries, totalBytes
}

// PrettyClass turns JVM internal names like "[B" into "byte[]".
func PrettyClass(c string) string {
	dims := 0
	for strings.HasPrefix(c, "[") {
		dims++
		c = c[1:]
	}
	if dims == 0 {
		return c
	}
	prim := map[string]string{"B": "byte", "C": "char", "D": "double", "F": "float", "I": "int", "J": "long", "S": "short", "Z": "boolean"}
	if p, ok := prim[c]; ok {
		c = p
	} else if strings.HasPrefix(c, "L") {
		c = strings.TrimSuffix(c[1:], ";")
	}
	return c + strings.Repeat("[]", dims)
}

// jdkPrefixes are classes that belong to the JDK or common runtimes rather
// than the application.
var jdkPrefixes = []string{"java.", "javax.", "jdk.", "sun.", "com.sun.", "byte[]", "char[]", "int[]", "long[]", "short[]", "boolean[]", "double[]", "float[]"}

// AppClasses returns the top n histogram entries that are not JDK classes.
// byte[] tells you little; the application class holding the byte[] is what
// names the leak.
func AppClasses(entries []HistogramEntry, n int) []HistogramEntry {
	var out []HistogramEntry
	for _, e := range entries {
		jdk := false
		for _, p := range jdkPrefixes {
			if strings.HasPrefix(e.Class, p) {
				jdk = true
				break
			}
		}
		if !jdk {
			out = append(out, e)
			if len(out) == n {
				break
			}
		}
	}
	return out
}
