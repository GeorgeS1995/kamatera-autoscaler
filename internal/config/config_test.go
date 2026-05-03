package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
datacenter: EU-FR
vlan_name: example-vlan
server_ip: 10.0.0.20
cloud_init_template: %s
pools:
  - name: general
    cpu_type: B
    cpu_cores: 2
    ram_mb: 4096
    disk_gb: 40
    image: ubuntu_server_24.04_64-bit
    min_nodes: 1
    max_nodes: 4
    node_labels: pool=general
    node_taints: ""
    pod_selector: pool=general
  - name: gpu-pool
    cpu_type: D
    cpu_cores: 4
    ram_mb: 8192
    disk_gb: 60
    image: ubuntu_server_24.04_64-bit
    min_nodes: 0
    max_nodes: 3
    node_labels: pool=gpu-pool,workload=gpu
    node_taints: dedicated=gpu:NoSchedule
    pod_selector: workload=gpu
`

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func newCloudInit(t *testing.T, dir string) string {
	t.Helper()
	return writeFile(t, dir, "cloud-init.tpl", "#cloud-config\n# server_ip={{.ServerIP}} token={{.JoinToken}} labels={{.NodeLabels}} taints={{.NodeTaints}}\n")
}

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	return writeFile(t, dir, "config.yaml", body)
}

func TestLoad_Valid(t *testing.T) {
	dir := t.TempDir()
	tpl := newCloudInit(t, dir)
	cfgPath := writeConfig(t, dir, fmtYAML(validYAML, tpl))

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Datacenter != "EU-FR" {
		t.Errorf("Datacenter = %q, want EU-FR", cfg.Datacenter)
	}
	if len(cfg.Pools) != 2 {
		t.Fatalf("expected 2 pools, got %d", len(cfg.Pools))
	}
	if cfg.PoolByName("general") == nil {
		t.Error("PoolByName(general) returned nil")
	}
	if cfg.PoolByName("does-not-exist") != nil {
		t.Error("PoolByName for missing pool should return nil")
	}
	if cfg.CloudInit == nil {
		t.Error("cloud-init template not parsed")
	}
	if cfg.PoolByName("general").Selector() == nil {
		t.Error("pool selector not parsed")
	}
}

func TestLoad_RejectsCredentialFieldsInYAML(t *testing.T) {
	dir := t.TempDir()
	tpl := newCloudInit(t, dir)
	body := fmtYAML(validYAML, tpl) + "\nkamatera_secret: oops\n"
	cfgPath := writeConfig(t, dir, body)

	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "kamatera_secret") {
		t.Fatalf("expected error mentioning kamatera_secret, got %v", err)
	}
}

func TestLoad_RejectsAllReservedFieldNames(t *testing.T) {
	for _, field := range reservedFields {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			tpl := newCloudInit(t, dir)
			body := fmtYAML(validYAML, tpl) + "\n" + field + ": oops\n"
			cfgPath := writeConfig(t, dir, body)
			if _, err := Load(cfgPath); err == nil {
				t.Errorf("expected reserved-field error for %q", field)
			}
		})
	}
}

func TestLoad_RejectsUnknownTopLevelField(t *testing.T) {
	dir := t.TempDir()
	tpl := newCloudInit(t, dir)
	body := fmtYAML(validYAML, tpl) + "\nrandom_extra_key: 42\n"
	cfgPath := writeConfig(t, dir, body)

	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "random_extra_key") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestLoad_MissingCloudInitFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeConfig(t, dir, fmtYAML(validYAML, filepath.Join(dir, "nope.tpl")))
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("expected error for missing cloud-init template")
	}
}

func TestLoad_BadCloudInitTemplate(t *testing.T) {
	dir := t.TempDir()
	tpl := writeFile(t, dir, "bad.tpl", "{{ .Unclosed ")
	cfgPath := writeConfig(t, dir, fmtYAML(validYAML, tpl))
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("expected error for bad cloud-init template")
	}
}

func TestValidate_DuplicatePool(t *testing.T) {
	cfg := &Config{
		Datacenter:            "EU-FR",
		CloudInitTemplatePath: "/x",
		Pools: []Pool{
			validPool("a"), validPool("a"),
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate-pool error, got %v", err)
	}
}

func TestValidate_MaxLessThanMin(t *testing.T) {
	p := validPool("a")
	p.MinNodes = 5
	p.MaxNodes = 2
	cfg := &Config{Datacenter: "EU-FR", CloudInitTemplatePath: "/x", Pools: []Pool{p}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_nodes") {
		t.Fatalf("expected max_nodes error, got %v", err)
	}
}

func TestValidate_BadCPUType(t *testing.T) {
	p := validPool("a")
	p.CPUType = "X"
	cfg := &Config{Datacenter: "EU-FR", CloudInitTemplatePath: "/x", Pools: []Pool{p}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "cpu_type") {
		t.Fatalf("expected cpu_type error, got %v", err)
	}
}

func TestValidate_InvalidPodSelector(t *testing.T) {
	p := validPool("a")
	p.PodSelector = "!!!not a selector"
	cfg := &Config{Datacenter: "EU-FR", CloudInitTemplatePath: "/x", Pools: []Pool{p}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "pod_selector") {
		t.Fatalf("expected pod_selector error, got %v", err)
	}
}

func TestValidate_MissingDatacenter(t *testing.T) {
	cfg := &Config{Pools: []Pool{validPool("a")}, CloudInitTemplatePath: "/x"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected datacenter required error")
	}
}

func TestValidate_NoPools(t *testing.T) {
	cfg := &Config{Datacenter: "EU-FR", CloudInitTemplatePath: "/x"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected at-least-one-pool error")
	}
}

func TestValidate_BadResources(t *testing.T) {
	cases := []func(p *Pool){
		func(p *Pool) { p.CPUCores = 0 },
		func(p *Pool) { p.RAMMB = 0 },
		func(p *Pool) { p.DiskGB = 0 },
		func(p *Pool) { p.Image = "" },
		func(p *Pool) { p.MinNodes = -1 },
		func(p *Pool) { p.PodSelector = "" },
		func(p *Pool) { p.Name = "" },
	}
	for i, mut := range cases {
		p := validPool("a")
		mut(&p)
		cfg := &Config{Datacenter: "EU-FR", CloudInitTemplatePath: "/x", Pools: []Pool{p}}
		if err := cfg.Validate(); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

func TestLoadCredentials_HappyAndMissing(t *testing.T) {
	envs := map[string]string{
		"KAMATERA_API_CLIENT_ID": "cid",
		"KAMATERA_API_SECRET":    "sec",
		"AUTOSCALER_JOIN_TOKEN":  "tok",
		"SSH_PUB_KEY":            "ssh-ed25519 AAAA",
	}
	t.Run("happy", func(t *testing.T) {
		for k, v := range envs {
			t.Setenv(k, v)
		}
		c, err := LoadCredentials()
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if c.KamateraClientID() != "cid" || c.KamateraSecret() != "sec" || c.JoinToken() != "tok" || c.SSHPubKey() != "ssh-ed25519 AAAA" {
			t.Errorf("creds not populated: %+v", c)
		}
		if got := c.String(); got != "<redacted>" {
			t.Errorf("String = %q, want <redacted>", got)
		}
	})
	for missing := range envs {
		t.Run("missing/"+missing, func(t *testing.T) {
			for k, v := range envs {
				if k == missing {
					t.Setenv(k, "")
					continue
				}
				t.Setenv(k, v)
			}
			if _, err := LoadCredentials(); err == nil {
				t.Errorf("expected error for missing %s", missing)
			}
		})
	}
}

func TestCredentialsRedaction(t *testing.T) {
	c := Credentials{kamateraClientID: "cid", kamateraSecret: "sec"}
	if got := c.String(); got != "<redacted>" {
		t.Errorf("String = %q", got)
	}
	if got := c.GoString(); got != "<redacted>" {
		t.Errorf("GoString = %q", got)
	}
}

func validPool(name string) Pool {
	return Pool{
		Name: name, CPUType: "B", CPUCores: 2, RAMMB: 2048, DiskGB: 20,
		Image: "ubuntu_server_24.04_64-bit", MinNodes: 1, MaxNodes: 4,
		NodeLabels: "pool=" + name, PodSelector: "pool=" + name,
	}
}

func fmtYAML(tmpl string, args ...any) string {
	out := tmpl
	for _, a := range args {
		out = strings.Replace(out, "%s", a.(string), 1)
	}
	return out
}
