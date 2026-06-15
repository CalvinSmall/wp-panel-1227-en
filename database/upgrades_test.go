package database

import (
	"path/filepath"
	"testing"
)

func openTempDB(t *testing.T) {
	t.Helper()
	if DB != nil {
		_ = Close()
		DB = nil
	}
	if err := Open(filepath.Join(t.TempDir(), "panel.db")); err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		_ = Close()
		DB = nil
	})
}

func TestFreshInstallRunsMigrationsAndRecordsLatestVersion(t *testing.T) {
	openTempDB(t)

	if err := RunMigrations(); err != nil {
		t.Fatalf("RunMigrations() error = %v", err)
	}
	if err := RunUpgrades(); err != nil {
		t.Fatalf("RunUpgrades() error = %v", err)
	}

	var version string
	if err := DB.QueryRow("SELECT version FROM schema_version ORDER BY updated_at DESC, rowid DESC LIMIT 1").Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != LatestVersion() {
		t.Fatalf("version = %q, want %q", version, LatestVersion())
	}

	for _, col := range []string{"php_pool_path", "nginx_conf_path", "wp_memory_limit", "cdn_realip_enabled"} {
		var exists int
		if err := DB.QueryRow("SELECT COUNT(*) FROM pragma_table_info('websites') WHERE name = ?", col).Scan(&exists); err != nil {
			t.Fatalf("query websites column %s: %v", col, err)
		}
		if exists != 1 {
			t.Fatalf("websites.%s exists = %d, want 1", col, exists)
		}
	}

	var groupCount int
	if err := DB.QueryRow("SELECT COUNT(*) FROM cdn_realip_groups WHERE builtin = 1").Scan(&groupCount); err != nil {
		t.Fatalf("query cdn_realip_groups: %v", err)
	}
	if groupCount < 2 {
		t.Fatalf("builtin cdn realip groups = %d, want at least 2", groupCount)
	}
	var cfSettingCount int
	if err := DB.QueryRow("SELECT COUNT(*) FROM security_settings WHERE skey = 'cloudflare_realip_ips'").Scan(&cfSettingCount); err != nil {
		t.Fatalf("query cloudflare_realip_ips setting: %v", err)
	}
	if cfSettingCount != 1 {
		t.Fatalf("cloudflare_realip_ips setting exists = %d, want 1", cfSettingCount)
	}
}

func TestUpgradeRunnerAdvancesExistingVersion(t *testing.T) {
	openTempDB(t)

	if err := RunMigrations(); err != nil {
		t.Fatalf("RunMigrations() error = %v", err)
	}
	if err := RunUpgrades(); err != nil {
		t.Fatalf("initial RunUpgrades() error = %v", err)
	}
	if _, err := DB.Exec("DELETE FROM schema_version"); err != nil {
		t.Fatalf("delete schema_version: %v", err)
	}
	if _, err := DB.Exec("INSERT INTO schema_version (version) VALUES ('1.0.9')"); err != nil {
		t.Fatalf("seed schema_version: %v", err)
	}

	if err := RunUpgrades(); err != nil {
		t.Fatalf("RunUpgrades() error = %v", err)
	}

	var version string
	if err := DB.QueryRow("SELECT version FROM schema_version ORDER BY updated_at DESC, rowid DESC LIMIT 1").Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != LatestVersion() {
		t.Fatalf("version = %q, want %q", version, LatestVersion())
	}
}

func TestUpgradeAddsCDNRealIPColumnToOldSchema(t *testing.T) {
	openTempDB(t)

	if _, err := DB.Exec(`CREATE TABLE websites (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		domain TEXT NOT NULL UNIQUE
	)`); err != nil {
		t.Fatalf("create old websites table: %v", err)
	}
	if _, err := DB.Exec(`CREATE TABLE schema_version (
		version TEXT PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create schema_version: %v", err)
	}
	if _, err := DB.Exec(`CREATE TABLE security_settings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		skey TEXT NOT NULL UNIQUE,
		svalue TEXT NOT NULL DEFAULT '',
		description TEXT DEFAULT '',
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create security_settings: %v", err)
	}
	if _, err := DB.Exec("INSERT INTO schema_version (version) VALUES ('1.0.11')"); err != nil {
		t.Fatalf("seed schema_version: %v", err)
	}

	if err := RunUpgrades(); err != nil {
		t.Fatalf("RunUpgrades() error = %v", err)
	}
	if err := RunUpgrades(); err != nil {
		t.Fatalf("second RunUpgrades() error = %v", err)
	}

	var exists int
	if err := DB.QueryRow("SELECT COUNT(*) FROM pragma_table_info('websites') WHERE name = 'cdn_realip_enabled'").Scan(&exists); err != nil {
		t.Fatalf("query cdn_realip_enabled: %v", err)
	}
	if exists != 1 {
		t.Fatalf("cdn_realip_enabled exists = %d, want 1", exists)
	}
}
