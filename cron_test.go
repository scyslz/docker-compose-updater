package main

import (
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
