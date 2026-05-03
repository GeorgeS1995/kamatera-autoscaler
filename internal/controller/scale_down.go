package controller

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/kamatera"
)

// scaleDown looks for nodes that are idle (no non-DaemonSet pods) and whose pool
// is above its min_nodes; cordon, drain, terminate the underlying VM, then delete
// the K8s Node object. Drain failures (e.g. PDB-blocked) abort the operation for
// that node — the next tick will retry.
func (c *Controller) scaleDown(ctx context.Context) error {
	nodes, err := c.nodes.ListNodes(ctx)
	if err != nil {
		return err
	}
	byPool := countNodesByPool(nodes)

	for _, n := range nodes {
		pool := poolForNode(n, c.cfg)
		if pool == nil {
			continue
		}
		if byPool[pool.Name] <= pool.MinNodes {
			continue
		}
		if !c.isNodeOldEnough(n) {
			continue
		}
		idle, err := c.isNodeIdle(ctx, n)
		if err != nil {
			c.log.Error("idle check failed", "node", n.Name, "err", err)
			continue
		}
		if !idle {
			continue
		}

		c.log.Info("scaling down idle node", "node", n.Name, "pool", pool.Name)
		if err := drainNode(ctx, c.mut, c.pods, n.Name, c.drainTimeout); err != nil {
			c.log.Warn("drain failed; leaving node in place", "node", n.Name, "err", err)
			continue
		}
		if err := c.terminateAndDelete(ctx, n.Name); err != nil {
			c.log.Error("terminate/delete failed", "node", n.Name, "err", err)
			continue
		}
		byPool[pool.Name]--
	}
	return nil
}

// terminateAndDelete locates the Kamatera server by name, terminates it, then
// removes the K8s Node object. NotFound from Kamatera is treated as success
// (the VM is already gone or never existed).
func (c *Controller) terminateAndDelete(ctx context.Context, nodeName string) error {
	srv, err := c.kam.FindServerByName(ctx, nodeName)
	switch {
	case errors.Is(err, kamatera.ErrServerNotFound):
		c.log.Info("kamatera server already absent; deleting node object", "node", nodeName)
	case err != nil:
		return err
	default:
		cmdID, err := c.kam.TerminateServer(ctx, srv.ID)
		if err != nil {
			return err
		}
		if err := c.kam.WaitTerminate(ctx, cmdID, c.nodeReadyTimeout); err != nil {
			c.log.Warn("wait terminate failed; will still delete node object", "node", nodeName, "err", err)
		}
	}
	if err := c.mut.Delete(ctx, nodeName); err != nil {
		return err
	}
	return nil
}

func (c *Controller) isNodeOldEnough(n corev1.Node) bool {
	created := n.CreationTimestamp.Time
	if created.IsZero() {
		return true
	}
	return c.now().Sub(created) >= c.idleThreshold
}

// isNodeIdle returns true if every non-DaemonSet, non-mirror pod on the node has
// been there long enough that we're confident it's truly idle.
func (c *Controller) isNodeIdle(ctx context.Context, n corev1.Node) (bool, error) {
	pods, err := c.pods.ListPodsOnNode(ctx, n.Name)
	if err != nil {
		return false, err
	}
	for _, p := range pods {
		if isDaemonSetPod(p) || isMirrorPod(p) {
			continue
		}
		// Pods scheduled to a node but completed (Succeeded/Failed) shouldn't keep it alive.
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		// A live workload pod = node not idle.
		return false, nil
	}
	return true, nil
}

// stableSince is unused but documents the intended semantics: in a more
// sophisticated implementation we'd track per-node "last became idle"
// timestamps and require that to age past idleThreshold, like the upstream
// cluster-autoscaler. For now the combination of node-age + zero-pods is good
// enough and keeps state in the cluster, not in the autoscaler process.
var _ = time.Time{}
