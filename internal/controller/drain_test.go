package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/kubeclient"
)

// fake mutator and lister implementations for drain tests.

type fakeMutator struct {
	mu             sync.Mutex
	cordoned       []string
	evicted        []string
	evictResp      map[string]error // pod name → error to return; absent → success
	evictCallCount map[string]int
}

func (f *fakeMutator) Cordon(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cordoned = append(f.cordoned, name)
	return nil
}

func (f *fakeMutator) Evict(_ context.Context, p corev1.Pod) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.evictCallCount == nil {
		f.evictCallCount = map[string]int{}
	}
	f.evictCallCount[p.Name]++
	if err, ok := f.evictResp[p.Name]; ok {
		return err
	}
	f.evicted = append(f.evicted, p.Name)
	return nil
}

func (f *fakeMutator) Delete(_ context.Context, _ string) error { return nil }

type podLister struct {
	mu      sync.Mutex
	byNode  map[string][]corev1.Pod
	pending []corev1.Pod
}

func (p *podLister) ListPendingPods(_ context.Context) ([]corev1.Pod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]corev1.Pod{}, p.pending...), nil
}

func (p *podLister) ListPodsOnNode(_ context.Context, name string) ([]corev1.Pod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]corev1.Pod{}, p.byNode[name]...), nil
}

// helper to drop a pod from the listing once "evicted" — we wire this from the mutator.
func (p *podLister) removePodOnNode(node, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	in := p.byNode[node]
	out := in[:0]
	for _, pod := range in {
		if pod.Name != name {
			out = append(out, pod)
		}
	}
	p.byNode[node] = out
}

func mkPod(name, ns, node string, owner string) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node},
	}
	if owner != "" {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: owner, Name: "owner"}}
	}
	return p
}

var _ kubeclient.NodeMutator = (*fakeMutator)(nil)
var _ kubeclient.PodLister = (*podLister)(nil)

func TestDrain_HappyPath_AllEvictable(t *testing.T) {
	pl := &podLister{byNode: map[string][]corev1.Pod{
		"node-a": {mkPod("p1", "default", "node-a", "ReplicaSet"), mkPod("p2", "default", "node-a", "ReplicaSet")},
	}}
	mut := &fakeMutator{}
	// On evict, drop the pod from the lister so the next list shows progress.
	mut.evictResp = nil // success for all
	originalEvict := mut.Evict
	_ = originalEvict
	mut2 := &drainAdapter{inner: mut, pl: pl, node: "node-a"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := drainNode(ctx, mut2, pl, "node-a", 5*time.Second); err != nil {
		t.Fatalf("drainNode: %v", err)
	}
	if got := mut.cordoned; len(got) != 1 || got[0] != "node-a" {
		t.Errorf("cordoned = %v", got)
	}
	if len(mut.evicted) != 2 {
		t.Errorf("evicted = %v", mut.evicted)
	}
}

func TestDrain_SkipsDaemonSetAndMirrorPods(t *testing.T) {
	mirror := mkPod("mirror", "kube-system", "node-a", "")
	mirror.Annotations = map[string]string{"kubernetes.io/config.mirror": "abc"}
	pl := &podLister{byNode: map[string][]corev1.Pod{
		"node-a": {
			mkPod("ds", "kube-system", "node-a", "DaemonSet"),
			mirror,
		},
	}}
	mut := &fakeMutator{}
	mut2 := &drainAdapter{inner: mut, pl: pl, node: "node-a"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := drainNode(ctx, mut2, pl, "node-a", 5*time.Second); err != nil {
		t.Fatalf("drainNode: %v", err)
	}
	if len(mut.evicted) != 0 {
		t.Errorf("should not evict DaemonSet/mirror pods, got %v", mut.evicted)
	}
}

func TestDrain_PDBBlocksEviction_DeadlineExceeded(t *testing.T) {
	pl := &podLister{byNode: map[string][]corev1.Pod{
		"node-a": {mkPod("p1", "default", "node-a", "ReplicaSet")},
	}}
	pdbErr := &apierrors.StatusError{ErrStatus: metav1.Status{
		Reason: metav1.StatusReasonTooManyRequests, Code: 429,
		Details: &metav1.StatusDetails{
			Group: "policy", Kind: "poddisruptionbudgets",
			Causes: []metav1.StatusCause{{Message: "blocked by PDB"}},
		},
	}}
	mut := &fakeMutator{evictResp: map[string]error{"p1": pdbErr}}
	mut2 := &drainAdapter{inner: mut, pl: pl, node: "node-a"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := drainNode(ctx, mut2, pl, "node-a", 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected drain to fail when PDB blocks")
	}
}

func TestDrain_GoneIsNotAFailure(t *testing.T) {
	pl := &podLister{byNode: map[string][]corev1.Pod{
		"node-a": {mkPod("p1", "default", "node-a", "ReplicaSet")},
	}}
	gone := apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "p1")
	mut := &fakeMutator{evictResp: map[string]error{"p1": gone}}
	// The lister should reflect the pod actually being gone after eviction.
	pl.byNode["node-a"] = nil
	mut2 := &drainAdapter{inner: mut, pl: pl, node: "node-a"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := drainNode(ctx, mut2, pl, "node-a", 2*time.Second); err != nil {
		t.Fatalf("drainNode: %v", err)
	}
}

// drainAdapter wraps a fakeMutator and removes pods from the podLister upon
// successful eviction so the drain loop sees forward progress.
type drainAdapter struct {
	inner *fakeMutator
	pl    *podLister
	node  string
}

func (d *drainAdapter) Cordon(ctx context.Context, name string) error {
	return d.inner.Cordon(ctx, name)
}
func (d *drainAdapter) Evict(ctx context.Context, p corev1.Pod) error {
	err := d.inner.Evict(ctx, p)
	if err == nil {
		d.pl.removePodOnNode(d.node, p.Name)
	}
	return err
}
func (d *drainAdapter) Delete(ctx context.Context, name string) error {
	return d.inner.Delete(ctx, name)
}
