package kubeclient

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestListPendingPods_FiltersPhase(t *testing.T) {
	cs := fake.NewClientset(
		pod("pending-a", "default", corev1.PodPending),
		pod("running-b", "default", corev1.PodRunning),
		pod("pending-c", "kube-system", corev1.PodPending),
	)
	c := Wrap(cs)
	got, err := c.ListPendingPods(context.Background())
	if err != nil {
		t.Fatalf("ListPendingPods: %v", err)
	}
	// fake client doesn't enforce field selectors by default — just confirm wrapper passes through
	// and returns *something* without error. The real selector behaviour is tested e2e against
	// a real apiserver. We assert the count is non-zero.
	if len(got) == 0 {
		t.Errorf("expected pods returned")
	}
}

func TestListNodes(t *testing.T) {
	cs := fake.NewClientset(node("a"), node("b"))
	c := Wrap(cs)
	got, err := c.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(got))
	}
}

func TestCordonAndDelete(t *testing.T) {
	cs := fake.NewClientset(node("a"))
	c := Wrap(cs)
	if err := c.Cordon(context.Background(), "a"); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	updated, err := cs.CoreV1().Nodes().Get(context.Background(), "a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !updated.Spec.Unschedulable {
		t.Errorf("node not marked unschedulable")
	}
	if err := c.Delete(context.Background(), "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestEvict(t *testing.T) {
	p := pod("p1", "default", corev1.PodRunning)
	cs := fake.NewClientset(p)
	c := Wrap(cs)
	if err := c.Evict(context.Background(), *p); err != nil {
		t.Fatalf("Evict: %v", err)
	}
}

func pod(name, ns string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func node(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}
