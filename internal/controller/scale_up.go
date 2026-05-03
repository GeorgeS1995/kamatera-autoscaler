package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/config"
	"github.com/GeorgeS1995/kamatera-autoscaler/internal/kamatera"
)

// scaleUp groups Pending pods by their target pool and creates new Kamatera VMs
// so the cluster can eventually schedule them. Each VM is tracked in c.creating
// so subsequent ticks don't double-provision while the new node is still booting.
func (c *Controller) scaleUp(ctx context.Context) error {
	pending, err := c.pods.ListPendingPods(ctx)
	if err != nil {
		return err
	}
	pendingByPool := groupPendingByPool(pending, c.cfg)
	if len(pendingByPool) == 0 {
		c.log.Debug("scale up: no pending pods need new nodes")
	}

	nodes, err := c.nodes.ListNodes(ctx)
	if err != nil {
		return err
	}
	nodesByPool := countNodesByPool(nodes)

	for poolName, count := range pendingByPool {
		pool := c.cfg.PoolByName(poolName)
		if pool == nil {
			continue
		}
		current := nodesByPool[poolName]
		inFlight := c.creating.Count(poolName)
		capacity := pool.MaxNodes - (current + inFlight)
		if capacity <= 0 {
			c.log.Info("pool at max capacity",
				"pool", poolName, "current", current, "in_flight", inFlight, "max", pool.MaxNodes,
				"pending_pods", count,
			)
			continue
		}
		// In-flight nodes are assumed to satisfy pending pods once they finish booting,
		// so we don't double-provision: toCreate = pending - inFlight, capped by capacity.
		toCreate := count - inFlight
		if toCreate <= 0 {
			c.log.Debug("scale up: in-flight nodes already cover pending pods",
				"pool", poolName, "pending", count, "in_flight", inFlight,
			)
			continue
		}
		if toCreate > capacity {
			toCreate = capacity
		}
		c.log.Info("scaling up pool",
			"pool", poolName, "pending_pods", count, "current", current, "in_flight", inFlight,
			"creating", toCreate, "max", pool.MaxNodes,
		)
		for i := 0; i < toCreate; i++ {
			nodeName := generateNodeName(poolName)
			if !c.creating.Add(poolName, nodeName) {
				continue
			}
			poolCopy := pool
			go c.createNode(ctx, poolCopy, nodeName)
		}
	}
	return nil
}

func (c *Controller) createNode(ctx context.Context, pool *config.Pool, nodeName string) {
	defer c.creating.Remove(nodeName)
	cloudInit, err := renderCloudInit(c.cfg, pool, c.cfg.Creds.JoinToken())
	if err != nil {
		c.log.Error("render cloud-init failed", "pool", pool.Name, "node", nodeName, "err", err)
		return
	}
	req := kamatera.CreateServerRequest{
		Name:         nodeName,
		Datacenter:   c.cfg.Datacenter,
		Image:        pool.Image,
		CPU:          fmt.Sprintf("%d%s", pool.CPUCores, pool.CPUType),
		RAM:          pool.RAMMB,
		Disk:         fmt.Sprintf("size=%d", pool.DiskGB),
		Network:      buildNetwork(c.cfg.VLANName),
		BillingCycle: "hourly",
		PowerOn:      "yes",
		ScriptFile:   cloudInit,
		SSHKey:       c.cfg.Creds.SSHPubKey(),
		// Kamatera marks `password` required even when an SSH key is provided.
		// `__generate__` asks the API to roll a random password we never use
		// (SSH access goes through SSHKey above).
		Password: "__generate__",
	}
	c.log.Info("creating server", "pool", pool.Name, "node", nodeName, "cpu", req.CPU, "ram_mb", req.RAM, "disk_gb", pool.DiskGB)

	cmdID, err := c.kam.CreateServer(ctx, req)
	if err != nil {
		// Recovered creates: the POST timed out at the transport layer but
		// Kamatera nevertheless provisioned the VM. Treat as success and skip
		// the queue wait — the server already exists.
		var rec *kamatera.ErrCreateRecovered
		if errors.As(err, &rec) {
			c.log.Info("server recovered after transport failure",
				"pool", pool.Name, "node", nodeName, "kamatera_id", rec.Server.ID)
			return
		}
		c.log.Error("create server failed", "pool", pool.Name, "node", nodeName, "err", err)
		return
	}
	c.log.Info("create server queued", "pool", pool.Name, "node", nodeName, "command_id", cmdID)

	srv, err := c.kam.WaitProvision(ctx, cmdID, nodeName, c.nodeReadyTimeout)
	if err != nil {
		c.log.Error("wait provision failed", "pool", pool.Name, "node", nodeName, "err", err)
		return
	}
	c.log.Info("server provisioned", "pool", pool.Name, "node", nodeName, "kamatera_id", srv.ID)
}

// buildNetwork forms the Kamatera "network" string. Two NICs: WAN (auto IP) and the
// configured private VLAN (auto IP within the VLAN).
func buildNetwork(vlanName string) string {
	parts := []string{"name=wan,ip=auto"}
	if vlanName != "" {
		parts = append(parts, fmt.Sprintf("name=%s,ip=auto", vlanName))
	}
	return strings.Join(parts, " ")
}

func groupPendingByPool(pods []corev1.Pod, cfg *config.Config) map[string]int {
	out := map[string]int{}
	for _, p := range pods {
		if !isPendingForResources(p) {
			continue
		}
		pool := poolForPod(p, cfg)
		if pool == nil {
			continue
		}
		out[pool.Name]++
	}
	return out
}

func countNodesByPool(nodes []corev1.Node) map[string]int {
	out := map[string]int{}
	for _, n := range nodes {
		if name, ok := n.Labels["pool"]; ok {
			out[name]++
		}
	}
	return out
}

// generateNodeName picks a unique-enough name for a new node. The pool prefix
// makes it easy to spot in Kamatera Console; the timestamp makes it sortable;
// the random suffix avoids collisions when multiple are created in the same second.
func generateNodeName(poolName string) string {
	var rnd [3]byte
	_, _ = rand.Read(rnd[:])
	return fmt.Sprintf("%s-%d-%s", poolName, time.Now().Unix(), hex.EncodeToString(rnd[:]))
}
