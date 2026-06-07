package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDumpDatabaseToGzipRejectsInvalidDatabaseName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.sql.gz")
	badNames := []string{
		"db; rm -rf /",
		"db name",
		"db-name",
		"db`name",
		strings.Repeat("a", 65),
	}

	for _, name := range badNames {
		if err := dumpDatabaseToGzip(name, "secret", path); err == nil {
			t.Fatalf("dumpDatabaseToGzip(%q) error = nil, want error", name)
		}
	}
}

func TestDumpDatabaseToGzipDoesNotOverwriteExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.sql.gz")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatalf("seed backup file: %v", err)
	}

	err := dumpDatabaseToGzip("valid_db", "secret", path)
	if err == nil {
		t.Fatal("dumpDatabaseToGzip existing file error = nil, want error")
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read backup file: %v", readErr)
	}
	if string(data) != "existing" {
		t.Fatalf("backup file overwritten: %q", string(data))
	}
}
