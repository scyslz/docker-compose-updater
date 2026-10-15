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
		CronSchedule:    "0 */6 * * *",
		Timezone:        "UTC",
		RunOnStart:      boolPtr(false),
		AutoUpdate:      boolPtr(true),
		CommandTimeout:  "15m",
		EnabledProjects: map[string]bool{"/x/compose.yaml": true},
		path:            path,
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
	if loaded.ProjectEnabled("/x/other.yaml") {
		t.Fatal("unlisted project should not be enabled")
	}
	if !loaded.ProjectEnabled("/x/compose.yaml") {
		t.Fatal("listed project should be enabled")
	}
}

func TestConfigNormalize(t *testing.T) {
	c := &Config{}
	c.normalize()
	if c.CronSchedule != "0 */6 * * *" {
		t.Fatalf("cron default = %q", c.CronSchedule)
	}
	if c.Timezone != "Asia/Shanghai" {
		t.Fatalf("tz default = %q", c.Timezone)
	}
	if c.EnabledProjects == nil {
		t.Fatal("enabled map should be initialized")
	}
	if !c.RunOnStartEnabled() || !c.AutoUpdateEnabled() {
		t.Fatal("fresh config should default run_on_start and auto_update to true")
	}
}

func TestSetProjectEnabledPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "updater.json")
	c := &Config{path: path}
	c.normalize()
	if err := c.SetProjectEnabled("/a/compose.yaml", true); err != nil {
		t.Fatal(err)
	}
	if !c.ProjectEnabled("/a/compose.yaml") {
		t.Fatal("project should be enabled")
	}
	if err := c.SetProjectEnabled("/a/compose.yaml", false); err != nil {
		t.Fatal(err)
	}
	if c.ProjectEnabled("/a/compose.yaml") {
		t.Fatal("project should be disabled")
	}
}

func TestLoadConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "updater.json")
	os.WriteFile(path, []byte(`{"cron_schedule":"0 1 * * *","timezone":"UTC","auto_update":false,"enabled_projects":{"/a/compose.yaml":true}}`), 0o644)

	t.Setenv("CONFIG_FILE", path)
	t.Setenv("INCLUDE_DIRS", "/ignored")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CronSchedule != "0 1 * * *" || cfg.Timezone != "UTC" {
		t.Fatalf("unexpected cron/tz: %q %q", cfg.CronSchedule, cfg.Timezone)
	}
	if cfg.AutoUpdateEnabled() {
		t.Fatal("auto_update should be false from file")
	}
	if !cfg.ProjectEnabled("/a/compose.yaml") {
		t.Fatal("enabled project should be loaded")
	}
}
