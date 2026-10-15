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
	compose    composeCommand
}

// composeCommand describes how to invoke Compose on this host. It supports
// both the Docker Compose v2 plugin (`docker compose`) and the legacy v1
// standalone binary (`docker-compose`).
type composeCommand struct {
	bin      string   // "docker" or "docker-compose"
	plugin   []string // extra args before the subcommand, e.g. ["compose"] for the v2 plugin
	v2       bool     // true when the v2 plugin is used
	supports bool     // whether `up --wait` is supported
	display  string   // human-readable form for logs
}

// args builds an argv for the given compose subcommand and arguments, inserting
// the plugin prefix when the v2 plugin is in use.
func (c composeCommand) args(sub string, rest ...string) []string {
	argv := []string{c.bin}
	argv = append(argv, c.plugin...)
	argv = append(argv, sub)
	argv = append(argv, rest...)
	return argv
}

// withFile builds an argv with the global `-f <file>` option placed before the
// subcommand, which Compose requires for global options.
func (c composeCommand) withFile(file, sub string, rest ...string) []string {
	argv := []string{c.bin}
	argv = append(argv, c.plugin...)
	argv = append(argv, "-f", file, sub)
	argv = append(argv, rest...)
	return argv
}

// detectCompose picks the best available Compose implementation. PREFER_COMPOSE
// may force "v2" or "v1". It returns an error when neither is usable.
func detectCompose(ctx context.Context) (composeCommand, error) {
	prefer := strings.ToLower(strings.TrimSpace(os.Getenv("PREFER_COMPOSE")))

	tryV2 := func() (composeCommand, bool) {
		if _, err := exec.LookPath("docker"); err != nil {
			return composeCommand{}, false
		}
		c := composeCommand{bin: "docker", plugin: []string{"compose"}, v2: true, display: "docker compose"}
		probe := exec.CommandContext(ctx, "docker", "compose", "version")
		if err := probe.Run(); err != nil {
			return composeCommand{}, false
		}
		return c, true
	}
	tryV1 := func() (composeCommand, bool) {
		if _, err := exec.LookPath("docker-compose"); err != nil {
			return composeCommand{}, false
		}
		c := composeCommand{bin: "docker-compose", v2: false, display: "docker-compose"}
		probe := exec.CommandContext(ctx, "docker-compose", "version")
		if err := probe.Run(); err != nil {
			return composeCommand{}, false
		}
		return c, true
	}

	order := []func() (composeCommand, bool){tryV2, tryV1}
	switch prefer {
	case "v2":
		order = []func() (composeCommand, bool){tryV2}
	case "v1":
		order = []func() (composeCommand, bool){tryV1}
	}
	for _, f := range order {
		if c, ok := f(); ok {
			c.supports = c.v2 // v1 (<=1.x) has no `up --wait`
			return c, nil
		}
	}
	return composeCommand{}, fmt.Errorf("no usable Compose found (need `docker compose` v2 or `docker-compose` v1)")
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
	cc, err := detectCompose(context.Background())
	if err != nil {
		log.Fatalf("compose not available: %v", err)
	}
	u.compose = cc
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
	log.Printf("compose-auto-updater started; compose=%s wait=%t include_dirs=%q exclude_dirs=%q depth=%d auto_update=%t cron=%q TZ=%s", cc.display, cc.supports, roots, excludes, depth, u.autoUpdate, cronExpr, zone)
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
	configOut, err := u.runCmd(ctx, filepath.Dir(file), u.compose.withFile(file, "config", "--format", "json"))
	if err != nil {
		log.Printf("skip %s: compose config failed: %v", file, err)
		return
	}
	var cfg composeConfig
	if err := json.Unmarshal(configOut, &cfg); err != nil {
		log.Printf("skip %s: cannot parse compose config: %v", file, err)
		return
	}
	psOut, err := u.runCmd(ctx, filepath.Dir(file), u.compose.withFile(file, "ps", "--services", "--status", "running"))
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

	// Record the image ID each running container currently uses, so we only
	// recreate the services whose image actually changed after pulling.
	before, err := u.containerImageIDs(ctx, filepath.Dir(file), file, eligible)
	if err != nil {
		log.Printf("skip %s: cannot read current image IDs: %v", file, err)
		return
	}

	if _, err := u.runCmd(ctx, filepath.Dir(file), u.compose.withFile(file, "pull", eligible...)); err != nil {
		log.Printf("pull failed for %s: %v", file, err)
		return
	}

	changed := u.changedServices(ctx, filepath.Dir(file), eligible, cfg, before)
	if len(changed) == 0 {
		log.Printf("%s: images unchanged, nothing to recreate", file)
		return
	}
	log.Printf("%s: image updated for: %s", file, strings.Join(changed, ", "))
	if !u.autoUpdate {
		log.Printf("AUTO_UPDATE=false: pulled images only; not recreating %s", file)
		return
	}
	upArgs := []string{"-d", "--no-deps", "--no-build"}
	if u.compose.supports {
		upArgs = append(upArgs, "--wait")
	}
	upArgs = append(upArgs, changed...)
	if out, err := u.runCmd(ctx, filepath.Dir(file), u.compose.withFile(file, "up", upArgs...)); err != nil {
		log.Printf("UPDATE FAILED for %s: %v\n%s", file, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("updated successfully: %s", file)
}

// containerImageIDs returns, for each running service, the image ID of its
// running container, keyed by compose service name. It uses the compose service
// label so the mapping is exact even when image references include digests.
func (u *updater) containerImageIDs(ctx context.Context, dir, file string, services []string) (map[string]string, error) {
	out, err := u.runCmd(ctx, dir, u.compose.withFile(file, "ps", append([]string{"-q"}, services...)...))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	result := map[string]string{}
	if len(ids) == 0 {
		return result, nil
	}
	inspectArgs := append([]string{"inspect", "--format",
		`{{.Config.Image}} {{index .Config.Labels "com.docker.compose.service"}} {{.Image}}`}, ids...)
	out, err = u.runDocker(ctx, dir, inspectArgs)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 3 {
			result[fields[1]] = normalizeImageID(fields[2])
		}
	}
	return result, nil
}

// changedServices compares each service's currently running container image ID
// against the ID that the service's configured image reference now resolves to
// locally (after pulling), returning the services whose image changed.
func (u *updater) changedServices(ctx context.Context, dir string, services []string, cfg composeConfig, before map[string]string) []string {
	var changed []string
	for _, name := range services {
		oldID := before[name]
		if oldID == "" {
			// No running container image recorded; be conservative and recreate.
			changed = append(changed, name)
			continue
		}
		ref := strings.TrimSpace(cfg.Services[name].Image)
		nowID, err := u.resolveImageID(ctx, dir, ref)
		if err != nil {
			log.Printf("cannot resolve image %q for %s, recreating to be safe: %v", ref, name, err)
			changed = append(changed, name)
			continue
		}
		if nowID != "" && nowID != oldID {
			changed = append(changed, name)
		}
	}
	return changed
}

// resolveImageID returns the local image ID (without the "sha256:" prefix) that
// the given image reference currently resolves to.
func (u *updater) resolveImageID(ctx context.Context, dir, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("empty image reference")
	}
	out, err := u.runDocker(ctx, dir, []string{"image", "inspect", "--format", "{{.Id}}", ref})
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if id == "" {
		return "", fmt.Errorf("no local image for reference %q", ref)
	}
	return normalizeImageID(id), nil
}

// normalizeImageID strips the "sha256:" prefix so IDs from `docker inspect`
// (which include it) compare equal to IDs from `compose images -q` (which do not).
func normalizeImageID(id string) string {
	id = strings.TrimSpace(id)
	return strings.TrimPrefix(id, "sha256:")
}

// runDocker runs a raw `docker` command (not a compose subcommand).
func (u *updater) runDocker(parent context.Context, dir string, args []string) ([]byte, error) {
	argv := append([]string{"docker"}, args...)
	return u.runCmd(parent, dir, argv)
}

func hasBuild(raw json.RawMessage) bool {
	v := strings.TrimSpace(string(raw))
	return v != "" && v != "null" && v != `""` && v != "{}"
}

func (u *updater) runCmd(parent context.Context, dir string, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, u.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.Bytes(), ctx.Err()
	}
	if err != nil {
		return out.Bytes(), fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(out.String()))
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
