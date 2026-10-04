/*
Copyright 2026 The cluster-api-provider-terraform Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package health

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// fakeClients returns wait.Clients over a fake clientset holding objs.
func fakeClients(objs ...runtime.Object) wait.Clients {
	return wait.Clients{Kube: fake.NewSimpleClientset(objs...)}
}

// mustContain fails t unless err is non-nil and its text holds every one
// of parts.
func mustContain(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error containing %q, got nil", parts)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q lacks %q", err, p)
		}
	}
}

// node builds and returns a Node named name with the condition types in
// conds set to the given statuses.
func node(name string, conds map[corev1.NodeConditionType]corev1.ConditionStatus) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for t, s := range conds {
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: t, Status: s})
	}
	return n
}

// healthyConds returns the condition set of a healthy node.
func healthyConds() map[corev1.NodeConditionType]corev1.ConditionStatus {
	return map[corev1.NodeConditionType]corev1.ConditionStatus{
		corev1.NodeReady:              corev1.ConditionTrue,
		corev1.NodeMemoryPressure:     corev1.ConditionFalse,
		corev1.NodeDiskPressure:       corev1.ConditionFalse,
		corev1.NodePIDPressure:        corev1.ConditionFalse,
		corev1.NodeNetworkUnavailable: corev1.ConditionFalse,
	}
}

// TestNodes table-tests the node check through a fake clientset.
func TestNodes(t *testing.T) {
	t.Parallel()
	bad := healthyConds()
	bad[corev1.NodeReady] = corev1.ConditionFalse
	bad[corev1.NodeDiskPressure] = corev1.ConditionTrue
	bad[corev1.NodeNetworkUnavailable] = corev1.ConditionTrue
	noReady := healthyConds()
	delete(noReady, corev1.NodeReady)
	pid := healthyConds()
	pid[corev1.NodePIDPressure] = corev1.ConditionUnknown

	ctx := context.Background()
	if err := Nodes(ctx, fakeClients(node("a", healthyConds()), node("b", healthyConds())), 2); err != nil {
		t.Fatalf("healthy: %v", err)
	}
	mustContain(t, Nodes(ctx, fakeClients(node("a", healthyConds())), 2), "want 2 nodes, found 1")
	mustContain(t, Nodes(ctx, fakeClients(node("a", bad), node("b", noReady), node("c", pid)), 3),
		"node a: Ready=False", "node a: DiskPressure=True", "node a: NetworkUnavailable=True",
		"node b: Ready=missing", "node c: PIDPressure=Unknown")

	failing := fake.NewSimpleClientset()
	failing.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	mustContain(t, Nodes(ctx, wait.Clients{Kube: failing}, 1), "list nodes", "boom")
}

// TestAPIServerNoRESTClient checks that the fake clientset, which has no
// REST client, yields a clear error rather than a panic.
func TestAPIServerNoRESTClient(t *testing.T) {
	t.Parallel()
	mustContain(t, APIServer(context.Background(), fakeClients()), "no REST client")
}

// pod builds and returns a pod called name in phase, whose Ready condition
// is ready and whose container statuses are cs.
func pod(name string, phase corev1.PodPhase, ready bool, cs ...corev1.ContainerStatus) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, UID: "uid-" + "x"}}
	p.Status.Phase = phase
	p.Status.ContainerStatuses = cs
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}
	return p
}

// okContainer returns a ready, never-restarted status for container name.
func okContainer(name string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
}

// TestEvalPod table-tests the pure pod evaluator.
func TestEvalPod(t *testing.T) {
	t.Parallel()
	crash := corev1.ContainerStatus{Name: "c", RestartCount: 3, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}
	oom := okContainer("c")
	oom.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}
	oom.RestartCount = 1
	exit := okContainer("c")
	exit.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 2}
	restarted := okContainer("c")
	restarted.RestartCount = 2
	unready := okContainer("c")
	unready.Ready = false
	initBad := corev1.ContainerStatus{Name: "i", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull", Message: "denied"}}}
	initDone := corev1.ContainerStatus{Name: "i", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}

	pending := pod("p", corev1.PodPending, false)
	pending.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/1 nodes available"}}
	failed := pod("p", corev1.PodFailed, false)
	failed.Status.Reason = "Evicted"
	withInit := pod("p", corev1.PodRunning, true, okContainer("c"))
	withInit.Status.InitContainerStatuses = []corev1.ContainerStatus{initDone}
	badInit := pod("p", corev1.PodPending, false)
	badInit.Status.InitContainerStatuses = []corev1.ContainerStatus{initBad}

	cases := []struct {
		name string
		pod  *corev1.Pod
		want []string
	}{
		{"healthy", pod("p", corev1.PodRunning, true, okContainer("c")), nil},
		{"succeeded", pod("p", corev1.PodSucceeded, false, unready), nil},
		{"completed init", withInit, nil},
		{"crashloop", pod("p", corev1.PodRunning, false, crash), []string{"pod ns/p: container c CrashLoopBackOff (restarts 3)"}},
		{"oom", pod("p", corev1.PodRunning, true, oom), []string{"container c OOMKilled (restarts 1)"}},
		{"exit code", pod("p", corev1.PodRunning, true, exit), []string{"container c Terminated", "last exit code 2"}},
		{"restart only", pod("p", corev1.PodRunning, true, restarted), []string{"container c Restarted (restarts 2)"}},
		{"unready", pod("p", corev1.PodRunning, false, unready), []string{"container c NotReady"}},
		{"pending", pending, []string{"pod ns/p: Pending", "PodScheduled: 0/1 nodes available"}},
		{"failed", failed, []string{"pod ns/p: Evicted"}},
		{"bad init", badInit, []string{"Pending", "container i ErrImagePull: denied"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, p := range evalPod(tc.pod) {
				got = append(got, p.String())
			}
			joined := strings.Join(got, "\n")
			if len(tc.want) == 0 && len(got) != 0 {
				t.Fatalf("want healthy, got %v", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("problems %q lack %q", joined, w)
				}
			}
		})
	}
}

// TestPods checks listing across namespaces, sorting and ProblemsError.
func TestPods(t *testing.T) {
	t.Parallel()
	good := pod("good", corev1.PodRunning, true, okContainer("c"))
	badA := pod("b", corev1.PodPending, false)
	badB := pod("a", corev1.PodPending, false)
	other := pod("o", corev1.PodPending, false)
	other.Namespace = "other"
	c := fakeClients(good, badA, badB, other)
	ctx := context.Background()

	got, err := Pods(ctx, c, []string{"ns"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Pod != "a" || got[1].Pod != "b" {
		t.Fatalf("want sorted problems a,b in ns; got %v", got)
	}
	all, _ := Pods(ctx, c, nil)
	if len(all) != 3 {
		t.Fatalf("empty namespaces means all; got %v", all)
	}
	mustContain(t, ProblemsError("pods", all), "health: pods:", "pod other/o", "pod ns/a")
	if ProblemsError("pods", nil) != nil {
		t.Fatal("no problems must be nil")
	}

	failing := fake.NewSimpleClientset()
	failing.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	if _, err := Pods(ctx, wait.Clients{Kube: failing}, nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("list error not surfaced: %v", err)
	}
}

// TestWorkloads covers the three evaluators and the list wrapper.
func TestWorkloads(t *testing.T) {
	t.Parallel()
	one := int32(1)
	three := int32(3)
	okDep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ok", Generation: 1}, Spec: appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}}}
	okDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ok"}, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 2, UpdatedNumberScheduled: 2}}
	okSTS := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ok"}, Spec: appsv1.StatefulSetSpec{Replicas: &one}, Status: appsv1.StatefulSetStatus{ReadyReplicas: 1}}
	ctx := context.Background()
	if err := Workloads(ctx, fakeClients(okDep, okDS, okSTS), nil); err != nil {
		t.Fatalf("healthy: %v", err)
	}

	badDep := okDep.DeepCopy()
	badDep.Name = "baddep"
	badDep.Status.Conditions = nil
	badDS := okDS.DeepCopy()
	badDS.Name = "badds"
	badDS.Status.NumberReady = 1
	badSTS := okSTS.DeepCopy()
	badSTS.Name = "badsts"
	badSTS.Spec.Replicas = &three
	mustContain(t, Workloads(ctx, fakeClients(okDep, badDep, badDS, badSTS), []string{"ns"}),
		"deployment ns/baddep: not Available", "daemonset ns/badds: 1/2 pods ready", "statefulset ns/badsts: 1/3 replicas ready")

	// Evaluator branches.
	dep := func(mut func(*appsv1.Deployment)) *appsv1.Deployment {
		d := okDep.DeepCopy()
		mut(d)
		return d
	}
	for name, tc := range map[string]struct {
		d    *appsv1.Deployment
		want string
	}{
		"generation": {dep(func(d *appsv1.Deployment) { d.Generation = 2 }), "generation not yet observed"},
		"updated":    {dep(func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 0 }), "0/1 replicas updated"},
		"old":        {dep(func(d *appsv1.Deployment) { d.Status.Replicas = 2 }), "1 old replicas"},
		"available":  {dep(func(d *appsv1.Deployment) { d.Status.AvailableReplicas = 0 }), "0/1 replicas available"},
	} {
		if ok, why := deploymentReady(tc.d); ok || !strings.Contains(why, tc.want) {
			t.Errorf("%s: got ok=%v why=%q want %q", name, ok, why, tc.want)
		}
	}
	nilRepl := dep(func(d *appsv1.Deployment) { d.Spec.Replicas = nil })
	if ok, why := deploymentReady(nilRepl); !ok {
		t.Errorf("nil replicas defaults to 1: %s", why)
	}
	ds := okDS.DeepCopy()
	ds.Generation = 3
	if ok, _ := daemonSetReady(ds); ok {
		t.Error("daemonset generation unobserved must fail")
	}
	ds = okDS.DeepCopy()
	ds.Status.UpdatedNumberScheduled = 1
	if ok, why := daemonSetReady(ds); ok || !strings.Contains(why, "1/2 pods updated") {
		t.Errorf("daemonset updated: %v %q", ok, why)
	}
	st := okSTS.DeepCopy()
	st.Generation = 2
	if ok, _ := statefulSetReady(st); ok {
		t.Error("statefulset generation unobserved must fail")
	}
	st = okSTS.DeepCopy()
	st.Spec.Replicas = nil
	if ok, why := statefulSetReady(st); !ok {
		t.Errorf("nil replicas defaults to 1: %s", why)
	}

	for _, res := range []string{"deployments", "daemonsets", "statefulsets"} {
		failing := fake.NewSimpleClientset()
		failing.PrependReactor("list", res, func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
		mustContain(t, Workloads(ctx, wait.Clients{Kube: failing}, nil), "list "+res, "boom")
	}
}

// TestEvalEvents table-tests the pure event evaluator.
func TestEvalEvents(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	after := metav1.NewTime(since.Add(time.Minute))
	before := metav1.NewTime(since.Add(-time.Minute))
	ev := func(typ, reason, msg string, mut func(*corev1.Event)) corev1.Event {
		e := corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ev-" + reason}, Type: typ, Reason: reason, Message: msg, LastTimestamp: before}
		e.InvolvedObject.Kind = "Pod"
		e.InvolvedObject.Name = "p"
		if mut != nil {
			mut(&e)
		}
		return e
	}
	events := []corev1.Event{
		ev("Warning", "Old", "old", nil),
		ev("Normal", "Fine", "fine", func(e *corev1.Event) { e.LastTimestamp = after }),
		ev("Warning", "ByLast", "m1", func(e *corev1.Event) { e.LastTimestamp = after }),
		ev("Warning", "ByEventTime", "m2", func(e *corev1.Event) { e.EventTime = metav1.NewMicroTime(after.Time) }),
		ev("Warning", "BySeries", "m3", func(e *corev1.Event) {
			e.Series = &corev1.EventSeries{LastObservedTime: metav1.NewMicroTime(after.Time)}
		}),
		ev("Warning", "Allowed", "known noise", func(e *corev1.Event) { e.LastTimestamp = after }),
	}
	allow := []*regexp.Regexp{regexp.MustCompile(`^Allowed: known`)}
	got := evalEvents(events, since, allow)
	if len(got) != 3 {
		t.Fatalf("want 3 findings, got %v", got)
	}
	for _, r := range []string{"ByLast", "ByEventTime", "BySeries"} {
		if !strings.Contains(strings.Join(got, "\n"), r) {
			t.Errorf("missing %s in %v", r, got)
		}
	}

	ctx := context.Background()
	c := fakeClients(&events[2], &events[5])
	mustContain(t, WarningEvents(ctx, c, []string{"ns"}, since, allow), "ByLast")
	if err := WarningEvents(ctx, fakeClients(&events[5]), nil, since, allow); err != nil {
		t.Fatalf("allowlisted: %v", err)
	}
	failing := fake.NewSimpleClientset()
	failing.PrependReactor("list", "events", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	mustContain(t, WarningEvents(ctx, wait.Clients{Kube: failing}, nil, since, nil), "boom")
}

// lease builds and returns a Lease held by holder, renewed at renew (nil
// for never).
func lease(holder string, renew *time.Time) *coordinationv1.Lease {
	l := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "l"}}
	if holder != "" {
		l.Spec.HolderIdentity = &holder
	}
	if renew != nil {
		mt := metav1.NewMicroTime(*renew)
		l.Spec.RenewTime = &mt
	}
	return l
}

// TestLeaseHeld table-tests LeaseHeld with a fixed clock.
func TestLeaseHeld(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	fresh := at.Add(-2 * time.Second)
	stale := at.Add(-time.Hour)
	if p := evalLeaseHeld(lease("mgr-abc_123", &fresh), "mgr-abc", 15*time.Second, at); p != nil {
		t.Fatalf("held: %v", p)
	}
	for name, tc := range map[string]struct {
		l    *coordinationv1.Lease
		want string
	}{
		"other holder": {lease("other_1", &fresh), "does not start with"},
		"no holder":    {lease("", &fresh), `holderIdentity ""`},
		"stale":        {lease("mgr-abc_1", &stale), "old, max"},
		"no renew":     {lease("mgr-abc_1", nil), "no renewTime"},
	} {
		p := evalLeaseHeld(tc.l, "mgr-abc", 15*time.Second, at)
		if len(p) == 0 || !strings.Contains(strings.Join(p, ";"), tc.want) {
			t.Errorf("%s: got %v want %q", name, p, tc.want)
		}
	}

	c := fakeClients(lease("mgr-abc_1", &fresh))
	ctx := context.Background()
	old := now
	// The clock var is shared; this test is the only one that changes it.
	now = func() time.Time { return at }
	defer func() { now = old }()
	if err := LeaseHeld(ctx, c, "ns", "l", "mgr", time.Minute); err != nil {
		t.Fatal(err)
	}
	mustContain(t, LeaseHeld(ctx, c, "ns", "l", "zzz", time.Minute), "not held")
	mustContain(t, LeaseHeld(ctx, c, "ns", "missing", "mgr", time.Minute), "get lease")
}

// TestLeaseRenewing checks that an advancing renewTime passes and a frozen
// one fails within the window.
func TestLeaseRenewing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	kube := fake.NewSimpleClientset(lease("h", &t0))
	c := wait.Clients{Kube: kube}

	mustContain(t, LeaseRenewing(ctx, c, "ns", "l", 100*time.Millisecond), "not renewing")

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(30 * time.Millisecond)
		_, _ = kube.CoordinationV1().Leases("ns").Update(ctx, lease("h", ptrTime(t0.Add(time.Second))), metav1.UpdateOptions{})
	}()
	if err := LeaseRenewing(ctx, c, "ns", "l", 5*time.Second); err != nil {
		t.Fatalf("renewing lease: %v", err)
	}
	<-done

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	mustContain(t, LeaseRenewing(cctx, c, "ns", "l", time.Minute), "context canceled")
	mustContain(t, LeaseRenewing(ctx, c, "ns", "missing", time.Second), "get lease")

	noRenew := wait.Clients{Kube: fake.NewSimpleClientset(lease("h", nil))}
	mustContain(t, LeaseRenewing(ctx, noRenew, "ns", "l", 50*time.Millisecond), "not renewing")
}

// ptrTime returns a pointer to t.
func ptrTime(t time.Time) *time.Time { return &t }

// TestLogHelpers covers the line matcher, the tail cap and the scanner
// over a fake opener.
func TestLogHelpers(t *testing.T) {
	t.Parallel()
	r := Rules{Fatal: DefaultFatal(), Allow: []*regexp.Regexp{regexp.MustCompile(`benign`)}}
	long := "panic: " + strings.Repeat("x", 500)
	got := matchLines([]string{
		"I0102 15:04:05 info", "E0102 15:04:05 real error", "E0102 15:04:05 benign thing",
		"goroutine panic: boom", "fatal error: out of memory", "F0102 15:04:05 fatal", long,
	}, r)
	if len(got) != 5 || !strings.HasSuffix(got[4], "...") || len(got[4]) > maxLineLen+3 {
		t.Fatalf("matchLines: %q", got)
	}

	// Tail: 10 lines of 11 bytes, limit 35 keeps the last 3 whole lines
	// (the partial first one is dropped).
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("line-0000" + string(rune('0'+i)) + "\n")
	}
	lines, err := tailLines(strings.NewReader(sb.String()), 35)
	if err != nil || len(lines) != 3 || lines[2] != "line-00009" {
		t.Fatalf("tailLines: %q %v", lines, err)
	}
	all, _ := tailLines(strings.NewReader("a\nb\n"), 1000)
	if len(all) != 2 {
		t.Fatalf("untruncated: %q", all)
	}
	if _, err := tailLines(errReader{}, 10); err == nil {
		t.Fatal("read error not surfaced")
	}

	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"},
		Spec:       corev1.PodSpec{InitContainers: []corev1.Container{{Name: "init"}}, Containers: []corev1.Container{{Name: "a"}, {Name: "b"}}},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "init"}},
			ContainerStatuses:     []corev1.ContainerStatus{{Name: "a", RestartCount: 1}, {Name: "b"}},
		},
	}}
	open := func(_ context.Context, _, _, container string, previous bool) (io.ReadCloser, error) {
		switch {
		case container == "b":
			return nil, errors.New("no logs")
		case container == "a" && previous:
			return io.NopCloser(strings.NewReader("ok\npanic: old crash\n")), nil
		case container == "a":
			return io.NopCloser(strings.NewReader("E0102 15:04:05 benign\nE0102 15:04:05 live error\n")), nil
		}
		return io.NopCloser(strings.NewReader("quiet\n")), nil
	}
	err = scanLogs(context.Background(), pods, "app=x", open, r)
	mustContain(t, err, "container a (previous): panic: old crash", "container a: E0102 15:04:05 live error", "container b: cannot read logs: no logs")
	if strings.Contains(err.Error(), "benign") || strings.Contains(err.Error(), "init") {
		t.Errorf("allowlisted or quiet lines reported: %v", err)
	}
	mustContain(t, scanLogs(context.Background(), nil, "app=x", open, r), `no pods match selector "app=x"`)
	clean := func(context.Context, string, string, string, bool) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("fine\n")), nil
	}
	if err := scanLogs(context.Background(), pods, "", clean, r); err != nil {
		t.Fatalf("clean logs: %v", err)
	}
}

// errReader is a reader that always fails.
type errReader struct{}

// Read always returns an error; the buffer is untouched.
func (errReader) Read(_ []byte) (int, error) { return 0, errors.New("read failed") }
