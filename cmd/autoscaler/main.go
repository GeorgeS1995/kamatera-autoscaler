// Command autoscaler is the entrypoint for the Kamatera Cluster Autoscaler.
//
// It loads pool configuration from a YAML file (default
// /etc/autoscaler/pools.yaml), reads credentials from environment variables,
// connects to the Kubernetes API (in-cluster or via KUBECONFIG), and runs the
// scale-up / scale-down control loops until SIGINT or SIGTERM is received.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/config"
	"github.com/GeorgeS1995/kamatera-autoscaler/internal/controller"
	"github.com/GeorgeS1995/kamatera-autoscaler/internal/kamatera"
	"github.com/GeorgeS1995/kamatera-autoscaler/internal/kubeclient"
	"github.com/GeorgeS1995/kamatera-autoscaler/internal/logging"
)

const usage = `kamatera-autoscaler — Kubernetes node autoscaler for Kamatera Cloud.

Configuration is sourced from a YAML file and the environment.

Required environment variables:
  KAMATERA_API_CLIENT_ID    Kamatera REST API client id.
  KAMATERA_API_SECRET       Kamatera REST API secret.
  AUTOSCALER_JOIN_TOKEN     Cluster join token used by your cloud-init template.
  SSH_PUB_KEY               SSH public key injected into provisioned VMs.

Optional environment variables:
  AUTOSCALER_CONFIG         Path to pools.yaml (default: /etc/autoscaler/pools.yaml).
  LOG_LEVEL                 debug | info | warn | error (default: info).
  KUBECONFIG                Path to kubeconfig (only used outside a cluster).
`

func main() {
	help := flag.Bool("help", false, "Print usage and exit.")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()
	if *help {
		fmt.Print(usage)
		return
	}

	log := logging.New(os.Getenv("LOG_LEVEL"))

	cfgPath := os.Getenv("AUTOSCALER_CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/autoscaler/pools.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Error("config load failed", "path", cfgPath, "err", err)
		os.Exit(2)
	}

	creds, err := config.LoadCredentials()
	if err != nil {
		log.Error("credentials missing", "err", err)
		os.Exit(2)
	}
	cfg.Creds = creds

	kc, err := kubeclient.NewInClusterOrKubeconfig()
	if err != nil {
		log.Error("kubernetes client init failed", "err", err)
		os.Exit(2)
	}

	kamClient := kamatera.NewClient(cfg.Creds)

	ctrl := controller.New(cfg, kc, kc, kc, kamClient, log, controller.Options{})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := ctrl.Run(ctx); err != nil {
		log.Error("controller exited with error", "err", err)
		os.Exit(1)
	}
}
