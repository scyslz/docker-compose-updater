package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveComposeFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := resolveComposeFile(dir); err == nil {
		t.Fatal("expected error for folder without a compose file")
	}
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveComposeFile(dir); err != nil || got != file {
		t.Errorf("resolveComposeFile(dir) = %q, %v; want %q", got, err, file)
	}
	if got, err := resolveComposeFile(file); err != nil || got != file {
		t.Errorf("resolveComposeFile(file) = %q, %v; want %q", got, err, file)
	}
	if _, err := resolveComposeFile("relative/path"); err == nil {
		t.Error("expected error for relative path")
	}
}

func TestFindComposeFilesDepth(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) string {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "compose.yml")
		if err := os.WriteFile(file, []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return file
	}
	near := mk("stacks")
	deep := mk("a/b/c") // three levels below root: still included
	tooDeep := mk("a/b/c/d/e")

	found, err := findComposeFiles(root, 3)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, f := range found {
		set[f] = true
	}
	if !set[near] || !set[deep] {
		t.Errorf("findComposeFiles missing expected files: %v", found)
	}
	if set[tooDeep] {
		t.Errorf("findComposeFiles included a file deeper than 3 levels: %v", found)
	}
}

func TestDerivedProjectName(t *testing.T) {
	cases := []struct{ dir, want string }{
		{"/home/hq/docker-data/My.App", "myapp"},
		{"/srv/-weird", "p-weird"},
		{"/", "compose"},
	}
	for _, c := range cases {
		if got := derivedProjectName(c.dir); got != c.want {
			t.Errorf("derivedProjectName(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

func TestAddImportedProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updater.json")
	t.Setenv("CONFIG_FILE", path)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	file := "/home/hq/docker-data/example/compose.yaml"
	if err := cfg.AddImportedProject(file); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	if err := cfg.AddImportedProject(file); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored Config
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.ImportedProjects) != 1 || stored.ImportedProjects[0] != file {
		t.Errorf("stored imports = %v, want [%s]", stored.ImportedProjects, file)
	}
	if got := cfg.ImportedFiles(); len(got) != 1 || got[0] != file {
		t.Errorf("ImportedFiles = %v", got)
	}
	if err := cfg.RemoveImportedProject(file); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ImportedFiles(); len(got) != 0 {
		t.Errorf("ImportedFiles after remove = %v, want empty", got)
	}

	if err := cfg.AddHiddenProject(file); err != nil {
		t.Fatal(err)
	}
	if got := cfg.HiddenProjectKeys(); len(got) != 1 || got[0] != file {
		t.Errorf("HiddenProjects = %v", got)
	}
	// Importing the file again unhides it.
	if err := cfg.AddImportedProject(file); err != nil {
		t.Fatal(err)
	}
	if got := cfg.HiddenProjectKeys(); len(got) != 0 {
		t.Errorf("HiddenProjects after re-import = %v, want empty", got)
	}
}
