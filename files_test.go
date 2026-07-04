package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testDomovoi(t *testing.T, allowedDirs ...string) *domovoi {
	t.Helper()
	if len(allowedDirs) == 0 {
		allowedDirs = []string{"/"}
	}
	for i, d := range allowedDirs {
		if resolved, err := filepath.EvalSymlinks(d); err == nil {
			allowedDirs[i] = resolved
		}
	}
	return &domovoi{
		allowedDirs: allowedDirs,
		log:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty tool result")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected text content, got %T", res.Content[0])
	}
	return tc.Text
}

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEditFileNotFound(t *testing.T) {
	d := testDomovoi(t)
	path := writeTemp(t, t.TempDir(), "f.txt", "hello world\n")
	_, _, err := d.editFile(context.Background(), nil, editFileInput{Path: path, OldString: "nope", NewString: "x"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestEditFileIdenticalStrings(t *testing.T) {
	d := testDomovoi(t)
	path := writeTemp(t, t.TempDir(), "f.txt", "hello\n")
	_, _, err := d.editFile(context.Background(), nil, editFileInput{Path: path, OldString: "hello", NewString: "hello"})
	if err == nil || !strings.Contains(err.Error(), "identical") {
		t.Fatalf("expected identical-strings error, got %v", err)
	}
}

func TestEditFileAmbiguous(t *testing.T) {
	d := testDomovoi(t)
	path := writeTemp(t, t.TempDir(), "f.txt", "aaa\nbbb\naaa\n")
	_, _, err := d.editFile(context.Background(), nil, editFileInput{Path: path, OldString: "aaa", NewString: "ccc"})
	if err == nil || !strings.Contains(err.Error(), "replace_all") {
		t.Fatalf("expected ambiguity error mentioning replace_all, got %v", err)
	}
	// File must be untouched after the failed edit.
	raw, _ := os.ReadFile(path)
	if string(raw) != "aaa\nbbb\naaa\n" {
		t.Fatalf("file was modified by failed edit: %q", raw)
	}
}

func TestEditFileSingle(t *testing.T) {
	d := testDomovoi(t)
	path := writeTemp(t, t.TempDir(), "f.txt", "one\ntwo\nthree\n")
	res, _, err := d.editFile(context.Background(), nil, editFileInput{Path: path, OldString: "two", NewString: "TWO"})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, res)
	if !strings.Contains(text, "replaced 1 occurrence(s)") {
		t.Errorf("missing replacement count: %q", text)
	}
	if !strings.Contains(text, "-two") || !strings.Contains(text, "+TWO") {
		t.Errorf("diff snippet missing -/+ lines: %q", text)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "one\nTWO\nthree\n" {
		t.Fatalf("unexpected content: %q", raw)
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	d := testDomovoi(t)
	path := writeTemp(t, t.TempDir(), "f.txt", "x=1\ny=1\nz=1\n")
	res, _, err := d.editFile(context.Background(), nil, editFileInput{Path: path, OldString: "=1", NewString: "=2", ReplaceAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(t, res), "replaced 3 occurrence(s)") {
		t.Errorf("expected 3 replacements: %q", resultText(t, res))
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "x=2\ny=2\nz=2\n" {
		t.Fatalf("unexpected content: %q", raw)
	}
}

func TestEditFileUnicode(t *testing.T) {
	d := testDomovoi(t)
	path := writeTemp(t, t.TempDir(), "f.txt", "домовой живёт за печкой\n")
	_, _, err := d.editFile(context.Background(), nil, editFileInput{Path: path, OldString: "печкой", NewString: "печью"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "домовой живёт за печью\n" {
		t.Fatalf("unexpected content: %q", raw)
	}
}

func TestEditFilePreservesMode(t *testing.T) {
	d := testDomovoi(t)
	path := writeTemp(t, t.TempDir(), "f.sh", "#!/bin/sh\necho hi\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.editFile(context.Background(), nil, editFileInput{Path: path, OldString: "hi", NewString: "bye"}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode not preserved: %v", info.Mode())
	}
}

func TestAllowlistBlocksOutside(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	d := testDomovoi(t, allowed)

	if _, err := d.resolvePath(filepath.Join(allowed, "ok.txt")); err != nil {
		t.Fatalf("path inside allowlist rejected: %v", err)
	}
	if _, err := d.resolvePath(filepath.Join(outside, "no.txt")); err == nil {
		t.Fatal("path outside allowlist accepted")
	}
	// Prefix must be segment-aware: /allowed-evil must not match /allowed.
	if _, err := d.resolvePath(allowed + "-evil/f.txt"); err == nil {
		t.Fatal("sibling directory sharing a name prefix was accepted")
	}
}

func TestAllowlistBlocksDotDotEscape(t *testing.T) {
	allowed := t.TempDir()
	d := testDomovoi(t, allowed)
	if _, err := d.resolvePath(filepath.Join(allowed, "..", "escape.txt")); err == nil {
		t.Fatal("..-escape accepted")
	}
}

func TestAllowlistBlocksSymlinkEscape(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	writeTemp(t, outside, "secret.txt", "secret\n")
	d := testDomovoi(t, allowed)

	// Symlink to a file outside the allowlist.
	link := filepath.Join(allowed, "link.txt")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := d.resolvePath(link); err == nil {
		t.Fatal("symlink escape to file accepted")
	}

	// Symlinked directory: a new file under it would land outside.
	dirLink := filepath.Join(allowed, "dir")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatal(err)
	}
	if _, err := d.resolvePath(filepath.Join(dirLink, "new.txt")); err == nil {
		t.Fatal("symlink escape via non-existent file under linked dir accepted")
	}
}

func TestReadFileNumbersAndPaging(t *testing.T) {
	d := testDomovoi(t)
	path := writeTemp(t, t.TempDir(), "f.txt", "alpha\nbeta\ngamma\ndelta\n")

	res, _, err := d.readFile(context.Background(), nil, readFileInput{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, res)
	if !strings.Contains(text, "1\talpha") || !strings.Contains(text, "4\tdelta") {
		t.Errorf("missing numbered lines: %q", text)
	}

	res, _, err = d.readFile(context.Background(), nil, readFileInput{Path: path, Offset: 2, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	text = resultText(t, res)
	if strings.Contains(text, "alpha") || !strings.Contains(text, "beta") ||
		!strings.Contains(text, "gamma") || strings.Contains(text, "delta") {
		t.Errorf("offset/limit window wrong: %q", text)
	}
}

func TestReadFileErrors(t *testing.T) {
	d := testDomovoi(t)
	dir := t.TempDir()

	if _, _, err := d.readFile(context.Background(), nil, readFileInput{Path: filepath.Join(dir, "missing")}); err == nil {
		t.Error("missing file accepted")
	}
	if _, _, err := d.readFile(context.Background(), nil, readFileInput{Path: dir}); err == nil {
		t.Error("directory accepted")
	}
	if _, _, err := d.readFile(context.Background(), nil, readFileInput{Path: "relative.txt"}); err == nil {
		t.Error("relative path accepted")
	}

	binary := writeTemp(t, dir, "bin", "\x00\x01\x02\xff\xfe")
	if _, _, err := d.readFile(context.Background(), nil, readFileInput{Path: binary}); err == nil {
		t.Error("binary file accepted")
	}

	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", maxReadBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	// A >5MB single line trips the line-length guard; a >5MB file with no
	// offset/limit must be refused up front either way.
	if _, _, err := d.readFile(context.Background(), nil, readFileInput{Path: big}); err == nil {
		t.Error("oversized file accepted without offset/limit")
	}
}

func TestWriteFileCreatesParents(t *testing.T) {
	d := testDomovoi(t)
	path := filepath.Join(t.TempDir(), "a", "b", "c.txt")
	res, _, err := d.writeFile(context.Background(), nil, writeFileInput{Path: path, Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(t, res), "wrote 5 bytes") {
		t.Errorf("unexpected result: %q", resultText(t, res))
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "hello" {
		t.Fatalf("content = %q, err = %v", raw, err)
	}
}

func TestDiffSnippetTruncated(t *testing.T) {
	oldLines := make([]string, 200)
	newLines := make([]string, 200)
	for i := range oldLines {
		oldLines[i] = "old"
		newLines[i] = "new"
	}
	snippet := diffSnippet(strings.Join(oldLines, "\n"), strings.Join(newLines, "\n"))
	if !strings.Contains(snippet, "snippet truncated") {
		t.Errorf("expected truncation marker in long diff:\n%s", snippet)
	}
	if got := strings.Count(snippet, "\n"); got > maxSnippetLines+2 {
		t.Errorf("snippet too long: %d lines", got)
	}
}
