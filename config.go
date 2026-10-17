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
	CronSchedule   string `json:"cron_schedule"`
	Timezone       string `json:"timezone"`
	RunOnStart     *bool  `json:"run_on_start,omitempty"`
	AutoUpdate     *bool  `json:"auto_update,omitempty"`
	CommandTimeout string `json:"command_timeout"`
	// EnabledProjects lists the compose file paths (as reported by the Docker
	// daemon) that participate in scheduled automatic updates. Projects not
	// listed are discovered and visible in the UI but never updated on schedule.
	EnabledProjects map[string]bool `json:"enabled_projects"`
	// LastUpdates remembers when each compose file was last updated
	// successfully (RFC3339). It is stored in the config file so the web UI can
	// still show it after the updater restarts or updates itself.
	LastUpdates map[string]string `json:"last_updates,omitempty"`

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
	if c.CronSchedule == "" {
		c.CronSchedule = "0 */6 * * *"
	}
	if c.Timezone == "" {
		c.Timezone = "Asia/Shanghai"
	}
	if c.CommandTimeout == "" {
		c.CommandTimeout = "30m"
	}
	if c.EnabledProjects == nil {
		c.EnabledProjects = map[string]bool{}
	}
	if c.LastUpdates == nil {
		c.LastUpdates = map[string]string{}
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
	enabled := make(map[string]bool, len(c.EnabledProjects))
	for k, v := range c.EnabledProjects {
		enabled[k] = v
	}
	lastUpdates := make(map[string]string, len(c.LastUpdates))
	for k, v := range c.LastUpdates {
		lastUpdates[k] = v
	}
	cp := &Config{
		CronSchedule:    c.CronSchedule,
		Timezone:        c.Timezone,
		RunOnStart:      c.RunOnStart,
		AutoUpdate:      c.AutoUpdate,
		CommandTimeout:  c.CommandTimeout,
		EnabledProjects: enabled,
		LastUpdates:     lastUpdates,
	}
	return cp
}

// ProjectEnabled reports whether a compose file participates in scheduled
// automatic updates (i.e. it is selected in the allowlist).
func (c *Config) ProjectEnabled(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.EnabledProjects[path]
}

// SetProjectEnabled adds or removes a compose file from the scheduled-update
// allowlist and persists the change.
func (c *Config) SetProjectEnabled(path string, enabled bool) error {
	c.mu.Lock()
	if enabled {
		c.EnabledProjects[path] = true
	} else {
		delete(c.EnabledProjects, path)
	}
	c.mu.Unlock()
	return c.save()
}

// LastUpdate returns the recorded last successful update time for a compose
// file, or "" when it has never been updated.
func (c *Config) LastUpdate(path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.LastUpdates[path]
}

// SetLastUpdate records the last successful update time for a compose file and
// persists it. It is a no-op when the timestamp is unchanged.
func (c *Config) SetLastUpdate(path, when string) error {
	if path == "" || when == "" {
		return nil
	}
	c.mu.Lock()
	if c.LastUpdates[path] == when {
		c.mu.Unlock()
		return nil
	}
	c.LastUpdates[path] = when
	c.mu.Unlock()
	return c.save()
}
