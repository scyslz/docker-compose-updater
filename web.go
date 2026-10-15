package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

// webConfig is the editable subset of Config exposed over the API.
type webConfig struct {
	CronSchedule   string `json:"cron_schedule"`
	Timezone       string `json:"timezone"`
	RunOnStart     bool   `json:"run_on_start"`
	AutoUpdate     bool   `json:"auto_update"`
	CommandTimeout string `json:"command_timeout"`
}

type webState struct {
	Config    webConfig        `json:"config"`
	Projects  []composeProject `json:"projects"`
	Results   []projectResult  `json:"results"`
	Enabled   map[string]bool  `json:"enabled"`
	Compose   string           `json:"compose"`
	Running   bool             `json:"running"`
	NextRun   string           `json:"next_run,omitempty"`
	LastRun   string           `json:"last_run,omitempty"`
	ScanError string           `json:"scan_error,omitempty"`
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
		go func() {
			res := u.checkAndUpdate(p)
			u.recordResult(res)
		}()
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

// recordResult merges a single-project result into the last scan results so the
// UI reflects manual actions too.
func (u *updater) recordResult(res projectResult) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, r := range u.lastResults {
		if r.File == res.File {
			u.lastResults[i] = res
			return
		}
	}
	u.lastResults = append(u.lastResults, res)
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
	u.cfg.mu.Unlock()

	u.mu.Lock()
	results := append([]projectResult(nil), u.lastResults...)
	running := u.running
	next := u.nextRun
	last := u.lastRun
	u.mu.Unlock()

	projects, err := u.discoverRunningProjects(context.Background())
	st := webState{
		Config:   wc,
		Projects: projects,
		Results:  results,
		Enabled:  enabled,
		Compose:  u.compose.display,
		Running:  running,
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
