package main

import (
	"encoding/json"
	"errors"
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
	// ImportedProjects lists compose files that were added explicitly through
	// the UI (or saved by the convert flow) even when no container exists yet.
	// They are shown as stopped projects until started.
	ImportedProjects []string `json:"imported_projects,omitempty"`
	// HiddenProjects lists stopped projects removed from the UI list. Nothing
	// on disk or in Docker is deleted; a project reappears once it is running.
	HiddenProjects []string `json:"hidden_projects,omitempty"`
	// ImageProxies lists registry prefix proxies (e.g.
	// https://gh-proxy.com/docker) available for image pulls. Any number may be
	// configured, but only the one named by ActiveProxy is used.
	ImageProxies []string `json:"image_proxies,omitempty"`
	// ActiveProxy is the image proxy currently used for pulls, or "" for
	// direct registry access. It always refers to an entry in ImageProxies.
	ActiveProxy string `json:"active_proxy,omitempty"`

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
	// Drop invalid or duplicate proxy entries from a hand-edited file and make
	// sure the active proxy still exists.
	proxies := make([]string, 0, len(c.ImageProxies))
	seen := make(map[string]bool, len(c.ImageProxies))
	for _, raw := range c.ImageProxies {
		normalized, err := normalizeProxyURL(raw)
		if err != nil || seen[normalized] {
			continue
		}
		seen[normalized] = true
		proxies = append(proxies, normalized)
	}
	c.ImageProxies = proxies
	if c.ActiveProxy != "" {
		if normalized, err := normalizeProxyURL(c.ActiveProxy); err == nil && seen[normalized] {
			c.ActiveProxy = normalized
		} else {
			c.ActiveProxy = ""
		}
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
		CronSchedule:     c.CronSchedule,
		Timezone:         c.Timezone,
		RunOnStart:       c.RunOnStart,
		AutoUpdate:       c.AutoUpdate,
		CommandTimeout:   c.CommandTimeout,
		EnabledProjects:  enabled,
		LastUpdates:      lastUpdates,
		ImportedProjects: append([]string(nil), c.ImportedProjects...),
		HiddenProjects:   append([]string(nil), c.HiddenProjects...),
		ImageProxies:     append([]string(nil), c.ImageProxies...),
		ActiveProxy:      c.ActiveProxy,
	}
	return cp
}

// ImageProxyList returns the configured image proxy URLs.
func (c *Config) ImageProxyList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ImageProxies...)
}

// ActiveImageProxy returns the image proxy used for pulls, or "" when pulls go
// straight to the registries.
func (c *Config) ActiveImageProxy() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ActiveProxy
}

// AddImageProxy validates and stores a new image proxy. The first proxy added
// becomes active automatically; existing entries are left untouched.
func (c *Config) AddImageProxy(raw string) error {
	proxy, err := normalizeProxyURL(raw)
	if err != nil {
		return err
	}
	c.mu.Lock()
	exists := false
	for _, p := range c.ImageProxies {
		if p == proxy {
			exists = true
			break
		}
	}
	changed := false
	if !exists {
		c.ImageProxies = append(c.ImageProxies, proxy)
		changed = true
	}
	if c.ActiveProxy == "" {
		c.ActiveProxy = proxy
		changed = true
	}
	c.mu.Unlock()
	if !changed {
		return nil
	}
	return c.save()
}

// RemoveImageProxy deletes a proxy and clears the active selection when it was
// the one in use.
func (c *Config) RemoveImageProxy(raw string) error {
	proxy, err := normalizeProxyURL(raw)
	if err != nil {
		return err
	}
	c.mu.Lock()
	kept := c.ImageProxies[:0]
	changed := false
	for _, p := range c.ImageProxies {
		if p == proxy {
			changed = true
			continue
		}
		kept = append(kept, p)
	}
	c.ImageProxies = kept
	if c.ActiveProxy == proxy {
		c.ActiveProxy = ""
		changed = true
	}
	c.mu.Unlock()
	if !changed {
		return nil
	}
	return c.save()
}

// SetActiveImageProxy selects the proxy used for pulls. An empty value disables
// proxying; any other value must be a configured proxy.
func (c *Config) SetActiveImageProxy(raw string) error {
	if strings.TrimSpace(raw) == "" {
		c.mu.Lock()
		changed := c.ActiveProxy != ""
		c.ActiveProxy = ""
		c.mu.Unlock()
		if !changed {
			return nil
		}
		return c.save()
	}
	proxy, err := normalizeProxyURL(raw)
	if err != nil {
		return err
	}
	c.mu.Lock()
	known := false
	for _, p := range c.ImageProxies {
		if p == proxy {
			known = true
			break
		}
	}
	if !known {
		c.mu.Unlock()
		return errors.New("unknown image proxy; add it first")
	}
	changed := c.ActiveProxy != proxy
	c.ActiveProxy = proxy
	c.mu.Unlock()
	if !changed {
		return nil
	}
	return c.save()
}

// ImportedFiles returns the explicitly imported compose-file paths.
func (c *Config) ImportedFiles() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ImportedProjects...)
}

// AddImportedProject records a compose file so it is listed even when it has
// no containers yet, and persists the change.
func (c *Config) AddImportedProject(file string) error {
	file = strings.TrimSpace(file)
	if file == "" {
		return nil
	}
	c.mu.Lock()
	for _, f := range c.ImportedProjects {
		if f == file {
			c.mu.Unlock()
			return nil
		}
	}
	c.ImportedProjects = append(c.ImportedProjects, file)
	// Importing a file again makes a previously hidden project visible.
	hidden := c.HiddenProjects[:0]
	for _, k := range c.HiddenProjects {
		if k != file {
			hidden = append(hidden, k)
		}
	}
	c.HiddenProjects = hidden
	c.mu.Unlock()
	return c.save()
}

// HiddenProjectKeys returns the keys of stopped projects removed from the list.
func (c *Config) HiddenProjectKeys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.HiddenProjects...)
}

// AddHiddenProject removes a stopped project from the UI list without touching
// its containers or files, and persists the change.
func (c *Config) AddHiddenProject(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	c.mu.Lock()
	for _, k := range c.HiddenProjects {
		if k == key {
			c.mu.Unlock()
			return nil
		}
	}
	c.HiddenProjects = append(c.HiddenProjects, key)
	c.mu.Unlock()
	return c.save()
}

// RemoveImportedProject drops a compose file from the imported list (without
// touching the file on disk) and persists the change.
func (c *Config) RemoveImportedProject(file string) error {
	c.mu.Lock()
	kept := c.ImportedProjects[:0]
	changed := false
	for _, f := range c.ImportedProjects {
		if f == file {
			changed = true
			continue
		}
		kept = append(kept, f)
	}
	c.ImportedProjects = kept
	c.mu.Unlock()
	if !changed {
		return nil
	}
	return c.save()
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
