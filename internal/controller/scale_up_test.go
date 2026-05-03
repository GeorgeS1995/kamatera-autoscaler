package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/config"
	"github.com/GeorgeS1995/kamatera-autoscaler/internal/kamatera"
)

// fakeKamatera records calls and returns canned responses.
type fakeKamatera struct {
	mu        sync.Mutex
	creates   []kamatera.CreateServerRequest
	createErr error
	terminate []string
	findResp  map[string]kamatera.Server
	findErr   error
	waitErr   error
}

func (f *fakeKamatera) CreateServer(_ context.Context, req kamatera.CreateServerRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.creates = append(f.creates, req)
	return "cmd-" + req.Name, nil
}

func (f *fakeKamatera) WaitProvision(_ context.Context, _, _ string, _ time.Duration) (kamatera.Server, error) {
	if f.waitErr != nil {
		return kamatera.Server{}, f.waitErr
	}
	return kamatera.Server{ID: "id-x"}, nil
}

func (f *fakeKamatera) WaitTerminate(_ context.Context, _ string, _ time.Duration) error {
	return f.waitErr
}

func (f *fakeKamatera) FindServerByName(_ context.Context, name string) (kamatera.Server, error) {
	if f.findErr != nil {
		return kamatera.Server{}, f.findErr
	}
	if s, ok := f.findResp[name]; ok {
		return s, nil
	}
	return kamatera.Server{}, kamatera.ErrServerNotFound
}

func (f *fakeKamatera) TerminateServer(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminate = append(f.terminate, id)
	return "term-" + id, nil
}

// nodeLister provides a static list of nodes; satisfies kubeclient.NodeLister.
type nodeLister struct {
	mu    sync.Mutex
	nodes []corev1.Node
}

func (n *nodeLister) ListNodes(_ context.Context) ([]corev1.Node, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]corev1.Node{}, n.nodes...), nil
}

func newCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Datacenter:            "EU-FR",
		VLANName:              "example-vlan",
		ServerIP:              "10.0.0.20",
		CloudInitTemplatePath: "/x",
		Pools: []config.Pool{
			{Name: "general", CPUType: "B", CPUCores: 2, RAMMB: 2048, DiskGB: 20, Image: "img", MinNodes: 1, MaxNodes: 4, NodeLabels: "pool=general", PodSelector: "pool=general"},
			{Name: "gpu-pool", CPUType: "D", CPUCores: 4, RAMMB: 8192, DiskGB: 60, Image: "img", MinNodes: 0, MaxNodes: 2, NodeLabels: "pool=gpu-pool,workload=gpu", PodSelector: "workload=gpu"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	tpl, err := template.New("x").Parse("#cloud-config server={{.ServerIP}} token={{.JoinToken}} labels={{.NodeLabels}} taints={{.NodeTaints}}")
	if err != nil {
		t.Fatal(err)
	}
	cfg.CloudInit = tpl
	return cfg
}

func mkPodWithSelector(name, ns string, sel map[string]string, pendingResources bool) corev1.Pod {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeSelector: sel},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	if pendingResources {
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  corev1.ConditionFalse,
			Reason:  corev1.PodReasonUnschedulable,
			Message: "0/3 nodes available: 3 Insufficient cpu",
		}}
	}
	return pod
}

func mkNodeInPool(name, pool string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: name, Labels: map[string]string{"pool": pool},
	}}
}

func newController(cfg *config.Config, pl *podLister, nl *nodeLister, mut interface {
	Cordon(context.Context, string) error
	Evict(context.Context, corev1.Pod) error
	Delete(context.Context, string) error
}, kam kamatera.Client) *Controller {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(cfg, pl, nl, mut, kam, log, Options{
		ScaleUpEvery:     time.Second,
		ScaleDownEvery:   time.Minute,
		NodeReadyTimeout: 10 * time.Millisecond,
		IdleThreshold:    time.Hour,
		DrainTimeout:     time.Second,
	})
}

func TestScaleUp_CreatesOneVMPerPendingPodCappedByMax(t *testing.T) {
	cfg := newCfg(t)
	pl := &podLister{
		pending: []corev1.Pod{
			mkPodWithSelector("p1", "ns", map[string]string{"pool": "general"}, true),
			mkPodWithSelector("p2", "ns", map[string]string{"pool": "general"}, true),
			mkPodWithSelector("p3", "ns", map[string]string{"pool": "general"}, true),
			mkPodWithSelector("p4", "ns", map[string]string{"pool": "general"}, true),
			mkPodWithSelector("p5", "ns", map[string]string{"pool": "general"}, true),
		},
		byNode: map[string][]corev1.Pod{},
	}
	nl := &nodeLister{nodes: []corev1.Node{
		mkNodeInPool("g-1", "general"),
		mkNodeInPool("g-2", "general"),
	}}
	kam := &fakeKamatera{}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, mut, kam)

	if err := c.scaleUp(context.Background()); err != nil {
		t.Fatalf("scaleUp: %v", err)
	}
	// Wait for goroutines.
	waitFor(t, 2*time.Second, func() bool {
		kam.mu.Lock()
		defer kam.mu.Unlock()
		return len(kam.creates) == 2
	})
	// max=4, current=2 → exactly 2 creates even though pending=5.
	if got := len(kam.creates); got != 2 {
		t.Errorf("creates = %d, want 2", got)
	}
}

func TestScaleUp_NoMatchingPool_DoesNothing(t *testing.T) {
	cfg := newCfg(t)
	pl := &podLister{pending: []corev1.Pod{
		mkPodWithSelector("p1", "ns", map[string]string{"pool": "phantom"}, true),
	}}
	nl := &nodeLister{}
	kam := &fakeKamatera{}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, mut, kam)
	if err := c.scaleUp(context.Background()); err != nil {
		t.Fatalf("scaleUp: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(kam.creates) != 0 {
		t.Errorf("expected no creates, got %d", len(kam.creates))
	}
}

func TestScaleUp_IgnoresNonResourcePending(t *testing.T) {
	cfg := newCfg(t)
	pl := &podLister{pending: []corev1.Pod{
		mkPodWithSelector("p1", "ns", map[string]string{"pool": "general"}, false),
	}}
	nl := &nodeLister{}
	kam := &fakeKamatera{}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, mut, kam)
	if err := c.scaleUp(context.Background()); err != nil {
		t.Fatalf("scaleUp: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(kam.creates) != 0 {
		t.Errorf("expected no creates, got %d", len(kam.creates))
	}
}

func TestScaleUp_AtMaxCapacity_Skips(t *testing.T) {
	cfg := newCfg(t)
	pl := &podLister{pending: []corev1.Pod{
		mkPodWithSelector("p1", "ns", map[string]string{"pool": "gpu-pool"}, true),
		mkPodWithSelector("p2", "ns", map[string]string{"pool": "gpu-pool"}, true),
	}}
	nl := &nodeLister{nodes: []corev1.Node{
		mkNodeInPool("g-1", "gpu-pool"),
		mkNodeInPool("g-2", "gpu-pool"),
	}}
	kam := &fakeKamatera{}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, mut, kam)
	if err := c.scaleUp(context.Background()); err != nil {
		t.Fatalf("scaleUp: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(kam.creates) != 0 {
		t.Errorf("at max, expected no creates; got %d", len(kam.creates))
	}
}

func TestScaleUp_InFlightTrackerPreventsDoubleProvisioning(t *testing.T) {
	cfg := newCfg(t)
	pl := &podLister{pending: []corev1.Pod{
		mkPodWithSelector("p1", "ns", map[string]string{"pool": "general"}, true),
	}}
	nl := &nodeLister{nodes: []corev1.Node{
		mkNodeInPool("g-1", "general"),
		mkNodeInPool("g-2", "general"),
	}}
	// Block CreateServer indefinitely so the goroutine stays "in flight".
	block := make(chan struct{})
	defer close(block)
	kam := &blockingKamatera{block: block}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, mut, kam)

	if err := c.scaleUp(context.Background()); err != nil {
		t.Fatalf("scaleUp: %v", err)
	}
	waitFor(t, time.Second, func() bool { return c.creating.Count("general") == 1 })

	// Second tick: still one pending pod, in-flight = 1, so no new create.
	if err := c.scaleUp(context.Background()); err != nil {
		t.Fatalf("scaleUp second: %v", err)
	}
	if got := c.creating.Count("general"); got != 1 {
		t.Errorf("creating = %d, want 1", got)
	}
}

// blockingKamatera blocks CreateServer until the channel is closed.
type blockingKamatera struct {
	block chan struct{}
	count int
	mu    sync.Mutex
}

func (b *blockingKamatera) CreateServer(_ context.Context, req kamatera.CreateServerRequest) (string, error) {
	b.mu.Lock()
	b.count++
	b.mu.Unlock()
	<-b.block
	return "cmd-" + req.Name, nil
}
func (b *blockingKamatera) WaitProvision(_ context.Context, _, _ string, _ time.Duration) (kamatera.Server, error) {
	return kamatera.Server{ID: "x"}, nil
}
func (b *blockingKamatera) WaitTerminate(_ context.Context, _ string, _ time.Duration) error {
	return nil
}
func (b *blockingKamatera) FindServerByName(_ context.Context, _ string) (kamatera.Server, error) {
	return kamatera.Server{}, kamatera.ErrServerNotFound
}
func (b *blockingKamatera) TerminateServer(_ context.Context, _ string) (string, error) {
	return "", nil
}

func TestScaleUp_CreateErrorReleasesTracker(t *testing.T) {
	cfg := newCfg(t)
	pl := &podLister{pending: []corev1.Pod{
		mkPodWithSelector("p1", "ns", map[string]string{"pool": "general"}, true),
	}}
	nl := &nodeLister{}
	kam := &fakeKamatera{createErr: errors.New("boom")}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, mut, kam)
	if err := c.scaleUp(context.Background()); err != nil {
		t.Fatalf("scaleUp: %v", err)
	}
	waitFor(t, time.Second, func() bool { return c.creating.Count("general") == 0 })
	if got := c.creating.Count("general"); got != 0 {
		t.Errorf("tracker not released: count = %d", got)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
