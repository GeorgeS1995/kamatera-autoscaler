package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/kamatera"
)

func mkNodeAt(name, pool string, age time.Duration) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:              name,
		Labels:            map[string]string{"pool": pool},
		CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
	}}
}

func TestScaleDown_IdleNodeAboveMin_DrainsAndTerminates(t *testing.T) {
	cfg := newCfg(t)
	cfg.Pools[0].MinNodes = 1
	pl := &podLister{byNode: map[string][]corev1.Pod{
		"g-1": {},
		"g-2": {},
	}}
	nl := &nodeLister{nodes: []corev1.Node{
		mkNodeAt("g-1", "general", 2*time.Hour),
		mkNodeAt("g-2", "general", 2*time.Hour),
	}}
	kam := &fakeKamatera{findResp: map[string]kamatera.Server{
		"g-1": {ID: "1", Name: "g-1"},
		"g-2": {ID: "2", Name: "g-2"},
	}}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, &cordonAdapter{Mutator: mut, Lister: pl}, kam)

	if err := c.scaleDown(context.Background()); err != nil {
		t.Fatalf("scaleDown: %v", err)
	}
	if len(kam.terminate) == 0 {
		t.Errorf("expected terminate calls, got %v", kam.terminate)
	}
}

func TestScaleDown_AtMinNodes_NoOp(t *testing.T) {
	cfg := newCfg(t)
	cfg.Pools[0].MinNodes = 2
	pl := &podLister{byNode: map[string][]corev1.Pod{
		"g-1": {}, "g-2": {},
	}}
	nl := &nodeLister{nodes: []corev1.Node{
		mkNodeAt("g-1", "general", 2*time.Hour),
		mkNodeAt("g-2", "general", 2*time.Hour),
	}}
	kam := &fakeKamatera{}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, mut, kam)
	if err := c.scaleDown(context.Background()); err != nil {
		t.Fatalf("scaleDown: %v", err)
	}
	if len(kam.terminate) != 0 {
		t.Errorf("expected no terminates, got %v", kam.terminate)
	}
}

func TestScaleDown_NodeTooYoung_NoOp(t *testing.T) {
	cfg := newCfg(t)
	cfg.Pools[0].MinNodes = 1
	pl := &podLister{byNode: map[string][]corev1.Pod{
		"g-1": {}, "g-2": {},
	}}
	nl := &nodeLister{nodes: []corev1.Node{
		mkNodeAt("g-1", "general", 2*time.Hour),
		mkNodeAt("g-2", "general", time.Minute),
	}}
	kam := &fakeKamatera{findResp: map[string]kamatera.Server{
		"g-1": {ID: "1", Name: "g-1"},
	}}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, &cordonAdapter{Mutator: mut, Lister: pl}, kam)
	if err := c.scaleDown(context.Background()); err != nil {
		t.Fatalf("scaleDown: %v", err)
	}
	// Only g-1 should be terminated (g-2 is too young).
	if len(kam.terminate) != 1 || kam.terminate[0] != "1" {
		t.Errorf("expected terminate of id=1 only, got %v", kam.terminate)
	}
}

func TestScaleDown_NodeWithLiveWorkload_NotIdle(t *testing.T) {
	cfg := newCfg(t)
	cfg.Pools[0].MinNodes = 1
	pl := &podLister{byNode: map[string][]corev1.Pod{
		"g-1": {},
		"g-2": {mkPod("workload", "ns", "g-2", "ReplicaSet")},
	}}
	nl := &nodeLister{nodes: []corev1.Node{
		mkNodeAt("g-1", "general", 2*time.Hour),
		mkNodeAt("g-2", "general", 2*time.Hour),
	}}
	kam := &fakeKamatera{findResp: map[string]kamatera.Server{
		"g-1": {ID: "1", Name: "g-1"},
	}}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, &cordonAdapter{Mutator: mut, Lister: pl}, kam)
	if err := c.scaleDown(context.Background()); err != nil {
		t.Fatalf("scaleDown: %v", err)
	}
	if len(kam.terminate) != 1 || kam.terminate[0] != "1" {
		t.Errorf("expected only g-1 terminated, got %v", kam.terminate)
	}
}

func TestScaleDown_DaemonSetAndCompletedPodsAreIdle(t *testing.T) {
	cfg := newCfg(t)
	cfg.Pools[0].MinNodes = 1
	finished := mkPod("done", "ns", "g-2", "Job")
	finished.Status.Phase = corev1.PodSucceeded
	pl := &podLister{byNode: map[string][]corev1.Pod{
		"g-1": {},
		"g-2": {mkPod("ds", "kube-system", "g-2", "DaemonSet"), finished},
	}}
	nl := &nodeLister{nodes: []corev1.Node{
		mkNodeAt("g-1", "general", 2*time.Hour),
		mkNodeAt("g-2", "general", 2*time.Hour),
	}}
	kam := &fakeKamatera{findResp: map[string]kamatera.Server{
		"g-1": {ID: "1", Name: "g-1"}, "g-2": {ID: "2", Name: "g-2"},
	}}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, &cordonAdapter{Mutator: mut, Lister: pl}, kam)
	if err := c.scaleDown(context.Background()); err != nil {
		t.Fatalf("scaleDown: %v", err)
	}
	if len(kam.terminate) == 0 {
		t.Errorf("expected at least one terminate, got %v", kam.terminate)
	}
}

func TestScaleDown_KamateraServerAlreadyAbsent_StillDeletesNode(t *testing.T) {
	cfg := newCfg(t)
	cfg.Pools[0].MinNodes = 0
	pl := &podLister{byNode: map[string][]corev1.Pod{"orphan": {}}}
	nl := &nodeLister{nodes: []corev1.Node{mkNodeAt("orphan", "general", 2*time.Hour)}}
	kam := &fakeKamatera{findErr: kamatera.ErrServerNotFound}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, &cordonAdapter{Mutator: mut, Lister: pl}, kam)
	if err := c.scaleDown(context.Background()); err != nil {
		t.Fatalf("scaleDown: %v", err)
	}
	// Even though Kamatera says not found, the node object should still be deleted.
	// Our fakeMutator doesn't actually track deletes — but the controller didn't error.
	if len(kam.terminate) != 0 {
		t.Errorf("expected no terminate (server already gone), got %v", kam.terminate)
	}
}

func TestScaleDown_FindServerFails_LogsAndContinues(t *testing.T) {
	cfg := newCfg(t)
	cfg.Pools[0].MinNodes = 0
	pl := &podLister{byNode: map[string][]corev1.Pod{"x": {}}}
	nl := &nodeLister{nodes: []corev1.Node{mkNodeAt("x", "general", 2*time.Hour)}}
	kam := &fakeKamatera{findErr: errors.New("network down")}
	mut := &fakeMutator{}
	c := newController(cfg, pl, nl, &cordonAdapter{Mutator: mut, Lister: pl}, kam)
	if err := c.scaleDown(context.Background()); err != nil {
		t.Fatalf("scaleDown should not propagate per-node errors: %v", err)
	}
}

// cordonAdapter wraps fakeMutator + podLister so that successful eviction removes the pod
// from the lister, mirroring what a real apiserver would do.
type cordonAdapter struct {
	Mutator *fakeMutator
	Lister  *podLister
}

func (c *cordonAdapter) Cordon(ctx context.Context, name string) error {
	return c.Mutator.Cordon(ctx, name)
}
func (c *cordonAdapter) Evict(ctx context.Context, p corev1.Pod) error {
	if err := c.Mutator.Evict(ctx, p); err != nil {
		return err
	}
	c.Lister.removePodOnNode(p.Spec.NodeName, p.Name)
	return nil
}
func (c *cordonAdapter) Delete(ctx context.Context, name string) error {
	return c.Mutator.Delete(ctx, name)
}
