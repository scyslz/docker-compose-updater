package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeProxyURL(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "https://gh-proxy.com/docker", want: "https://gh-proxy.com/docker"},
		{in: "https://gh-proxy.com/docker/", want: "https://gh-proxy.com/docker"},
		{in: "  gh-proxy.com/docker  ", want: "https://gh-proxy.com/docker"},
		{in: "http://mirror.local:5000/proxy", want: "http://mirror.local:5000/proxy"},
		{in: "", wantErr: true},
		{in: "https://gh-proxy.com/docker?x=1", wantErr: true},
		{in: "ftp://gh-proxy.com/docker", wantErr: true},
		{in: "https://", wantErr: true},
	}
	for _, tt := range tests {
		got, err := normalizeProxyURL(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("normalizeProxyURL(%q) = %q, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("normalizeProxyURL(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("normalizeProxyURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestProxiedImageRef(t *testing.T) {
	const proxy = "https://gh-proxy.com/docker"
	tests := []struct {
		image string
		want  string
	}{
		{"nginx:latest", proxy + "/nginx:latest"},
		{"nginx", proxy + "/nginx"},
		{"library/redis:7", proxy + "/redis:7"},
		{"docker.io/library/nginx:1.25", proxy + "/nginx:1.25"},
		{"index.docker.io/library/alpine:3.20", proxy + "/alpine:3.20"},
		{"adguard/adguardhome:latest", proxy + "/adguard/adguardhome:latest"},
		{"ghcr.io/owner/app:v1", proxy + "/ghcr.io/owner/app:v1"},
		{"quay.io/calico/cni:v3", proxy + "/quay.io/calico/cni:v3"},
		// Digest-pinned images cannot be re-tagged and stay direct.
		{"nginx@sha256:abc123", "nginx@sha256:abc123"},
		// Already accelerated references are left alone.
		{"gh-proxy.com/docker/nginx:latest", "gh-proxy.com/docker/nginx:latest"},
	}
	for _, tt := range tests {
		if got := proxiedImageRef(proxy, tt.image); got != tt.want {
			t.Errorf("proxiedImageRef(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
	if got := proxiedImageRef("", "nginx:latest"); got != "nginx:latest" {
		t.Errorf("empty proxy changed image to %q", got)
	}
	if got := proxiedImageRef(proxy+"/", "nginx:latest"); got != proxy+"/nginx:latest" {
		t.Errorf("trailing slash proxy gave %q", got)
	}
}

func TestProjectImages(t *testing.T) {
	cfg := composeConfig{Services: map[string]serviceConfig{
		"web":    {Image: "nginx:alpine"},
		"api":    {Image: "ghcr.io/x/api:1"},
		"dup":    {Image: "nginx:alpine"},
		"built":  {Image: "local/app:dev", Build: json.RawMessage(`{"context":"."}`)},
		"build2": {Build: json.RawMessage(`"."`)},
	}}
	got := strings.Join(projectImages(cfg), ",")
	want := "ghcr.io/x/api:1,nginx:alpine"
	if got != want {
		t.Fatalf("projectImages = %q, want %q", got, want)
	}
}

func TestImageProxyPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updater.json")
	cfg := &Config{path: path}
	cfg.normalize()

	if err := cfg.AddImageProxy("gh-proxy.com/docker/"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ActiveImageProxy(); got != "https://gh-proxy.com/docker" {
		t.Fatalf("first proxy should become active, got %q", got)
	}
	if err := cfg.AddImageProxy("https://mirror.example.com/docker"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ActiveImageProxy(); got != "https://gh-proxy.com/docker" {
		t.Fatalf("adding a second proxy changed the active one to %q", got)
	}
	if err := cfg.SetActiveImageProxy("https://mirror.example.com/docker"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ActiveImageProxy(); got != "https://mirror.example.com/docker" {
		t.Fatalf("active = %q", got)
	}
	if err := cfg.SetActiveImageProxy("https://unknown.example.com/docker"); err == nil {
		t.Fatal("activating an unknown proxy should fail")
	}

	// The selection survives a reload.
	loaded, err := loadConfigFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ActiveImageProxy(); got != "https://mirror.example.com/docker" {
		t.Fatalf("reloaded active = %q", got)
	}
	if len(loaded.ImageProxyList()) != 2 {
		t.Fatalf("reloaded proxies = %v", loaded.ImageProxyList())
	}

	// Removing the active proxy falls back to direct pulls.
	if err := loaded.RemoveImageProxy("https://mirror.example.com/docker"); err != nil {
		t.Fatal(err)
	}
	if got := loaded.ActiveImageProxy(); got != "" {
		t.Fatalf("active after removal = %q, want empty", got)
	}
	if err := loaded.SetActiveImageProxy(""); err != nil {
		t.Fatal(err)
	}
}

// loadConfigFrom loads a specific config file path, mirroring loadConfig but
// without the CONFIG_FILE environment.
func loadConfigFrom(path string) (*Config, error) {
	cfg := &Config{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	cfg.path = path
	cfg.normalize()
	return cfg, nil
}

func TestNormalizeDropsInvalidProxies(t *testing.T) {
	cfg := &Config{
		ImageProxies: []string{"gh-proxy.com/docker/", "not a url", "https://gh-proxy.com/docker"},
		ActiveProxy:  "https://removed.example.com/docker",
	}
	cfg.normalize()
	list := cfg.ImageProxyList()
	if len(list) != 1 || list[0] != "https://gh-proxy.com/docker" {
		t.Fatalf("normalized proxies = %v", list)
	}
	if got := cfg.ActiveImageProxy(); got != "" {
		t.Fatalf("active proxy = %q, want empty for a missing entry", got)
	}
}

func TestCancelTask(t *testing.T) {
	cfg := &Config{path: filepath.Join(t.TempDir(), "updater.json")}
	cfg.normalize()
	u := &updater{cfg: cfg}
	p := composeProject{Project: "demo", File: "/srv/demo/compose.yaml"}

	if _, ok := u.startLogDetail(p, "manual", "check", "checking…"); !ok {
		t.Fatal("startLogDetail refused the first run")
	}
	ctx := u.taskContext(p.File)
	select {
	case <-ctx.Done():
		t.Fatal("task context was cancelled before the request")
	default:
	}

	if !u.cancelTask(p.File) {
		t.Fatal("cancelTask reported nothing running")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancelling did not cancel the task context")
	}

	u.mu.Lock()
	logStatus := u.logs[0].Status
	resultStatus := u.lastResults[0].Status
	u.mu.Unlock()
	if logStatus != "cancelling" || resultStatus != "cancelling" {
		t.Fatalf("statuses after cancel = %q/%q, want cancelling", logStatus, resultStatus)
	}

	// The runner finishes with a cancelled result; finishLog replaces the
	// entry, records a duration, and releases the task.
	time.Sleep(5 * time.Millisecond)
	u.finishLog(projectResult{
		File: p.File, Project: p.Project, Trigger: "manual",
		Status: "cancelled", Detail: "cancelled by user",
		When: time.Now().Add(-5 * time.Millisecond).Format(time.RFC3339),
	})
	u.mu.Lock()
	entry := u.logs[0]
	_, stillRunning := u.tasks[p.File]
	u.mu.Unlock()
	if entry.Status != "cancelled" {
		t.Fatalf("finished status = %q", entry.Status)
	}
	if entry.DurationMs <= 0 {
		t.Fatalf("duration = %dms, want > 0", entry.DurationMs)
	}
	if stillRunning {
		t.Fatal("task was not released after finishing")
	}
	if u.cancelTask(p.File) {
		t.Fatal("cancelTask found a task after it finished")
	}
}
