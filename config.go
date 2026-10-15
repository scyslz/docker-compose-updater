package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// env returns the trimmed environment value or fallback when unset/blank.
func env(k, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return fallback
}

// Config is the persisted, editable configuration. It is stored as a single
// JSON file (CONFIG_FILE, default /config/updater.json) so it can survive
// restarts and be edited through the web UI. Environment variables seed the
// defaults when the file is absent; once the file exists, its values win.
type Config struct {
	IncludeDirs    []string `json:"include_dirs"`
	ExcludeDirs    []string `json:"exclude_dirs"`
	ScanDepth      int      `json:"scan_depth"`
	CronSchedule   string   `json:"cron_schedule"`
	Timezone       string   `json:"timezone"`
	RunOnStart     *bool    `json:"run_on_start,omitempty"`
	AutoUpdate     *bool    `json:"auto_update,omitempty"`
	CommandTimeout string   `json:"command_timeout"`
	// DisabledProjects maps an absolute compose file path to true when the user
	// has excluded it from automatic updates via the web UI.
	DisabledProjects map[string]bool `json:"disabled_projects"`

	mu   sync.Mutex
	path string
}

// boolPtr returns a pointer to v, for constructing Config values in tests.
func boolPtr(v bool) *bool { return &v }

// RunOnStartEnabled reports the effective run-on-start setting (default true).
func (c *Config) RunOnStartEnabled() bool { return c.RunOnStart == nil || *c.RunOnStart }

// AutoUpdateEnabled reports the effective auto-update setting (default true).
func (c *Config) AutoUpdateEnabled() bool { return c.AutoUpdate == nil || *c.AutoUpdate }

// loadConfig builds the effective configuration. Values come from, in order of
// precedence: the JSON file (if present), then environment variables, then
// built-in defaults. The returned Config remembers its path for saving.
// loadConfig builds the effective configuration. All updater settings live in
// the JSON file (CONFIG_FILE, default /config/updater.json). Only bootstrap
// parameters that cannot live inside that file come from the environment:
// CONFIG_FILE (its own location) and WEB_ADDR (handled in main).
func loadConfig() (*Config, error) {
	path := env("CONFIG_FILE", "/config/updater.json")

	cfg := &Config{path: path}

	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		cfg.path = path
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	cfg.normalize()
	return cfg, nil
}

// normalize fills sane defaults and cleans path lists so the struct is always
// usable regardless of what a hand-edited file contained.
func (c *Config) normalize() {
	if len(c.IncludeDirs) == 0 {
		c.IncludeDirs = []string{"/stacks"}
	}
	clean := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, p := range in {
			if p = filepath.Clean(p); p != "" && p != "." {
				out = append(out, p)
			}
		}
		return out
	}
	c.IncludeDirs = clean(c.IncludeDirs)
	c.ExcludeDirs = clean(c.ExcludeDirs)
	if c.ScanDepth < 0 {
		c.ScanDepth = 0
	}
	if c.CronSchedule == "" {
		c.CronSchedule = "0 */6 * * *"
	}
	if c.Timezone == "" {
		c.Timezone = "Asia/Shanghai"
	}
	if c.CommandTimeout == "" {
		c.CommandTimeout = "30m"
	}
	if c.DisabledProjects == nil {
		c.DisabledProjects = map[string]bool{}
	}
}

// save writes the configuration back to its file atomically.
func (c *Config) save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.snapshot(), "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// snapshot returns a copy without the mutex, safe to marshal.
func (c *Config) snapshot() *Config {
	cp := &Config{
		IncludeDirs:      c.IncludeDirs,
		ExcludeDirs:      c.ExcludeDirs,
		ScanDepth:        c.ScanDepth,
		CronSchedule:     c.CronSchedule,
		Timezone:         c.Timezone,
		RunOnStart:       c.RunOnStart,
		AutoUpdate:       c.AutoUpdate,
		CommandTimeout:   c.CommandTimeout,
		DisabledProjects: c.DisabledProjects,
	}
	return cp
}

// ProjectEnabled reports whether a discovered compose file participates in
// automatic updates.
func (c *Config) ProjectEnabled(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.DisabledProjects[path]
}

// SetProjectEnabled toggles automatic updates for a compose file and persists.
func (c *Config) SetProjectEnabled(path string, enabled bool) error {
	c.mu.Lock()
	if enabled {
		delete(c.DisabledProjects, path)
	} else {
		c.DisabledProjects[path] = true
	}
	c.mu.Unlock()
	return c.save()
}
