package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSudoElevation exercises the real re-exec-under-sudo path end to end. It
// builds an actual domovoi binary (the go-test binary does not run our main(),
// so the worker subcommand would be unreachable from an in-process server),
// starts it as an HTTP server, and drives the four tools with sudo=true against
// root-owned files. It is skipped unless passwordless sudo is available.
func TestSudoElevation(t *testing.T) {
	if _, err := exec.LookPath("sudo"); err != nil {
		t.Skip("sudo not installed")
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		t.Skip("passwordless sudo not available; skipping elevation test")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "domovoi")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building domovoi: %v\n%s", err, out)
	}

	// A file only root can read, inside an allowed dir.
	secret := filepath.Join(dir, "secret.txt")
	root := exec.Command("sudo", "-n", "sh", "-c",
		"umask 077; printf 'top secret\\n' > "+secret+"; chown root:root "+secret+"; chmod 600 "+secret)
	if out, err := root.CombinedOutput(); err != nil {
		t.Fatalf("creating root-owned file: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("sudo", "-n", "rm", "-f", secret, filepath.Join(dir, "written.txt")).Run()
	})

	addr := freeAddr(t)
	token := "sudo-test-token"
	srv := exec.Command(bin, "--listen", addr, "--token", token, "--allowed-dirs", dir)
	srv.Stderr = os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Kill()
		_ = srv.Wait()
	})
	waitHealthy(t, addr)

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   "http://" + addr + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token: token}},
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	call := func(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res
	}
	firstText := func(res *mcp.CallToolResult) string {
		if len(res.Content) == 0 {
			return ""
		}
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			return tc.Text
		}
		return ""
	}

	t.Run("read without sudo is denied", func(t *testing.T) {
		res := call(t, "read_file", map[string]any{"path": secret})
		if !res.IsError {
			t.Fatal("expected permission error reading root-owned file without sudo")
		}
	})

	t.Run("read with sudo succeeds", func(t *testing.T) {
		res := call(t, "read_file", map[string]any{"path": secret, "sudo": true})
		if res.IsError {
			t.Fatalf("read_file sudo error: %s", firstText(res))
		}
		if !strings.Contains(firstText(res), "top secret") {
			t.Fatalf("read_file sudo output: %q", firstText(res))
		}
	})

	t.Run("run_command with sudo runs as root", func(t *testing.T) {
		res := call(t, "run_command", map[string]any{"command": "id -un", "sudo": true})
		if res.IsError {
			t.Fatalf("run_command sudo error: %s", firstText(res))
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("structured content is %T", res.StructuredContent)
		}
		if got := strings.TrimSpace(sc["stdout"].(string)); got != "root" {
			t.Fatalf("id -un = %q, want root", got)
		}
	})

	t.Run("write then edit with sudo", func(t *testing.T) {
		written := filepath.Join(dir, "written.txt")
		res := call(t, "write_file", map[string]any{"path": written, "content": "alpha\n", "sudo": true})
		if res.IsError {
			t.Fatalf("write_file sudo error: %s", firstText(res))
		}
		// Verify it landed as root.
		res = call(t, "run_command", map[string]any{"command": "stat -c %U " + written, "sudo": true})
		sc := res.StructuredContent.(map[string]any)
		if got := strings.TrimSpace(sc["stdout"].(string)); got != "root" {
			t.Fatalf("owner of written file = %q, want root", got)
		}

		res = call(t, "edit_file", map[string]any{"path": written, "old_string": "alpha", "new_string": "beta", "sudo": true})
		if res.IsError {
			t.Fatalf("edit_file sudo error: %s", firstText(res))
		}
		if !strings.Contains(firstText(res), "+beta") {
			t.Fatalf("edit_file sudo diff: %q", firstText(res))
		}
	})
}

// freeAddr returns a currently-unused 127.0.0.1 host:port. There is a small
// race between closing the listener and the server rebinding, acceptable in a
// test.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server at %s never became healthy", addr)
}
