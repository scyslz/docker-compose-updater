package main

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// errUnknownStandalone is returned when the requested container is no longer in
// the discovered standalone list.
var errUnknownStandalone = errors.New("unknown standalone container")

// standaloneFilePrefix marks the synthetic "file" of a standalone container in
// the API and in log/result keys. Such containers have no compose file, but the
// UI and the log bookkeeping need a stable identifier that cannot collide with
// a real path.
const standaloneFilePrefix = "container:"

func standaloneKey(name string) string { return standaloneFilePrefix + name }

// discoverStandaloneContainers lists containers that are NOT part of a Compose
// project, so the UI can offer lifecycle actions and "Convert to Compose" for
// them. Stopped containers are included so they can be started again.
func (u *updater) discoverStandaloneContainers(ctx context.Context) ([]composeProject, error) {
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
	// One inspect for every container, including the Compose project label, so
	// listing does not require a second round-trip per container.
	const format = `{{.Name}}|{{.Config.Image}}|{{.State.Running}}|` +
		`{{index .Config.Labels "com.docker.compose.project"}}|{{.State.Status}}|` +
		`{{if index .State "Health"}}{{index (index .State "Health") "Status"}}{{end}}`
	out, err := u.runDocker(ctx, "", append([]string{"inspect", "--format", format}, ids...))
	if err != nil {
		return nil, err
	}
	var projects []composeProject
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 6)
		if len(parts) != 6 {
			continue
		}
		name := strings.TrimPrefix(strings.TrimSpace(parts[0]), "/")
		running := strings.TrimSpace(parts[2]) == "true"
		composeProjectLabel := strings.TrimSpace(parts[3])
		status := strings.TrimSpace(parts[4])
		health := strings.TrimSpace(parts[5])
		if health == "<no value>" {
			health = ""
		}
		// Skip compose-managed containers (they have the project label).
		if composeProjectLabel != "" && composeProjectLabel != "<no value>" {
			continue
		}
		if name == "" {
			continue
		}
		runningCount := 0
		if running {
			runningCount = 1
		}
		projects = append(projects, composeProject{
			Project:    name,
			File:       standaloneKey(name),
			Compose:    false,
			Running:    running,
			Health:     health,
			Restarting: status == "restarting",
			Services:   runningCount,
			Total:      1,
		})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Project < projects[j].Project })
	return projects, nil
}

// standaloneContainer returns one standalone container from the same allowlist
// used for discovery, so the API cannot act on arbitrary container names.
func (u *updater) standaloneContainer(ctx context.Context, name string) (composeProject, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return composeProject{}, errUnknownStandalone
	}
	list, err := u.discoverStandaloneContainers(ctx)
	if err != nil {
		return composeProject{}, err
	}
	for _, p := range list {
		if p.Project == name {
			return p, nil
		}
	}
	return composeProject{}, errUnknownStandalone
}
