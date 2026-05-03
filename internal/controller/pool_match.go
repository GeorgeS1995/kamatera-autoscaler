package controller

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/config"
)

// isPendingForResources returns true iff the pod is Pending due to insufficient
// CPU or memory on existing nodes. Other unschedulable reasons (PVC binding,
// taint mismatch, affinity) are not the autoscaler's problem.
func isPendingForResources(p corev1.Pod) bool {
	if p.Status.Phase != corev1.PodPending {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse {
			continue
		}
		if c.Reason != corev1.PodReasonUnschedulable {
			continue
		}
		msg := c.Message
		if strings.Contains(msg, "Insufficient cpu") ||
			strings.Contains(msg, "Insufficient memory") ||
			strings.Contains(msg, "Insufficient ephemeral-storage") {
			return true
		}
	}
	return false
}

// poolForPod returns the first pool whose pod_selector matches the pod's nodeSelector.
// Matching semantics: the pool's selector must match the labelset formed by the pod's
// spec.nodeSelector (which is itself a labels.Set). This is the same semantics K8s uses
// when scheduling — a pod that asks for nodeSelector{a=1,b=2} can only land on a node
// that has both labels, and the pool offers nodes labeled to satisfy it.
//
// Returns nil if no pool matches.
func poolForPod(p corev1.Pod, cfg *config.Config) *config.Pool {
	if len(p.Spec.NodeSelector) == 0 {
		return nil
	}
	set := labels.Set(p.Spec.NodeSelector)
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		if pool.Selector() == nil {
			continue
		}
		if pool.Selector().Matches(set) {
			return pool
		}
	}
	return nil
}

// poolForNode returns the pool that owns the given node, based on the node's `pool` label.
// Returns nil if no matching pool is configured.
func poolForNode(n corev1.Node, cfg *config.Config) *config.Pool {
	name := n.Labels["pool"]
	if name == "" {
		return nil
	}
	return cfg.PoolByName(name)
}
