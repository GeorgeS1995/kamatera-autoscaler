package config

import (
	"errors"
	"fmt"
	"os"
	"text/template"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/labels"
)

// Config is the runtime configuration loaded from a YAML file plus environment-only credentials.
type Config struct {
	Datacenter            string `yaml:"datacenter"`
	VLANName              string `yaml:"vlan_name"`
	ServerIP              string `yaml:"server_ip"`
	CloudInitTemplatePath string `yaml:"cloud_init_template"`
	Pools                 []Pool `yaml:"pools"`

	// CloudInit is the parsed cloud-init template, populated by Load.
	CloudInit *template.Template `yaml:"-"`

	// Creds is populated by LoadCredentials, never from YAML.
	Creds Credentials `yaml:"-"`
}

// Pool describes a homogeneous group of nodes managed by the autoscaler.
type Pool struct {
	Name        string `yaml:"name"`
	CPUType     string `yaml:"cpu_type"`
	CPUCores    int    `yaml:"cpu_cores"`
	RAMMB       int    `yaml:"ram_mb"`
	DiskGB      int    `yaml:"disk_gb"`
	Image       string `yaml:"image"`
	MinNodes    int    `yaml:"min_nodes"`
	MaxNodes    int    `yaml:"max_nodes"`
	NodeLabels  string `yaml:"node_labels"`
	NodeTaints  string `yaml:"node_taints"`
	PodSelector string `yaml:"pod_selector"`

	// parsedSelector is computed by Validate.
	parsedSelector labels.Selector `yaml:"-"`
}

// Selector returns the parsed labels.Selector for matching pod nodeSelectors against this pool.
func (p *Pool) Selector() labels.Selector { return p.parsedSelector }

// Credentials hold secrets sourced exclusively from environment variables.
type Credentials struct {
	kamateraClientID string
	kamateraSecret   string
	joinToken        string
	sshPubKey        string
}

// KamateraClientID returns the API client ID. Use only when calling Kamatera.
func (c Credentials) KamateraClientID() string { return c.kamateraClientID }

// KamateraSecret returns the API secret.
func (c Credentials) KamateraSecret() string { return c.kamateraSecret }

// JoinToken returns the cluster join token (e.g. K3S_TOKEN) provided to cloud-init.
func (c Credentials) JoinToken() string { return c.joinToken }

// SSHPubKey returns the SSH public key injected into provisioned VMs.
func (c Credentials) SSHPubKey() string { return c.sshPubKey }

// String returns a redacted placeholder so accidental %v / %+v logging never leaks secrets.
func (c Credentials) String() string { return "<redacted>" }

// GoString matches String for fmt %#v.
func (c Credentials) GoString() string { return "<redacted>" }

// reservedFields are field names we refuse to accept in the YAML to enforce env-only credential loading.
var reservedFields = []string{
	"kamatera_client_id", "kamatera_secret",
	"api_client_id", "api_secret",
	"join_token", "k3s_token",
	"ssh_pub_key", "ssh_key",
	"creds", "credentials", "secrets",
}

// Load parses the YAML at path, parses the cloud-init template, and validates the result.
// Credentials are NOT loaded here — call LoadCredentials separately.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	if err := rejectReservedFields(raw); err != nil {
		return nil, err
	}

	dec := yaml.NewDecoder(bytesReader(raw))
	dec.KnownFields(true)
	cfg := &Config{}
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	tmplBytes, err := os.ReadFile(cfg.CloudInitTemplatePath)
	if err != nil {
		return nil, fmt.Errorf("read cloud-init template %s: %w", cfg.CloudInitTemplatePath, err)
	}
	tmpl, err := template.New("cloud-init").Option("missingkey=error").Parse(string(tmplBytes))
	if err != nil {
		return nil, fmt.Errorf("parse cloud-init template: %w", err)
	}
	cfg.CloudInit = tmpl
	return cfg, nil
}

// LoadCredentials pulls secrets from process environment and fails fast if any are missing.
func LoadCredentials() (Credentials, error) {
	required := map[string]string{
		"KAMATERA_API_CLIENT_ID": "",
		"KAMATERA_API_SECRET":    "",
		"AUTOSCALER_JOIN_TOKEN":  "",
		"SSH_PUB_KEY":            "",
	}
	for k := range required {
		v := os.Getenv(k)
		if v == "" {
			return Credentials{}, fmt.Errorf("required env var %s is empty", k)
		}
		required[k] = v
	}
	return Credentials{
		kamateraClientID: required["KAMATERA_API_CLIENT_ID"],
		kamateraSecret:   required["KAMATERA_API_SECRET"],
		joinToken:        required["AUTOSCALER_JOIN_TOKEN"],
		sshPubKey:        required["SSH_PUB_KEY"],
	}, nil
}

// Validate enforces invariants on a parsed Config (excluding the cloud-init template, which Load handles).
func (c *Config) Validate() error {
	if c.Datacenter == "" {
		return errors.New("datacenter is required")
	}
	if c.CloudInitTemplatePath == "" {
		return errors.New("cloud_init_template is required")
	}
	if len(c.Pools) == 0 {
		return errors.New("at least one pool is required")
	}

	seen := make(map[string]struct{}, len(c.Pools))
	for i := range c.Pools {
		p := &c.Pools[i]
		if p.Name == "" {
			return fmt.Errorf("pool[%d]: name is required", i)
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("duplicate pool name %q", p.Name)
		}
		seen[p.Name] = struct{}{}

		if !isValidCPUType(p.CPUType) {
			return fmt.Errorf("pool %q: cpu_type must be one of A, B, D, T (got %q)", p.Name, p.CPUType)
		}
		if p.CPUCores <= 0 {
			return fmt.Errorf("pool %q: cpu_cores must be > 0", p.Name)
		}
		if p.RAMMB <= 0 {
			return fmt.Errorf("pool %q: ram_mb must be > 0", p.Name)
		}
		if p.DiskGB <= 0 {
			return fmt.Errorf("pool %q: disk_gb must be > 0", p.Name)
		}
		if p.Image == "" {
			return fmt.Errorf("pool %q: image is required", p.Name)
		}
		if p.MinNodes < 0 {
			return fmt.Errorf("pool %q: min_nodes must be >= 0", p.Name)
		}
		if p.MaxNodes < p.MinNodes {
			return fmt.Errorf("pool %q: max_nodes (%d) must be >= min_nodes (%d)", p.Name, p.MaxNodes, p.MinNodes)
		}
		if p.PodSelector == "" {
			return fmt.Errorf("pool %q: pod_selector is required", p.Name)
		}
		sel, err := labels.Parse(p.PodSelector)
		if err != nil {
			return fmt.Errorf("pool %q: pod_selector %q is not a valid label selector: %w", p.Name, p.PodSelector, err)
		}
		p.parsedSelector = sel
	}
	return nil
}

// PoolByName returns a pointer to the pool with the given name, or nil if not found.
func (c *Config) PoolByName(name string) *Pool {
	for i := range c.Pools {
		if c.Pools[i].Name == name {
			return &c.Pools[i]
		}
	}
	return nil
}

func isValidCPUType(t string) bool {
	switch t {
	case "A", "B", "D", "T":
		return true
	}
	return false
}
