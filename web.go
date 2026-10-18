package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

// maxCheckLogs caps the in-memory check log ring shown in the web UI.
const maxCheckLogs = 100

// maxComposeBytes caps how much compose-file content the editor will read or
// write, so a mistaken path cannot exhaust memory.
const maxComposeBytes = 1 << 20

// webConfig is the editable subset of Config exposed over the API.
type webConfig struct {
	CronSchedule   string `json:"cron_schedule"`
	Timezone       string `json:"timezone"`
	RunOnStart     bool   `json:"run_on_start"`
	AutoUpdate     bool   `json:"auto_update"`
	CommandTimeout string `json:"command_timeout"`
}

type webState struct {
	Config      webConfig         `json:"config"`
	Projects    []composeProject  `json:"projects"`
	Results     []projectResult   `json:"results"`
	Logs        []checkLog        `json:"logs"`
	Enabled     map[string]bool   `json:"enabled"`
	LastUpdates map[string]string `json:"last_updates,omitempty"`
	Compose     string            `json:"compose"`
	Running     bool              `json:"running"`
	NextRun     string            `json:"next_run,omitempty"`
	LastRun     string            `json:"last_run,omitempty"`
	ScanError   string            `json:"scan_error,omitempty"`
}

// startWeb runs the HTTP management UI until the process exits. Set WEB_ADDR to
// "off" to disable it.
func startWeb(u *updater, addr string) {
	if strings.EqualFold(addr, "off") || strings.EqualFold(addr, "disable") {
		log.Printf("web UI disabled")
		return
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, u.state())
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var wc webConfig
		if err := json.NewDecoder(r.Body).Decode(&wc); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := u.applyConfig(wc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, u.state())
	})

	mux.HandleFunc("/api/project", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			File    string `json:"file"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.File) == "" {
			http.Error(w, "file is required", http.StatusBadRequest)
			return
		}
		if err := u.cfg.SetProjectEnabled(body.File, body.Enabled); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, u.state())
	})

	// Check & update a single project immediately.
	mux.HandleFunc("/api/check", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			File       string `json:"file"`
			WorkingDir string `json:"working_dir"`
			Project    string `json:"project"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.File) == "" {
			http.Error(w, "file is required", http.StatusBadRequest)
			return
		}
		p := composeProject{File: body.File, WorkingDir: body.WorkingDir, Project: body.Project, Mounted: fileReadable(body.File)}
		p.Running = u.projectRunning(r.Context(), body.Project)
		if _, ok := u.startLog(p, "manual"); !ok {
			writeJSON(w, map[string]string{"status": "already-running"})
			return
		}
		go u.runManualCheck(p)
		writeJSON(w, map[string]string{"status": "started"})
	})

	// Check & update several projects immediately (e.g. the filtered list).
	mux.HandleFunc("/api/check-many", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Projects []struct {
				File       string `json:"file"`
				WorkingDir string `json:"working_dir"`
				Project    string `json:"project"`
			} `json:"projects"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		started, skipped := 0, 0
		for _, it := range body.Projects {
			if strings.TrimSpace(it.File) == "" {
				skipped++
				continue
			}
			p := composeProject{File: it.File, WorkingDir: it.WorkingDir, Project: it.Project, Mounted: fileReadable(it.File)}
			p.Running = u.projectRunning(r.Context(), it.Project)
			if _, ok := u.startLog(p, "manual"); !ok {
				skipped++
				continue
			}
			started++
			go u.runManualCheck(p)
		}
		writeJSON(w, map[string]any{"status": "started", "started": started, "skipped": skipped})
	})

	// Read or save the primary compose file of a known project.
	mux.HandleFunc("/api/compose", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			p, err := u.projectByFile(r.Context(), r.URL.Query().Get("file"), r.URL.Query().Get("project"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			info, err := os.Stat(p.File)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			if info.Size() > maxComposeBytes {
				http.Error(w, "compose file is too large to edit", http.StatusRequestEntityTooLarge)
				return
			}
			data, err := os.ReadFile(p.File)
			if err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			writeJSON(w, map[string]any{
				"file":     p.File,
				"project":  p.Project,
				"content":  string(data),
				"writable": p.Writable,
			})
		case http.MethodPost:
			r.Body = http.MaxBytesReader(w, r.Body, maxComposeBytes+64*1024)
			var body struct {
				File    string `json:"file"`
				Project string `json:"project"`
				Content string `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
				return
			}
			if len(body.Content) > maxComposeBytes {
				http.Error(w, "compose file is too large to save", http.StatusRequestEntityTooLarge)
				return
			}
			p, err := u.projectByFile(r.Context(), body.File, body.Project)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			if !p.Writable {
				http.Error(w, "compose file is read-only inside the container; mount its directory read-write (:rw) to enable editing", http.StatusForbidden)
				return
			}
			if u.isBusy(p.File) {
				http.Error(w, "a check or lifecycle action is currently running for this project; try again when it finishes", http.StatusConflict)
				return
			}
			backup, err := writeComposeFile(p.File, body.Content)
			if err != nil {
				http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
			warning := u.validateCompose(r.Context(), p)
			writeJSON(w, map[string]any{"status": "saved", "backup": backup, "warning": warning})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Start, stop, or restart a known project.
	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			File    string `json:"file"`
			Project string `json:"project"`
			Action  string `json:"action"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		action := strings.ToLower(strings.TrimSpace(body.Action))
		if _, _, ok := actionArgs(action, false); !ok {
			http.Error(w, "unsupported action", http.StatusBadRequest)
			return
		}
		p, err := u.projectByFile(r.Context(), body.File, body.Project)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if !p.Mounted {
			http.Error(w, "compose file is not readable inside the container; mount its directory to enable this action", http.StatusBadRequest)
			return
		}
		if p.Self && action != "start" {
			http.Error(w, "refusing to "+action+" the updater's own project from its own UI", http.StatusBadRequest)
			return
		}
		progress := map[string]string{
			"start":    "starting…",
			"stop":     "stopping…",
			"restart":  "restarting…",
			"recreate": "recreating…",
		}[action]
		if _, ok := u.startLogDetail(p, "manual", progress); !ok {
			writeJSON(w, map[string]string{"status": "already-running"})
			return
		}
		go u.runAction(p, action)
		writeJSON(w, map[string]string{"status": "started"})
	})

	// Run a full scan of all running projects.
	mux.HandleFunc("/api/run", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		go u.run()
		writeJSON(w, map[string]string{"status": "started"})
	})

	log.Printf("web UI listening on %s", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("web server error: %v", err)
	}
}

// runManualCheck performs a single manual check and records its outcome.
func (u *updater) runManualCheck(p composeProject) {
	res := u.checkAndUpdate(p)
	u.recordResult(res)
	u.finishLog(res)
}

// projectByFile returns the discovered project that owns the given primary
// compose file. It is used as an allowlist for the file editor and lifecycle
// actions, so the unauthenticated UI cannot read or write arbitrary paths.
func (u *updater) projectByFile(ctx context.Context, file, project string) (composeProject, error) {
	file = strings.TrimSpace(file)
	project = strings.TrimSpace(project)
	if file == "" {
		return composeProject{}, errors.New("file is required")
	}
	projects, err := u.discoverRunningProjects(ctx)
	if err != nil {
		return composeProject{}, err
	}
	var match *composeProject
	for i := range projects {
		p := &projects[i]
		if p.File != file || (project != "" && p.Project != project) {
			continue
		}
		if match != nil && match.Project != p.Project {
			return composeProject{}, errors.New("compose file is shared by multiple projects; specify project")
		}
		match = p
	}
	if match == nil {
		return composeProject{}, errors.New("unknown compose file")
	}
	return *match, nil
}

// isBusy reports whether a check or lifecycle action is currently running for
// the given compose file.
func (u *updater) isBusy(file string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range u.logs {
		if u.logs[i].File == file && u.logs[i].Status == "running" {
			return true
		}
	}
	return false
}

// actionArgs maps a lifecycle action to its Compose subcommand. Recreate runs
// `compose up --force-recreate` so edited images and other config changes are
// applied even when Compose would otherwise consider the container current.
func actionArgs(action string, wait bool) (string, []string, bool) {
	switch action {
	case "start", "stop", "restart":
		return action, nil, true
	case "recreate":
		args := []string{"-d", "--no-build", "--force-recreate"}
		if wait {
			args = append(args, "--wait")
		}
		return "up", args, true
	}
	return "", nil, false
}

// runAction starts, stops, restarts, or recreates a project and records the
// outcome like a manual check. startLog has already marked it as running.
func (u *updater) runAction(p composeProject, action string) {
	ctx, cancel := context.WithTimeout(context.Background(), u.timeout())
	defer cancel()
	dir := p.WorkingDir
	if dir == "" {
		dir = filepath.Dir(p.File)
	}
	sub, extra, ok := actionArgs(action, u.compose.supports)
	if !ok {
		log.Printf("[manual] %s: unsupported action %q", p.Key(), action)
		return
	}
	argv := u.compose.withProjectFile(p.Project, p.File, sub, extra...)
	out, err := u.runCmdStream(ctx, dir, argv, func(line string) {
		u.progressLog(p.Key(), line)
	})
	res := nowResult(p, u.cfg.ProjectEnabled(p.Key()))
	res.Trigger = "manual"
	if err != nil {
		res.Status = "failed"
		res.Detail = action + " failed: " + err.Error()
	} else {
		switch action {
		case "start":
			res.Status = "started"
		case "stop":
			res.Status = "stopped"
		case "restart":
			res.Status = "restarted"
		case "recreate":
			res.Status = "recreated"
		}
		res.Detail = lastNonEmptyLine(out)
		if res.Detail == "" {
			res.Detail = action + " via web UI"
		}
		if action == "recreate" {
			res = u.markUpdated(res, "recreated", res.Detail)
		}
	}
	u.finishLog(res)
	u.recordResult(res)
	log.Printf("[manual] %s -> %s%s", p.Key(), res.Status, detailSuffix(res.Detail))
}

// lastNonEmptyLine returns the last non-blank line of command output, which is
// the most useful summary for a start/stop/restart log entry.
func lastNonEmptyLine(out []byte) string {
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

// writeComposeFile backs up and atomically replaces a compose file, preserving
// its permissions. The backup is written next to the file as <name>.bak.
func writeComposeFile(path, content string) (string, error) {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	backup := path + ".bak"
	if err := os.WriteFile(backup, old, info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("backup failed: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".compose-edit-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.WriteString(content); err != nil {
		cleanup()
		return "", err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		cleanup()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return backup, nil
}

// validateCompose runs `compose config` after a save and returns a warning (not
// an error) when the edited file no longer parses. The save is still kept so
// the user can fix it in place; the previous version remains in the backup.
func (u *updater) validateCompose(ctx context.Context, p composeProject) string {
	dir := p.WorkingDir
	if dir == "" {
		dir = filepath.Dir(p.File)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := u.runCmd(ctx, dir, u.compose.withProjectFile(p.Project, p.File, "config", "--quiet")); err != nil {
		return "compose validation failed: " + err.Error()
	}
	return ""
}

// recordResult merges a single-project result into the last scan results so the
// UI reflects manual actions too.
func (u *updater) recordResult(res projectResult) {
	u.mu.Lock()
	defer u.mu.Unlock()
	fallback := u.cfg.LastUpdate(res.File)
	for i, r := range u.lastResults {
		if r.File == res.File {
			if res.UpdatedAt == "" {
				res.UpdatedAt = r.UpdatedAt
			}
			if res.UpdatedAt == "" {
				res.UpdatedAt = fallback
			}
			u.lastResults[i] = res
			return
		}
	}
	if res.UpdatedAt == "" {
		res.UpdatedAt = fallback
	}
	u.lastResults = append(u.lastResults, res)
}

// startLog records the beginning of a check so the UI shows activity while a
// (potentially slow) pull/update runs. It returns the start time when a new
// check began, or ok=false when a check for the same project is already in
// progress (so callers can skip the duplicate). The start time is also written
// into the current result so the "last check" column updates immediately.
func (u *updater) startLog(p composeProject, trigger string) (time.Time, bool) {
	return u.startLogDetail(p, trigger, "checking…")
}

// startLogDetail is startLog with a custom in-progress message, used by
// lifecycle actions (starting/stopping/restarting).
func (u *updater) startLogDetail(p composeProject, trigger, detail string) (time.Time, bool) {
	now := time.Now()
	entry := checkLog{
		When:    now.Format(time.RFC3339),
		Trigger: trigger,
		Project: p.Project,
		File:    p.File,
		Status:  "running",
		Detail:  detail,
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range u.logs {
		if u.logs[i].File == p.File && u.logs[i].Status == "running" {
			log.Printf("[%s] %s already being checked; ignoring duplicate trigger", trigger, p.Project)
			return time.Time{}, false
		}
	}
	u.logs = append([]checkLog{entry}, u.logs...)
	if len(u.logs) > maxCheckLogs {
		u.logs = u.logs[:maxCheckLogs]
	}
	// Reflect the in-progress check in the result list immediately, so the
	// "last check" time updates at the start (not after the run completes).
	// Keep the previous last-update time: an in-progress check has not updated
	// anything yet.
	running := projectResult{
		File:      p.File,
		Project:   p.Project,
		Status:    "running",
		Detail:    detail,
		When:      now.Format(time.RFC3339),
		UpdatedAt: u.cfg.LastUpdate(p.File),
		Trigger:   trigger,
	}
	replaced := false
	for i := range u.lastResults {
		if u.lastResults[i].File == p.File {
			if u.lastResults[i].UpdatedAt != "" {
				running.UpdatedAt = u.lastResults[i].UpdatedAt
			}
			u.lastResults[i] = running
			replaced = true
			break
		}
	}
	if !replaced {
		u.lastResults = append(u.lastResults, running)
	}
	log.Printf("[%s] %s %s (%s)", trigger, strings.TrimSuffix(detail, "…"), p.Project, p.File)
	return now, true
}

// progressLog appends output lines to the in-progress log entry for a project
// so the UI can show the full pull/update output while it runs.
func (u *updater) progressLog(key, line string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range u.logs {
		if u.logs[i].File == key && u.logs[i].Status == "running" {
			cur := u.logs[i].Detail
			if cur == "" || cur == "checking…" {
				u.logs[i].Detail = line
			} else {
				u.logs[i].Detail = cur + "\n" + line
			}
			// Bound the stored text so a chatty command cannot grow unbounded.
			const maxLen = 20000
			if len(u.logs[i].Detail) > maxLen {
				u.logs[i].Detail = u.logs[i].Detail[len(u.logs[i].Detail)-maxLen:]
			}
			return
		}
	}
}

// finishLog replaces the in-progress entry for a project with its final result.
func (u *updater) finishLog(res projectResult) {
	entry := checkLog{
		When:    res.When,
		Trigger: res.Trigger,
		Project: res.Project,
		File:    res.File,
		Status:  res.Status,
		Detail:  res.Detail,
	}
	u.mu.Lock()
	replaced := false
	for i := range u.logs {
		if u.logs[i].File == res.File && u.logs[i].Status == "running" {
			u.logs[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		u.logs = append([]checkLog{entry}, u.logs...)
		if len(u.logs) > maxCheckLogs {
			u.logs = u.logs[:maxCheckLogs]
		}
	}
	u.mu.Unlock()
	log.Printf("[%s] %s -> %s%s", entry.Trigger, entry.Project, entry.Status, detailSuffix(entry.Detail))
}

func detailSuffix(d string) string {
	if d == "" {
		return ""
	}
	return " (" + d + ")"
}

func (u *updater) state() webState {
	u.cfg.mu.Lock()
	wc := webConfig{
		CronSchedule:   u.cfg.CronSchedule,
		Timezone:       u.cfg.Timezone,
		RunOnStart:     u.cfg.RunOnStartEnabled(),
		AutoUpdate:     u.cfg.AutoUpdateEnabled(),
		CommandTimeout: u.cfg.CommandTimeout,
	}
	enabled := map[string]bool{}
	for k, v := range u.cfg.EnabledProjects {
		enabled[k] = v
	}
	lastUpdates := map[string]string{}
	for k, v := range u.cfg.LastUpdates {
		lastUpdates[k] = v
	}
	u.cfg.mu.Unlock()

	u.mu.Lock()
	results := append([]projectResult(nil), u.lastResults...)
	logs := append([]checkLog(nil), u.logs...)
	running := u.running
	next := u.nextRun
	last := u.lastRun
	u.mu.Unlock()

	projects, err := u.discoverRunningProjects(context.Background())
	st := webState{
		Config:      wc,
		Projects:    projects,
		Results:     results,
		Logs:        logs,
		Enabled:     enabled,
		LastUpdates: lastUpdates,
		Compose:     u.compose.display,
		Running:     running,
	}
	if err != nil {
		st.ScanError = err.Error()
	}
	if !next.IsZero() {
		st.NextRun = next.Format(time.RFC3339)
	}
	if !last.IsZero() {
		st.LastRun = last.Format(time.RFC3339)
	}
	return st
}

// applyConfig validates and persists a new configuration, preserving the
// per-project allowlist.
func (u *updater) applyConfig(wc webConfig) error {
	if _, err := time.LoadLocation(wc.Timezone); err != nil {
		return errors.New("invalid timezone " + wc.Timezone)
	}
	if _, err := parseCron(wc.CronSchedule); err != nil {
		return errors.New("invalid cron: " + err.Error())
	}
	if _, err := time.ParseDuration(wc.CommandTimeout); err != nil {
		return errors.New("invalid command timeout " + wc.CommandTimeout)
	}

	u.cfg.mu.Lock()
	u.cfg.CronSchedule = wc.CronSchedule
	u.cfg.Timezone = wc.Timezone
	u.cfg.RunOnStart = boolPtr(wc.RunOnStart)
	u.cfg.AutoUpdate = boolPtr(wc.AutoUpdate)
	u.cfg.CommandTimeout = wc.CommandTimeout
	u.cfg.mu.Unlock()

	u.cfg.normalize()
	return u.cfg.save()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
