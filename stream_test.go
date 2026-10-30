package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Docker reports pull progress by rewriting one line with carriage returns.
// Those updates must reach the UI immediately instead of waiting for a newline.
func TestRunCmdStreamEmitsCarriageReturnProgress(t *testing.T) {
	u := &updater{cfg: &Config{CommandTimeout: "10s"}}
	var got []string
	out, err := u.runCmdStream(context.Background(), "",
		[]string{"sh", "-c", "printf 'one\\rtwo\\rthree\\n'"}, func(s string) {
			got = append(got, s)
		})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "one,two,three" {
		t.Fatalf("progress lines = %v", got)
	}
	if strings.TrimSpace(string(out)) != "one\ntwo\nthree" {
		t.Fatalf("captured output = %q", out)
	}
}

func TestRunCmdStreamIdleAbortsStalledCommand(t *testing.T) {
	u := &updater{cfg: &Config{CommandTimeout: "10s"}}
	start := time.Now()
	_, err := u.runCmdStreamIdle(context.Background(), "",
		[]string{"sh", "-c", "sleep 5"}, 300*time.Millisecond, nil)
	if !errors.Is(err, errNoProgress) {
		t.Fatalf("err = %v, want errNoProgress", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("stalled command took %s to abort", elapsed)
	}
}

func TestRunCmdStreamIdleKeepsRunningWhileProgressArrives(t *testing.T) {
	u := &updater{cfg: &Config{CommandTimeout: "10s"}}
	_, err := u.runCmdStreamIdle(context.Background(), "",
		[]string{"sh", "-c", "for i in 1 2 3 4 5; do echo tick; sleep 0.1; done"},
		time.Second, nil)
	if err != nil {
		t.Fatalf("continuous output was aborted: %v", err)
	}
}
