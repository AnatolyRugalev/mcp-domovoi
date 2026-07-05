// Command domovoi is a minimal fleet MCP server: file and shell tools
// (read_file, write_file, edit_file, run_command) plus server_info and
// self_update, over streamable HTTP, secured by a bearer token. One instance
// per machine; a central MCP gateway federates them as named targets.
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
	name        string
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
	name := fs.String("name", envDefault("NAME", ""), "human name for this host, shown to the agent so it knows which remote machine it is operating on (env DOMOVOI_NAME; defaults to the system hostname)")
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
	dirs, err := resolveAllowedDirs(*allowed)
	if err != nil {
		return nil, err
	}
	return &config{listen: *listen, token: *token, path: *path, name: hostName(*name), allowedDirs: dirs}, nil
}

// hostName returns the configured name, or the system hostname, or "this host"
// as a last resort. It anchors every tool description and the server
// instructions so the agent treats domovoi as a specific remote machine rather
// than its own local environment.
func hostName(configured string) string {
	if configured != "" {
		return configured
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "this host"
}

// resolveAllowedDirs parses a colon-separated list of absolute path prefixes,
// cleaning each and resolving symlinks up front so later prefix checks compare
// like with like.
func resolveAllowedDirs(spec string) ([]string, error) {
	var dirs []string
	for _, d := range strings.Split(spec, ":") {
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			return nil, fmt.Errorf("allowed dir %q is not absolute", d)
		}
		clean := filepath.Clean(d)
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			clean = resolved
		}
		dirs = append(dirs, clean)
	}
	if len(dirs) == 0 {
		return nil, errors.New("allowed-dirs must contain at least one directory")
	}
	return dirs, nil
}

// domovoi holds server-wide state shared by the tool handlers.
type domovoi struct {
	// name identifies the remote machine this instance controls; it is woven
	// into the server instructions and tool descriptions so a connected agent
	// understands it is acting on that host, not on its own local environment.
	name        string
	allowedDirs []string
	log         *slog.Logger
	// elevated is true in the root worker subprocess (see elevate.go). When
	// set, the tools perform their operations directly instead of re-executing
	// under sudo, which is what makes the sudo option run as root.
	elevated bool
}

// callDetail annotates a log detail string with a sudo marker so elevated
// calls are distinguishable in the logs.
func callDetail(detail string, sudo bool) string {
	if sudo {
		return detail + " (sudo)"
	}
	return detail
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
	host := d.name
	opts := &mcp.ServerOptions{
		Instructions: fmt.Sprintf("These tools operate on the remote machine %q over the network — "+
			"its filesystem, its processes, its shell. This is NOT your own local environment or the "+
			"host you are running on: read_file, write_file, edit_file and run_command all act on %q, "+
			"and every path and command is resolved there, never locally. Use run_command for anything "+
			"the file tools do not cover (listing directories, moving files, installing packages, "+
			"managing services). Set the sudo option on any tool to act as root on %q.", host, host, host),
	}
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "domovoi",
		Title:   fmt.Sprintf("domovoi on %s", host),
		Version: version,
	}, opts)
	mcp.AddTool(server, &mcp.Tool{
		Name: "read_file",
		Description: fmt.Sprintf("Read a file from the filesystem of the remote host %q (not your local "+
			"machine). Returns content with cat -n style line numbers (line number, tab, line text). "+
			"Use offset and limit to page through large files. Set sudo to read files only root can access.", host),
	}, d.readFile)
	mcp.AddTool(server, &mcp.Tool{
		Name: "write_file",
		Description: fmt.Sprintf("Write content to a file on the remote host %q (not your local machine), "+
			"creating parent directories as needed and overwriting any existing file. Returns the number "+
			"of bytes written. Set sudo to write as root.", host),
	}, d.writeFile)
	mcp.AddTool(server, &mcp.Tool{
		Name: "edit_file",
		Description: fmt.Sprintf("Replace an exact string in a file on the remote host %q (not your local "+
			"machine). old_string must match exactly (including whitespace) and must be unique in the file "+
			"unless replace_all is true. Returns the number of replacements and a diff snippet of the "+
			"changed region. Set sudo to edit files owned by root.", host),
	}, d.editFile)
	mcp.AddTool(server, &mcp.Tool{
		Name: "run_command",
		Description: fmt.Sprintf("Run a shell command on the remote host %q (not your local machine; "+
			"bash -lc, or sh -c if bash is absent). Returns stdout, stderr, exit_code, duration_ms and "+
			"timed_out. A non-zero exit code is a normal result, not an error. Output is truncated to the "+
			"last 100KB per stream. Set sudo to run the command as root.", host),
	}, d.runCommand)
	mcp.AddTool(server, &mcp.Tool{
		Name: "server_info",
		Description: fmt.Sprintf("Report this domovoi server's own identity: version, host name (%q), "+
			"OS/arch, and whether passwordless sudo is available. Use it to confirm which machine you are "+
			"connected to and what version it runs.", host),
	}, d.serverInfo)
	mcp.AddTool(server, &mcp.Tool{
		Name: "self_update",
		Description: fmt.Sprintf("Update the domovoi server on host %q to a newer release and restart it "+
			"onto the new binary. Downloads the release from GitHub, verifies its checksum, and replaces "+
			"the running binary in place. By default installs the latest release and restarts (which drops "+
			"this connection); pass version to pin a tag or restart:false to install without restarting.", host),
	}, d.selfUpdate)
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
	d := &domovoi{name: cfg.name, allowedDirs: cfg.allowedDirs, log: logger}
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
	// "worker" is the privileged self-re-exec: domovoi runs itself under sudo
	// in this mode to serve one MCP session over stdio as root (see elevate.go).
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		if err := runWorker(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "domovoi worker:", err)
			os.Exit(1)
		}
		return
	}

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
		"name", cfg.name,
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
