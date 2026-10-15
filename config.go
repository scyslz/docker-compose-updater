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
	RunOnStart     bool     `json:"run_on_start"`
	AutoUpdate     bool     `json:"auto_update"`
	CommandTimeout string   `json:"command_timeout"`
	// DisabledProjects maps an absolute compose file path to true when the user
	// has excluded it from automatic updates via the web UI.
	DisabledProjects map[string]bool `json:"disabled_projects"`

	mu   sync.Mutex
	path string
}

// loadConfig builds the effective configuration. Values come from, in order of
// precedence: the JSON file (if present), then environment variables, then
// built-in defaults. The returned Config remembers its path for saving.
func loadConfig() (*Config, error) {
	path := env("CONFIG_FILE", "/config/updater.json")

	cfg := &Config{
		ScanDepth:      5,
		CronSchedule:   "0 */6 * * *",
		Timezone:       "Asia/Shanghai",
		RunOnStart:     true,
		AutoUpdate:     true,
		CommandTimeout: "30m",
		path:           path,
	}

	// Seed from environment first (used when no file exists yet).
	if v := strings.TrimSpace(os.Getenv("INCLUDE_DIRS")); v != "" {
		cfg.IncludeDirs = splitList(v)
	} else if v := strings.TrimSpace(os.Getenv("SCAN_PATHS")); v != "" {
		cfg.IncludeDirs = splitList(v)
	} else {
		cfg.IncludeDirs = []string{"/stacks"}
	}
	cfg.ExcludeDirs = splitList(os.Getenv("EXCLUDE_DIRS"))
	if v := os.Getenv("SCAN_DEPTH"); v != "" {
		var d int
		if _, err := fmt.Sscanf(v, "%d", &d); err == nil && d >= 0 {
			cfg.ScanDepth = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("CRON_SCHEDULE")); v != "" {
		cfg.CronSchedule = v
	}
	if v := strings.TrimSpace(os.Getenv("TZ")); v != "" {
		cfg.Timezone = v
	}
	if v := strings.TrimSpace(os.Getenv("RUN_ON_START")); v != "" {
		cfg.RunOnStart = strings.EqualFold(v, "true")
	}
	if v := strings.TrimSpace(os.Getenv("AUTO_UPDATE")); v != "" {
		cfg.AutoUpdate = strings.EqualFold(v, "true")
	}
	if v := strings.TrimSpace(os.Getenv("COMMAND_TIMEOUT")); v != "" {
		cfg.CommandTimeout = v
	}

	// The file, when present, overrides the environment-derived defaults.
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
