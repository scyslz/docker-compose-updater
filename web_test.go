package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestActionArgs(t *testing.T) {
	tests := []struct {
		name     string
		action   string
		wait     bool
		wantSub  string
		wantArgs []string
		wantOK   bool
	}{
		{name: "start", action: "start", wantSub: "start", wantOK: true},
		{name: "stop", action: "stop", wantSub: "stop", wantOK: true},
		{name: "restart", action: "restart", wantSub: "restart", wantOK: true},
		{
			name: "recreate with wait", action: "recreate", wait: true, wantSub: "up",
			wantArgs: []string{"-d", "--no-build", "--force-recreate", "--wait"}, wantOK: true,
		},
		{
			name: "recreate without wait", action: "recreate", wantSub: "up",
			wantArgs: []string{"-d", "--no-build", "--force-recreate"}, wantOK: true,
		},
		{name: "unsupported", action: "down"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, args, ok := actionArgs(tt.action, tt.wait)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if sub != tt.wantSub {
				t.Fatalf("sub = %q, want %q", sub, tt.wantSub)
			}
			if !slices.Equal(args, tt.wantArgs) {
				t.Fatalf("args = %v, want %v", args, tt.wantArgs)
			}
		})
	}
}

func TestWriteComposeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	old := []byte("services: {}\n# old\n")
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}

	backup, err := writeComposeFile(path, "services:\n  web:\n    image: nginx\n")
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "services:\n  web:\n    image: nginx\n" {
		t.Fatalf("file content = %q", got)
	}
	bak, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(bak) != string(old) {
		t.Fatalf("backup content = %q, want %q", bak, old)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteComposeFileFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	link := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := writeComposeFile(link, "new\n"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("writing through a symlink replaced the link instead of its target")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new\n" {
		t.Fatalf("target content = %q", got)
	}
}

func TestLastNonEmptyLine(t *testing.T) {
	if got := lastNonEmptyLine([]byte("a\n\n b \n")); got != "b" {
		t.Fatalf("lastNonEmptyLine = %q, want b", got)
	}
	if got := lastNonEmptyLine(nil); got != "" {
		t.Fatalf("lastNonEmptyLine(nil) = %q, want empty", got)
	}
}
