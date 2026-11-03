package jvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// handle pins the target so later operations cannot be redirected:
//
//   - tmp is an O_PATH fd on the target's /tmp, opened once through the
//     /proc/<pid>/root magic link. Files are then created, inspected and
//     removed relative to it with O_NOFOLLOW / AT_SYMLINK_NOFOLLOW, so a
//     symlink planted in the container cannot point the root agent at a host
//     file (an absolute symlink under /proc/<pid>/root resolves against the
//     agent's root, which with hostPID reaches the host).
//   - pidfd signals exactly the process we checked, even if it exits and its
//     PID is reused. Kernels without pidfd fall back to re-checking the
//     process start time before signalling.
type handle struct {
	tmp   int
	pidfd int
	start string
}

// New prepares an attach client for host pid. It returns an error if the
// process is not a HotSpot JVM. Call Close when done.
func New(pid int) (*Client, error) {
	if !IsJVM(pid) {
		return nil, fmt.Errorf("pid %d is not a JVM (no libjvm.so mapped)", pid)
	}
	nspid, uid, gid, err := NSPid(pid)
	if err != nil {
		return nil, err
	}
	start, err := startTime(pid)
	if err != nil {
		return nil, err
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		pidfd = -1
	}
	root, err := unix.Open(filepath.Join(ProcRoot, strconv.Itoa(pid), "root"), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		closeFD(pidfd)
		return nil, fmt.Errorf("open target root: %w", err)
	}
	tmp, err := unix.Openat(root, "tmp", unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	_ = unix.Close(root)
	if err != nil {
		closeFD(pidfd)
		return nil, fmt.Errorf("open target /tmp (a symlinked /tmp is not supported): %w", err)
	}
	// The process may have exited and the PID been reused between the checks
	// above and pinning it.
	if s, err := startTime(pid); err != nil || s != start {
		closeFD(pidfd)
		_ = unix.Close(tmp)
		return nil, errors.New("process exited during attach")
	}
	return &Client{Pid: pid, NSPid: nspid, UID: uid, GID: gid, Timeout: 30 * time.Second,
		h: handle{tmp: tmp, pidfd: pidfd, start: start}}, nil
}

// Close releases the pinned descriptors.
func (c *Client) Close() error {
	closeFD(c.h.pidfd)
	closeFD(c.h.tmp)
	c.h.pidfd, c.h.tmp = -1, -1
	return nil
}

func closeFD(fd int) {
	if fd >= 0 {
		_ = unix.Close(fd)
	}
}

func (c *Client) socketName() string  { return ".java_pid" + strconv.Itoa(c.NSPid) }
func (c *Client) triggerName() string { return ".attach_pid" + strconv.Itoa(c.NSPid) }

// isSocket checks the name in the target's /tmp without following symlinks.
func (c *Client) isSocket(name string) bool {
	var st unix.Stat_t
	err := unix.Fstatat(c.h.tmp, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	return err == nil && st.Mode&unix.S_IFMT == unix.S_IFSOCK
}

// ensureListener starts the JVM attach listener if its socket is not there.
func (c *Client) ensureListener(ctx context.Context) error {
	if c.isSocket(c.socketName()) {
		return nil
	}
	fd, err := unix.Openat(c.h.tmp, c.triggerName(), unix.O_CREAT|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o660)
	if err != nil {
		return fmt.Errorf("create attach trigger: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return errors.New("attach trigger is not a regular file")
	}
	// The JVM only honours a trigger file owned by its own euid (or root).
	_ = unix.Fchown(fd, c.UID, c.GID)
	_ = unix.Close(fd)
	defer func() { _ = unix.Unlinkat(c.h.tmp, c.triggerName(), 0) }()

	if err := c.signal(unix.SIGQUIT); err != nil {
		return fmt.Errorf("signal jvm: %w", err)
	}
	for {
		if c.isSocket(c.socketName()) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("jvm attach listener did not start (is -XX:+DisableAttachMechanism set?): %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// signal sends sig to the pinned process only.
func (c *Client) signal(sig unix.Signal) error {
	if c.h.pidfd >= 0 {
		return unix.PidfdSendSignal(c.h.pidfd, sig, nil, 0)
	}
	if s, err := startTime(c.Pid); err != nil || s != c.h.start {
		return errors.New("process is gone")
	}
	return unix.Kill(c.Pid, sig)
}

// dial connects to the attach socket and checks that the listener really is
// the target process, so a swapped-in symlink cannot route the request to
// another socket on the host.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	if !c.isSocket(c.socketName()) {
		return nil, errors.New("attach socket is missing or not a socket")
	}
	// Through the pinned directory fd, not /proc/<pid>/root/tmp: the path is
	// short (sun_path is 108 bytes) and cannot be re-pointed above /tmp.
	path := "/proc/self/fd/" + strconv.Itoa(c.h.tmp) + "/" + c.socketName()
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("connect attach socket: %w", err)
	}
	pid, err := peerPid(conn)
	if err != nil || pid != c.Pid {
		conn.Close()
		return nil, fmt.Errorf("attach socket is served by pid %d, not the JVM (%d)", pid, c.Pid)
	}
	return conn, nil
}

func peerPid(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return int(cred.Pid), nil
}

// OpenTmpFile opens a regular file in the target's /tmp for reading, without
// following symlinks. Used to collect heap dumps the JVM wrote there.
func (c *Client) OpenTmpFile(name string) (*os.File, error) {
	if strings.ContainsAny(name, "/\x00") {
		return nil, errors.New("bad file name")
	}
	fd, err := unix.Openat(c.h.tmp, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, errors.New(name + " is not a regular file")
	}
	return os.NewFile(uintptr(fd), name), nil
}

// RemoveTmpFile deletes a file from the target's /tmp (never following a symlink).
func (c *Client) RemoveTmpFile(name string) {
	if !strings.ContainsAny(name, "/\x00") {
		_ = unix.Unlinkat(c.h.tmp, name, 0)
	}
}

// startTime is field 22 of /proc/<pid>/stat: the process start time in clock
// ticks since boot. Together with the PID it identifies one process.
func startTime(pid int) (string, error) {
	f, err := os.Open(filepath.Join(ProcRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", err
	}
	// comm (field 2) can contain spaces and parentheses; fields resume after the last ')'.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return "", errors.New("bad stat")
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 20 {
		return "", errors.New("bad stat")
	}
	return fields[19], nil
}
