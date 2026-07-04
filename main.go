// Command domovoi is a minimal fleet MCP server: four tools (read_file,
// write_file, edit_file, run_command) over streamable HTTP, secured by a
// bearer token. One instance per machine; a central MCP gateway federates
// them as named targets.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	listen      string
	token       string
	path        string
	allowedDirs []string
}

// envDefault returns DOMOVOI_<key> if set, otherwise def.
func envDefault(key, def string) string {
	if v := os.Getenv("DOMOVOI_" + key); v != "" {
		return v
	}
	return def
}

func parseConfig(args []string) (*config, error) {
	fs := flag.NewFlagSet("domovoi", flag.ContinueOnError)
	listen := fs.String("listen", envDefault("LISTEN", "0.0.0.0:8811"), "address to listen on (env DOMOVOI_LISTEN)")
	token := fs.String("token", envDefault("TOKEN", ""), "bearer token required on every MCP request (env DOMOVOI_TOKEN)")
	path := fs.String("path", envDefault("PATH", "/mcp"), "URL path of the MCP endpoint (env DOMOVOI_PATH)")
	allowed := fs.String("allowed-dirs", envDefault("ALLOWED_DIRS", "/"), "colon-separated path prefixes the file tools may touch (env DOMOVOI_ALLOWED_DIRS)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *token == "" {
		return nil, errors.New("a token is required: set --token or DOMOVOI_TOKEN")
	}
	if !strings.HasPrefix(*path, "/") {
		return nil, fmt.Errorf("--path must start with /, got %q", *path)
	}
	var dirs []string
	for _, d := range strings.Split(*allowed, ":") {
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			return nil, fmt.Errorf("allowed dir %q is not absolute", d)
		}
		clean := filepath.Clean(d)
		// Resolve symlinks now so the prefix check compares like with like.
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			clean = resolved
		}
		dirs = append(dirs, clean)
	}
	if len(dirs) == 0 {
		return nil, errors.New("--allowed-dirs must contain at least one directory")
	}
	return &config{listen: *listen, token: *token, path: *path, allowedDirs: dirs}, nil
}

// domovoi holds server-wide state shared by the tool handlers.
type domovoi struct {
	allowedDirs []string
	log         *slog.Logger
}

func (d *domovoi) logCall(tool, detail string, start time.Time, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error: " + err.Error()
	}
	d.log.Info("tool call",
		"tool", tool,
		"detail", detail,
		"outcome", outcome,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

func newMCPServer(d *domovoi) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "domovoi", Version: version}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name: "read_file",
		Description: "Read a file from the local filesystem. Returns content with cat -n style " +
			"line numbers (line number, tab, line text). Use offset and limit to page through large files.",
	}, d.readFile)
	mcp.AddTool(server, &mcp.Tool{
		Name: "write_file",
		Description: "Write content to a file, creating parent directories as needed and " +
			"overwriting any existing file. Returns the number of bytes written.",
	}, d.writeFile)
	mcp.AddTool(server, &mcp.Tool{
		Name: "edit_file",
		Description: "Replace an exact string in a file. old_string must match exactly (including " +
			"whitespace) and must be unique in the file unless replace_all is true. Returns the number " +
			"of replacements and a diff snippet of the changed region.",
	}, d.editFile)
	mcp.AddTool(server, &mcp.Tool{
		Name: "run_command",
		Description: "Run a shell command on this machine (bash -lc, or sh -c if bash is absent). " +
			"Returns stdout, stderr, exit_code, duration_ms and timed_out. A non-zero exit code is a " +
			"normal result, not an error. Output is truncated to the last 100KB per stream.",
	}, d.runCommand)
	return server
}

// requireAuth wraps next, rejecting requests without the expected bearer token.
// Tokens are hashed before comparison so the comparison is constant-time even
// for tokens of different lengths.
func requireAuth(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got string
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			got = strings.TrimPrefix(h, "Bearer ")
		}
		sum := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// newHandler builds the complete HTTP handler: /healthz plus the
// token-protected MCP endpoint at cfg.path.
func newHandler(cfg *config, logger *slog.Logger) http.Handler {
	d := &domovoi{allowedDirs: cfg.allowedDirs, log: logger}
	server := newMCPServer(d)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "domovoi %s\n", version)
	})
	mux.Handle(cfg.path, requireAuth(cfg.token, mcpHandler))
	return mux
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "domovoi:", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:    cfg.listen,
		Handler: newHandler(cfg, logger),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("domovoi listening",
		"version", version,
		"addr", cfg.listen,
		"path", cfg.path,
		"allowed_dirs", strings.Join(cfg.allowedDirs, ":"),
	)

	select {
	case err := <-errc:
		logger.Error("server failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown", "error", err)
		}
	}
}
