package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var composeNames = map[string]bool{
	"compose.yml": true, "compose.yaml": true,
	"docker-compose.yml": true, "docker-compose.yaml": true,
}

type serviceConfig struct {
	Image string          `json:"image"`
	Build json.RawMessage `json:"build"`
}
type composeConfig struct {
	Services map[string]serviceConfig `json:"services"`
}

type updater struct {
	roots      []string
	excludes   []string
	depth      int
	autoUpdate bool
	timeout    time.Duration
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	// INCLUDE_DIRS is the preferred setting; SCAN_PATHS remains as a backwards-compatible alias.
	includeValue := strings.TrimSpace(os.Getenv("INCLUDE_DIRS"))
	if includeValue == "" {
		includeValue = env("SCAN_PATHS", "/stacks")
	}
	roots := splitList(includeValue)
	excludes := splitList(os.Getenv("EXCLUDE_DIRS"))
	if len(roots) == 0 {
		log.Fatal("INCLUDE_DIRS/SCAN_PATHS must contain at least one directory")
	}
	depth := 5
	if v := os.Getenv("SCAN_DEPTH"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &depth); err != nil || depth < 0 {
			log.Fatalf("invalid SCAN_DEPTH %q", v)
		}
	}
	timeout := 30 * time.Minute
	if v := os.Getenv("COMMAND_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("invalid COMMAND_TIMEOUT: %v", err)
		}
		timeout = d
	}
	u := &updater{roots: roots, excludes: excludes, depth: depth, autoUpdate: strings.EqualFold(env("AUTO_UPDATE", "true"), "true"), timeout: timeout}
	zone := env("TZ", "Asia/Shanghai")
	loc, err := time.LoadLocation(zone)
	if err != nil {
		log.Fatalf("invalid TZ %q: %v", zone, err)
	}
	cronExpr := env("CRON_SCHEDULE", "0 4 * * *")
	schedule, err := parseCron(cronExpr)
	if err != nil {
		log.Fatalf("invalid CRON_SCHEDULE %q: %v", cronExpr, err)
	}
	log.Printf("compose-auto-updater started; include_dirs=%q exclude_dirs=%q depth=%d auto_update=%t cron=%q TZ=%s", roots, excludes, depth, u.autoUpdate, cronExpr, zone)
	if strings.EqualFold(env("RUN_ON_START", "true"), "true") {
		u.run()
	}
	for {
		now := time.Now().In(loc)
		next := schedule.Next(now)
		if next.IsZero() {
			log.Fatal("could not find next run time for CRON_SCHEDULE")
		}
		delay := time.Until(next)
		log.Printf("next scan at %s (in %s)", next.Format(time.RFC3339), delay.Round(time.Second))
		t := time.NewTimer(delay)
		<-t.C
		u.run()
	}
}

func (u *updater) run() {
	started := time.Now()
	files := u.discover()
	log.Printf("scan started: found %d Compose file(s)", len(files))
	for _, file := range files {
		u.process(file)
	}
	log.Printf("scan finished in %s", time.Since(started).Round(time.Second))
}

func (u *updater) discover() []string {
	seen := map[string]bool{}
	var files []string
	for _, root := range u.roots {
		root = filepath.Clean(root)
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			log.Printf("skip scan root %q: not accessible directory", root)
			continue
		}
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				log.Printf("walk %s: %v", path, err)
				return nil
			}
			if d.IsDir() {
				if path != root && u.depth >= 0 {
					rel, _ := filepath.Rel(root, path)
					level := strings.Count(rel, string(filepath.Separator)) + 1
					if level > u.depth {
						return filepath.SkipDir
					}
				}
				if u.excluded(path) {
					return filepath.SkipDir
				}
				return nil
			}
			if composeNames[strings.ToLower(d.Name())] {
				abs, e := filepath.Abs(path)
				if e == nil && !seen[abs] {
					seen[abs] = true
					files = append(files, abs)
				}
			}
			return nil
		})
		if err != nil {
			log.Printf("scan %s: %v", root, err)
		}
	}
	sort.Strings(files)
	return files
}

func (u *updater) excluded(path string) bool {
	p, _ := filepath.Abs(path)
	for _, x := range u.excludes {
		x, _ = filepath.Abs(filepath.Clean(x))
		rel, err := filepath.Rel(x, p)
		if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
			return true
		}
	}
	return false
}

func (u *updater) process(file string) {
	ctx, cancel := context.WithTimeout(context.Background(), u.timeout)
	defer cancel()
	log.Printf("project: %s", file)
	configOut, err := u.command(ctx, filepath.Dir(file), "compose", "-f", file, "config", "--format", "json")
	if err != nil {
		log.Printf("skip %s: compose config failed: %v", file, err)
		return
	}
	var cfg composeConfig
	if err := json.Unmarshal(configOut, &cfg); err != nil {
		log.Printf("skip %s: cannot parse compose config: %v", file, err)
		return
	}
	psOut, err := u.command(ctx, filepath.Dir(file), "compose", "-f", file, "ps", "--services", "--status", "running")
	if err != nil {
		log.Printf("skip %s: cannot list running services: %v", file, err)
		return
	}
	running := map[string]bool{}
	for _, s := range strings.Split(string(psOut), "\n") {
		s = strings.TrimSpace(s)
		if s != "" {
			running[s] = true
		}
	}
	if len(running) == 0 {
		log.Printf("skip %s: no running Compose services", file)
		return
	}
	var eligible []string
	for name, svc := range cfg.Services {
		if !running[name] || strings.TrimSpace(svc.Image) == "" || hasBuild(svc.Build) {
			continue
		}
		eligible = append(eligible, name)
	}
	sort.Strings(eligible)
	if len(eligible) == 0 {
		log.Printf("skip %s: no running image-only services eligible for update", file)
		return
	}
	log.Printf("%s: eligible running services: %s", file, strings.Join(eligible, ", "))
	args := []string{"compose", "-f", file, "pull"}
	args = append(args, eligible...)
	if _, err := u.command(ctx, filepath.Dir(file), args...); err != nil {
		log.Printf("pull failed for %s: %v", file, err)
		return
	}
	if !u.autoUpdate {
		log.Printf("AUTO_UPDATE=false: pulled images only; not recreating %s", file)
		return
	}
	args = []string{"compose", "-f", file, "up", "-d", "--no-deps", "--no-build", "--wait"}
	args = append(args, eligible...)
	if out, err := u.command(ctx, filepath.Dir(file), args...); err != nil {
		log.Printf("UPDATE FAILED for %s: %v\n%s", file, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("updated successfully: %s", file)
}

func hasBuild(raw json.RawMessage) bool {
	v := strings.TrimSpace(string(raw))
	return v != "" && v != "null" && v != `""` && v != "{}"
}

func (u *updater) command(parent context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, u.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.Bytes(), ctx.Err()
	}
	if err != nil {
		return out.Bytes(), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

func splitList(s string) []string {
	var out []string
	s = strings.ReplaceAll(s, ";", ",")
	s = strings.ReplaceAll(s, "\n", ",")
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, filepath.Clean(v))
		}
	}
	return out
}
func env(k, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return fallback
}
