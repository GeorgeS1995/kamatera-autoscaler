package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/kubeclient"
)

// drainNode cordons the node, then evicts every non-DaemonSet, non-mirror pod
// on it. Eviction respects PodDisruptionBudgets — if any pod's eviction is
// blocked by a PDB the function retries until the timeout, after which it
// returns an error so the caller does NOT terminate the underlying VM.
func drainNode(ctx context.Context, mut kubeclient.NodeMutator, pods kubeclient.PodLister, nodeName string, timeout time.Duration) error {
	if err := mut.Cordon(ctx, nodeName); err != nil {
		return fmt.Errorf("cordon: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("drain %s: deadline exceeded", nodeName)
		}
		ps, err := pods.ListPodsOnNode(ctx, nodeName)
		if err != nil {
			return fmt.Errorf("list pods on %s: %w", nodeName, err)
		}
		evictable := filterEvictable(ps)
		if len(evictable) == 0 {
			return nil
		}
		blocked := false
		for _, p := range evictable {
			if err := mut.Evict(ctx, p); err != nil {
				if apierrors.IsTooManyRequests(err) {
					// PDB blocked — retry after backoff.
					blocked = true
					continue
				}
				if apierrors.IsNotFound(err) {
					continue
				}
				return fmt.Errorf("evict %s/%s: %w", p.Namespace, p.Name, err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(blocked)):
		}
	}
}

func backoff(blocked bool) time.Duration {
	if blocked {
		return 5 * time.Second
	}
	return 2 * time.Second
}

// filterEvictable returns pods that should be evicted: not DaemonSet-managed,
// not static/mirror pods, and not already terminating.
func filterEvictable(in []corev1.Pod) []corev1.Pod {
	out := make([]corev1.Pod, 0, len(in))
	for _, p := range in {
		if isDaemonSetPod(p) || isMirrorPod(p) {
			continue
		}
		if p.DeletionTimestamp != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

func isDaemonSetPod(p corev1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

func isMirrorPod(p corev1.Pod) bool {
	_, ok := p.Annotations["kubernetes.io/config.mirror"]
	return ok
}
