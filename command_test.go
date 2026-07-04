package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunCommandBasic(t *testing.T) {
	d := testDomovoi(t)
	_, out, err := d.runCommand(context.Background(), nil, runCommandInput{Command: "echo hello; echo oops >&2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.Stdout) != "hello" {
		t.Errorf("stdout = %q", out.Stdout)
	}
	if strings.TrimSpace(out.Stderr) != "oops" {
		t.Errorf("stderr = %q", out.Stderr)
	}
	if out.ExitCode != 0 || out.TimedOut {
		t.Errorf("exit_code = %d, timed_out = %v", out.ExitCode, out.TimedOut)
	}
}

func TestRunCommandNonZeroExitIsNotAnError(t *testing.T) {
	d := testDomovoi(t)
	_, out, err := d.runCommand(context.Background(), nil, runCommandInput{Command: "exit 3"})
	if err != nil {
		t.Fatalf("non-zero exit must not be a tool error: %v", err)
	}
	if out.ExitCode != 3 {
		t.Errorf("exit_code = %d, want 3", out.ExitCode)
	}
}

func TestRunCommandWorkdir(t *testing.T) {
	d := testDomovoi(t)
	dir := t.TempDir()
	_, out, err := d.runCommand(context.Background(), nil, runCommandInput{Command: "pwd", Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// The tempdir may itself sit behind a symlink (e.g. /tmp on some systems),
	// so compare the basename.
	if !strings.Contains(out.Stdout, dir) && !strings.Contains(dir, strings.TrimSpace(out.Stdout)) {
		t.Errorf("pwd = %q, want %q", strings.TrimSpace(out.Stdout), dir)
	}

	if _, _, err := d.runCommand(context.Background(), nil, runCommandInput{Command: "true", Workdir: "/does/not/exist"}); err == nil {
		t.Error("nonexistent workdir accepted")
	}
}

func TestRunCommandTruncatesTail(t *testing.T) {
	d := testDomovoi(t)
	// 200KB of numbered lines; the tail (highest numbers) must survive.
	_, out, err := d.runCommand(context.Background(), nil, runCommandInput{Command: "seq 1 30000"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Stdout) > maxOutputBytes+200 {
		t.Errorf("stdout not truncated: %d bytes", len(out.Stdout))
	}
	if !strings.Contains(out.Stdout, "output truncated") {
		t.Error("missing truncation marker")
	}
	if !strings.Contains(out.Stdout, "30000") {
		t.Error("tail of output missing; truncation kept the head instead")
	}
	if strings.Contains(out.Stdout, "\n1\n") {
		t.Error("head of output present; truncation should keep the tail")
	}
}

func TestRunCommandTimeoutKillsProcessGroup(t *testing.T) {
	d := testDomovoi(t)
	start := time.Now()
	// The background child inherits the pipe; if only the shell were killed,
	// waiting on output copiers would hang until the grandchild exits.
	_, out, err := d.runCommand(context.Background(), nil, runCommandInput{
		Command:        "sleep 60 & sleep 60",
		TimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.TimedOut {
		t.Error("timed_out = false, want true")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v; process group was not killed", elapsed)
	}
	if out.ExitCode == 0 {
		t.Errorf("exit_code = 0 after timeout kill")
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{limit: 10}
	tb.Write([]byte("hello"))
	if got := tb.String(); got != "hello" {
		t.Errorf("short write: %q", got)
	}
	tb.Write([]byte("0123456789abcdef"))
	got := tb.String()
	if !strings.HasSuffix(got, "6789abcdef") {
		t.Errorf("tail not kept: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("missing marker: %q", got)
	}
}
