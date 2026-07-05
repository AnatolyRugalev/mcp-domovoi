package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callAsRoot re-executes this same binary under sudo in "worker" mode and
// forwards one tool call to it over stdio, returning the worker's result. The
// worker runs the tool's normal Go code as root, so all behavior (line
// numbering, UTF-8 checks, diff snippets, the path allowlist) is identical --
// only the effective user differs. The input is passed through unchanged; the
// worker ignores its sudo flag because it is already elevated.
func (d *domovoi) callAsRoot(ctx context.Context, tool string, args any) (*mcp.CallToolResult, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating own binary to elevate: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sudo", "-n", exe, "worker",
		"--name", d.name,
		"--allowed-dirs", strings.Join(d.allowedDirs, ":"))
	// Capture the worker/sudo stderr so a failure (e.g. sudo needs a password)
	// produces a useful message rather than an opaque transport error.
	stderr := &tailBuffer{limit: 8 * 1024}
	cmd.Stderr = stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "domovoi", Version: version}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("could not elevate via sudo (%w): %s", err, msg)
		}
		return nil, fmt.Errorf("could not elevate via sudo: %w", err)
	}
	defer session.Close()

	return session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
}

// errorFromResult turns a worker CallToolResult that reported a tool error back
// into a Go error, so the parent re-wraps it as this server's own tool error.
func errorFromResult(res *mcp.CallToolResult) error {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if b.Len() == 0 {
		return errors.New("elevated call failed")
	}
	return errors.New(b.String())
}

// remarshal reserializes from into to via JSON; used to recover a typed output
// value from a worker result's structured content.
func remarshal(from, to any) error {
	raw, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, to)
}

// runWorker serves a single MCP session over stdio as the current (root) user.
// It is reached only via `domovoi worker`, which callAsRoot invokes under sudo.
func runWorker(args []string) error {
	fs := flag.NewFlagSet("domovoi worker", flag.ContinueOnError)
	name := fs.String("name", "", "human name for this host (mirrors the parent's --name)")
	allowed := fs.String("allowed-dirs", "/", "colon-separated path prefixes the file tools may touch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dirs, err := resolveAllowedDirs(*allowed)
	if err != nil {
		return err
	}
	// stdout is the MCP transport here, so logs must go to stderr (the parent
	// captures it and surfaces it only on failure).
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	d := &domovoi{name: hostName(*name), allowedDirs: dirs, log: logger, elevated: true}
	return newMCPServer(d).Run(context.Background(), &mcp.StdioTransport{})
}
