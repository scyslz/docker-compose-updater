package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type serviceConfig struct {
	Image string          `json:"image"`
	Build json.RawMessage `json:"build"`
}
type composeConfig struct {
	Services map[string]serviceConfig `json:"services"`
}

type updater struct {
	cfg     *Config
	compose composeCommand

	mu          sync.Mutex
	running     bool
	lastRun     time.Time
	nextRun     time.Time
	lastResults []projectResult
	logs        []checkLog
}

// projectResult is the outcome of processing one compose file, surfaced to the
// web UI.
type projectResult struct {
	File     string   `json:"file"`
	Project  string   `json:"project,omitempty"`
	Enabled  bool     `json:"enabled"`
	Status   string   `json:"status"` // updated, unchanged, skipped, unmounted, pulled, failed
	Detail   string   `json:"detail"`
	Services []string `json:"services,omitempty"`
	Updated  []string `json:"updated,omitempty"`
	When     string   `json:"when"`
	Trigger  string   `json:"trigger,omitempty"` // "manual" or "cron"
	logged   bool     // internal: whether this result should appear in the check log
}

// checkLog is a single historical check event shown in the web UI.
type checkLog struct {
	When    string `json:"when"`
	Trigger string `json:"trigger"` // "manual" or "cron"
	Project string `json:"project"`
	File    string `json:"file"`
	Status  string `json:"status"` // running, updated, unchanged, skipped, unmounted, pulled, failed
	Detail  string `json:"detail,omitempty"`
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

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cc, err := detectCompose(context.Background())
	if err != nil {
		log.Fatalf("compose not available: %v", err)
	}
	u := &updater{cfg: cfg, compose: cc}
	log.Printf("compose-auto-updater started; compose=%s wait=%t auto_update=%t cron=%q tz=%s config=%s",
		cc.display, cc.supports, cfg.AutoUpdateEnabled(), cfg.CronSchedule, cfg.Timezone, cfg.path)

	// Optional web management UI; disabled with WEB_ADDR=off.
	go startWeb(u, env("WEB_ADDR", ":8080"))

	if cfg.RunOnStartEnabled() {
		go u.run()
	}

	// Scheduler: recompute the next run each cycle so config changes (cron,
	// timezone) take effect without a restart.
	timer := time.NewTimer(time.Hour)
	for {
		schedule, err := u.schedule()
		if err != nil {
			log.Printf("invalid schedule, retrying in 1m: %v", err)
			timer.Reset(time.Minute)
			<-timer.C
			continue
		}
		now := time.Now().In(schedule.loc)
		next := schedule.Next(now)
		if next.IsZero() {
			log.Printf("could not find next run time, retrying in 1m")
			timer.Reset(time.Minute)
			<-timer.C
			continue
		}
		u.setNextRun(next)
		delay := time.Until(next)
		log.Printf("next scan at %s (in %s)", next.Format(time.RFC3339), delay.Round(time.Second))
		timer.Reset(delay)
		<-timer.C
		u.run()
	}
}

// namedSchedule wraps a parsed cron with the location it should be evaluated in.
type namedSchedule struct {
	cron *cronSchedule
	loc  *time.Location
}

// schedule parses the current configuration into a runnable schedule.
func (u *updater) schedule() (*namedSchedule, error) {
	u.cfg.mu.Lock()
	cronExpr := u.cfg.CronSchedule
	zone := u.cfg.Timezone
	u.cfg.mu.Unlock()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q: %w", zone, err)
	}
	cron, err := parseCron(cronExpr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron %q: %w", cronExpr, err)
	}
	return &namedSchedule{cron: cron, loc: loc}, nil
}

func (s *namedSchedule) Next(after time.Time) time.Time {
	return s.cron.Next(after.In(s.loc))
}

func (u *updater) setNextRun(t time.Time) {
	u.mu.Lock()
	u.nextRun = t
	u.mu.Unlock()
}

func (u *updater) run() {
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		log.Printf("scan already running; skipping this trigger")
		return
	}
	u.running = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.running = false
		u.lastRun = time.Now()
		u.mu.Unlock()
	}()

	started := time.Now()
	projects, err := u.discoverRunningProjects(context.Background())
	if err != nil {
		log.Printf("scan: cannot list running projects: %v", err)
		return
	}
	log.Printf("scan started: found %d running Compose project(s)", len(projects))
	results := make([]projectResult, 0, len(projects))
	for _, p := range projects {
		// Projects not selected for scheduled updates, or not mounted (so their
		// compose file cannot be read), are skipped without a check result: they
		// keep no "last check" and produce no log entry.
		if !u.cfg.ProjectEnabled(p.Key()) || !p.Mounted {
			continue
		}
		if !u.startLog(p, "cron") {
			continue
		}
		res := u.processProject(p, false, "cron")
		u.finishLog(res)
		results = append(results, res)
	}
	u.mu.Lock()
	u.lastResults = results
	u.mu.Unlock()
	log.Printf("scan finished in %s", time.Since(started).Round(time.Second))
}

// projectResultNow builds a timestamped result for the current moment.
func nowResult(p composeProject, enabled bool) projectResult {
	return projectResult{File: p.File, Project: p.Project, Enabled: enabled, When: time.Now().Format(time.RFC3339)}
}

// timeout returns the configured per-command timeout.
func (u *updater) timeout() time.Duration {
	u.cfg.mu.Lock()
	s := u.cfg.CommandTimeout
	u.cfg.mu.Unlock()
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 30 * time.Minute
	}
	return d
}

// processProject inspects and updates a single running project, returning a
// result for the web UI. When forced (manual "check & update" action) the
// per-project allowlist is ignored, but the global auto_update switch and the
// build-only filter still apply. trigger is "manual" or "cron".
func (u *updater) processProject(p composeProject, forced bool, trigger string) projectResult {
	u.cfg.mu.Lock()
	autoUpdate := u.cfg.AutoUpdateEnabled()
	u.cfg.mu.Unlock()
	enabled := u.cfg.ProjectEnabled(p.Key())
	if !forced && !enabled {
		res := nowResult(p, enabled)
		res.Trigger = trigger
		res.Status, res.Detail = "skipped", "not selected for scheduled updates"
		// Do not log projects that simply are not scheduled: they have no real
		// check to report and would flood the log on every cron run.
		res.logged = false
		return res
	}
	return u.process(p, enabled, autoUpdate, trigger)
}

func (u *updater) process(p composeProject, enabled, autoUpdate bool, trigger string) projectResult {
	res := nowResult(p, enabled)
	res.Trigger = trigger
	res.logged = true
	log.Printf("[%s] checking %s (%s)", trigger, p.Project, p.File)
	file := p.File
	if file == "" {
		res.Status, res.Detail = "failed", "no compose file in project labels"
		log.Printf("skip %s: %v", p.Project, res.Detail)
		return res
	}
	if !fileReadable(file) {
		res.Status, res.Detail = "unmounted", "compose file not mounted/readable inside the container: "+file
		log.Printf("skip %s: %v", p.Key(), res.Detail)
		return res
	}
	dir := p.WorkingDir
	if dir == "" {
		dir = filepath.Dir(file)
	}
	ctx, cancel := context.WithTimeout(context.Background(), u.timeout())
	defer cancel()
	log.Printf("project: %s (%s)", p.Project, file)
	configOut, err := u.runCmd(ctx, dir, u.compose.withFile(file, "config", "--format", "json"))
	if err != nil {
		res.Status, res.Detail = "failed", "compose config failed: "+err.Error()
		log.Printf("skip %s: %v", p.Key(), res.Detail)
		return res
	}
	var cfg composeConfig
	if err := json.Unmarshal(configOut, &cfg); err != nil {
		res.Status, res.Detail = "failed", "cannot parse compose config: "+err.Error()
		log.Printf("skip %s: %v", p.Key(), res.Detail)
		return res
	}
	psOut, err := u.runCmd(ctx, dir, u.compose.withFile(file, "ps", "--services", "--status", "running"))
	if err != nil {
		res.Status, res.Detail = "failed", "cannot list running services: "+err.Error()
		log.Printf("skip %s: %v", p.Key(), res.Detail)
		return res
	}
	running := map[string]bool{}
	for _, s := range strings.Split(string(psOut), "\n") {
		s = strings.TrimSpace(s)
		if s != "" {
			running[s] = true
		}
	}
	if len(running) == 0 {
		res.Status, res.Detail = "skipped", "no running Compose services"
		log.Printf("skip %s: %v", p.Key(), res.Detail)
		return res
	}
	var eligible []string
	var buildOnly, notRunning, noImage []string
	for name, svc := range cfg.Services {
		if !running[name] {
			notRunning = append(notRunning, name)
			continue
		}
		if hasBuild(svc.Build) {
			buildOnly = append(buildOnly, name)
			continue
		}
		if strings.TrimSpace(svc.Image) == "" {
			noImage = append(noImage, name)
			continue
		}
		eligible = append(eligible, name)
	}
	sort.Strings(eligible)
	sort.Strings(buildOnly)
	sort.Strings(noImage)
	sort.Strings(notRunning)
	res.Services = eligible
	if len(eligible) == 0 {
		res.Status = "skipped"
		switch {
		case len(buildOnly) > 0 && len(notRunning) == 0 && len(noImage) == 0:
			res.Detail = "built locally (build:), not pulled from a registry — nothing to update: " + strings.Join(buildOnly, ", ")
		case len(notRunning) > 0 && len(buildOnly) == 0 && len(noImage) == 0:
			res.Detail = "no running services"
		default:
			var parts []string
			if len(buildOnly) > 0 {
				parts = append(parts, "build-only: "+strings.Join(buildOnly, ", "))
			}
			if len(noImage) > 0 {
				parts = append(parts, "no image: "+strings.Join(noImage, ", "))
			}
			if len(notRunning) > 0 {
				parts = append(parts, "not running: "+strings.Join(notRunning, ", "))
			}
			res.Detail = "no image-based services eligible for update (" + strings.Join(parts, "; ") + ")"
		}
		log.Printf("skip %s: %v", p.Key(), res.Detail)
		return res
	}
	log.Printf("%s: eligible running services: %s", p.Key(), strings.Join(eligible, ", "))

	// Record the image ID each running container currently uses, so we only
	// recreate the services whose image actually changed after pulling.
	before, err := u.containerImageIDs(ctx, dir, file, eligible)
	if err != nil {
		res.Status, res.Detail = "failed", "cannot read current image IDs: "+err.Error()
		log.Printf("skip %s: %v", p.Key(), res.Detail)
		return res
	}

	if _, err := u.runCmdStream(ctx, dir, u.compose.withFile(file, "pull", eligible...), func(line string) {
		u.progressLog(p.Key(), "pulling: "+line)
	}); err != nil {
		res.Status, res.Detail = "failed", "pull failed: "+err.Error()
		log.Printf("pull failed for %s: %v", p.Key(), err)
		return res
	}

	changed := u.changedServices(ctx, dir, eligible, cfg, before)
	res.Updated = changed
	if len(changed) == 0 {
		res.Status, res.Detail = "unchanged", "images unchanged, nothing to recreate"
		log.Printf("%s: %v", p.Key(), res.Detail)
		return res
	}
	log.Printf("%s: image updated for: %s", p.Key(), strings.Join(changed, ", "))
	if !autoUpdate {
		res.Status, res.Detail = "pulled", "images pulled only (auto_update=false)"
		log.Printf("AUTO_UPDATE=false: pulled images only; not recreating %s", p.Key())
		return res
	}
	upArgs := []string{"-d", "--no-deps", "--no-build"}
	if u.compose.supports {
		upArgs = append(upArgs, "--wait")
	}
	upArgs = append(upArgs, changed...)
	if out, err := u.runCmd(ctx, dir, u.compose.withFile(file, "up", upArgs...)); err != nil {
		res.Status, res.Detail = "failed", "update failed: "+err.Error()
		log.Printf("UPDATE FAILED for %s: %v\n%s", p.Key(), err, strings.TrimSpace(string(out)))
		return res
	}
	res.Status, res.Detail = "updated", "recreated: "+strings.Join(changed, ", ")
	log.Printf("updated successfully: %s", p.Key())
	return res
}

// checkAndUpdate runs a single project immediately (manual action), bypassing
// the scheduled-update allowlist but honoring the global settings.
func (u *updater) checkAndUpdate(p composeProject) projectResult {
	return u.processProject(p, true, "manual")
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

// hasBuild reports whether a compose service defines a build section. It
// accepts the value exactly as emitted by `docker compose config --format json`,
// where a build is an object (possibly empty) or a non-empty string, and "no
// build" is null/absent. Explicit JSON handling avoids false negatives when the
// build section is an object like {"context":"."}.
func hasBuild(raw json.RawMessage) bool {
	v := strings.TrimSpace(string(raw))
	if v == "" || v == "null" {
		return false
	}
	// Non-empty string form (e.g. "." or "./dir") means a build.
	if len(v) > 0 && v[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return strings.TrimSpace(s) != ""
		}
		return true
	}
	// Object form: any non-empty object means a build.
	if v[0] == '{' {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) == nil {
			return len(obj) > 0
		}
		return true
	}
	// Any other non-null JSON value is treated conservatively as a build.
	return true
}

func (u *updater) runCmd(parent context.Context, dir string, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, u.timeout())
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

// runCmdStream is like runCmd but reports progress: onProgress is called with
// the latest non-empty line of combined output (throttled) as the command runs.
// The full output is still returned for callers that parse it.
func (u *updater) runCmdStream(parent context.Context, dir string, argv []string, onProgress func(string)) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, u.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	var full bytes.Buffer
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := bufio.NewReaderSize(pr, 64*1024)
		for {
			chunk, err := r.ReadString('\n')
			// Also split on carriage returns, which docker progress uses to
			// redraw the same line in place.
			for _, seg := range strings.FieldsFunc(chunk, func(c rune) bool { return c == '\r' }) {
				seg = strings.TrimSpace(stripANSI(seg))
				if seg == "" {
					continue
				}
				mu.Lock()
				full.WriteString(seg)
				full.WriteByte('\n')
				mu.Unlock()
				if onProgress != nil {
					onProgress(seg)
				}
			}
			if err != nil {
				return
			}
		}
	}()

	err := cmd.Run()
	_ = pw.Close()
	<-done
	mu.Lock()
	out := append([]byte(nil), full.Bytes()...)
	mu.Unlock()
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// stripANSI removes common ANSI escape sequences (color/progress control) so
// captured progress lines stay readable in the web UI.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) {
				c := s[i]
				if (c >= '0' && c <= '9') || c == ';' || c == '?' {
					i++
					continue
				}
				break
			}
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
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
