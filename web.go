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
	"sort"
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
	Proxies     []string          `json:"proxies"`
	ActiveProxy string            `json:"active_proxy,omitempty"`
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

	// Cancel a running check or lifecycle action.
	mux.HandleFunc("/api/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			File string `json:"file"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		file := strings.TrimSpace(body.File)
		if file == "" {
			http.Error(w, "file is required", http.StatusBadRequest)
			return
		}
		if !u.cancelTask(file) {
			http.Error(w, "nothing is running for this project", http.StatusConflict)
			return
		}
		writeJSON(w, map[string]string{"status": "cancelling"})
	})

	// Manage image proxy prefixes: add, remove, or activate one.
	mux.HandleFunc("/api/proxy", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			URL    string `json:"url"`
			Action string `json:"action"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		var err error
		switch strings.ToLower(strings.TrimSpace(body.Action)) {
		case "add":
			err = u.cfg.AddImageProxy(body.URL)
		case "remove":
			err = u.cfg.RemoveImageProxy(body.URL)
		case "activate":
			err = u.cfg.SetActiveImageProxy(body.URL)
		case "deactivate":
			err = u.cfg.SetActiveImageProxy("")
		default:
			http.Error(w, "unsupported action", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, u.state())
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

	// List directories for the folder picker used when saving a converted
	// compose file. Only directories are returned, never file contents.
	mux.HandleFunc("/api/browse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimSpace(r.URL.Query().Get("path"))
		if p == "" {
			p = "/"
		}
		if !filepath.IsAbs(p) {
			p = "/" + p
		}
		p = filepath.Clean(p)
		// If the requested path does not exist yet (e.g. the suggested
		// converted-project folder), start at its nearest existing ancestor.
		for {
			if info, err := os.Stat(p); err == nil && info.IsDir() {
				break
			}
			parent := filepath.Dir(p)
			if parent == p {
				p = "/"
				break
			}
			p = parent
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type dirEntry struct {
			Name string `json:"name"`
			Path string `json:"path"`
		}
		dirs := make([]dirEntry, 0, len(entries))
		for _, e := range entries {
			full := filepath.Join(p, e.Name())
			info, err := os.Stat(full)
			if err != nil || !info.IsDir() {
				continue
			}
			dirs = append(dirs, dirEntry{Name: e.Name(), Path: full})
			if len(dirs) >= 500 {
				break
			}
		}
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
		parent := filepath.Dir(p)
		if p == "/" {
			parent = ""
		}
		writable := false
		if info, err := os.Stat(p); err == nil {
			writable = info.Mode().Perm()&0200 != 0
		}
		writeJSON(w, map[string]any{
			"path":     p,
			"parent":   parent,
			"dirs":     dirs,
			"writable": writable,
		})
	})

	// Create a folder from the directory picker.
	mux.HandleFunc("/api/mkdir", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Parent string `json:"parent"`
			Name   string `json:"name"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		parent := strings.TrimSpace(body.Parent)
		name := strings.TrimSpace(body.Name)
		if parent == "" || !filepath.IsAbs(parent) {
			http.Error(w, "parent must be an absolute path", http.StatusBadRequest)
			return
		}
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			http.Error(w, "invalid folder name", http.StatusBadRequest)
			return
		}
		path := filepath.Join(parent, name)
		if err := os.Mkdir(path, 0755); err != nil {
			if errors.Is(err, os.ErrExist) {
				http.Error(w, "folder already exists", http.StatusConflict)
				return
			}
			http.Error(w, "create failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"path": path})
	})

	// Import a compose file (or the folder containing it) so it is listed even
	// before it has been started. The file must be visible inside the container.
	mux.HandleFunc("/api/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Dir string `json:"dir"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		file, err := findComposeFiles(body.Dir, 3)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(file) == 0 {
			http.Error(w, "no compose file found in that folder or within 3 subfolder levels", http.StatusBadRequest)
			return
		}
		imported := make([]string, 0, len(file))
		warnings := []string{}
		for i, f := range file {
			if err := u.cfg.AddImportedProject(f); err != nil {
				warnings = append(warnings, "import "+f+" failed: "+err.Error())
				continue
			}
			imported = append(imported, f)
			// Validate the first few files only, so importing a large tree
			// stays fast.
			if i < 20 {
				if msg := u.validateComposeFile(r.Context(), filepath.Dir(f), f); msg != "" {
					warnings = append(warnings, f+": "+msg)
				}
			}
		}
		if len(imported) == 0 {
			http.Error(w, "could not import any compose files", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"status":   "imported",
			"files":    imported,
			"warnings": warnings,
		})
	})

	// Remove an imported compose file from the list. The file itself is left
	// untouched; running projects must be stopped and are not removable.
	mux.HandleFunc("/api/unimport", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			File    string `json:"file"`
			Project string `json:"project"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		p, err := u.projectByFile(r.Context(), body.File, body.Project)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if !p.Imported || p.Running {
			http.Error(w, "only imported projects without running containers can be removed", http.StatusBadRequest)
			return
		}
		if err := u.cfg.RemoveImportedProject(p.File); err != nil {
			http.Error(w, "remove failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"status": "removed", "file": p.File})
	})

	// Delete a stopped project: remove its stopped containers (compose down for
	// Compose projects, docker rm for standalone containers). Running projects
	// are refused. Volumes and compose files are kept.
	mux.HandleFunc("/api/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			File    string `json:"file"`
			Project string `json:"project"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		var p composeProject
		var err error
		if strings.HasPrefix(body.File, standaloneFilePrefix) {
			p, err = u.standaloneContainer(r.Context(), strings.TrimPrefix(body.File, standaloneFilePrefix))
		} else {
			p, err = u.projectByFile(r.Context(), body.File, body.Project)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if p.Running {
			http.Error(w, "stop the project before removing it", http.StatusBadRequest)
			return
		}
		// Removing only hides the entry from the list. Containers, volumes and
		// files on disk are never deleted.
		detail := "removed from list (containers and files kept)"
		if p.Imported && p.Total == 0 {
			if err := u.cfg.RemoveImportedProject(p.File); err != nil {
				http.Error(w, "remove failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			key := p.File
			if key == "" {
				key = p.Project
			}
			if err := u.cfg.AddHiddenProject(key); err != nil {
				http.Error(w, "remove failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
			// Drop an import entry too, so the stopped project does not
			// reappear from the imported list.
			if err := u.cfg.RemoveImportedProject(p.File); err != nil {
				log.Printf("cannot remove import entry for %s: %v", p.File, err)
			}
		}
		writeJSON(w, map[string]any{"status": "removed", "file": p.File, "detail": detail})
	})

	// Convert a standalone container to compose: return generated compose.yaml content.
	mux.HandleFunc("/api/convert", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimSpace(r.URL.Query().Get("project"))
		if name == "" {
			http.Error(w, "project required", http.StatusBadRequest)
			return
		}
		// Only containers discovered as standalone can be converted; this keeps
		// the unauthenticated endpoint from inspecting arbitrary containers.
		if _, err := u.standaloneContainer(r.Context(), name); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		content, warnings, err := generateComposeFromContainer(r.Context(), u, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{
			"project":      name,
			"content":      content,
			"warnings":     warnings,
			"suggestedDir": u.suggestedConvertDir(r.Context()),
		})
	})

	// Save compose.yaml to a new directory (for converted projects).
	mux.HandleFunc("/api/compose/save-new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxComposeBytes+64*1024)
		var body struct {
			Dir     string `json:"dir"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Dir) == "" {
			http.Error(w, "dir required", http.StatusBadRequest)
			return
		}
		if !filepath.IsAbs(body.Dir) {
			http.Error(w, "dir must be an absolute path", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(body.Dir, 0755); err != nil {
			http.Error(w, "mkdir failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		path := body.Dir + "/compose.yaml"
		if err := os.WriteFile(path, []byte(body.Content), 0644); err != nil {
			http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Validate the generated file so a bad edit is reported while the
		// editor is still open. The file is kept either way. Importing it makes
		// the new project appear in the list as stopped, ready to start.
		warning := u.validateComposeFile(r.Context(), body.Dir, path)
		if err := u.cfg.AddImportedProject(path); err != nil {
			warning = strings.TrimSpace(warning + " import failed: " + err.Error())
		}
		writeJSON(w, map[string]any{"status": "saved", "file": path, "warning": warning})
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
		var p composeProject
		var err error
		if strings.HasPrefix(body.File, standaloneFilePrefix) {
			name := strings.TrimPrefix(body.File, standaloneFilePrefix)
			if body.Project != "" && body.Project != name {
				http.Error(w, "project does not match the container", http.StatusBadRequest)
				return
			}
			p, err = u.standaloneContainer(r.Context(), name)
		} else {
			p, err = u.projectByFile(r.Context(), body.File, body.Project)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if p.Compose {
			if !p.Mounted {
				http.Error(w, "compose file is not readable inside the container; mount its directory to enable this action", http.StatusBadRequest)
				return
			}
		} else if action == "recreate" {
			http.Error(w, "recreate needs a Compose project; convert this container first", http.StatusBadRequest)
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
		if _, ok := u.startLogDetail(p, "manual", action, progress); !ok {
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
		if u.logs[i].File == file && (u.logs[i].Status == "processing" || u.logs[i].Status == "cancelling") {
			return true
		}
	}
	return false
}

// cancelTask asks the operation running for the given key to stop. It returns
// false when nothing is running, so the API can report a stale request.
func (u *updater) cancelTask(key string) bool {
	u.mu.Lock()
	task, ok := u.tasks[key]
	if !ok {
		u.mu.Unlock()
		return false
	}
	for i := range u.logs {
		if u.logs[i].File == key && u.logs[i].Status == "processing" {
			u.logs[i].Status = "cancelling"
			u.logs[i].Detail = "cancelling…"
			break
		}
	}
	for i := range u.lastResults {
		if u.lastResults[i].File == key && u.lastResults[i].Status == "processing" {
			u.lastResults[i].Status = "cancelling"
			u.lastResults[i].Detail = "cancelling…"
			break
		}
	}
	u.mu.Unlock()
	log.Printf("cancel requested for %s", key)
	task.cancel()
	return true
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
	ctx := u.taskContext(p.Key())
	dir := p.WorkingDir
	if dir == "" {
		dir = filepath.Dir(p.File)
	}
	// fail ends the action with an error, or as cancelled when the user stopped
	// it from the web UI.
	fail := func(detail string) {
		res := nowResult(p, u.cfg.ProjectEnabled(p.Key()))
		res.Trigger = "manual"
		res.Action = action
		if ctx.Err() != nil {
			res.Status, res.Detail = "cancelled", "cancelled by user"
		} else {
			res.Status, res.Detail = "failed", detail
		}
		u.finishLog(res)
		u.recordResult(res)
		log.Printf("[manual] %s -> %s%s", p.Key(), res.Status, detailSuffix(res.Detail))
	}
	sub, extra, ok := actionArgs(action, u.compose.supports)
	if !ok {
		fail(fmt.Sprintf("unsupported action %q", action))
		return
	}
	var argv []string
	if p.Compose {
		if action == "start" && p.Total == 0 {
			// Imported project that has never been created: `compose start`
			// fails because there is no container yet, so bring it up instead.
			args := []string{"-d", "--no-build"}
			if u.compose.supports {
				args = append(args, "--wait")
			}
			argv = u.compose.withProjectFile(p.Project, p.File, "up", args...)
		} else {
			argv = u.compose.withProjectFile(p.Project, p.File, sub, extra...)
		}
	} else if sub == "start" || sub == "stop" || sub == "restart" {
		// Standalone containers have no compose file: use the Docker CLI
		// directly. dir stays empty because there is no project directory.
		dir = ""
		argv = []string{"docker", sub, p.Project}
	} else {
		fail(fmt.Sprintf("action %q is not available for standalone containers", action))
		return
	}
	out, err := u.runCmdStream(ctx, dir, argv, func(line string) {
		u.progressLog(p.Key(), line)
	})
	res := nowResult(p, u.cfg.ProjectEnabled(p.Key()))
	res.Trigger = "manual"
	res.Action = action
	if err != nil {
		if ctx.Err() != nil {
			res.Status, res.Detail = "cancelled", "cancelled by user"
		} else {
			res.Status = "failed"
			res.Detail = action + " failed: " + err.Error()
		}
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

// findComposeFiles returns compose files in root and up to maxDepth levels of
// subdirectories below it. One file per directory is returned, preferring the
// conventional names. Hidden directories are skipped.
func findComposeFiles(root string, maxDepth int) ([]string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("folder required")
	}
	if !filepath.IsAbs(root) {
		return nil, errors.New("path must be absolute")
	}
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	names := []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}
	if info.Mode().IsRegular() {
		base := strings.ToLower(filepath.Base(root))
		for _, n := range names {
			if base == n {
				return []string{root}, nil
			}
		}
		return nil, errors.New("not a recognized compose file name")
	}
	if !info.IsDir() {
		return nil, errors.New("not a file or folder")
	}

	const maxFiles = 100
	var found []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if len(found) >= maxFiles {
			return
		}
		for _, n := range names {
			full := filepath.Join(dir, n)
			if fi, err := os.Stat(full); err == nil && fi.Mode().IsRegular() {
				found = append(found, full)
				break
			}
		}
		if depth >= maxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if len(found) >= maxFiles {
				return
			}
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			full := filepath.Join(dir, e.Name())
			fi, err := os.Stat(full)
			if err != nil || !fi.IsDir() {
				continue
			}
			walk(full, depth+1)
		}
	}
	walk(root, 0)
	sort.Strings(found)
	return found, nil
}

// resolveComposeFile accepts a compose file or a directory containing one of the
// conventional compose file names.
func resolveComposeFile(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("folder required")
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("path must be absolute")
	}
	p = filepath.Clean(p)
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	names := []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}
	if info.Mode().IsRegular() {
		base := strings.ToLower(filepath.Base(p))
		for _, n := range names {
			if base == n {
				return p, nil
			}
		}
		return "", errors.New("not a recognized compose file name")
	}
	if !info.IsDir() {
		return "", errors.New("not a file or folder")
	}
	for _, n := range names {
		full := filepath.Join(p, n)
		if fi, err := os.Stat(full); err == nil && fi.Mode().IsRegular() {
			return full, nil
		}
	}
	return "", errors.New("no compose file in folder (expected compose.yaml, compose.yml, docker-compose.yaml, or docker-compose.yml)")
}

// suggestedConvertDir picks a starting folder for the convert save flow: the
// most common parent of mounted Compose projects, so the picker opens where
// the user's stacks actually live. Falls back to /tmp/converted.
func (u *updater) suggestedConvertDir(ctx context.Context) string {
	projects, err := u.discoverRunningProjects(ctx)
	if err == nil {
		counts := map[string]int{}
		for _, p := range projects {
			if !p.Compose || p.Self || p.WorkingDir == "" {
				continue
			}
			dir := filepath.Dir(p.WorkingDir)
			if dir == "/" || strings.HasPrefix(dir, "/tmp") {
				continue
			}
			counts[dir]++
		}
		best, bestCount := "", 0
		for dir, n := range counts {
			if n > bestCount || (n == bestCount && dir < best) {
				best, bestCount = dir, n
			}
		}
		if best != "" {
			return best
		}
	}
	return "/tmp/converted"
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

// validateComposeFile checks a compose file that is not a known project yet,
// such as a freshly converted standalone container.
func (u *updater) validateComposeFile(ctx context.Context, dir, file string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := u.runCmd(ctx, dir, u.compose.withFile(file, "config", "--quiet")); err != nil {
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
	return u.startLogDetail(p, trigger, "check", "checking…")
}

// startLogDetail is startLog with an explicit action and in-progress message,
// used by lifecycle actions (starting/stopping/restarting).
func (u *updater) startLogDetail(p composeProject, trigger, action, detail string) (time.Time, bool) {
	now := time.Now()
	entry := checkLog{
		When:      now.Format(time.RFC3339),
		Trigger:   trigger,
		Project:   p.Project,
		File:      p.File,
		Status:    "processing",
		Detail:    detail,
		Action:    action,
		startedAt: now,
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range u.logs {
		if u.logs[i].File == p.File && (u.logs[i].Status == "processing" || u.logs[i].Status == "cancelling") {
			log.Printf("[%s] %s already being checked; ignoring duplicate trigger", trigger, p.Project)
			return time.Time{}, false
		}
	}
	// Register a cancellable context for this operation so the UI can stop it.
	u.newTaskContext(p.File)
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
		Status:    "processing",
		Detail:    detail,
		When:      now.Format(time.RFC3339),
		UpdatedAt: u.cfg.LastUpdate(p.File),
		Trigger:   trigger,
		Action:    action,
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
		if u.logs[i].File == key && u.logs[i].Status == "processing" {
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
		Action:  res.Action,
	}
	u.mu.Lock()
	cancel := u.dropTask(res.File)
	replaced := false
	for i := range u.logs {
		if u.logs[i].File == res.File && (u.logs[i].Status == "processing" || u.logs[i].Status == "cancelling") {
			if !u.logs[i].startedAt.IsZero() {
				entry.DurationMs = time.Since(u.logs[i].startedAt).Milliseconds()
			}
			u.logs[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		if entry.When != "" {
			if started, err := time.Parse(time.RFC3339, entry.When); err == nil {
				entry.DurationMs = time.Since(started).Milliseconds()
			}
		}
		u.logs = append([]checkLog{entry}, u.logs...)
		if len(u.logs) > maxCheckLogs {
			u.logs = u.logs[:maxCheckLogs]
		}
	}
	u.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	log.Printf("[%s] %s -> %s in %s%s", entry.Trigger, entry.Project, entry.Status,
		time.Duration(entry.DurationMs)*time.Millisecond, detailSuffix(entry.Detail))
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
	proxies := append([]string{}, u.cfg.ImageProxies...)
	activeProxy := u.cfg.ActiveProxy
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
		Proxies:     proxies,
		ActiveProxy: activeProxy,
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
