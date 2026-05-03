// Package kubeclient wraps client-go behind small interfaces so the controller
// can be unit-tested with the fake clientset.
package kubeclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// PodLister enumerates pods relevant to the autoscaler.
type PodLister interface {
	ListPendingPods(ctx context.Context) ([]corev1.Pod, error)
	ListPodsOnNode(ctx context.Context, nodeName string) ([]corev1.Pod, error)
}

// NodeLister enumerates K8s nodes.
type NodeLister interface {
	ListNodes(ctx context.Context) ([]corev1.Node, error)
}

// NodeMutator performs cordon, eviction, and node deletion.
type NodeMutator interface {
	Cordon(ctx context.Context, nodeName string) error
	Evict(ctx context.Context, pod corev1.Pod) error
	Delete(ctx context.Context, nodeName string) error
}

// Client implements all three interfaces, wrapping a kubernetes.Interface.
type Client struct{ K kubernetes.Interface }

// NewInClusterOrKubeconfig builds a Client using in-cluster auth in production,
// or KUBECONFIG (or the default ~/.kube/config) for local development.
func NewInClusterOrKubeconfig() (*Client, error) {
	cfg, err := buildRestConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}
	return &Client{K: cs}, nil
}

// Wrap creates a Client around an arbitrary kubernetes.Interface (used by tests).
func Wrap(cs kubernetes.Interface) *Client { return &Client{K: cs} }

func buildRestConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			path = filepath.Join(home, ".kube", "config")
		}
	}
	if path == "" {
		return nil, errors.New("no in-cluster config and no kubeconfig path")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	return cfg, nil
}

// ListPendingPods returns all pods in Pending phase, cluster-wide.
func (c *Client) ListPendingPods(ctx context.Context) ([]corev1.Pod, error) {
	list, err := c.K.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Pending",
	})
	if err != nil {
		return nil, fmt.Errorf("list pending pods: %w", err)
	}
	return list.Items, nil
}

// ListPodsOnNode returns all pods scheduled on the given node.
func (c *Client) ListPodsOnNode(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	list, err := c.K.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods on node %s: %w", nodeName, err)
	}
	return list.Items, nil
}

// ListNodes returns all nodes in the cluster.
func (c *Client) ListNodes(ctx context.Context) ([]corev1.Node, error) {
	list, err := c.K.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	return list.Items, nil
}

// Cordon marks the node unschedulable via JSON-patch.
func (c *Client) Cordon(ctx context.Context, nodeName string) error {
	patch := []byte(`{"spec":{"unschedulable":true}}`)
	_, err := c.K.CoreV1().Nodes().Patch(ctx, nodeName, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("cordon node %s: %w", nodeName, err)
	}
	return nil
}

// Evict creates a policy/v1 Eviction for the given pod, respecting PodDisruptionBudgets.
func (c *Client) Evict(ctx context.Context, pod corev1.Pod) error {
	ev := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	if err := c.K.CoreV1().Pods(pod.Namespace).EvictV1(ctx, ev); err != nil {
		return fmt.Errorf("evict pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// Delete removes the K8s Node object (the underlying VM is terminated separately).
func (c *Client) Delete(ctx context.Context, nodeName string) error {
	if err := c.K.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete node %s: %w", nodeName, err)
	}
	return nil
}
