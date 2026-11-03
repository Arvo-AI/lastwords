package cgroup

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// containerIDRe matches the 64-hex container ID that container runtimes put
// in cgroup directory names:
//
//	cri-containerd-<id>.scope   (containerd, systemd driver)
//	crio-<id>.scope             (CRI-O, systemd driver)
//	docker-<id>.scope           (dockershim / cri-dockerd)
//	<id>                        (cgroupfs driver)
var containerIDRe = regexp.MustCompile(`(?:^|[-_:])([0-9a-f]{64})(?:\.scope)?$`)

// ContainerIDFromPath extracts the container ID from a cgroup path or
// directory name, or returns "".
func ContainerIDFromPath(p string) string {
	base := filepath.Base(strings.TrimRight(p, "/"))
	// CRI-O runs each container's monitor in crio-conmon-<id>.scope, next to
	// the container's crio-<id>.scope. It has the same ID but no memory limit.
	if strings.Contains(base, "conmon") {
		return ""
	}
	m := containerIDRe.FindStringSubmatch(base)
	if m == nil {
		return ""
	}
	return m[1]
}

// ContainerIDFromProcCgroup extracts the container ID from the text of a
// /proc/<pid>/cgroup file (any line, v1 or v2).
func ContainerIDFromProcCgroup(text string) string {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if id := ContainerIDFromPath(parts[2]); id != "" {
			return id
		}
	}
	return ""
}

// Discover walks the host cgroup tree under root and returns container ID ->
// cgroup directory for every container cgroup found. On cgroup v1 it walks the
// memory controller only.
//
// Only directories under a "kubepods" subtree are considered, which skips the
// agent's own unrelated system cgroups and keeps the walk cheap.
func Discover(root string) (map[string]string, error) {
	base := root
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		// cgroup v1: use the memory hierarchy.
		base = filepath.Join(root, "memory")
	}
	out := make(map[string]string)
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Containers come and go while we walk; skip vanished dirs.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		if rel != "." && !strings.Contains(rel, "kubepods") {
			// Allow descending into the first level so we can find kubepods.slice / kubepods.
			if strings.Count(rel, string(filepath.Separator)) >= 1 {
				return fs.SkipDir
			}
			return nil
		}
		if id := ContainerIDFromPath(d.Name()); id != "" {
			out[id] = path
			return fs.SkipDir
		}
		return nil
	})
	return out, err
}
