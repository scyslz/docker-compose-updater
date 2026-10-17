package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyServices(t *testing.T) {
	services := map[string]serviceConfig{
		"image-only":  {Image: "nginx:alpine"},
		"image-build": {Image: "ghcr.io/x/y:latest", Build: json.RawMessage(`"."`)},
		"build-only":  {Build: json.RawMessage(`{"context":"."}`)},
		"no-image":    {},
		"not-running": {Image: "redis:7"},
	}
	running := map[string]bool{
		"image-only":  true,
		"image-build": true,
		"build-only":  true,
		"no-image":    true,
	}

	eligible, buildOnly, notRunning, noImage := classifyServices(services, running)
	if strings.Join(eligible, ",") != "image-build,image-only" {
		t.Fatalf("eligible = %v, want [image-build image-only]", eligible)
	}
	if strings.Join(buildOnly, ",") != "build-only" {
		t.Fatalf("buildOnly = %v, want [build-only]", buildOnly)
	}
	if strings.Join(noImage, ",") != "no-image" {
		t.Fatalf("noImage = %v, want [no-image]", noImage)
	}
	if strings.Join(notRunning, ",") != "not-running" {
		t.Fatalf("notRunning = %v, want [not-running]", notRunning)
	}
}

func TestSelfUpdateHelperArgsV2(t *testing.T) {
	cc := composeCommand{bin: "docker", plugin: []string{"compose"}, v2: true, supports: true, display: "docker compose"}
	args := selfUpdateHelperArgs(cc, "sha256:abc", "/srv/app", "myproj", "/srv/app/compose.yaml",
		[]string{"-d", "--no-deps", "--no-build", "--wait", "web"},
		[]string{"COMPOSE_AUTO_UPDATER_IMAGE=ghcr.io/x/y:pinned"})

	got := strings.Join(args, " ")
	want := "run --rm -d --entrypoint docker " +
		"-v /var/run/docker.sock:/var/run/docker.sock " +
		"-v /srv/app:/srv/app -w /srv/app " +
		"-e COMPOSE_AUTO_UPDATER_IMAGE=ghcr.io/x/y:pinned sha256:abc " +
		"compose -p myproj -f /srv/app/compose.yaml up -d --no-deps --no-build --wait web"
	if got != want {
		t.Fatalf("v2 helper args mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestSelfUpdateHelperArgsV1(t *testing.T) {
	cc := composeCommand{bin: "docker-compose", v2: false, display: "docker-compose"}
	args := selfUpdateHelperArgs(cc, "docker:cli", "/srv/app", "myproj", "/srv/app/compose.yaml",
		[]string{"-d", "--no-deps", "--no-build", "web"}, nil)

	got := strings.Join(args, " ")
	want := "run --rm -d --entrypoint docker-compose " +
		"-v /var/run/docker.sock:/var/run/docker.sock " +
		"-v /srv/app:/srv/app -w /srv/app docker:cli " +
		"-p myproj -f /srv/app/compose.yaml up -d --no-deps --no-build web"
	if got != want {
		t.Fatalf("v1 helper args mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestDryRunShowsChanges(t *testing.T) {
	unchanged := " DRY-RUN MODE -  Container helium  Running\n" +
		" DRY-RUN MODE -  Container helium  Waiting\n" +
		" DRY-RUN MODE -  Container helium  Healthy\n"
	if dryRunShowsChanges(unchanged) {
		t.Fatal("unchanged dry-run output reported a change")
	}

	changed := " DRY-RUN MODE -  Container helium  Recreate\n" +
		" DRY-RUN MODE -  Container helium  Recreated\n" +
		" DRY-RUN MODE -  Container abc_helium  Starting\n"
	if !dryRunShowsChanges(changed) {
		t.Fatal("changed dry-run output was not detected")
	}

	created := " DRY-RUN MODE -  Container helium  Create\n"
	if !dryRunShowsChanges(created) {
		t.Fatal("created container was not detected")
	}
}

func TestComposeEnv(t *testing.T) {
	got := composeEnv([]string{
		"PATH=/usr/bin",
		"COMPOSE_PROFILES=web",
		"COMPOSE_AUTO_UPDATER_IMAGE=ghcr.io/x/y:pinned",
	})
	want := []string{
		"COMPOSE_PROFILES=web",
		"COMPOSE_AUTO_UPDATER_IMAGE=ghcr.io/x/y:pinned",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("composeEnv = %v, want %v", got, want)
	}
}
