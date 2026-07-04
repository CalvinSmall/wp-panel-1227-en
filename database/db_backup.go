package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupDatabase performs a consistent hot backup of the live database using VACUUM INTO
func BackupDatabase(backupDir string) (string, error) {
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create backup directory: %w", err)
	}

	ts := time.Now().Format("20060102_150405")
	backupPath := filepath.Join(backupDir, fmt.Sprintf("panel_%s.db", ts))

	if _, err := DB.Exec("VACUUM INTO ?", backupPath); err != nil {
		os.Remove(backupPath)
		return "", fmt.Errorf("VACUUM INTO failed: %w", err)
	}

	return backupPath, nil
}

// DBBackupInfo holds metadata about a backup file
type DBBackupInfo struct {
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	SizeText  string `json:"size_text"`
	CreatedAt string `json:"created_at"`
}

// ListDBBackups lists all panel database backups
func ListDBBackups(backupDir string) ([]DBBackupInfo, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []DBBackupInfo{}, nil
		}
		return nil, err
	}

	var backups []DBBackupInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "panel_") || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Parse time from filename: panel_20260107_023000.db
		name := strings.TrimPrefix(e.Name(), "panel_")
		name = strings.TrimSuffix(name, ".db")
		displayTime := name
		if t, err := time.Parse("20060102_150405", name); err == nil {
			displayTime = t.Format("2006-01-02 15:04:05")
		}

		backups = append(backups, DBBackupInfo{
			Filename:  e.Name(),
			Size:      info.Size(),
			SizeText:  formatDBSize(info.Size()),
			CreatedAt: displayTime,
		})
	}

	// Sort by time descending
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].Filename > backups[j].Filename
	})

	if backups == nil {
		backups = []DBBackupInfo{}
	}
	return backups, nil
}

// CleanupOldDBBackups keeps the most recent keepCount backups and deletes the rest
func CleanupOldDBBackups(backupDir string, keepCount int) int {
	backups, err := ListDBBackups(backupDir)
	if err != nil || len(backups) <= keepCount {
		return 0
	}

	removed := 0
	for i := keepCount; i < len(backups); i++ {
		path := filepath.Join(backupDir, backups[i].Filename)
		if os.Remove(path) == nil {
			removed++
		}
	}
	return removed
}

// RestoreDBBackupPath returns the full path of the specified backup file, validating its existence
func RestoreDBBackupPath(backupDir, filename string) (string, error) {
	// Security check: prevent path traversal
	clean := filepath.Clean(filename)
	if clean != filename || strings.Contains(clean, "/") || strings.Contains(clean, "\\") {
		return "", fmt.Errorf("invalid filename")
	}
	if !strings.HasPrefix(clean, "panel_") || !strings.HasSuffix(clean, ".db") {
		return "", fmt.Errorf("invalid backup filename")
	}

	fullPath := filepath.Join(backupDir, clean)
	if _, err := os.Stat(fullPath); err != nil {
		return "", fmt.Errorf("backup file does not exist")
	}
	return fullPath, nil
}

// VerifyDBBackup opens the backup file and runs PRAGMA integrity_check to verify backup integrity
func VerifyDBBackup(backupPath string) error {
	db, err := sql.Open("sqlite", backupPath)
	if err != nil {
		return fmt.Errorf("failed to open backup file: %w", err)
	}
	defer db.Close()

	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("integrity check failed: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("backup file corrupted: %s", result)
	}
	return nil
}

func formatDBSize(size int64) string {
	const (
		KB = 1024
		MB = KB * 1024
	)
	switch {
	case size >= MB:
		return fmt.Sprintf("%.1f MB", float64(size)/float64(MB))
	case size >= KB:
		return fmt.Sprintf("%.1f KB", float64(size)/float64(KB))
	default:
		return fmt.Sprintf("%d B", size)
	}
}
