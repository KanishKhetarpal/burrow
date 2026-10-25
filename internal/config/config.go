// Package config loads and validates the Burrow agent configuration.
//
// The agent refuses to start on an invalid config (docs/04-API.md §10): a bad
// config is a security problem, not an inconvenience, because every other layer
// (file roots, device allowlist, project paths) is expressed here.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where setup Phase 7 installs the config.
const DefaultPath = `C:\ProgramData\Burrow\burrow.yaml`

// WakeMethod is how the PC gets powered on.
type WakeMethod string

const (
	WakeSmartPlug WakeMethod = "smartplug"
	WakeWaker     WakeMethod = "waker"
	WakeRouter    WakeMethod = "router"
	WakeNone      WakeMethod = "none"
)

// DeviceKind distinguishes the two allowed client devices.
type DeviceKind string

const (
	KindPhone  DeviceKind = "phone"
	KindLaptop DeviceKind = "laptop"
)

// Config is the whole of burrow.yaml.
type Config struct {
	Owner           Owner      `yaml:"owner"`
	Device          Device     `yaml:"device"`
	Agent           Agent      `yaml:"agent"`
	Claude          Claude     `yaml:"claude"`
	ProjectsParents []string   `yaml:"projects_parents"`
	Projects        []Project  `yaml:"projects"`
	Launchers       []Launcher `yaml:"launchers"`
	Apps            []App      `yaml:"apps"`
	Files           Files      `yaml:"files"`
	Wake            Wake       `yaml:"wake"`
	Power           Power      `yaml:"power"`
	SSH             SSH        `yaml:"ssh"`
	Audit           AuditCfg   `yaml:"audit"`
	Logging         LoggingCfg `yaml:"logging"`

	// path is where this config was loaded from; not serialized.
	path string `yaml:"-"`
}

type Owner struct {
	TailscaleLogin string `yaml:"tailscale_login"`
}

type Device struct {
	Allowed    []AllowedDevice `yaml:"allowed"`
	MaxDevices int             `yaml:"max_devices"`
}

type AllowedDevice struct {
	Name   string     `yaml:"name"`
	Kind   DeviceKind `yaml:"kind"`
	NodeID string     `yaml:"node_id"`
}

type Agent struct {
	Hostname        string  `yaml:"hostname"`
	StateDir        string  `yaml:"state_dir"`
	TsnetLogsUpload bool    `yaml:"tsnet_logs_upload"`
	Auth            AuthCfg `yaml:"auth"`
}

type AuthCfg struct {
	SessionIdleMinutes   int `yaml:"session_idle_minutes"`
	SessionAbsoluteHours int `yaml:"session_absolute_hours"`
	StepupSeconds        int `yaml:"stepup_seconds"`
	LoginMaxFailures     int `yaml:"login_max_failures"`
	LockoutMinutes       int `yaml:"lockout_minutes"`
	PairingWindowMinutes int `yaml:"pairing_window_minutes"`
}

// IdleTimeout is the sliding session timeout.
func (a AuthCfg) IdleTimeout() time.Duration {
	return time.Duration(a.SessionIdleMinutes) * time.Minute
}

// AbsoluteTimeout is the hard session lifetime, regardless of activity.
func (a AuthCfg) AbsoluteTimeout() time.Duration {
	return time.Duration(a.SessionAbsoluteHours) * time.Hour
}

// StepupWindow is how long a passkey assertion counts as "fresh".
func (a AuthCfg) StepupWindow() time.Duration {
	return time.Duration(a.StepupSeconds) * time.Second
}

// Lockout is how long login is barred after too many failures.
func (a AuthCfg) Lockout() time.Duration {
	return time.Duration(a.LockoutMinutes) * time.Minute
}

// PairingWindow is how long a "burrow pair" window stays open.
func (a AuthCfg) PairingWindow() time.Duration {
	return time.Duration(a.PairingWindowMinutes) * time.Minute
}

type Claude struct {
	Binary                string `yaml:"binary"`
	GitBashPath           string `yaml:"git_bash_path"`
	DefaultMode           string `yaml:"default_mode"`
	DefaultPermissionMode string `yaml:"default_permission_mode"`
	ServerSpawn           string `yaml:"server_spawn"`
	ServerCapacity        int    `yaml:"server_capacity"`
	MaxSessions           int    `yaml:"max_sessions"`
	RestrictedToken       bool   `yaml:"restricted_token"`
	ReadyTimeoutSeconds   int    `yaml:"ready_timeout_seconds"`
	ScrollbackBytes       int    `yaml:"scrollback_bytes"`
	// AllowBypassPermissions lets a session be started in bypassPermissions
	// mode, each time behind a fresh step-up. Off unless set here, on the PC:
	// nothing reachable from a phone can turn it on (docs/DEVIATIONS.md D-8).
	AllowBypassPermissions bool `yaml:"allow_bypass_permissions"`
}

type Project struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// Launcher is a one-tap button in the app that starts Claude with a given
// login — e.g. one per Claude account, each with its own CLAUDE_CONFIG_DIR.
// Only burrow.yaml defines launchers; no API creates or edits them.
type Launcher struct {
	Name string `yaml:"name"`
	// Path is the folder the button starts Claude in, like opening a terminal
	// there and typing `claude`. With Path (or Project) set, one tap starts the
	// session and opens its terminal; with neither, the app asks which project.
	Path string `yaml:"path,omitempty"`
	// Project optionally names an entry in projects to start in instead.
	Project string `yaml:"project,omitempty"`
	// ClaudeConfigDir becomes CLAUDE_CONFIG_DIR for the session, which picks
	// the Claude login. Empty uses the account's default (~/.claude).
	ClaudeConfigDir string `yaml:"claude_config_dir,omitempty"`
	// PermissionMode is the default for sessions started from this button.
	PermissionMode string `yaml:"permission_mode,omitempty"`
}

// App is an "Open on PC" button: a program started on the owner's desktop,
// e.g. Chrome in a given profile. Only burrow.yaml defines apps; no API
// creates or edits them, so the phone can only open what is listed here.
type App struct {
	Name string   `yaml:"name"`
	Path string   `yaml:"path"`
	Args []string `yaml:"args,omitempty"`
}

// App returns the app with the given name.
func (c *Config) App(name string) (App, bool) {
	for _, a := range c.Apps {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return App{}, false
}

// Launcher returns the launcher with the given name.
func (c *Config) Launcher(name string) (Launcher, bool) {
	for _, l := range c.Launchers {
		if strings.EqualFold(l.Name, name) {
			return l, true
		}
	}
	return Launcher{}, false
}

type Files struct {
	Roots             []Root   `yaml:"roots"`
	UploadDefaultRoot string   `yaml:"upload_default_root"`
	MaxUploadMB       int64    `yaml:"max_upload_mb"`
	ExtraDenylist     []string `yaml:"extra_denylist"`
}

type Root struct {
	Name     string `yaml:"name"`
	Path     string `yaml:"path"`
	Writable bool   `yaml:"writable"`
}

type Wake struct {
	Method                 WakeMethod `yaml:"method"`
	Shortcuts              Shortcuts  `yaml:"shortcuts"`
	WakeTimeoutSeconds     int        `yaml:"wake_timeout_seconds"`
	PowerCycleAfterSeconds int        `yaml:"power_cycle_after_seconds"`
	OffConfirmSeconds      int        `yaml:"off_confirm_seconds"`
	WakerURL               string     `yaml:"waker_url"`
	RouterHint             string     `yaml:"router_hint"`

	// MAC and LANPrefix are only needed for method: waker (future Pi).
	MAC       string `yaml:"mac,omitempty"`
	LANPrefix string `yaml:"lan_prefix,omitempty"`
}

type Shortcuts struct {
	On         string `yaml:"on"`
	Off        string `yaml:"off"`
	PowerCycle string `yaml:"power_cycle"`
}

type Power struct {
	KeepAwakeWhileSessions  bool `yaml:"keep_awake_while_sessions"`
	AutoShutdownIdleMinutes int  `yaml:"auto_shutdown_idle_minutes"`
}

type SSH struct {
	User string `yaml:"user"`
	Host string `yaml:"host"`
}

type AuditCfg struct {
	Path string `yaml:"path"`
}

type LoggingCfg struct {
	Dir       string `yaml:"dir"`
	Level     string `yaml:"level"`
	MaxFiles  int    `yaml:"max_files"`
	MaxSizeMB int    `yaml:"max_size_mb"`
}

// Load reads and validates the config at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw, path)
}

// Parse validates YAML bytes. Separated from Load so tests need no files.
func Parse(raw []byte, path string) (*Config, error) {
	c := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.path = path
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Default returns the config with every default applied. Parse starts here, so
// a key left out of burrow.yaml gets the documented default rather than a zero.
func Default() *Config {
	return &Config{
		Device: Device{MaxDevices: 3},
		Agent: Agent{
			StateDir:        `C:\ProgramData\Burrow\state`,
			TsnetLogsUpload: false,
			Auth: AuthCfg{
				SessionIdleMinutes:   30,
				SessionAbsoluteHours: 12,
				StepupSeconds:        300,
				LoginMaxFailures:     5,
				LockoutMinutes:       15,
				PairingWindowMinutes: 10,
			},
		},
		Claude: Claude{
			Binary:                "claude",
			DefaultMode:           "chat",
			DefaultPermissionMode: "default",
			ServerSpawn:           "same-dir",
			ServerCapacity:        8,
			MaxSessions:           6,
			RestrictedToken:       true,
			ReadyTimeoutSeconds:   30,
			ScrollbackBytes:       2 << 20,
		},
		Files: Files{
			UploadDefaultRoot: "Downloads",
			MaxUploadMB:       2048,
		},
		Wake: Wake{
			Method:                 WakeSmartPlug,
			Shortcuts:              Shortcuts{On: "PC On", Off: "PC Off", PowerCycle: "PC Power Cycle"},
			WakeTimeoutSeconds:     180,
			PowerCycleAfterSeconds: 90,
			OffConfirmSeconds:      30,
		},
		Power: Power{KeepAwakeWhileSessions: true},
		Audit: AuditCfg{Path: `C:\ProgramData\Burrow\audit\audit.jsonl`},
		Logging: LoggingCfg{
			Dir:       `C:\ProgramData\Burrow\logs`,
			Level:     "info",
			MaxFiles:  10,
			MaxSizeMB: 10,
		},
	}
}

// Path reports where this config was loaded from.
func (c *Config) Path() string { return c.path }

// ErrPlaceholder marks a config that setup has not finished filling in.
var ErrPlaceholder = errors.New("config still contains a <PLACEHOLDER>")

// Validate enforces docs/04-API.md §10 plus the structural invariants the rest
// of the agent assumes. It is deliberately strict: refusing to boot is safer
// than serving a half-configured file API.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	// --- owner ---
	if strings.TrimSpace(c.Owner.TailscaleLogin) == "" {
		add("owner.tailscale_login is required")
	} else if hasPlaceholder(c.Owner.TailscaleLogin) {
		add("owner.tailscale_login: %w", ErrPlaceholder)
	}

	// --- devices ---
	if c.Device.MaxDevices < 1 || c.Device.MaxDevices > 3 {
		add("device.max_devices must be 1..3 (got %d)", c.Device.MaxDevices)
	}
	if len(c.Device.Allowed) > c.Device.MaxDevices {
		add("device.allowed has %d entries, more than device.max_devices (%d)",
			len(c.Device.Allowed), c.Device.MaxDevices)
	}
	seenNode := map[string]bool{}
	for i, d := range c.Device.Allowed {
		if strings.TrimSpace(d.NodeID) == "" || hasPlaceholder(d.NodeID) {
			add("device.allowed[%d].node_id is empty or a placeholder", i)
		}
		if seenNode[d.NodeID] {
			add("device.allowed[%d].node_id %q is listed twice", i, d.NodeID)
		}
		seenNode[d.NodeID] = true
		switch d.Kind {
		case KindPhone, KindLaptop:
		default:
			add("device.allowed[%d].kind must be phone or laptop (got %q)", i, d.Kind)
		}
	}

	// --- agent ---
	if strings.TrimSpace(c.Agent.Hostname) == "" || hasPlaceholder(c.Agent.Hostname) {
		add("agent.hostname is empty or a placeholder")
	}
	if strings.TrimSpace(c.Agent.StateDir) == "" {
		add("agent.state_dir is required")
	}
	a := c.Agent.Auth
	if a.SessionIdleMinutes < 1 || a.SessionIdleMinutes > 1440 {
		add("agent.auth.session_idle_minutes must be 1..1440")
	}
	if a.SessionAbsoluteHours < 1 || a.SessionAbsoluteHours > 168 {
		add("agent.auth.session_absolute_hours must be 1..168")
	}
	if a.StepupSeconds < 30 || a.StepupSeconds > 3600 {
		add("agent.auth.stepup_seconds must be 30..3600")
	}
	if a.LoginMaxFailures < 1 || a.LoginMaxFailures > 20 {
		add("agent.auth.login_max_failures must be 1..20")
	}
	if a.LockoutMinutes < 1 {
		add("agent.auth.lockout_minutes must be >= 1")
	}
	if a.PairingWindowMinutes < 1 || a.PairingWindowMinutes > 60 {
		add("agent.auth.pairing_window_minutes must be 1..60")
	}

	// --- claude ---
	switch c.Claude.DefaultMode {
	case "chat", "server":
	default:
		add("claude.default_mode must be chat or server (got %q)", c.Claude.DefaultMode)
	}
	if err := ValidatePermissionMode(c.Claude.DefaultPermissionMode); err != nil {
		add("claude.default_permission_mode: %v", err)
	}
	switch c.Claude.ServerSpawn {
	case "same-dir", "worktree":
	default:
		add("claude.server_spawn must be same-dir or worktree (got %q)", c.Claude.ServerSpawn)
	}
	if c.Claude.MaxSessions < 1 || c.Claude.MaxSessions > 32 {
		add("claude.max_sessions must be 1..32")
	}
	if c.Claude.ScrollbackBytes < 64<<10 || c.Claude.ScrollbackBytes > 64<<20 {
		add("claude.scrollback_bytes must be 64KiB..64MiB")
	}
	if c.Claude.ReadyTimeoutSeconds < 5 {
		add("claude.ready_timeout_seconds must be >= 5")
	}

	// --- projects ---
	for i, p := range c.ProjectsParents {
		if err := checkDir(p); err != nil {
			add("projects_parents[%d] (%s): %v", i, p, err)
		}
	}
	seenProj := map[string]bool{}
	for i, p := range c.Projects {
		if strings.TrimSpace(p.Name) == "" {
			add("projects[%d].name is required", i)
		}
		if seenProj[strings.ToLower(p.Name)] {
			add("projects[%d].name %q is listed twice", i, p.Name)
		}
		seenProj[strings.ToLower(p.Name)] = true
		if err := checkDir(p.Path); err != nil {
			add("projects[%d] (%s): %v", i, p.Path, err)
		}
	}

	// --- launchers ---
	seenLauncher := map[string]bool{}
	for i, l := range c.Launchers {
		if strings.TrimSpace(l.Name) == "" {
			add("launchers[%d].name is required", i)
		}
		if seenLauncher[strings.ToLower(l.Name)] {
			add("launchers[%d].name %q is listed twice", i, l.Name)
		}
		seenLauncher[strings.ToLower(l.Name)] = true
		if l.Path != "" && l.Project != "" {
			add("launchers[%d]: set path or project, not both", i)
		}
		if l.Path != "" {
			if err := checkDir(l.Path); err != nil {
				add("launchers[%d].path (%s): %v", i, l.Path, err)
			}
		}
		if l.Project != "" && !seenProj[strings.ToLower(l.Project)] {
			add("launchers[%d].project %q is not one of projects", i, l.Project)
		}
		if l.ClaudeConfigDir != "" {
			if err := checkDir(l.ClaudeConfigDir); err != nil {
				add("launchers[%d].claude_config_dir (%s): %v", i, l.ClaudeConfigDir, err)
			}
		}
		if l.PermissionMode != "" {
			if err := ValidateSessionPermissionMode(l.PermissionMode, c.Claude.AllowBypassPermissions); err != nil {
				add("launchers[%d].permission_mode: %v", i, err)
			}
		}
	}

	// --- apps ---
	seenApp := map[string]bool{}
	for i, a := range c.Apps {
		if strings.TrimSpace(a.Name) == "" {
			add("apps[%d].name is required", i)
		}
		if seenApp[strings.ToLower(a.Name)] {
			add("apps[%d].name %q is listed twice", i, a.Name)
		}
		seenApp[strings.ToLower(a.Name)] = true
		if !filepath.IsAbs(a.Path) {
			add("apps[%d].path must be an absolute path to the program", i)
		} else if st, err := os.Stat(a.Path); err != nil || st.IsDir() {
			add("apps[%d].path (%s): not a program file", i, a.Path)
		}
	}

	// --- files ---
	if len(c.Files.Roots) == 0 {
		add("files.roots must list at least one root")
	}
	seenRoot := map[string]bool{}
	for i, r := range c.Files.Roots {
		if strings.TrimSpace(r.Name) == "" {
			add("files.roots[%d].name is required", i)
		}
		if seenRoot[strings.ToLower(r.Name)] {
			add("files.roots[%d].name %q is listed twice", i, r.Name)
		}
		seenRoot[strings.ToLower(r.Name)] = true
		if err := checkRootPath(r.Path); err != nil {
			add("files.roots[%d] (%s): %v", i, r.Path, err)
		}
	}
	if c.Files.UploadDefaultRoot != "" && !seenRoot[strings.ToLower(c.Files.UploadDefaultRoot)] {
		add("files.upload_default_root %q is not one of files.roots", c.Files.UploadDefaultRoot)
	}
	if c.Files.MaxUploadMB < 1 || c.Files.MaxUploadMB > 65536 {
		add("files.max_upload_mb must be 1..65536")
	}

	// --- wake ---
	switch c.Wake.Method {
	case WakeSmartPlug:
		s := c.Wake.Shortcuts
		if strings.TrimSpace(s.On) == "" || strings.TrimSpace(s.Off) == "" || strings.TrimSpace(s.PowerCycle) == "" {
			add("wake.method smartplug requires all three wake.shortcuts names (on, off, power_cycle)")
		}
	case WakeWaker:
		if strings.TrimSpace(c.Wake.WakerURL) == "" {
			add("wake.method waker requires wake.waker_url")
		} else if !strings.HasPrefix(c.Wake.WakerURL, "https://") {
			add("wake.waker_url must be an https:// URL")
		}
	case WakeRouter, WakeNone:
	default:
		add("wake.method must be smartplug, waker, router or none (got %q)", c.Wake.Method)
	}
	if c.Wake.WakeTimeoutSeconds < 30 {
		add("wake.wake_timeout_seconds must be >= 30")
	}
	if c.Wake.PowerCycleAfterSeconds < 30 || c.Wake.PowerCycleAfterSeconds > c.Wake.WakeTimeoutSeconds {
		add("wake.power_cycle_after_seconds must be 30..wake_timeout_seconds")
	}
	if c.Wake.OffConfirmSeconds < 10 {
		add("wake.off_confirm_seconds must be >= 10")
	}

	// --- power ---
	if c.Power.AutoShutdownIdleMinutes < 0 {
		add("power.auto_shutdown_idle_minutes must be >= 0 (0 = off)")
	}

	// --- ssh ---
	if hasPlaceholder(c.SSH.User) || hasPlaceholder(c.SSH.Host) {
		add("ssh.user / ssh.host: %v", ErrPlaceholder)
	}

	// --- audit / logging ---
	if strings.TrimSpace(c.Audit.Path) == "" {
		add("audit.path is required")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		add("logging.level must be debug, info, warn or error (got %q)", c.Logging.Level)
	}

	return errors.Join(errs...)
}

// ValidatePermissionMode rejects anything outside the three allowed modes.
// bypassPermissions is refused by name so a config typo can never grant it
// (docs/03-SECURITY.md T7).
func ValidatePermissionMode(m string) error {
	switch m {
	case "default", "acceptEdits", "plan":
		return nil
	case "bypassPermissions":
		return errors.New("bypassPermissions is never allowed")
	default:
		return fmt.Errorf("unknown permission mode %q", m)
	}
}

// ErrBypassDisabled is returned for a bypassPermissions session when the
// owner has not set claude.allow_bypass_permissions.
var ErrBypassDisabled = errors.New("bypassPermissions is not enabled on this PC (claude.allow_bypass_permissions)")

// ValidateSessionPermissionMode is ValidatePermissionMode for one session:
// bypassPermissions is accepted only when the owner enabled it in burrow.yaml.
// It can never be the default (default_permission_mode still goes through
// ValidatePermissionMode), so every bypass session is an explicit choice.
func ValidateSessionPermissionMode(m string, allowBypass bool) error {
	if m == "bypassPermissions" {
		if allowBypass {
			return nil
		}
		return ErrBypassDisabled
	}
	return ValidatePermissionMode(m)
}

// Root returns the configured root with the given name.
func (c *Config) Root(name string) (Root, bool) {
	for _, r := range c.Files.Roots {
		if strings.EqualFold(r.Name, name) {
			return r, true
		}
	}
	return Root{}, false
}

// AllowedNode reports whether a Tailscale node StableID is a paired device.
func (c *Config) AllowedNode(nodeID string) (AllowedDevice, bool) {
	for _, d := range c.Device.Allowed {
		if d.NodeID == nodeID {
			return d, true
		}
	}
	return AllowedDevice{}, false
}

// UnderProjectsParent reports whether p sits inside a configured parent dir.
// Used to bound which paths the app may add as projects.
func (c *Config) UnderProjectsParent(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	abs = normalize(abs)
	for _, parent := range c.ProjectsParents {
		pa, err := filepath.Abs(parent)
		if err != nil {
			continue
		}
		pa = normalize(pa)
		if abs == pa || strings.HasPrefix(abs, pa+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func normalize(p string) string {
	return strings.ToLower(filepath.Clean(p))
}

func hasPlaceholder(s string) bool {
	return strings.Contains(s, "<") && strings.Contains(s, ">")
}

func checkDir(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("path is required")
	}
	if hasPlaceholder(p) {
		return ErrPlaceholder
	}
	st, err := os.Stat(p)
	if err != nil {
		return errors.New("does not exist")
	}
	if !st.IsDir() {
		return errors.New("is not a directory")
	}
	return nil
}

// checkRootPath additionally refuses roots that are too broad to be safe: a
// drive root or the user profile itself would pull AppData, .ssh and browser
// profiles into the file API even with the denylist (docs/04-API.md §10).
func checkRootPath(p string) error {
	if err := checkDir(p); err != nil {
		return err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return fmt.Errorf("cannot resolve: %w", err)
	}
	clean := filepath.Clean(abs)
	if vol := filepath.VolumeName(clean); vol != "" && clean == vol+string(filepath.Separator) {
		return errors.New("a drive root is too broad to be a file root")
	}
	if clean == string(filepath.Separator) {
		return errors.New("the filesystem root is too broad to be a file root")
	}
	for _, env := range []string{"USERPROFILE", "HOME"} {
		if home := os.Getenv(env); home != "" {
			if normalize(clean) == normalize(home) {
				return errors.New("the user profile itself is too broad; use a subfolder")
			}
		}
	}
	return nil
}
