package handlers

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSanitizeBackupPart(t *testing.T) {
	got := sanitizeBackupPart("v1.2.3; rm -rf /_ok")
	want := "v1.2.3rm-rf_ok"
	if got != want {
		t.Fatalf("sanitizeBackupPart() = %q, want %q", got, want)
	}
}

func TestVersionedBackupPath(t *testing.T) {
	got := versionedBackupPath("v1.2.3; bad")
	if !strings.HasPrefix(got, installPath+".bak.v1.2.3bad.") {
		t.Fatalf("versionedBackupPath() = %q", got)
	}
	if strings.ContainsAny(strings.TrimPrefix(got, installPath+".bak."), " ;/\\") {
		t.Fatalf("versionedBackupPath contains unsafe characters: %q", got)
	}
}

func TestCopyFileCopiesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello"), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	if err := copyFile(src, dst, 0750); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("dst content = %q, want hello", string(data))
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0750 {
		t.Fatalf("dst mode = %o, want 0750", info.Mode().Perm())
	}
}

func TestCopyFileRemovesPartialDestinationOnFailure(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "missing")
	dst := filepath.Join(dir, "dst")

	if err := copyFile(src, dst, 0755); err == nil {
		t.Fatal("copyFile missing source error = nil, want error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst exists after failed copy, err=%v", err)
	}
}
