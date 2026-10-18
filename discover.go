package main

import (
	"context"
	"os"
	"sort"
	"strings"
)

// composeProject describes a Docker Compose project discovered from the Docker
// daemon (no filesystem scan required to list it). Both running and stopped
// projects are listed; the Running flag distinguishes them.
type composeProject struct {
	Project     string `json:"project"`        // com.docker.compose.project
	File        string `json:"file"`           // primary compose file path (host view)
	ConfigFiles string `json:"config_files"`   // raw config_files label (may be comma-separated)
	WorkingDir  string `json:"working_dir"`    // com.docker.compose.project.working_dir
	Mounted     bool   `json:"mounted"`        // whether File is readable inside this container
	Writable    bool   `json:"writable"`       // whether File can be edited through the web UI
	Self        bool   `json:"self,omitempty"` // this updater's own project
	Running     bool   `json:"running"`        // whether the project has any running container
	Services    int    `json:"services"`       // number of running containers in the project
	Total       int    `json:"total"`          // number of containers (running or stopped)
}

// discoverRunningProjects lists compose projects by reading Docker container
// labels, including stopped ones. It requires no directory mounting; the
// "Mounted" flag reports whether the compose file is readable inside this
// container. Stopped projects are only updatable manually (the scheduler skips
// them).
func (u *updater) discoverRunningProjects(ctx context.Context) ([]composeProject, error) {
	// Inspect all containers (running and stopped) so stopped projects are
	// listed too.
	idsOut, err := u.runDocker(ctx, "", []string{"ps", "-aq"})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(string(idsOut), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	const format = `{{index .Config.Labels "com.docker.compose.project"}}|` +
		`{{index .Config.Labels "com.docker.compose.project.config_files"}}|` +
		`{{index .Config.Labels "com.docker.compose.project.working_dir"}}|` +
		`{{.State.Running}}|{{.Id}}`
	inspectArgs := append([]string{"inspect", "--format", format}, ids...)
	out, err := u.runDocker(ctx, "", inspectArgs)
	if err != nil {
		return nil, err
	}

	byKey := map[string]*composeProject{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 5)
		if len(parts) != 5 {
			continue
		}
		// Docker's Go templates print "<no value>" for missing labels; treat it
		// as empty so containers that are not part of a Compose project are
		// skipped instead of appearing as a bogus project.
		field := func(i int) string {
			s := strings.TrimSpace(parts[i])
			if s == "<no value>" {
				return ""
			}
			return s
		}
		proj, configFiles, workdir, running, containerID := field(0), field(1), field(2), field(3), field(4)
		if proj == "" {
			continue // not a compose container
		}
		key := proj + "\x00" + configFiles
		p, ok := byKey[key]
		if !ok {
			primary := primaryComposeFile(configFiles)
			p = &composeProject{
				Project:     proj,
				File:        primary,
				ConfigFiles: configFiles,
				WorkingDir:  workdir,
				Mounted:     primary != "" && fileReadable(primary),
				Writable:    primary != "" && fileWritable(primary),
			}
			byKey[key] = p
		}
		if u.selfID != "" && strings.HasPrefix(strings.TrimSpace(containerID), u.selfID) {
			p.Self = true
		}
		p.Total++
		if strings.TrimSpace(running) == "true" {
			p.Running = true
			p.Services++
		}
	}

	projects := make([]composeProject, 0, len(byKey))
	for _, p := range byKey {
		projects = append(projects, *p)
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Project != projects[j].Project {
			return projects[i].Project < projects[j].Project
		}
		return projects[i].File < projects[j].File
	})
	return projects, nil
}

// primaryComposeFile returns the first file from a config_files label value,
// which may be a comma-separated list when the project was started with
// multiple -f flags.
func primaryComposeFile(configFiles string) string {
	for _, f := range strings.Split(configFiles, ",") {
		if f = strings.TrimSpace(f); f != "" {
			return f
		}
	}
	return ""
}

// projectRunning reports whether the compose project currently has at least one
// running container, by filtering on the project label.
func (u *updater) projectRunning(ctx context.Context, project string) bool {
	if strings.TrimSpace(project) == "" {
		return false
	}
	out, err := u.runDocker(ctx, "", []string{"ps", "-q", "--filter", "label=com.docker.compose.project=" + project})
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// fileReadable reports whether path exists and can be opened for reading.
func fileReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// fileWritable reports whether path can be opened for writing. It does not
// modify the file or its contents.
func fileWritable(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// projectKey is the stable identifier used in config and API responses.
func (p composeProject) Key() string {
	if p.File != "" {
		return p.File
	}
	return p.Project
}
