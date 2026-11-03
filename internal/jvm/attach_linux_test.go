package jvm

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeTarget lays out ProcRoot/<pid>/{maps,status,stat,root/tmp} for this test
// process, so New treats it as a JVM whose container root is a temp dir.
func fakeTarget(t *testing.T) (pid int, tmp string, outside string) {
	t.Helper()
	pid = os.Getpid()
	proc := t.TempDir()
	dir := filepath.Join(proc, strconv.Itoa(pid))
	root := filepath.Join(t.TempDir(), "rootfs")
	tmp = filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"maps":   "7f0000000000-7f0000001000 r-xp 00000000 08:01 1 /usr/lib/jvm/lib/server/libjvm.so\n",
		"status": "Name:\tjava\nNSpid:\t" + strconv.Itoa(pid) + "\t7\nUid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\n",
		"stat":   string(stat),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(root, filepath.Join(dir, "root")); err != nil {
		t.Fatal(err)
	}
	old := ProcRoot
	ProcRoot = proc
	t.Cleanup(func() { ProcRoot = old })
	return pid, tmp, t.TempDir() // outside stands in for the host filesystem
}

func TestTriggerSymlinkCannotReachHostFiles(t *testing.T) {
	pid, tmp, outside := fakeTarget(t)
	victim := filepath.Join(outside, "shadow")
	// Planted by the container: its trigger file points at a host path.
	if err := os.Symlink(victim, filepath.Join(tmp, ".attach_pid7")); err != nil {
		t.Fatal(err)
	}
	c, err := New(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.ensureListener(ctx); err == nil {
		t.Fatal("ensureListener followed a planted symlink")
	}
	if _, err := os.Lstat(victim); !os.IsNotExist(err) {
		t.Fatal("a file was created through the symlink outside the container")
	}
}

func TestHeapDumpSymlinkIsRefused(t *testing.T) {
	pid, tmp, outside := fakeTarget(t)
	secret := filepath.Join(outside, "kubelet.key")
	if err := os.WriteFile(secret, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(tmp, "dump.hprof")); err != nil {
		t.Fatal(err)
	}
	c, err := New(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if f, err := c.OpenTmpFile("dump.hprof"); err == nil {
		f.Close()
		t.Fatal("opened a host file through a planted symlink")
	}
	if err := os.WriteFile(filepath.Join(tmp, "real.hprof"), []byte("HPROF"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := c.OpenTmpFile("real.hprof")
	if err != nil {
		t.Fatalf("regular dump file: %v", err)
	}
	f.Close()
}

func TestDialChecksTheListenerIsTheJVM(t *testing.T) {
	pid, tmp, _ := fakeTarget(t)
	ln, err := net.Listen("unix", filepath.Join(tmp, ".java_pid7"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 256)
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte("0\nok from jvm\n"))
			conn.Close()
		}
	}()
	c, err := New(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := c.Jcmd(context.Background(), "VM.version")
	if err != nil || out != "ok from jvm\n" {
		t.Fatalf("listener served by the target: out=%q err=%v", out, err)
	}
	// Same socket, but we expect another process: the request must not be sent.
	c.Pid = 1
	if _, err := c.dial(context.Background()); err == nil {
		t.Fatal("dial accepted a socket served by another process")
	}
}

func TestExecHonoursContext(t *testing.T) {
	pid, tmp, _ := fakeTarget(t)
	ln, err := net.Listen("unix", filepath.Join(tmp, ".java_pid7"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // a JVM stuck in GC: accepts and never answers
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	c, err := New(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Jcmd(ctx, "GC.class_histogram"); err == nil {
		t.Fatal("expected a timeout")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Exec ignored the context for %s", d)
	}
}
