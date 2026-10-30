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
	selfID  string // this container's ID (short), if running inside Docker

	mu          sync.Mutex
	running     bool
	lastRun     time.Time
	nextRun     time.Time
	lastResults []projectResult
	logs        []checkLog
	// tasks holds the cancellable context of every in-progress check or
	// lifecycle action, keyed by the project's log key (compose file path or
	// "container:<name>"). It lets the UI cancel a running operation.
	tasks map[string]*runningTask
}

// runningTask is the cancellable context for one in-progress operation.
type runningTask struct {
	ctx    context.Context
	cancel context.CancelFunc
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
	// UpdatedAt is the time of the last successful update (updated/pulled/
	// started), independent of the latest check. It is persisted in the config
	// file and carried across checks that did not update anything.
	UpdatedAt string `json:"updated_at,omitempty"`
	Trigger   string `json:"trigger,omitempty"` // "manual" or "cron"
	// Action is the operation that produced this result (check, start, stop,
	// restart, recreate). The UI uses it to turn the triggering button into a
	// Cancel button while the operation runs.
	Action string `json:"action,omitempty"`
	logged bool   // internal: whether this result should appear in the check log
}

// checkLog is a single historical check event shown in the web UI.
type checkLog struct {
	When    string `json:"when"`
	Trigger string `json:"trigger"` // "manual" or "cron"
	Project string `json:"project"`
	File    string `json:"file"`
	Status  string `json:"status"` // processing, updated, unchanged, skipped, unmounted, pulled, failed, cancelled
	Detail  string `json:"detail,omitempty"`
	// DurationMs is how long the operation took. It stays 0 while the entry is
	// still processing; the UI then shows a live counter based on When.
	DurationMs int64 `json:"duration_ms"`

	// Action is the operation being performed (check, start, stop, restart,
	// recreate), so the UI can show Cancel on the button that started it.
	Action string `json:"action,omitempty"`

	// startedAt is the wall-clock start used to compute DurationMs. It is not
	// serialized.
	startedAt time.Time
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

// withProjectFile builds an argv with both `-p <project>` and `-f <file>`
// global options. Passing the project name explicitly is required when the
// project was started with a different name than Compose would derive from the
// file's directory (e.g. `docker compose -p foo` or a differently named dir),
// otherwise `ps`/`pull`/`up` target the wrong project and see no services.
func (c composeCommand) withProjectFile(project, file, sub string, rest ...string) []string {
	argv := []string{c.bin}
	argv = append(argv, c.plugin...)
	if project != "" {
		argv = append(argv, "-p", project)
	}
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

// selfContainerID returns this process's container ID (short) when running
// inside Docker, or "" otherwise. It reads /proc/self/mountinfo, whose root
// mount source contains the container ID, falling back to the hostname (Docker
// sets the hostname to the container ID by default).
func selfContainerID() string {
	if data, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			// Look for the cgroup/docker/<id> path segment.
			if i := strings.Index(line, "/docker/"); i >= 0 {
				rest := line[i+len("/docker/"):]
				id := rest
				if j := strings.IndexAny(rest, " /"); j >= 0 {
					id = rest[:j]
				}
				if len(id) >= 12 {
					return id[:12]
				}
			}
		}
	}
	if h, err := os.Hostname(); err == nil && len(h) == 12 {
		return h
	}
	return ""
}

// projectIsSelf reports whether this updater's own container belongs to the
// given compose project.
func (u *updater) projectIsSelf(ctx context.Context, project string) bool {
	if u.selfID == "" || project == "" {
		return false
	}
	out, err := u.runDocker(ctx, "", []string{"ps", "-q", "--filter", "label=com.docker.compose.project=" + project})
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if id := strings.TrimSpace(line); id != "" && strings.HasPrefix(id, u.selfID) {
			return true
		}
	}
	return false
}

// selfUpdate runs `compose up` for the updater's own project via a detached
// helper container so the operation survives the updater being recreated. It
// mounts the Docker socket and the project working directory and runs the same
// compose command with the same arguments.
func (u *updater) selfUpdate(ctx context.Context, dir, project, file string, upArgs []string) error {
	helper := selfUpdateHelperArgs(u.compose, u.helperImage(ctx), dir, project, file, upArgs, composeEnv(os.Environ()))
	log.Printf("self-update: launching detached helper: docker %s", strings.Join(helper, " "))
	// Use a context detached from cancellation: the updater container will be
	// recreated and this process may be stopped; the helper runs independently.
	out, err := u.runDocker(context.WithoutCancel(ctx), dir, helper)
	if err != nil {
		return fmt.Errorf("helper launch failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// helperImage returns the image the detached self-update helper should run:
// this updater's own image, so the helper uses the same Compose implementation
// (and therefore the same config hashing) as `compose pull`/`config` run by the
// updater. Falling back to docker:cli can make Compose compute a different
// config hash, which recreates the container on every self-update.
func (u *updater) helperImage(ctx context.Context) string {
	if u.selfID != "" {
		out, err := u.runDocker(ctx, "", []string{"inspect", "--format", "{{.Image}}", u.selfID})
		if err == nil {
			if id := strings.TrimSpace(string(out)); id != "" {
				return id
			}
		}
	}
	return "docker:cli"
}

// composeEnv returns the COMPOSE_* variables from environ so the helper
// interpolates the compose file exactly like this process (for example an
// image override such as COMPOSE_AUTO_UPDATER_IMAGE).
func composeEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if strings.HasPrefix(kv, "COMPOSE_") {
			out = append(out, kv)
		}
	}
	return out
}

// selfUpdateHelperArgs builds the `docker run ...` argv for a detached helper
// that performs a compose update independently of this container's lifecycle.
func selfUpdateHelperArgs(cc composeCommand, image, dir, project, file string, upArgs, env []string) []string {
	entrypoint := "docker"
	var inner []string
	if cc.v2 {
		inner = append(inner, "compose")
	} else {
		entrypoint = "docker-compose"
	}
	if project != "" {
		inner = append(inner, "-p", project)
	}
	inner = append(inner, "-f", file, "up")
	inner = append(inner, upArgs...)

	helper := []string{
		"run", "--rm", "-d",
		"--entrypoint", entrypoint,
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", dir + ":" + dir,
		"-w", dir,
	}
	for _, kv := range env {
		helper = append(helper, "-e", kv)
	}
	helper = append(helper, image)
	return append(helper, inner...)
}

// selfUpdatePending reports whether `compose up` would create or recreate any
// container for this project. It uses Compose's dry-run mode, which inspects
// the project without changing it, so the updater can report "unchanged" and
// avoid launching a helper when the running container is already up to date.
func (u *updater) selfUpdatePending(ctx context.Context, dir, project, file string, upArgs []string) bool {
	if !u.compose.v2 {
		// Compose v1 has no dry-run; assume a change so the update still runs.
		return true
	}
	dryArgs := append([]string{"up"}, upArgs...)
	out, err := u.runCmdStream(ctx, dir, u.compose.withProjectFile(project, file, "--dry-run", dryArgs...), nil)
	if err != nil {
		// Older Compose releases lack --dry-run. Be conservative and run the
		// update rather than silently skipping it.
		log.Printf("self-update: dry-run unavailable (%v); updating unconditionally", err)
		return true
	}
	return dryRunShowsChanges(string(out))
}

// dryRunShowsChanges reports whether `compose --dry-run up` output contains an
// action that would create or recreate a container. A no-op dry-run only
// mentions Running/Waiting/Healthy.
func dryRunShowsChanges(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "recreat") ||
		strings.Contains(lower, "creat") ||
		strings.Contains(lower, "starting")
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
	u := &updater{cfg: cfg, compose: cc, selfID: selfContainerID()}
	log.Printf("compose-auto-updater started; compose=%s wait=%t auto_update=%t cron=%q tz=%s config=%s self=%s",
		cc.display, cc.supports, cfg.AutoUpdateEnabled(), cfg.CronSchedule, cfg.Timezone, cfg.path, u.selfID)

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

// taskContext returns the cancellable context registered for an in-progress
// operation. When none is registered (a runner invoked directly, as in tests),
// a fresh one is created so the operation can still be cancelled.
func (u *updater) taskContext(key string) context.Context {
	u.mu.Lock()
	defer u.mu.Unlock()
	if t, ok := u.tasks[key]; ok {
		return t.ctx
	}
	ctx, cancel := context.WithCancel(context.Background())
	if u.tasks == nil {
		u.tasks = map[string]*runningTask{}
	}
	u.tasks[key] = &runningTask{ctx: ctx, cancel: cancel}
	return ctx
}

// newTaskContext starts a cancellable context for a new operation and replaces
// any task already registered under the same key. The caller must hold u.mu.
func (u *updater) newTaskContext(key string) context.Context {
	if u.tasks == nil {
		u.tasks = map[string]*runningTask{}
	}
	if old, ok := u.tasks[key]; ok {
		old.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.tasks[key] = &runningTask{ctx: ctx, cancel: cancel}
	return ctx
}

// dropTask forgets the task registered for key and returns its cancel func so
// the caller can invoke it outside the mutex.
func (u *updater) dropTask(key string) context.CancelFunc {
	if t, ok := u.tasks[key]; ok {
		delete(u.tasks, key)
		return t.cancel
	}
	return nil
}

// maxParallelProjects caps how many projects are pulled/updated at the same
// time during a scheduled scan. Each project's `compose pull` already pulls its
// images concurrently; this cap bounds the number of independent Compose
// invocations so the Docker daemon and registries are not overwhelmed.
const maxParallelProjects = 4

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
	log.Printf("scan started: found %d Compose project(s)", len(projects))

	// The self project is handled after every other project has finished: its
	// update recreates this container, which would otherwise kill in-flight
	// pulls for other projects.
	var queue, self []composeProject
	for _, p := range projects {
		// The scheduler only processes running projects. Stopped projects (and
		// those not selected for scheduled updates, or whose compose file is not
		// mounted) are skipped without a check result: they keep no "last check"
		// and produce no log entry. Stopped projects remain manually updatable.
		if !p.Running || !u.cfg.ProjectEnabled(p.Key()) || !p.Mounted {
			continue
		}
		if p.Self {
			self = append(self, p)
		} else {
			queue = append(queue, p)
		}
	}

	// Pull/update independent projects concurrently with a bounded worker pool.
	// startLog is called before dispatch so the UI shows all queued checks.
	sem := make(chan struct{}, maxParallelProjects)
	var wg sync.WaitGroup
	for _, p := range queue {
		if _, ok := u.startLog(p, "cron"); !ok {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(p composeProject) {
			defer wg.Done()
			defer func() { <-sem }()
			res := u.processProject(p, false, "cron")
			u.finishLog(res)
			// Merge so results for projects not covered by this scan (e.g.
			// stopped projects updated manually) are preserved.
			u.recordResult(res)
		}(p)
	}
	wg.Wait()

	// Run self-updates last and one at a time.
	for _, p := range self {
		if _, ok := u.startLog(p, "cron"); !ok {
			continue
		}
		res := u.processProject(p, false, "cron")
		u.finishLog(res)
		u.recordResult(res)
	}
	log.Printf("scan finished in %s", time.Since(started).Round(time.Second))
}

// projectResultNow builds a timestamped result for the current moment. The
// caller overrides When with the check start time when one is known.
func nowResult(p composeProject, enabled bool) projectResult {
	return projectResult{File: p.File, Project: p.Project, Enabled: enabled, When: time.Now().Format(time.RFC3339)}
}

// markUpdated records a successful update on the result and persists its time so
// the UI can keep showing "last update" across restarts and log trimming.
func (u *updater) markUpdated(res projectResult, status, detail string) projectResult {
	res.Status, res.Detail = status, detail
	when := res.When
	if when == "" {
		when = time.Now().Format(time.RFC3339)
	}
	res.UpdatedAt = when
	if err := u.cfg.SetLastUpdate(res.File, when); err != nil {
		log.Printf("cannot persist last update time for %s: %v", res.File, err)
	}
	return res
}

// startTimeFor returns the start time recorded for an in-progress result of the
// given file, or "" if none is in progress.
func (u *updater) startTimeFor(file string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range u.lastResults {
		if u.lastResults[i].File == file && u.lastResults[i].When != "" {
			return u.lastResults[i].When
		}
	}
	return ""
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
	res.Action = "check"
	res.logged = true
	// Keep the start time recorded when this check began (from startLog) so the
	// "last check" column reflects the start, not the completion.
	if st := u.startTimeFor(p.File); st != "" {
		res.When = st
	}
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
	ctx := u.taskContext(p.Key())
	// fail records a failed step, or a cancellation when the user stopped this
	// operation from the web UI (the context is cancelled in that case).
	fail := func(detail string) projectResult {
		if ctx.Err() != nil {
			res.Status, res.Detail = "cancelled", "cancelled by user"
			return res
		}
		res.Status, res.Detail = "failed", detail
		log.Printf("%s: %s", p.Key(), detail)
		return res
	}
	log.Printf("project: %s (%s)", p.Project, file)
	configOut, err := u.runCmd(ctx, dir, u.compose.withProjectFile(p.Project, file, "config", "--format", "json"))
	if err != nil {
		return fail("compose config failed: " + err.Error())
	}
	var cfg composeConfig
	if err := json.Unmarshal(configOut, &cfg); err != nil {
		return fail("cannot parse compose config: " + err.Error())
	}
	// Pull current images for the whole project. Compose skips services that have
	// no registry image (build-only), so the image list is derived from the
	// resolved config instead of enumerating services manually.
	u.progressLog(p.Key(), "pulling…")
	if err := u.pullProjectImages(ctx, p, dir, projectImages(cfg)); err != nil {
		return fail("pull failed: " + err.Error())
	}

	upArgs := []string{"-d", "--no-build"}
	if u.compose.supports {
		upArgs = append(upArgs, "--wait")
	}

	// Stopped project (manual check only; the scheduler never reaches here):
	// start it on the freshly pulled images.
	if !p.Running {
		u.progressLog(p.Key(), "starting…")
		if _, err := u.runCmdStream(ctx, dir, u.compose.withProjectFile(p.Project, file, "up", upArgs...), func(line string) {
			u.progressLog(p.Key(), line)
		}); err != nil {
			return fail("start failed: " + err.Error())
		}
		res = u.markUpdated(res, "started", "stopped project: pulled and started")
		log.Printf("pulled and started stopped project %s", p.Key())
		return res
	}

	if !autoUpdate {
		res = u.markUpdated(res, "pulled", "images pulled only (auto_update=false)")
		log.Printf("AUTO_UPDATE=false: pulled images only; not recreating %s", p.Key())
		return res
	}

	// If the update would recreate this updater's own container, run it through a
	// detached helper so it survives our recreation. Because this process is
	// replaced before `compose up` finishes, ask Compose first (via dry-run)
	// whether anything would actually change; otherwise every check would report
	// a successful "updated" even when the container is already current.
	if u.projectIsSelf(ctx, p.Project) {
		if err := ctx.Err(); err != nil {
			return fail("self-update cancelled")
		}
		if !u.selfUpdatePending(ctx, dir, p.Project, file, upArgs) {
			res.Status, res.Detail = "unchanged", "images unchanged, nothing to recreate"
			log.Printf("%s: %v", p.Key(), res.Detail)
			return res
		}
		// The helper is deliberately detached from this context so it survives
		// our own recreation; honour an explicit cancel before launching it.
		if err := ctx.Err(); err != nil {
			return fail("self-update cancelled")
		}
		if err := u.selfUpdate(ctx, dir, p.Project, file, upArgs); err != nil {
			return fail("self-update failed: " + err.Error())
		}
		res = u.markUpdated(res, "updated", "self-update started (detached)")
		log.Printf("self-update started for %s", p.Key())
		return res
	}

	// Bring the project up: Compose recreates only containers whose image or
	// config changed, leaving the rest untouched.
	var upBuf bytes.Buffer
	u.progressLog(p.Key(), "updating…")
	if _, err := u.runCmdStream(ctx, dir, u.compose.withProjectFile(p.Project, file, "up", upArgs...), func(line string) {
		upBuf.WriteString(line + "\n")
		u.progressLog(p.Key(), line)
	}); err != nil {
		return fail("update failed: " + err.Error())
	}
	if strings.Contains(upBuf.String(), "Recreated") || strings.Contains(upBuf.String(), "Created") {
		res = u.markUpdated(res, "updated", "recreated changed services")
		log.Printf("updated successfully: %s", p.Key())
	} else {
		res.Status, res.Detail = "unchanged", "images unchanged, nothing to recreate"
		log.Printf("%s: %v", p.Key(), res.Detail)
	}
	return res
}

// checkAndUpdate runs a single project immediately (manual action), bypassing
// the scheduled-update allowlist but honoring the global settings.
func (u *updater) checkAndUpdate(p composeProject) projectResult {
	return u.processProject(p, true, "manual")
}

// classifyServices splits a project's services into update-eligible and
// ineligible buckets based on the compose config and which services are
// running. A service with an explicit image reference is eligible even if it
// also declares a build section (docker compose uses the image tag as the build
// result name); only build-without-image services cannot be updated.
func classifyServices(services map[string]serviceConfig, running map[string]bool) (eligible, buildOnly, notRunning, noImage []string) {
	for name, svc := range services {
		if !running[name] {
			notRunning = append(notRunning, name)
			continue
		}
		if strings.TrimSpace(svc.Image) == "" {
			if hasBuild(svc.Build) {
				buildOnly = append(buildOnly, name)
			} else {
				noImage = append(noImage, name)
			}
			continue
		}
		eligible = append(eligible, name)
	}
	sort.Strings(eligible)
	sort.Strings(buildOnly)
	sort.Strings(noImage)
	sort.Strings(notRunning)
	return
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

// pullProjectImages refreshes the project's images. Without an active proxy it
// runs `compose pull`, exactly like before. With a proxy it pulls each image
// through the proxy prefix and re-tags it under the name the compose file uses,
// so `compose up` reuses the freshly pulled layers. A failed proxied pull falls
// back to a direct pull (or a direct `compose pull`), keeping private registries
// and digest-pinned images working.
func (u *updater) pullProjectImages(ctx context.Context, p composeProject, dir string, images []string) error {
	progress := func(line string) { u.progressLog(p.Key(), "pulling: "+line) }
	composePull := func() error {
		_, err := u.runCmdStream(ctx, dir, u.compose.withProjectFile(p.Project, p.File, "pull"), progress)
		return err
	}

	proxy := u.cfg.ActiveImageProxy()
	if proxy == "" || len(images) == 0 {
		return composePull()
	}
	for _, image := range images {
		if err := ctx.Err(); err != nil {
			return err
		}
		proxied := proxiedImageRef(proxy, image)
		if proxied == image {
			// Not proxyable (digest-pinned or already accelerated).
			if _, err := u.runCmdStream(ctx, dir, []string{"docker", "pull", image}, progress); err != nil {
				return err
			}
			continue
		}
		u.progressLog(p.Key(), "pulling via proxy: "+proxied)
		if _, err := u.runCmdStream(ctx, dir, []string{"docker", "pull", proxied}, progress); err != nil {
			if ctx.Err() != nil {
				return err
			}
			u.progressLog(p.Key(), "proxy pull failed, retrying directly: "+image)
			if _, err := u.runCmdStream(ctx, dir, []string{"docker", "pull", image}, progress); err != nil {
				return err
			}
			continue
		}
		if _, err := u.runCmd(ctx, dir, []string{"docker", "tag", proxied, image}); err != nil {
			return fmt.Errorf("tag %s as %s: %w", proxied, image, err)
		}
	}
	return nil
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
	// Keep stdout and stderr separate so diagnostics printed on stderr (e.g.
	// Compose's "version is obsolete" warning) do not corrupt parsed stdout
	// such as `compose config --format json`.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.Bytes(), ctx.Err()
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, msg)
	}
	return stdout.Bytes(), nil
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
