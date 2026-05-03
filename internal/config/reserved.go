package config

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// rejectReservedFields scans the YAML for top-level / pool-level keys that must come from
// environment variables (credentials, tokens, SSH keys). Their presence in YAML is a
// configuration error to prevent secrets from being committed alongside cluster config.
func rejectReservedFields(raw []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if err == io.EOF {
				return nil
			}
			return nil // let the strict parser surface the real error
		}
		if err := walkForbiddenKeys(&node); err != nil {
			return err
		}
	}
}

func walkForbiddenKeys(n *yaml.Node) error {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		for _, c := range n.Content {
			if err := walkForbiddenKeys(c); err != nil {
				return err
			}
		}
		return nil
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind == yaml.ScalarNode && isReservedKey(key.Value) {
				return fmt.Errorf("config: field %q is reserved and must come from environment, not YAML (line %d)", key.Value, key.Line)
			}
			if i+1 < len(n.Content) {
				if err := walkForbiddenKeys(n.Content[i+1]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if n.Kind == yaml.SequenceNode {
		for _, c := range n.Content {
			if err := walkForbiddenKeys(c); err != nil {
				return err
			}
		}
	}
	return nil
}

func isReservedKey(k string) bool {
	low := strings.ToLower(k)
	for _, r := range reservedFields {
		if low == r {
			return true
		}
	}
	return false
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
