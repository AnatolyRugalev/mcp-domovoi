package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultTimeoutSeconds = 60
	maxTimeoutSeconds     = 600
	maxOutputBytes        = 100 * 1024 // per stream, tail kept
)

type runCommandInput struct {
	Command        string `json:"command" jsonschema:"shell command to run (bash -lc, or sh -c if bash is absent)"`
	Workdir        string `json:"workdir,omitempty" jsonschema:"working directory (default: the server's home directory, or /)"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"kill the command after this many seconds (default 60, max 600)"`
}

type runCommandOutput struct {
	Stdout     string `json:"stdout" jsonschema:"captured stdout (last 100KB)"`
	Stderr     string `json:"stderr" jsonschema:"captured stderr (last 100KB)"`
	ExitCode   int    `json:"exit_code" jsonschema:"process exit code; -1 if killed by a signal"`
	DurationMs int64  `json:"duration_ms" jsonschema:"wall-clock run time in milliseconds"`
	TimedOut   bool   `json:"timed_out" jsonschema:"true if the command was killed on timeout"`
}

func (d *domovoi) runCommand(ctx context.Context, req *mcp.CallToolRequest, in runCommandInput) (res *mcp.CallToolResult, out runCommandOutput, err error) {
	start := time.Now()
	defer func() { d.logCall("run_command", in.Command, start, err) }()

	shell, shellArg := "sh", "-c"
	if p, lookErr := exec.LookPath("bash"); lookErr == nil {
		shell, shellArg = p, "-lc"
	}

	workdir := in.Workdir
	if workdir == "" {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			workdir = home
		} else {
			workdir = "/"
		}
	}
	if info, statErr := os.Stat(workdir); statErr != nil || !info.IsDir() {
		return nil, out, fmt.Errorf("workdir %q is not an existing directory", workdir)
	}

	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if in.TimeoutSeconds <= 0 {
		timeout = defaultTimeoutSeconds * time.Second
	} else if in.TimeoutSeconds > maxTimeoutSeconds {
		timeout = maxTimeoutSeconds * time.Second
	}

	cmd := exec.Command(shell, shellArg, in.Command)
	cmd.Dir = workdir
	// New process group so that on timeout we can kill the whole tree, not
	// just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout := &tailBuffer{limit: maxOutputBytes}
	stderr := &tailBuffer{limit: maxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, out, fmt.Errorf("starting command: %w", err)
	}
	killGroup := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timedOut := false
	select {
	case <-done:
	case <-time.After(timeout):
		timedOut = true
		killGroup()
		<-done
	case <-ctx.Done():
		killGroup()
		<-done
	}

	out = runCommandOutput{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ExitCode:   cmd.ProcessState.ExitCode(),
		DurationMs: time.Since(start).Milliseconds(),
		TimedOut:   timedOut,
	}
	return nil, out, nil
}

// tailBuffer keeps the last limit bytes written to it and remembers whether
// anything was discarded. It is safe for concurrent writes (stdout/stderr
// copiers run in separate goroutines).
type tailBuffer struct {
	mu        sync.Mutex
	limit     int
	buf       []byte
	truncated bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		// Trim with hysteresis so repeated writes don't recopy every time.
		if len(t.buf) > 2*t.limit {
			t.buf = append(t.buf[:0:0], t.buf[len(t.buf)-t.limit:]...)
		}
		t.truncated = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	buf := t.buf
	if len(buf) > t.limit {
		buf = buf[len(buf)-t.limit:]
	}
	if t.truncated {
		return fmt.Sprintf("[... output truncated, showing last %d bytes ...]\n%s", len(buf), buf)
	}
	return string(buf)
}
