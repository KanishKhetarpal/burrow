package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validYAML builds a config that passes validation, with roots and projects
// pointing at real directories created by the test.
func validYAML(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "Documents")
	code := filepath.Join(dir, "code")
	proj := filepath.Join(code, "demo")
	for _, d := range []string{root, code, proj} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	y := `
owner:
  tailscale_login: "owner@example.com"
device:
  allowed:
    - { name: "iPhone", kind: "phone", node_id: "nABC123" }
  max_devices: 3
agent:
  hostname: "n-ab12cd34"
  state_dir: ` + q(filepath.Join(dir, "state")) + `
projects_parents:
  - ` + q(code) + `
projects:
  - name: "demo"
    path: ` + q(proj) + `
files:
  roots:
    - { name: "Documents", path: ` + q(root) + `, writable: true }
  upload_default_root: "Documents"
ssh:
  user: "pcuser"
  host: "pc.example.ts.net"
`
	return y, dir
}

func q(s string) string { return "'" + s + "'" }

func TestParseValid(t *testing.T) {
	y, _ := validYAML(t)
	c, err := Parse([]byte(y), "test.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Defaults must survive a config that omits them.
	if c.Claude.MaxSessions != 6 {
		t.Errorf("claude.max_sessions default = %d, want 6", c.Claude.MaxSessions)
	}
	if !c.Claude.RestrictedToken {
		t.Error("claude.restricted_token default = false, want true")
	}
	if c.Wake.Method != WakeSmartPlug {
		t.Errorf("wake.method default = %q, want smartplug", c.Wake.Method)
	}
	if c.Agent.TsnetLogsUpload {
		t.Error("agent.tsnet_logs_upload default = true, want false")
	}
	if c.Agent.Auth.IdleTimeout().Minutes() != 30 {
		t.Errorf("idle timeout = %v, want 30m", c.Agent.Auth.IdleTimeout())
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	y, _ := validYAML(t)
	_, err := Parse([]byte(y+"\nbogus_key: 1\n"), "test.yaml")
	if err == nil {
		t.Fatal("Parse accepted an unknown key; a typo in a security setting must not be ignored")
	}
}

func TestValidateRejects(t *testing.T) {
	base, dir := validYAML(t)

	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{{
		name:   "empty owner login",
		mutate: func(y string) string { return strings.Replace(y, `"owner@example.com"`, `""`, 1) },
		want:   "owner.tailscale_login is required",
	}, {
		name:   "placeholder owner login",
		mutate: func(y string) string { return strings.Replace(y, `"owner@example.com"`, `"<OWNER_TAILSCALE_LOGIN>"`, 1) },
		want:   "PLACEHOLDER",
	}, {
		name:   "drive root as file root",
		mutate: func(y string) string { return strings.Replace(y, q(filepath.Join(dir, "Documents")), `'C:\'`, 1) },
		want:   "drive root is too broad",
	}, {
		name: "nonexistent project path",
		mutate: func(y string) string {
			return strings.Replace(y, q(filepath.Join(dir, "code", "demo")), q(filepath.Join(dir, "nope")), 1)
		},
		want: "does not exist",
	}, {
		name:   "bypassPermissions as default mode",
		mutate: func(y string) string { return y + "\nclaude:\n  default_permission_mode: \"bypassPermissions\"\n" },
		want:   "bypassPermissions is never allowed",
	}, {
		name:   "waker method without url",
		mutate: func(y string) string { return y + "\nwake:\n  method: \"waker\"\n" },
		want:   "requires wake.waker_url",
	}, {
		name: "smartplug without shortcut names",
		mutate: func(y string) string {
			return y + "\nwake:\n  method: \"smartplug\"\n  shortcuts:\n    on: \"\"\n    off: \"\"\n    power_cycle: \"\"\n"
		},
		want: "all three wake.shortcuts",
	}, {
		name: "upload root not among roots",
		mutate: func(y string) string {
			return strings.Replace(y, `upload_default_root: "Documents"`, `upload_default_root: "Nope"`, 1)
		},
		want: "is not one of files.roots",
	}, {
		name: "duplicate device node id",
		mutate: func(y string) string {
			return strings.Replace(y,
				`    - { name: "iPhone", kind: "phone", node_id: "nABC123" }`,
				"    - { name: \"iPhone\", kind: \"phone\", node_id: \"nABC123\" }\n    - { name: \"Laptop\", kind: \"laptop\", node_id: \"nABC123\" }", 1)
		},
		want: "listed twice",
	}, {
		name:   "bad device kind",
		mutate: func(y string) string { return strings.Replace(y, `kind: "phone"`, `kind: "tablet"`, 1) },
		want:   "must be phone or laptop",
	}, {
		name:   "too many devices",
		mutate: func(y string) string { return strings.Replace(y, "max_devices: 3", "max_devices: 9", 1) },
		want:   "device.max_devices must be 1..3",
	}, {
		name:   "power cycle later than wake timeout",
		mutate: func(y string) string { return y + "\nwake:\n  power_cycle_after_seconds: 600\n" },
		want:   "power_cycle_after_seconds must be",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.mutate(base)), "test.yaml")
			if err == nil {
				t.Fatalf("Parse accepted an invalid config (wanted error mentioning %q)", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v\nwant it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateRejectsUserProfileAsRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	y := `
owner:
  tailscale_login: "owner@example.com"
agent:
  hostname: "n-ab12cd34"
files:
  roots:
    - { name: "Home", path: '` + home + `', writable: true }
  upload_default_root: "Home"
`
	_, err := Parse([]byte(y), "test.yaml")
	if err == nil || !strings.Contains(err.Error(), "user profile itself is too broad") {
		t.Fatalf("Parse err = %v, want a refusal to use the profile root", err)
	}
}

func TestValidatePermissionMode(t *testing.T) {
	for _, ok := range []string{"default", "acceptEdits", "plan"} {
		if err := ValidatePermissionMode(ok); err != nil {
			t.Errorf("ValidatePermissionMode(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"bypassPermissions", "", "yolo", "Default"} {
		if err := ValidatePermissionMode(bad); err == nil {
			t.Errorf("ValidatePermissionMode(%q) = nil, want an error", bad)
		}
	}
}

func TestUnderProjectsParent(t *testing.T) {
	dir := t.TempDir()
	code := filepath.Join(dir, "code")
	inside := filepath.Join(code, "proj", "sub")
	outside := filepath.Join(dir, "elsewhere")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := &Config{ProjectsParents: []string{code}}

	if !c.UnderProjectsParent(inside) {
		t.Errorf("UnderProjectsParent(%q) = false, want true", inside)
	}
	if !c.UnderProjectsParent(code) {
		t.Errorf("UnderProjectsParent(parent itself) = false, want true")
	}
	if c.UnderProjectsParent(outside) {
		t.Errorf("UnderProjectsParent(%q) = true, want false", outside)
	}
	// A sibling whose name merely starts with the parent's name must not pass.
	sibling := code + "-other"
	if c.UnderProjectsParent(sibling) {
		t.Errorf("UnderProjectsParent(%q) = true, want false (prefix must respect separators)", sibling)
	}
}

func TestRootAndAllowedNodeLookup(t *testing.T) {
	c := &Config{
		Files: Files{Roots: []Root{{Name: "Documents", Path: `D:\docs`, Writable: true}}},
		Device: Device{Allowed: []AllowedDevice{
			{Name: "iPhone", Kind: KindPhone, NodeID: "nPHONE"},
		}},
	}
	if r, ok := c.Root("documents"); !ok || r.Path != `D:\docs` {
		t.Errorf("Root(\"documents\") = %+v, %v; want a case-insensitive hit", r, ok)
	}
	if _, ok := c.Root("nope"); ok {
		t.Error("Root(\"nope\") matched")
	}
	if d, ok := c.AllowedNode("nPHONE"); !ok || d.Kind != KindPhone {
		t.Errorf("AllowedNode(nPHONE) = %+v, %v", d, ok)
	}
	// Node IDs are case-sensitive identifiers from Tailscale; a near miss must
	// not be accepted.
	if _, ok := c.AllowedNode("nphone"); ok {
		t.Error("AllowedNode matched a different-cased node id")
	}
}
