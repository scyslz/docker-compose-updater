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
	args := selfUpdateHelperArgs(cc, "/srv/app", "myproj", "/srv/app/compose.yaml",
		[]string{"-d", "--no-deps", "--no-build", "--wait", "web"})

	got := strings.Join(args, " ")
	want := "run --rm -d -v /var/run/docker.sock:/var/run/docker.sock " +
		"-v /srv/app:/srv/app -w /srv/app docker:cli " +
		"docker compose -p myproj -f /srv/app/compose.yaml up -d --no-deps --no-build --wait web"
	if got != want {
		t.Fatalf("v2 helper args mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestSelfUpdateHelperArgsV1(t *testing.T) {
	cc := composeCommand{bin: "docker-compose", v2: false, display: "docker-compose"}
	args := selfUpdateHelperArgs(cc, "/srv/app", "myproj", "/srv/app/compose.yaml",
		[]string{"-d", "--no-deps", "--no-build", "web"})

	got := strings.Join(args, " ")
	want := "run --rm -d -v /var/run/docker.sock:/var/run/docker.sock " +
		"-v /srv/app:/srv/app -w /srv/app docker:cli " +
		"docker-compose -p myproj -f /srv/app/compose.yaml up -d --no-deps --no-build web"
	if got != want {
		t.Fatalf("v1 helper args mismatch\n got: %s\nwant: %s", got, want)
	}
}
