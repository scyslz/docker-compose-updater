package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

// webConfig is the editable subset of Config exposed over the API.
type webConfig struct {
	IncludeDirs    []string `json:"include_dirs"`
	ExcludeDirs    []string `json:"exclude_dirs"`
	ScanDepth      int      `json:"scan_depth"`
	CronSchedule   string   `json:"cron_schedule"`
	Timezone       string   `json:"timezone"`
	RunOnStart     bool     `json:"run_on_start"`
	AutoUpdate     bool     `json:"auto_update"`
	CommandTimeout string   `json:"command_timeout"`
}

type webState struct {
	Config   webConfig       `json:"config"`
	Projects []string        `json:"projects"`
	Results  []projectResult `json:"results"`
	Disabled map[string]bool `json:"disabled"`
	Compose  string          `json:"compose"`
	Running  bool            `json:"running"`
	NextRun  string          `json:"next_run,omitempty"`
	LastRun  string          `json:"last_run,omitempty"`
}

// startWeb runs the HTTP management UI until the process exits. It is a no-op
// when WEB_ADDR is set to "off" or empty-after-trim of "disable".
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

func (u *updater) state() webState {
	u.cfg.mu.Lock()
	wc := webConfig{
		IncludeDirs:    append([]string(nil), u.cfg.IncludeDirs...),
		ExcludeDirs:    append([]string(nil), u.cfg.ExcludeDirs...),
		ScanDepth:      u.cfg.ScanDepth,
		CronSchedule:   u.cfg.CronSchedule,
		Timezone:       u.cfg.Timezone,
		RunOnStart:     u.cfg.RunOnStartEnabled(),
		AutoUpdate:     u.cfg.AutoUpdateEnabled(),
		CommandTimeout: u.cfg.CommandTimeout,
	}
	disabled := map[string]bool{}
	for k, v := range u.cfg.DisabledProjects {
		disabled[k] = v
	}
	u.cfg.mu.Unlock()

	// Discover projects so the UI can list them even before a scan.
	projects := u.discover()

	u.mu.Lock()
	results := append([]projectResult(nil), u.lastResults...)
	running := u.running
	next := u.nextRun
	last := u.lastRun
	u.mu.Unlock()

	st := webState{
		Config:   wc,
		Projects: projects,
		Results:  results,
		Disabled: disabled,
		Compose:  u.compose.display,
		Running:  running,
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
// per-project disabled set.
func (u *updater) applyConfig(wc webConfig) error {
	// Validate cron and timezone before committing.
	if _, err := time.LoadLocation(wc.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q", wc.Timezone)
	}
	if _, err := parseCron(wc.CronSchedule); err != nil {
		return fmt.Errorf("invalid cron %q: %w", wc.CronSchedule, err)
	}
	if _, err := time.ParseDuration(wc.CommandTimeout); err != nil {
		return fmt.Errorf("invalid command timeout %q", wc.CommandTimeout)
	}

	u.cfg.mu.Lock()
	u.cfg.IncludeDirs = wc.IncludeDirs
	u.cfg.ExcludeDirs = wc.ExcludeDirs
	u.cfg.ScanDepth = wc.ScanDepth
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
