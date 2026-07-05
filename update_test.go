package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// makeArchive builds a gzipped tar containing the given files (name->content)
// and returns the archive bytes plus a checksums.txt line for archiveName.
func makeArchive(t *testing.T, archiveName string, files map[string]string) (archive, checksums []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive = buf.Bytes()
	sum := sha256.Sum256(archive)
	checksums = []byte(hex.EncodeToString(sum[:]) + "  " + archiveName + "\n")
	return archive, checksums
}

func TestVerifyAndExtract(t *testing.T) {
	const name = "domovoi_0.1.4_linux_amd64.tar.gz"
	archive, checksums := makeArchive(t, name, map[string]string{
		"README.md":       "docs",
		"domovoi":         "BINARY-BYTES",
		"domovoi.service": "unit",
	})

	t.Run("extracts the binary when the checksum matches", func(t *testing.T) {
		got, err := verifyAndExtract(archive, checksums, name)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "BINARY-BYTES" {
			t.Fatalf("extracted %q, want BINARY-BYTES", got)
		}
	})

	t.Run("rejects a tampered archive", func(t *testing.T) {
		tampered := append([]byte(nil), archive...)
		tampered[len(tampered)-1] ^= 0xff
		if _, err := verifyAndExtract(tampered, checksums, name); err == nil ||
			!strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("expected checksum mismatch, got %v", err)
		}
	})

	t.Run("errors when no checksum is listed", func(t *testing.T) {
		if _, err := verifyAndExtract(archive, []byte("deadbeef  other.tar.gz\n"), name); err == nil ||
			!strings.Contains(err.Error(), "no checksum") {
			t.Fatalf("expected missing-checksum error, got %v", err)
		}
	})

	t.Run("errors when the binary is absent", func(t *testing.T) {
		noBin, sums := makeArchive(t, name, map[string]string{"README.md": "docs"})
		if _, err := verifyAndExtract(noBin, sums, name); err == nil ||
			!strings.Contains(err.Error(), "does not contain a domovoi binary") {
			t.Fatalf("expected missing-binary error, got %v", err)
		}
	})
}

func TestNormalizeVersion(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"v0.1.4", "0.1.4"},
		{"0.1.4", "0.1.4"},
		{"dev", "dev"},
	} {
		if got := normalizeVersion(tc.in); got != tc.want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
