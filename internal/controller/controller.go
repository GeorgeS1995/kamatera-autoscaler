// Package controller implements the autoscaler control loop: it watches K8s
// for unschedulable pods, provisions Kamatera VMs to satisfy them, and drains
// idle nodes whose pool is above its configured min_nodes.
package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/config"
	"github.com/GeorgeS1995/kamatera-autoscaler/internal/kamatera"
	"github.com/GeorgeS1995/kamatera-autoscaler/internal/kubeclient"
)

// Controller wires the K8s and Kamatera clients together with policy.
type Controller struct {
	cfg      *config.Config
	pods     kubeclient.PodLister
	nodes    kubeclient.NodeLister
	mut      kubeclient.NodeMutator
	kam      kamatera.Client
	creating *PendingTracker
	log      *slog.Logger

	scaleUpEvery     time.Duration
	scaleDownEvery   time.Duration
	nodeReadyTimeout time.Duration
	idleThreshold    time.Duration
	drainTimeout     time.Duration

	// scaleDownInProgress prevents two scaleDown ticks from overlapping.
	// scaleDown can run for many minutes (drain timeout + per-node terminate)
	// while ticks fire every 10 min by default; using TryLock here means a
	// second tick that arrives during a stuck cycle is silently skipped
	// instead of stacking up and racing on shared state.
	scaleDownInProgress sync.Mutex

	// now is overridable in tests.
	now func() time.Time
}

// Options configures a Controller; zero values fall back to sensible defaults.
type Options struct {
	ScaleUpEvery     time.Duration
	ScaleDownEvery   time.Duration
	NodeReadyTimeout time.Duration
	IdleThreshold    time.Duration
	DrainTimeout     time.Duration
}

// New constructs a Controller with the provided wiring and options.
func New(cfg *config.Config,
	pods kubeclient.PodLister, nodes kubeclient.NodeLister, mut kubeclient.NodeMutator,
	kam kamatera.Client, log *slog.Logger, opts Options,
) *Controller {
	c := &Controller{
		cfg: cfg, pods: pods, nodes: nodes, mut: mut, kam: kam, log: log,
		scaleUpEvery:     orDefault(opts.ScaleUpEvery, 30*time.Second),
		scaleDownEvery:   orDefault(opts.ScaleDownEvery, 10*time.Minute),
		nodeReadyTimeout: orDefault(opts.NodeReadyTimeout, 5*time.Minute),
		idleThreshold:    orDefault(opts.IdleThreshold, 10*time.Minute),
		drainTimeout:     orDefault(opts.DrainTimeout, 5*time.Minute),
		now:              time.Now,
	}
	c.creating = NewPendingTracker(c.nodeReadyTimeout)
	return c
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Run drives both control loops until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	c.log.Info("controller starting",
		"scale_up_every", c.scaleUpEvery,
		"scale_down_every", c.scaleDownEvery,
		"idle_threshold", c.idleThreshold,
		"node_ready_timeout", c.nodeReadyTimeout,
		"pools", len(c.cfg.Pools),
	)
	upTicker := time.NewTicker(c.scaleUpEvery)
	downTicker := time.NewTicker(c.scaleDownEvery)
	defer upTicker.Stop()
	defer downTicker.Stop()

	// First scale-up tick happens immediately; first scale-down waits a full interval
	// so we don't churn just after a restart.
	c.runScaleUp(ctx)

	for {
		select {
		case <-ctx.Done():
			c.log.Info("shutting down gracefully")
			return nil
		case <-upTicker.C:
			c.runScaleUp(ctx)
		case <-downTicker.C:
			c.runScaleDown(ctx)
		}
	}
}

func (c *Controller) runScaleUp(ctx context.Context) {
	if err := c.scaleUp(ctx); err != nil {
		c.log.Error("scale up cycle failed", "err", err)
	}
}

func (c *Controller) runScaleDown(ctx context.Context) {
	if !c.scaleDownInProgress.TryLock() {
		c.log.Warn("scale down still running from previous tick; skipping")
		return
	}
	defer c.scaleDownInProgress.Unlock()
	if err := c.scaleDown(ctx); err != nil {
		c.log.Error("scale down cycle failed", "err", err)
	}
}
