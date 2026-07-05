package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxReadBytes    = 5 * 1024 * 1024 // cap on read_file output
	maxLineBytes    = 1024 * 1024     // longest single line read_file will handle
	defaultReadLim  = 2000
	diffContext     = 3  // context lines around a change in edit_file snippets
	maxSnippetLines = 60 // cap on edit_file diff snippet length
)

// resolvePath validates that path is absolute, resolves symlinks (on the
// longest existing ancestor, so not-yet-existing files still resolve), and
// checks the result against the allowlist. It returns the resolved path,
// which all file operations must use so symlinks can't escape the allowlist.
func (d *domovoi) resolvePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute, got %q", path)
	}
	resolved, err := resolveSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", path, err)
	}
	for _, dir := range d.allowedDirs {
		if dir == "/" || resolved == dir || strings.HasPrefix(resolved, dir+"/") {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("path %q (resolved to %q) is outside the allowed directories (%s)",
		path, resolved, strings.Join(d.allowedDirs, ":"))
}

// resolveSymlinks is filepath.EvalSymlinks that tolerates non-existent
// trailing components: it resolves the deepest existing ancestor and rejoins
// the rest, so a symlinked parent of a new file is still seen through.
func resolveSymlinks(path string) (string, error) {
	suffix := ""
	cur := path
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
}

type readFileInput struct {
	Path   string `json:"path" jsonschema:"absolute path of the file to read"`
	Offset int    `json:"offset,omitempty" jsonschema:"1-based line number to start reading from (default 1)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum number of lines to read (default 2000)"`
	Sudo   bool   `json:"sudo,omitempty" jsonschema:"read the file as root via sudo (default false)"`
}

func (d *domovoi) readFile(ctx context.Context, req *mcp.CallToolRequest, in readFileInput) (res *mcp.CallToolResult, _ any, err error) {
	start := time.Now()
	defer func() { d.logCall("read_file", callDetail(in.Path, in.Sudo), start, err) }()

	if in.Sudo && !d.elevated {
		res, cerr := d.callAsRoot(ctx, "read_file", in)
		if cerr != nil {
			return nil, nil, cerr
		}
		if res.IsError {
			return nil, nil, errorFromResult(res)
		}
		return res, nil, nil
	}

	path, err := d.resolvePath(in.Path)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		return nil, nil, fmt.Errorf("%s is a directory; use run_command with ls to list it", in.Path)
	}
	if info.Size() > maxReadBytes && in.Offset == 0 && in.Limit == 0 {
		return nil, nil, fmt.Errorf("file is %d bytes (limit %d); pass offset and limit to read it in chunks",
			info.Size(), maxReadBytes)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	offset := in.Offset
	if offset < 1 {
		offset = 1
	}
	limit := in.Limit
	if limit < 1 {
		limit = defaultReadLim
	}

	var b strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxLineBytes)
	lineNo := 0
	emitted := 0
	for scanner.Scan() {
		lineNo++
		if lineNo < offset {
			continue
		}
		if emitted >= limit {
			break
		}
		line := scanner.Text()
		if !utf8.ValidString(line) || strings.ContainsRune(line, 0) {
			return nil, nil, fmt.Errorf("%s does not appear to be valid UTF-8 text (binary file?)", in.Path)
		}
		fmt.Fprintf(&b, "%6d\t%s\n", lineNo, line)
		emitted++
		if b.Len() > maxReadBytes {
			return nil, nil, fmt.Errorf("output exceeds %d bytes; use a smaller limit or read in chunks with offset/limit", maxReadBytes)
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, nil, fmt.Errorf("%s contains a line longer than %d bytes (binary file?)", in.Path, maxLineBytes)
		}
		return nil, nil, err
	}
	if lineNo == 0 {
		return textResult("(file is empty)"), nil, nil
	}
	if emitted == 0 {
		return nil, nil, fmt.Errorf("offset %d is past the end of the file (%d lines)", offset, lineNo)
	}
	return textResult(b.String()), nil, nil
}

type writeFileInput struct {
	Path    string `json:"path" jsonschema:"absolute path of the file to write"`
	Content string `json:"content" jsonschema:"full content to write to the file"`
	Sudo    bool   `json:"sudo,omitempty" jsonschema:"write the file as root via sudo (default false)"`
}

func (d *domovoi) writeFile(ctx context.Context, req *mcp.CallToolRequest, in writeFileInput) (res *mcp.CallToolResult, _ any, err error) {
	start := time.Now()
	defer func() { d.logCall("write_file", callDetail(in.Path, in.Sudo), start, err) }()

	if in.Sudo && !d.elevated {
		res, cerr := d.callAsRoot(ctx, "write_file", in)
		if cerr != nil {
			return nil, nil, cerr
		}
		if res.IsError {
			return nil, nil, errorFromResult(res)
		}
		return res, nil, nil
	}

	path, err := d.resolvePath(in.Path)
	if err != nil {
		return nil, nil, err
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return nil, nil, fmt.Errorf("%s is a directory", in.Path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(path, []byte(in.Content), 0o644); err != nil {
		return nil, nil, err
	}
	return textResult(fmt.Sprintf("wrote %d bytes to %s", len(in.Content), path)), nil, nil
}

type editFileInput struct {
	Path       string `json:"path" jsonschema:"absolute path of the file to edit"`
	OldString  string `json:"old_string" jsonschema:"exact text to replace (must match including whitespace)"`
	NewString  string `json:"new_string" jsonschema:"text to replace it with"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"replace every occurrence instead of requiring a unique match (default false)"`
	Sudo       bool   `json:"sudo,omitempty" jsonschema:"edit the file as root via sudo (default false)"`
}

func (d *domovoi) editFile(ctx context.Context, req *mcp.CallToolRequest, in editFileInput) (res *mcp.CallToolResult, _ any, err error) {
	start := time.Now()
	defer func() { d.logCall("edit_file", callDetail(in.Path, in.Sudo), start, err) }()

	if in.Sudo && !d.elevated {
		res, cerr := d.callAsRoot(ctx, "edit_file", in)
		if cerr != nil {
			return nil, nil, cerr
		}
		if res.IsError {
			return nil, nil, errorFromResult(res)
		}
		return res, nil, nil
	}

	if in.OldString == in.NewString {
		return nil, nil, errors.New("old_string and new_string are identical")
	}
	if in.OldString == "" {
		return nil, nil, errors.New("old_string must not be empty")
	}
	path, err := d.resolvePath(in.Path)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		return nil, nil, fmt.Errorf("%s is a directory", in.Path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if !utf8.Valid(raw) {
		return nil, nil, fmt.Errorf("%s does not appear to be valid UTF-8 text (binary file?)", in.Path)
	}
	content := string(raw)

	count := strings.Count(content, in.OldString)
	switch {
	case count == 0:
		return nil, nil, errors.New("old_string not found in file")
	case count > 1 && !in.ReplaceAll:
		return nil, nil, fmt.Errorf("old_string matches %d times; add surrounding context to make it unique, or set replace_all", count)
	}

	replaced := count
	var updated string
	if in.ReplaceAll {
		updated = strings.ReplaceAll(content, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(content, in.OldString, in.NewString, 1)
		replaced = 1
	}
	if err := os.WriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
		return nil, nil, err
	}

	msg := fmt.Sprintf("replaced %d occurrence(s) in %s\n\n%s", replaced, path, diffSnippet(content, updated))
	return textResult(msg), nil, nil
}

// diffSnippet returns a small unified-diff-style excerpt covering the changed
// region between old and new, with a few lines of context.
func diffSnippet(old, new string) string {
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(new, "\n")

	// Trim common prefix and suffix (in lines).
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	ctxStart := max(0, prefix-diffContext)
	oldEnd := min(len(oldLines), len(oldLines)-suffix+diffContext)
	newEnd := min(len(newLines), len(newLines)-suffix+diffContext)

	var b strings.Builder
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", ctxStart+1, oldEnd-ctxStart, ctxStart+1, newEnd-ctxStart)
	lines := 0
	emit := func(mark string, line string) bool {
		if lines >= maxSnippetLines {
			return false
		}
		b.WriteString(mark)
		b.WriteString(line)
		b.WriteByte('\n')
		lines++
		return true
	}
	truncated := false
	for i := ctxStart; i < prefix; i++ {
		if !emit(" ", oldLines[i]) {
			truncated = true
			break
		}
	}
	if !truncated {
		for i := prefix; i < len(oldLines)-suffix; i++ {
			if !emit("-", oldLines[i]) {
				truncated = true
				break
			}
		}
	}
	if !truncated {
		for i := prefix; i < len(newLines)-suffix; i++ {
			if !emit("+", newLines[i]) {
				truncated = true
				break
			}
		}
	}
	if !truncated {
		for i := len(newLines) - suffix; i < newEnd; i++ {
			if !emit(" ", newLines[i]) {
				truncated = true
				break
			}
		}
	}
	if truncated {
		b.WriteString("... (snippet truncated)\n")
	}
	return b.String()
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}
