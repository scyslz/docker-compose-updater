package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseCronAndNext(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ expr, after, want string }{
		{"0 4 * * *", "2026-10-15T05:00:00+08:00", "2026-10-16T04:00:00+08:00"},
		{"*/15 * * * *", "2026-10-15T04:07:00+08:00", "2026-10-15T04:15:00+08:00"},
		{"0 3 * * 1-5", "2026-10-16T04:00:00+08:00", "2026-10-19T03:00:00+08:00"},
		{"0 4 1 * *", "2026-10-15T05:00:00+08:00", "2026-11-01T04:00:00+08:00"},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			c, err := parseCron(tt.expr)
			if err != nil {
				t.Fatal(err)
			}
			after, err := time.Parse(time.RFC3339, tt.after)
			if err != nil {
				t.Fatal(err)
			}
			got := c.Next(after.In(loc)).Format(time.RFC3339)
			if got != tt.want {
				t.Fatalf("Next(%s) = %s, want %s", tt.after, got, tt.want)
			}
		})
	}
}

func TestParseCronRejectsInvalid(t *testing.T) {
	for _, expr := range []string{"", "0 4 * *", "61 * * * *", "0 25 * * *", "*/0 * * * *"} {
		if _, err := parseCron(expr); err == nil {
			t.Errorf("parseCron(%q) expected error", expr)
		}
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" /data/a ; /opt/b, /srv/c ")
	if len(got) != 3 || got[0] != "/data/a" || got[1] != "/opt/b" || got[2] != "/srv/c" {
		t.Fatalf("unexpected splitList result: %#v", got)
	}
}

func TestComposeCommandArgs(t *testing.T) {
	v2 := composeCommand{bin: "docker", plugin: []string{"compose"}, v2: true, supports: true, display: "docker compose"}
	got := strings.Join(v2.args("config", "-f", "c.yaml", "--format", "json"), " ")
	want := "docker compose config -f c.yaml --format json"
	if got != want {
		t.Fatalf("v2 args = %q, want %q", got, want)
	}

	v1 := composeCommand{bin: "docker-compose", v2: false, supports: false, display: "docker-compose"}
	got = strings.Join(v1.args("ps", "-f", "c.yaml", "--services"), " ")
	want = "docker-compose ps -f c.yaml --services"
	if got != want {
		t.Fatalf("v1 args = %q, want %q", got, want)
	}
	if v1.supports {
		t.Fatal("v1 should not advertise --wait support")
	}
}

func TestComposeWithFile(t *testing.T) {
	v2 := composeCommand{bin: "docker", plugin: []string{"compose"}, v2: true}
	got := strings.Join(v2.withFile("c.yaml", "up", "-d", "--wait", "web"), " ")
	want := "docker compose -f c.yaml up -d --wait web"
	if got != want {
		t.Fatalf("withFile = %q, want %q", got, want)
	}
}

func TestNormalizeImageID(t *testing.T) {
	if got := normalizeImageID("sha256:abc"); got != "abc" {
		t.Fatalf("normalizeImageID = %q, want abc", got)
	}
	if got := normalizeImageID("abc"); got != "abc" {
		t.Fatalf("normalizeImageID = %q, want abc", got)
	}
}

func TestHasBuild(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{``, false},
		{`null`, false},
		{`""`, false},
		{`{}`, false},
		{`{"context":"."}`, true},
		{`{"context":".","dockerfile":"Dockerfile"}`, true},
		{`"."`, true},
		{`"./dir"`, true},
	}
	for _, c := range cases {
		if got := hasBuild(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("hasBuild(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}
