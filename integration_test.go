package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct{ token string }

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(req)
}

func TestIntegrationHTTP(t *testing.T) {
	scratch := t.TempDir()
	cfg := &config{
		listen:      "unused",
		token:       "test-token",
		path:        "/mcp",
		allowedDirs: []string{"/"},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ts := httptest.NewServer(newHandler(cfg, logger))
	defer ts.Close()

	ctx := context.Background()

	t.Run("healthz without auth", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz status = %d", resp.StatusCode)
		}
	})

	t.Run("bad token gets 401", func(t *testing.T) {
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
		transport := &mcp.StreamableClientTransport{
			Endpoint:   ts.URL + "/mcp",
			HTTPClient: &http.Client{Transport: bearerTransport{token: "wrong-token"}},
		}
		if _, err := client.Connect(ctx, transport, nil); err == nil {
			t.Fatal("connect succeeded with a bad token")
		} else if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "Unauthorized") {
			t.Fatalf("expected a 401/Unauthorized error, got: %v", err)
		}

		// No header at all must also be rejected.
		resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status without auth header = %d, want 401", resp.StatusCode)
		}
	})

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token: "test-token"}},
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	t.Run("lists exactly four tools", func(t *testing.T) {
		res, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		sort.Strings(names)
		want := []string{"edit_file", "read_file", "run_command", "write_file"}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Fatalf("tools = %v, want %v", names, want)
		}
	})

	call := func(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res
	}
	text := func(t *testing.T, res *mcp.CallToolResult) string {
		t.Helper()
		if len(res.Content) == 0 {
			t.Fatal("no content")
		}
		tc, ok := res.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatalf("content[0] is %T", res.Content[0])
		}
		return tc.Text
	}

	file := filepath.Join(scratch, "notes.txt")

	t.Run("write read edit read run round-trip", func(t *testing.T) {
		res := call(t, "write_file", map[string]any{"path": file, "content": "line one\nline two\nline three\n"})
		if res.IsError {
			t.Fatalf("write_file error: %s", text(t, res))
		}

		res = call(t, "read_file", map[string]any{"path": file})
		if got := text(t, res); !strings.Contains(got, "2\tline two") {
			t.Fatalf("read_file output: %q", got)
		}

		res = call(t, "edit_file", map[string]any{"path": file, "old_string": "line two", "new_string": "line 2"})
		if res.IsError {
			t.Fatalf("edit_file error: %s", text(t, res))
		}
		if got := text(t, res); !strings.Contains(got, "+line 2") {
			t.Fatalf("edit_file diff snippet: %q", got)
		}

		res = call(t, "read_file", map[string]any{"path": file, "offset": 2, "limit": 1})
		if got := text(t, res); !strings.Contains(got, "line 2") || strings.Contains(got, "line one") {
			t.Fatalf("read_file after edit: %q", got)
		}

		res = call(t, "run_command", map[string]any{"command": "wc -l < " + file, "workdir": scratch})
		if res.IsError {
			t.Fatalf("run_command error: %s", text(t, res))
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("structured content is %T", res.StructuredContent)
		}
		if got := strings.TrimSpace(sc["stdout"].(string)); got != "3" {
			t.Errorf("stdout = %q, want 3", got)
		}
		if sc["exit_code"].(float64) != 0 {
			t.Errorf("exit_code = %v", sc["exit_code"])
		}
	})

	t.Run("tool errors surface as IsError", func(t *testing.T) {
		res := call(t, "read_file", map[string]any{"path": filepath.Join(scratch, "missing.txt")})
		if !res.IsError {
			t.Fatal("expected IsError for missing file")
		}
	})
}
