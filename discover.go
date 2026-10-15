package main

import (
	"context"
	"os"
	"sort"
	"strings"
)

// composeProject describes a running Docker Compose project discovered from the
// Docker daemon (no filesystem scan required to list it).
type composeProject struct {
	Project     string `json:"project"`      // com.docker.compose.project
	File        string `json:"file"`         // primary compose file path (host view)
	ConfigFiles string `json:"config_files"` // raw config_files label (may be comma-separated)
	WorkingDir  string `json:"working_dir"`  // com.docker.compose.project.working_dir
	Mounted     bool   `json:"mounted"`      // whether File is readable inside this container
	Services    int    `json:"services"`     // number of running containers in the project
}

// discoverRunningProjects lists compose projects that currently have running
// containers, by reading Docker container labels. It requires no directory
// mounting; the "Mounted" flag reports whether the compose file is actually
// readable inside this container.
func (u *updater) discoverRunningProjects(ctx context.Context) ([]composeProject, error) {
	// Batch: get all running container IDs, then inspect their labels at once.
	idsOut, err := u.runDocker(ctx, "", []string{"ps", "-q"})
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
		`{{index .Config.Labels "com.docker.compose.project.working_dir"}}`
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
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[0]) == "" {
			continue // not a compose container
		}
		proj, configFiles, workdir := parts[0], parts[1], parts[2]
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
			}
			byKey[key] = p
		}
		p.Services++
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

// fileReadable reports whether path exists and can be opened for reading.
func fileReadable(path string) bool {
	f, err := os.Open(path)
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
