package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SaveDevices writes the config back to disk after the device allowlist
// changed, which happens when a device is paired or revoked.
//
// The whole file is re-marshalled rather than patched in place. That is the
// honest trade: YAML comments in the installed burrow.yaml are not preserved,
// but a surgical text edit of a nested block is the kind of thing that silently
// corrupts a security-critical file. The commented reference copy lives in
// config/burrow.example.yaml, and setup regenerates the installed file.
func SaveDevices(c *Config) error {
	if c.path == "" {
		return fmt.Errorf("config: no path to save to")
	}
	if err := c.Validate(); err != nil {
		// Never write a config the agent would refuse to start with.
		return fmt.Errorf("config: refusing to save an invalid config: %w", err)
	}

	raw, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}

	// Written to a temp file in the same directory and renamed, so a crash
	// mid-write cannot leave a half-written config behind.
	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, ".burrow-config-*.yaml")
	if err != nil {
		return fmt.Errorf("config: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("config: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("config: flush: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close: %w", err)
	}
	// Windows will not rename onto an existing file.
	if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("config: replace: %w", err)
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		return fmt.Errorf("config: rename: %w", err)
	}
	return nil
}

// SetPath records where a config came from, for tests and for a config built
// in memory by the CLI.
func (c *Config) SetPath(p string) { c.path = p }
