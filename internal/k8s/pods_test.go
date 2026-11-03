package k8s

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func crashLoopPod(restarts int) *corev1.Pod {
	id := func(n int) string { return fmt.Sprintf("containerd://%064d", n) }
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod", UID: "uid-1"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", ContainerID: id(restarts), RestartCount: int32(restarts),
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	if restarts > 0 {
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
			ContainerID: id(restarts - 1), ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.Now(),
		}
	}
	return p
}

func TestCrashLoopReportsEachDeathOnceAndDoesNotGrow(t *testing.T) {
	w := NewWatcher(fake.NewClientset(), "node-1")
	var deaths []string
	w.OnDeath = func(ev TerminationEvent) { deaths = append(deaths, ev.ContainerID) }

	prev := crashLoopPod(0)
	w.upsert(nil, prev)
	for r := 1; r <= 50; r++ {
		cur := crashLoopPod(r)
		w.upsert(prev, cur)
		w.upsert(cur, cur) // resync: nothing new
		prev = cur
	}
	if len(deaths) != 50 {
		t.Fatalf("got %d deaths, want 50", len(deaths))
	}
	if len(w.byID) > 2 || len(w.seen) > 1 {
		t.Fatalf("index grew with restarts: %d ids, %d seen", len(w.byID), len(w.seen))
	}
	// The instance that just died is still resolvable (cores arrive late).
	if _, ok := w.Lookup(fmt.Sprintf("%064d", 49)); !ok {
		t.Fatal("last dead container should still resolve to its pod")
	}
	w.remove(prev)
	if len(w.byID) != 0 || len(w.seen) != 0 {
		t.Fatalf("pod delete left %d ids, %d seen", len(w.byID), len(w.seen))
	}
}

func TestStartupSkipsOldDeaths(t *testing.T) {
	w := NewWatcher(fake.NewClientset(), "node-1")
	n := 0
	w.OnDeath = func(TerminationEvent) { n++ }
	p := crashLoopPod(3)
	p.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt = metav1.NewTime(time.Now().Add(-time.Hour))
	w.upsert(nil, p)
	if n != 0 {
		t.Fatal("a death from before the agent started should not be captured")
	}
}

// A rollout restart SIGTERMs the old pod; its exit 143 is a shutdown, not a
// crash. An OOM kill while it shuts down is still a real death.
func TestDeletingPodShutdownIsNotACrash(t *testing.T) {
	for _, tc := range []struct {
		reason string
		code   int32
		want   int
	}{{"Error", 143, 0}, {"OOMKilled", 137, 1}} {
		w := NewWatcher(fake.NewClientset(), "node-1")
		deaths := 0
		w.OnDeath = func(TerminationEvent) { deaths++ }
		running := crashLoopPod(0)
		w.upsert(nil, running)
		stopping := crashLoopPod(0)
		now := metav1.Now()
		stopping.DeletionTimestamp = &now
		stopping.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ContainerID: running.Status.ContainerStatuses[0].ContainerID, ExitCode: tc.code, Reason: tc.reason, FinishedAt: now,
		}}
		w.upsert(running, stopping)
		if deaths != tc.want {
			t.Errorf("%s during deletion: %d deaths reported, want %d", tc.reason, deaths, tc.want)
		}
	}
}

// The kubelet SIGKILLs the containers of a pod it evicts. That is reported
// once, as the eviction, not again as a crash.
func TestEvictionIsNotAlsoACrash(t *testing.T) {
	w := NewWatcher(fake.NewClientset(), "node-1")
	deaths := 0
	w.OnDeath = func(TerminationEvent) { deaths++ }
	running := crashLoopPod(0)
	w.upsert(nil, running)
	evicted := crashLoopPod(0)
	evicted.Status.Phase, evicted.Status.Reason = corev1.PodFailed, "Evicted"
	evicted.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ContainerID: running.Status.ContainerStatuses[0].ContainerID, ExitCode: 137, Reason: "Error", FinishedAt: metav1.Now(),
	}}
	w.upsert(running, evicted)
	if deaths != 0 {
		t.Fatalf("eviction kill reported as %d crash(es)", deaths)
	}
}

func TestInitContainerFailureIsMarked(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	for _, tc := range []struct {
		name    string
		policy  *corev1.ContainerRestartPolicy
		blocked bool
	}{{"migrate", nil, true}, {"mesh-proxy", &always, false}} {
		w := NewWatcher(fake.NewClientset(), "node-1")
		var got []TerminationEvent
		w.OnDeath = func(ev TerminationEvent) { got = append(got, ev) }
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "prod", UID: "uid-2"},
			Spec:       corev1.PodSpec{InitContainers: []corev1.Container{{Name: tc.name, RestartPolicy: tc.policy}}},
			Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
				Name: tc.name, RestartCount: 1,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ContainerID: "containerd://" + fmt.Sprintf("%064d", 7), ExitCode: 3, Reason: "Error", FinishedAt: metav1.Now(),
				}},
			}}},
		}
		w.upsert(nil, p)
		if len(got) != 1 || got[0].Init != tc.blocked {
			t.Fatalf("%s: got %+v, want one death with Init=%v", tc.name, got, tc.blocked)
		}
	}
}
