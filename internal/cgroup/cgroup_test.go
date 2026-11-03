package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadV2WorkingSetExcludesInactiveFile(t *testing.T) {
	d := t.TempDir()
	write(t, d, "memory.current", "1000\n")
	write(t, d, "memory.max", "2000\n")
	write(t, d, "memory.stat", "anon 700\nfile 300\ninactive_file 200\n")
	write(t, d, "memory.events", "low 0\nhigh 0\nmax 3\noom 1\noom_kill 1\n")
	write(t, d, "memory.pressure", "some avg10=1.50 avg60=0.20 avg300=0.00 total=10\nfull avg10=0.75 avg60=0.10 avg300=0.00 total=5\n")
	s, err := Read(d)
	if err != nil {
		t.Fatal(err)
	}
	if s.WorkingSet() != 800 {
		t.Fatalf("working set = %d, want 800", s.WorkingSet())
	}
	if s.Ratio() != 0.4 {
		t.Fatalf("ratio = %v", s.Ratio())
	}
	if s.OOMKills() != 1 || s.PSI == nil || s.PSI.FullAvg10 != 0.75 {
		t.Fatalf("events/psi wrong: %+v %+v", s.Events, s.PSI)
	}
}

func TestReadV2Unlimited(t *testing.T) {
	d := t.TempDir()
	write(t, d, "memory.current", "10\n")
	write(t, d, "memory.max", "max\n")
	s, err := Read(d)
	if err != nil {
		t.Fatal(err)
	}
	if s.Limit != 0 || s.Ratio() != 0 {
		t.Fatalf("unlimited should give limit 0, got %d", s.Limit)
	}
}

func TestReadV1(t *testing.T) {
	d := t.TempDir()
	write(t, d, "memory.usage_in_bytes", "500\n")
	write(t, d, "memory.limit_in_bytes", "9223372036854771712\n")
	write(t, d, "memory.stat", "total_inactive_file 100\n")
	write(t, d, "memory.oom_control", "oom_kill_disable 0\nunder_oom 0\noom_kill 2\n")
	s, err := Read(d)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != V1 || s.Limit != 0 || s.WorkingSet() != 400 || s.OOMKills() != 2 {
		t.Fatalf("v1 parse wrong: %+v ws=%d", s, s.WorkingSet())
	}
}

func TestContainerIDFromPath(t *testing.T) {
	id := "3f5a0d9be0b6b0f2a0f6d0d6b5c9d3c1e2f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8"
	cases := map[string]string{
		"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podx.slice/cri-containerd-" + id + ".scope": id,
		"/kubepods/burstable/pod123/" + id:      id,
		"/system.slice/crio-" + id + ".scope":   id,
		"/kubepods.slice/kubepods-podabc.slice": "",
		"/user.slice":                           "",
	}
	for in, want := range cases {
		if got := ContainerIDFromPath(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
	if got := ContainerIDFromProcCgroup("0::/kubepods.slice/x/cri-containerd-" + id + ".scope\n"); got != id {
		t.Errorf("proc cgroup parse: %q", got)
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	write(t, root, "cgroup.controllers", "memory\n")
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cdir := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod1.slice", "cri-containerd-"+id+".scope")
	write(t, cdir, "memory.current", "1\n")
	write(t, filepath.Join(root, "system.slice", "docker-"+id+".scope"), "memory.current", "1\n")
	m, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || m[id] != cdir {
		t.Fatalf("discover = %v", m)
	}
}

func TestDiscoverCRIOPicksContainerNotConmon(t *testing.T) {
	root := t.TempDir()
	write(t, root, "cgroup.controllers", "memory\n")
	id := "0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // sorts before "conmon"
	pod := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod1.slice")
	cdir := filepath.Join(pod, "crio-"+id+".scope")
	write(t, cdir, "memory.current", "1\n")
	write(t, filepath.Join(pod, "crio-conmon-"+id+".scope"), "memory.current", "1\n")
	m, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if m[id] != cdir {
		t.Fatalf("got %q, want the container cgroup %q", m[id], cdir)
	}
}
