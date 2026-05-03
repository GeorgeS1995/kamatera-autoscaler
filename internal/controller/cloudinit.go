package controller

import (
	"bytes"
	"fmt"

	"github.com/GeorgeS1995/kamatera-autoscaler/internal/config"
)

// CloudInitVars are the named values exposed to the cloud-init template.
type CloudInitVars struct {
	ServerIP   string
	JoinToken  string
	NodeLabels string
	NodeTaints string
}

// renderCloudInit executes the configured template with the pool-specific values.
func renderCloudInit(cfg *config.Config, pool *config.Pool, joinToken string) (string, error) {
	if cfg.CloudInit == nil {
		return "", fmt.Errorf("cloud-init template is not loaded")
	}
	var buf bytes.Buffer
	vars := CloudInitVars{
		ServerIP:   cfg.ServerIP,
		JoinToken:  joinToken,
		NodeLabels: pool.NodeLabels,
		NodeTaints: pool.NodeTaints,
	}
	if err := cfg.CloudInit.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("render cloud-init: %w", err)
	}
	return buf.String(), nil
}
