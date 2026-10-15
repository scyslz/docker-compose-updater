package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "updater.json")

	cfg := &Config{
		IncludeDirs:      []string{"/data/docker", "/opt/stacks"},
		ExcludeDirs:      []string{"/data/docker/archive"},
		ScanDepth:        4,
		CronSchedule:     "0 */6 * * *",
		Timezone:         "UTC",
		RunOnStart:       boolPtr(false),
		AutoUpdate:       boolPtr(true),
		CommandTimeout:   "15m",
		DisabledProjects: map[string]bool{"/x/compose.yaml": true},
		path:             path,
	}
	if err := cfg.save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty config file")
	}

	var loaded Config
	loaded.path = path
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	if !loaded.ProjectEnabled("/x/other.yaml") {
		t.Fatal("default project should be enabled")
	}
	if loaded.ProjectEnabled("/x/compose.yaml") {
		t.Fatal("disabled project should be disabled")
	}
}

func TestConfigNormalize(t *testing.T) {
	c := &Config{}
	c.normalize()
	if len(c.IncludeDirs) != 1 || c.IncludeDirs[0] != "/stacks" {
		t.Fatalf("include default = %v", c.IncludeDirs)
	}
	if c.CronSchedule != "0 */6 * * *" {
		t.Fatalf("cron default = %q", c.CronSchedule)
	}
	if c.DisabledProjects == nil {
		t.Fatal("disabled map should be initialized")
	}
	// Fresh config (nil bools) must default to enabled behavior.
	if !c.RunOnStartEnabled() || !c.AutoUpdateEnabled() {
		t.Fatal("fresh config should default run_on_start and auto_update to true")
	}
}

func TestLoadConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "updater.json")
	os.WriteFile(path, []byte(`{"include_dirs":["/a"],"cron_schedule":"0 1 * * *","timezone":"UTC","auto_update":false}`), 0o644)

	t.Setenv("CONFIG_FILE", path)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.IncludeDirs) != 1 || cfg.IncludeDirs[0] != "/a" {
		t.Fatalf("unexpected include dirs: %v", cfg.IncludeDirs)
	}
	if cfg.CronSchedule != "0 1 * * *" || cfg.Timezone != "UTC" {
		t.Fatalf("unexpected cron/tz: %q %q", cfg.CronSchedule, cfg.Timezone)
	}
	if cfg.AutoUpdateEnabled() {
		t.Fatal("auto_update should be false from file")
	}
}
