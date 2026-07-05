package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	githubOwner      = "AnatolyRugalev"
	githubRepo       = "mcp-domovoi"
	maxDownloadBytes = 64 * 1024 * 1024 // generous cap on a release archive
	reexecDelay      = time.Second      // let the tool response flush before we re-exec
)

// updateHTTP is the client used for release lookups and downloads; a bounded
// timeout keeps a stuck mirror from hanging the tool.
var updateHTTP = &http.Client{Timeout: 60 * time.Second}

type serverInfoInput struct{}

type serverInfoOutput struct {
	Version       string `json:"version" jsonschema:"the domovoi version this server is running"`
	Name          string `json:"name" jsonschema:"the configured name of this host"`
	OS            string `json:"os" jsonschema:"operating system (GOOS)"`
	Arch          string `json:"arch" jsonschema:"CPU architecture (GOARCH)"`
	GoVersion     string `json:"go_version" jsonschema:"Go runtime version the binary was built with"`
	Executable    string `json:"executable" jsonschema:"path of the running domovoi binary"`
	SudoAvailable bool   `json:"sudo_available" jsonschema:"true if passwordless sudo is usable, so the sudo option on other tools will work"`
}

func (d *domovoi) serverInfo(ctx context.Context, req *mcp.CallToolRequest, in serverInfoInput) (res *mcp.CallToolResult, out serverInfoOutput, err error) {
	start := time.Now()
	defer func() { d.logCall("server_info", "", start, err) }()

	exe, _ := os.Executable()
	out = serverInfoOutput{
		Version:       version,
		Name:          d.name,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		GoVersion:     runtime.Version(),
		Executable:    exe,
		SudoAvailable: sudoAvailable(),
	}
	text := fmt.Sprintf("domovoi %s on %q (%s/%s)", out.Version, out.Name, out.OS, out.Arch)
	return textResult(text), out, nil
}

// sudoAvailable reports whether `sudo -n` runs without a password prompt, i.e.
// whether the sudo option on the other tools can succeed.
func sudoAvailable() bool {
	if _, err := exec.LookPath("sudo"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "sudo", "-n", "true").Run() == nil
}

type selfUpdateInput struct {
	Version string `json:"version,omitempty" jsonschema:"release tag to install (e.g. v0.1.3); defaults to the latest release"`
	Restart *bool  `json:"restart,omitempty" jsonschema:"restart onto the new binary once installed, dropping this connection (default true)"`
}

type selfUpdateOutput struct {
	PreviousVersion string `json:"previous_version" jsonschema:"version that was running before the update"`
	TargetVersion   string `json:"target_version" jsonschema:"version that was installed (or already running)"`
	Updated         bool   `json:"updated" jsonschema:"true if a new binary was installed"`
	Restarting      bool   `json:"restarting" jsonschema:"true if the server is restarting onto the new binary (this connection will drop)"`
	Message         string `json:"message" jsonschema:"human-readable summary"`
}

// selfUpdate downloads a release of domovoi, verifies its checksum, atomically
// replaces the running binary, and (by default) re-executes the process onto the
// new binary. Re-executing in place keeps the same PID, so a systemd unit stays
// active without any external restart; the tradeoff is that this MCP connection
// drops when it happens.
func (d *domovoi) selfUpdate(ctx context.Context, req *mcp.CallToolRequest, in selfUpdateInput) (res *mcp.CallToolResult, out selfUpdateOutput, err error) {
	start := time.Now()
	defer func() { d.logCall("self_update", in.Version, start, err) }()

	restart := true
	if in.Restart != nil {
		restart = *in.Restart
	}
	out.PreviousVersion = version

	tag := in.Version
	if tag == "" {
		tag, err = latestReleaseTag(ctx)
		if err != nil {
			return nil, out, fmt.Errorf("finding the latest release: %w", err)
		}
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	out.TargetVersion = tag

	if version != "dev" && normalizeVersion(tag) == normalizeVersion(version) {
		out.Message = fmt.Sprintf("already running %s; nothing to do", version)
		return textResult(out.Message), out, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, out, fmt.Errorf("locating the running binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, out, fmt.Errorf("resolving the running binary path: %w", err)
	}

	assetVersion := strings.TrimPrefix(tag, "v")
	archiveName := fmt.Sprintf("domovoi_%s_%s_%s.tar.gz", assetVersion, runtime.GOOS, runtime.GOARCH)
	base := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/", githubOwner, githubRepo, tag)

	archive, err := httpGet(ctx, base+archiveName)
	if err != nil {
		return nil, out, fmt.Errorf("downloading %s: %w", archiveName, err)
	}
	checksums, err := httpGet(ctx, base+"checksums.txt")
	if err != nil {
		return nil, out, fmt.Errorf("downloading checksums: %w", err)
	}
	binary, err := verifyAndExtract(archive, checksums, archiveName)
	if err != nil {
		return nil, out, err
	}
	if err := swapBinary(exe, binary); err != nil {
		return nil, out, err
	}
	out.Updated = true

	if !restart {
		out.Message = fmt.Sprintf("installed %s over %s at %s; restart the service to run it", tag, out.PreviousVersion, exe)
		return textResult(out.Message), out, nil
	}

	out.Restarting = true
	out.Message = fmt.Sprintf("installed %s (was %s); restarting onto the new binary now — this connection will drop", tag, out.PreviousVersion)
	// Re-exec after the response has had a moment to flush. syscall.Exec
	// replaces this process image with the new binary, keeping the same PID and
	// environment (so the token from the systemd EnvironmentFile is preserved).
	go func() {
		time.Sleep(reexecDelay)
		if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
			d.log.Error("self_update re-exec failed", "error", err, "exe", exe)
		}
	}()
	return textResult(out.Message), out, nil
}

// normalizeVersion strips a leading v so tag and build-stamped version compare.
func normalizeVersion(v string) string { return strings.TrimPrefix(v, "v") }

func latestReleaseTag(ctx context.Context) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("github API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", errors.New("latest release has no tag_name")
	}
	return rel.TagName, nil
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes))
}

// verifyAndExtract checks the archive's SHA-256 against the checksums file and
// returns the domovoi binary from inside the tarball.
func verifyAndExtract(archive, checksums []byte, archiveName string) ([]byte, error) {
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		// checksums.txt lines are "<hex>  <filename>".
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[1] == archiveName {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return nil, fmt.Errorf("no checksum listed for %s", archiveName)
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, want) {
		return nil, fmt.Errorf("checksum mismatch for %s: got %s, want %s", archiveName, got, want)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive: %w", err)
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "domovoi" {
			return io.ReadAll(tr)
		}
	}
	return nil, errors.New("archive does not contain a domovoi binary")
}

// swapBinary atomically replaces exe with data. It writes a temp file in the
// same directory (so the rename stays on one filesystem) and renames it over
// the target, which requires write permission on exe and its directory.
func swapBinary(exe string, data []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".domovoi-update-*")
	if err != nil {
		return fmt.Errorf("cannot stage update next to %s (need write permission on its directory): %w", exe, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // harmless no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpName, exe); err != nil {
		return fmt.Errorf("cannot replace %s (need write permission on it and its directory): %w", exe, err)
	}
	return nil
}
