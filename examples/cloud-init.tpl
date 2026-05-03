#cloud-config
# Example cloud-init template for joining a k3s cluster as an agent.
# Available variables:
#   {{ .ServerIP }}    — control-plane IP (config.server_ip)
#   {{ .JoinToken }}   — value of AUTOSCALER_JOIN_TOKEN env var
#   {{ .NodeLabels }}  — pool.node_labels (comma-separated key=value)
#   {{ .NodeTaints }}  — pool.node_taints (comma-separated key=value:Effect)
#
# Replace this with the install flow appropriate for your distribution
# (k3s / RKE2 / kubeadm / Talos / ...).

package_update: true
packages:
  - curl

runcmd:
  - |
    TAINT_FLAG=""
    if [ -n "{{ .NodeTaints }}" ]; then
      TAINT_FLAG="--node-taint={{ .NodeTaints }}"
    fi
    curl -sfL https://get.k3s.io | \
      INSTALL_K3S_VERSION=v1.31.0+k3s1 \
      K3S_URL=https://{{ .ServerIP }}:6443 \
      K3S_TOKEN={{ .JoinToken }} \
      INSTALL_K3S_EXEC="agent --node-label={{ .NodeLabels }} $TAINT_FLAG" \
      sh -
