// Package database — Version upgrade mechanism
//
// Division of labor between the two files:
//
//   - migrations.go   Full table creation + seed data for fresh installs, always represents the
//                     latest database state. Executes fully on every startup (idempotent via
//                     IF NOT EXISTS / OR IGNORE).
//   - upgrades.go     Incremental upgrade steps for upgrading from older versions. Executes
//                     sequentially only when the version is behind.
//
// Database change workflow:
//
//   1. Add CREATE / INSERT statements in the appropriate location in migrations.go (for fresh installs).
//   2. Append an Upgrade entry at the end of upgrades.go (for upgrades).
//   3. Upgrade entries must be preserved permanently; never delete old entries. Users may upgrade
//      across multiple versions, and deleting entries would cause older versions to skip necessary
//      ALTER TABLE and other incremental migrations.
//
// Runtime logic (main.go startup → database.Open → RunMigrations → RunUpgrades):
//
//   Fresh install:  migrations create all tables + seeds → upgrades find version table empty → skip all upgrades → write latest version
//   Upgrade:        migrations execute idempotently (no actual changes) → upgrades find version behind → execute missing upgrades one by one → update version
//   Already latest: migrations execute idempotently → upgrades find version already latest → skip
//
// Version numbering convention:
//   Semantic versioning (e.g. "1.0.0"), consistent with Git tags. LatestVersion() returns the version
//   of the last entry in the upgrades list (or "1.0.0" if empty), representing the database version
//   represented by the current code.

package database

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// Upgrade defines the database changes needed for a single version upgrade.
// SQL statements should use idempotent patterns like IF NOT EXISTS / OR IGNORE to ensure
// safe re-execution. Func is an optional Go code migration executed after SQL, used for
// non-database operations like filesystem cleanup.
type Upgrade struct {
	Version     string       // Target version, e.g. "1.0.0"
	Description string       // What this upgrade does
	SQL         []string     // SQL statements to execute
	Func        func() error // Optional Go function migration
}

// registeredFuncs stores upgrade functions registered by external packages, solving circular dependency issues (database cannot import executor).
var registeredFuncs = map[string]func() error{}

// RegisterUpgrade allows external packages to register upgrade functions; version must match a Version in the upgrades list.
func RegisterUpgrade(version string, fn func() error) {
	registeredFuncs[version] = fn
}

// upgrades is ordered by version (old→new), must be preserved permanently; never delete old entries
// (cross-version upgrades depend on the complete migration chain).
// History was cleared once at the v1.0.0 release; all upgrade entries accumulate from that point.
var upgrades = []Upgrade{
	{
		Version:     "1.0.1",
		Description: "Migrate wp-panel-config.json outside web directory and rotate API Key",
		Func:        migratePluginConfigs,
	},
	{
		Version:     "1.0.2",
		Description: "Add XML-RPC site toggle, disabled by default",
		SQL: []string{
			`ALTER TABLE websites ADD COLUMN xmlrpc_enabled INTEGER NOT NULL DEFAULT 0`,
		},
	},
	{
		Version:     "1.0.3",
		Description: "Add running column to cron_jobs + add default plugin Redis Cache",
		SQL: []string{
			`ALTER TABLE cron_jobs ADD COLUMN running INTEGER NOT NULL DEFAULT 0`,
			`INSERT OR IGNORE INTO wp_extension_config (etype, slug, name, enabled) VALUES ('plugin', 'redis-cache', 'Redis Cache', 1)`,
		},
	},
	{
		Version:     "1.0.4",
		Description: "Strengthen per-site Unix user group isolation and sensitive file permissions",
	},
	{
		Version:     "1.0.5",
		Description: "Add system available update alert toggle",
		SQL: []string{
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('alert_system_update', 'true', 'System available update alert')`,
		},
	},
	{
		Version:     "1.0.6",
		Description: "Add panel new version alert toggle",
		SQL: []string{
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('alert_panel_update', 'true', 'Panel new version alert')`,
		},
	},
	{
		Version:     "1.0.7",
		Description: "Add WP_DEBUG / post revisions / memory limit optimization options",
		SQL: []string{
			`ALTER TABLE websites ADD COLUMN wp_debug_enabled INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE websites ADD COLUMN wp_post_revisions INTEGER NOT NULL DEFAULT -1`,
			`ALTER TABLE websites ADD COLUMN wp_memory_limit TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		Version:     "1.0.8",
		Description: "Add anonymous installation statistics toggle",
		SQL: []string{
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('telemetry_enabled', 'true', 'Anonymous installation statistics (only reports machine ID and version)')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('telemetry_url', '', 'Custom statistics reporting URL (leave empty for default)')`,
		},
	},
	{
		Version:     "1.0.9",
		Description: "Add GitHub reverse proxy address setting",
		SQL: []string{
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('github_proxy', '', 'GitHub reverse proxy address, leave empty for direct connection')`,
		},
	},
	{
		Version:     "1.0.10",
		Description: "Backfill WP_CACHE_KEY_SALT for existing WordPress sites",
	},
	{
		Version:     "1.0.11",
		Description: "Add WordPress security log path whitelist setting",
		SQL: []string{
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('wp_security_log_whitelist', '', 'WordPress security log path whitelist')`,
		},
	},
	{
		Version:     "1.0.12",
		Description: "Add per-site CDN real IP configuration groups",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS cdn_realip_groups (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				name        TEXT    NOT NULL UNIQUE,
				provider    TEXT    NOT NULL DEFAULT 'custom',
				header_name TEXT    NOT NULL,
				ip_ranges   TEXT    NOT NULL DEFAULT '',
				builtin     INTEGER NOT NULL DEFAULT 0,
				enabled     INTEGER NOT NULL DEFAULT 1,
				description TEXT    DEFAULT '',
				created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE INDEX IF NOT EXISTS idx_cdn_realip_groups_enabled ON cdn_realip_groups(enabled)`,
			`CREATE TABLE IF NOT EXISTS website_cdn_realip_groups (
				website_id INTEGER NOT NULL,
				group_id   INTEGER NOT NULL,
				created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (website_id, group_id),
				FOREIGN KEY (website_id) REFERENCES websites(id) ON DELETE CASCADE,
				FOREIGN KEY (group_id) REFERENCES cdn_realip_groups(id) ON DELETE CASCADE
			)`,
			`CREATE INDEX IF NOT EXISTS idx_website_cdn_realip_groups_group ON website_cdn_realip_groups(group_id)`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('cloudflare_realip_ips', '', 'Cloudflare Real IP official IP ranges')`,
			`INSERT OR IGNORE INTO cdn_realip_groups (name, provider, header_name, ip_ranges, builtin, enabled, description) VALUES
				('Cloudflare', 'cloudflare', 'CF-Connecting-IP', '', 1, 1, 'Cloudflare official IP ranges, auto-fetched by panel'),
				('General CDN (compatible mode)', 'compatible', 'X-Forwarded-For', '', 1, 1, 'Skip source IP verification, trust X-Forwarded-For directly')`,
		},
		Func: ensureCDNRealIPEnabledColumn,
	},
	{
		Version:     "1.0.13",
		Description: "Add Bot UA unified rate limiting settings",
		SQL: []string{
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('bot_limit_enabled', 'false', 'Enable Bot UA unified rate limiting')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('bot_limit_rpm', '30', 'Max requests per minute per site for bots')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('bot_limit_burst', '20', 'Bot burst buffer allowance')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('googlebot_ips', '', 'Googlebot official IP range cache')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('bingbot_ips', '', 'Bingbot official IP range cache')`,
		},
	},
	{
		Version:     "1.0.14",
		Description: "Record the last SSL request failure reason per site",
		Func:        ensureSSLLastErrorColumn,
	},
	{
		Version:     "1.0.15",
		Description: "Add panel auto-update settings",
		SQL: []string{
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_enabled', 'false', 'Enable panel auto-update')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_mode', 'patch_only', 'Panel auto-update mode: patch_only/all_stable')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_window', '03:00-05:00', 'Panel auto-update time window')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_release_delay_minutes', '15', 'Panel auto-update release delay in minutes')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_signature_timeout_minutes', '120', 'Panel auto-update signature wait timeout in minutes')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_last_target_version', '', 'Panel auto-update last target version')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_last_check_at', '', 'Panel auto-update last check time')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_last_attempt_at', '', 'Panel auto-update last attempt time')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_last_status', '', 'Panel auto-update last status')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_last_stage', '', 'Panel auto-update last stage')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_last_error', '', 'Panel auto-update last error')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_last_success_at', '', 'Panel auto-update last success time')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_last_success_version', '', 'Panel auto-update last success version')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_signature_wait_version', '', 'Panel auto-update signature wait version')`,
			`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES ('panel_auto_update_signature_wait_at', '', 'Panel auto-update signature wait start time')`,
		},
	},
	{
		Version:     "1.0.16",
		Description: "Add per-site SSL certificate export toggle",
		Func:        ensureSSLExportEnabledColumn,
	},
	{
		Version:     "1.0.17",
		Description: "Add PHP site web entry directory configuration",
		Func:        ensureDocumentRootSubdirColumn,
	},
	{
		Version:     "1.0.18",
		Description: "Add per-site AI read-only diagnostics settings and session records",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS ai_settings (
				id              INTEGER PRIMARY KEY,
				enabled         INTEGER NOT NULL DEFAULT 0,
				provider        TEXT    NOT NULL DEFAULT 'deepseek',
				base_url        TEXT    NOT NULL DEFAULT 'https://api.deepseek.com',
				model           TEXT    NOT NULL DEFAULT 'deepseek-v4-pro',
				api_key         TEXT    NOT NULL DEFAULT '',
				timeout_seconds INTEGER NOT NULL DEFAULT 60,
				created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`,
			`INSERT OR IGNORE INTO ai_settings (id) VALUES (1)`,
			`CREATE TABLE IF NOT EXISTS ai_sessions (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				site_id        INTEGER NOT NULL,
				symptom        TEXT    NOT NULL DEFAULT '',
				status         TEXT    NOT NULL DEFAULT 'pending',
				risk_level     TEXT    NOT NULL DEFAULT '',
				summary        TEXT    NOT NULL DEFAULT '',
				report_json    TEXT    NOT NULL DEFAULT '',
				raw_text       TEXT    NOT NULL DEFAULT '',
				prompt_chars   INTEGER NOT NULL DEFAULT 0,
				response_chars INTEGER NOT NULL DEFAULT 0,
				error_message  TEXT    NOT NULL DEFAULT '',
				created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				FOREIGN KEY (site_id) REFERENCES websites(id) ON DELETE CASCADE
			)`,
			`CREATE INDEX IF NOT EXISTS idx_ai_sessions_site ON ai_sessions(site_id, created_at)`,
			`CREATE INDEX IF NOT EXISTS idx_ai_sessions_status ON ai_sessions(site_id, status)`,
		},
	},
	{
		Version:     "1.0.19",
		Description: "Add AI diagnostics follow-up message records",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS ai_messages (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				session_id     INTEGER NOT NULL,
				role           TEXT    NOT NULL DEFAULT '',
				content        TEXT    NOT NULL DEFAULT '',
				prompt_chars   INTEGER NOT NULL DEFAULT 0,
				response_chars INTEGER NOT NULL DEFAULT 0,
				error_message  TEXT    NOT NULL DEFAULT '',
				created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				FOREIGN KEY (session_id) REFERENCES ai_sessions(id) ON DELETE CASCADE
			)`,
			`CREATE INDEX IF NOT EXISTS idx_ai_messages_session ON ai_messages(session_id, created_at)`,
		},
	},
	{
		Version:     "1.0.20",
		Description: "Add S3-compatible object storage backend for remote backups",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS remote_backup_settings (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				enabled     INTEGER NOT NULL DEFAULT 0,
				backup_type TEXT    NOT NULL DEFAULT 'rsync',
				host        TEXT    NOT NULL DEFAULT '',
				port        INTEGER NOT NULL DEFAULT 22,
				username    TEXT    NOT NULL DEFAULT 'root',
				auth_type   TEXT    NOT NULL DEFAULT 'password',
				password    TEXT    NOT NULL DEFAULT '',
				ssh_key     TEXT    NOT NULL DEFAULT '',
				remote_path TEXT    NOT NULL DEFAULT '',
				keep_local  INTEGER NOT NULL DEFAULT 1,
				s3_endpoint      TEXT NOT NULL DEFAULT '',
				s3_bucket        TEXT NOT NULL DEFAULT '',
				s3_region        TEXT NOT NULL DEFAULT 'auto',
				s3_access_key_id TEXT NOT NULL DEFAULT '',
				s3_secret_key    TEXT NOT NULL DEFAULT '',
				s3_path_prefix   TEXT NOT NULL DEFAULT '',
				created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`,
			`INSERT OR IGNORE INTO remote_backup_settings (id) VALUES (1)`,
			`ALTER TABLE remote_backup_settings ADD COLUMN backup_type TEXT NOT NULL DEFAULT 'rsync'`,
			`ALTER TABLE remote_backup_settings ADD COLUMN s3_endpoint TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE remote_backup_settings ADD COLUMN s3_bucket TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE remote_backup_settings ADD COLUMN s3_region TEXT NOT NULL DEFAULT 'auto'`,
			`ALTER TABLE remote_backup_settings ADD COLUMN s3_access_key_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE remote_backup_settings ADD COLUMN s3_secret_key TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE remote_backup_settings ADD COLUMN s3_path_prefix TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		Version:     "1.0.21",
		Description: "Add WordPress site file lock toggle",
		Func:        ensureFileLockEnabledColumn,
	},
	{
		Version:     "1.0.22",
		Description: "Add WordPress file security event records",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS file_security_events (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				site_id        INTEGER NOT NULL DEFAULT 0,
				domain         TEXT    NOT NULL DEFAULT '',
				event_type     TEXT    NOT NULL DEFAULT '',
				source         TEXT    NOT NULL DEFAULT '',
				risk_level     TEXT    NOT NULL DEFAULT 'medium',
				path           TEXT    NOT NULL DEFAULT '',
				request_method TEXT    NOT NULL DEFAULT '',
				ip_address     TEXT    NOT NULL DEFAULT '',
				user_agent     TEXT    NOT NULL DEFAULT '',
				status         INTEGER NOT NULL DEFAULT 0,
				file_size      INTEGER NOT NULL DEFAULT 0,
				file_mtime     DATETIME,
				message        TEXT    NOT NULL DEFAULT '',
				first_seen     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				last_seen      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				event_count    INTEGER NOT NULL DEFAULT 1,
				resolved_at    DATETIME,
				created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				FOREIGN KEY (site_id) REFERENCES websites(id) ON DELETE CASCADE
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_file_security_events_unique ON file_security_events(site_id, event_type, path, ip_address, request_method)`,
			`CREATE INDEX IF NOT EXISTS idx_file_security_events_last_seen ON file_security_events(resolved_at, last_seen)`,
			`CREATE INDEX IF NOT EXISTS idx_file_security_events_site ON file_security_events(site_id, resolved_at, last_seen)`,
		},
	},
	{
		Version:     "1.0.23",
		Description: "Add WordPress file lock enabled timestamp field",
		Func:        ensureFileLockEnabledAtColumn,
	},
}

func ensureFileLockEnabledColumn() error {
	var tableExists int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'websites'`).Scan(&tableExists); err != nil {
		return err
	}
	if tableExists == 0 {
		return nil
	}
	var exists int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('websites') WHERE name = 'file_lock_enabled'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 1 {
		return nil
	}
	_, err := DB.Exec(`ALTER TABLE websites ADD COLUMN file_lock_enabled INTEGER NOT NULL DEFAULT 0`)
	return err
}

func ensureFileLockEnabledAtColumn() error {
	var tableExists int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'websites'`).Scan(&tableExists); err != nil {
		return err
	}
	if tableExists == 0 {
		return nil
	}
	var exists int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('websites') WHERE name = 'file_lock_enabled_at'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 1 {
		return nil
	}
	_, err := DB.Exec(`ALTER TABLE websites ADD COLUMN file_lock_enabled_at TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		return err
	}
	_, err = DB.Exec(`
		UPDATE websites
		SET file_lock_enabled_at = CURRENT_TIMESTAMP
		WHERE file_lock_enabled = 1
			AND COALESCE(file_lock_enabled_at, '') = ''
	`)
	return err
}

func ensureDocumentRootSubdirColumn() error {
	var tableExists int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'websites'`).Scan(&tableExists); err != nil {
		return err
	}
	if tableExists == 0 {
		return nil
	}
	var exists int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('websites') WHERE name = 'document_root_subdir'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 1 {
		return nil
	}
	_, err := DB.Exec(`ALTER TABLE websites ADD COLUMN document_root_subdir TEXT NOT NULL DEFAULT ''`)
	return err
}

func ensureSSLLastErrorColumn() error {
	var tableExists int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'websites'`).Scan(&tableExists); err != nil {
		return err
	}
	if tableExists == 0 {
		return nil
	}
	var exists int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('websites') WHERE name = 'ssl_last_error'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 1 {
		return nil
	}
	_, err := DB.Exec(`ALTER TABLE websites ADD COLUMN ssl_last_error TEXT NOT NULL DEFAULT ''`)
	return err
}

func ensureCDNRealIPEnabledColumn() error {
	var exists int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('websites') WHERE name = 'cdn_realip_enabled'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 1 {
		return nil
	}
	_, err := DB.Exec(`ALTER TABLE websites ADD COLUMN cdn_realip_enabled INTEGER NOT NULL DEFAULT 0`)
	return err
}

func ensureSSLExportEnabledColumn() error {
	var tableExists int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'websites'`).Scan(&tableExists); err != nil {
		return err
	}
	if tableExists == 0 {
		return nil
	}
	var exists int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('websites') WHERE name = 'ssl_export_enabled'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 1 {
		return nil
	}
	_, err := DB.Exec(`ALTER TABLE websites ADD COLUMN ssl_export_enabled INTEGER NOT NULL DEFAULT 0`)
	return err
}

// LatestVersion returns the latest version number from the upgrades list.
func LatestVersion() string {
	if len(upgrades) == 0 {
		return "1.0.0"
	}
	return upgrades[len(upgrades)-1].Version
}

// newInstallCanary extracts the table and column names from the last ALTER TABLE ADD COLUMN
// in the upgrades list, used to determine if the database already contains the latest schema
// (canary column for fresh install detection).
func newInstallCanary() (table, column string) {
	for i := len(upgrades) - 1; i >= 0; i-- {
		for _, sql := range upgrades[i].SQL {
			upper := strings.ToUpper(strings.TrimSpace(sql))
			if strings.HasPrefix(upper, "ALTER TABLE") && strings.Contains(upper, "ADD COLUMN") {
				fields := strings.Fields(sql)
				// ALTER TABLE <table> ADD COLUMN <column> ...
				for j, f := range fields {
					if strings.ToUpper(f) == "TABLE" && j+1 < len(fields) {
						table = fields[j+1]
					}
					if strings.ToUpper(f) == "COLUMN" && j+1 < len(fields) {
						column = fields[j+1]
						if idx := strings.Index(column, "("); idx > 0 {
							column = column[:idx]
						}
					}
				}
				if table != "" && column != "" {
					return
				}
			}
		}
	}
	return "", ""
}

func isBetaVersion(v string) bool {
	return strings.Contains(strings.ToLower(v), "beta")
}

// RunUpgrades executes all pending version upgrades. Fresh installs are already at the latest version and skip all upgrades.
func RunUpgrades() error {
	if DB == nil {
		return fmt.Errorf("database not initialized")
	}

	// Ensure the version tracking table exists
	if _, err := DB.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version    TEXT NOT NULL,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("failed to create schema_version table: %w", err)
	}

	// Query current version
	var currentVersion string
	if err := DB.QueryRow("SELECT version FROM schema_version ORDER BY updated_at DESC, rowid DESC LIMIT 1").Scan(&currentVersion); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to query current version: %w", err)
	}

	// Fresh install detection: when currentVersion is empty, check if the database already
	// contains the latest schema. migrations.go already creates all tables, so if the column
	// from the latest upgrade exists, it's a fresh install with no upgrades needed.
	if currentVersion == "" {
		if table, col := newInstallCanary(); col != "" {
			var exists int
			if err := DB.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?", table, col).Scan(&exists); err != nil {
				return fmt.Errorf("failed to detect database structure: %w", err)
			}
			if exists > 0 {
				log.Printf("[Upgrade] Fresh install database, skipping all upgrade steps")
				if _, err := DB.Exec("INSERT INTO schema_version (version) VALUES (?)", LatestVersion()); err != nil {
					return fmt.Errorf("failed to record fresh install version: %w", err)
				}
				return nil
			}
		}
	}

	// Normalize beta version to 1.0.0 release baseline
	if currentVersion != "" && isBetaVersion(currentVersion) {
		log.Printf("[Upgrade] Beta version %s normalized to 1.0.0", currentVersion)
		if _, err := DB.Exec("DELETE FROM schema_version"); err != nil {
			log.Printf("[Upgrade] Failed to clean beta version records: %v", err)
		} else if _, err := DB.Exec("INSERT INTO schema_version (version) VALUES ('1.0.0')"); err != nil {
			log.Printf("[Upgrade] Failed to write normalized version: %v", err)
		} else {
			currentVersion = "1.0.0"
		}
	}

	// Validate current version legality: must be in the upgrades list, or the baseline 1.0.0, or empty (fresh install)
	if currentVersion != "" && currentVersion != "1.0.0" && currentVersion != LatestVersion() {
		found := false
		for _, u := range upgrades {
			if u.Version == currentVersion {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown database version %s, please manually migrate to 1.0.0 first", currentVersion)
		}
	}

	// Baseline 1.0.0 is treated as having applied all old upgrades; start from the first entry in upgrades
	applied := currentVersion == "" || currentVersion == "1.0.0"

	for _, u := range upgrades {
		if !applied {
			if u.Version == currentVersion {
				applied = true
			}
			continue
		}

		log.Printf("[Upgrade] Executing %s: %s", u.Version, u.Description)

		for _, sql := range u.SQL {
			if _, err := DB.Exec(sql); err != nil {
				if strings.Contains(err.Error(), "duplicate column name") {
					log.Printf("[Upgrade] %s: Column already exists, skipping (%s)", u.Version, strings.TrimSpace(sql))
					continue
				}
				return fmt.Errorf("upgrade %s failed: %w\nSQL: %s", u.Version, err, sql)
			}
		}

		fn := u.Func
		if fn == nil {
			fn = registeredFuncs[u.Version]
		}
		if fn != nil {
			if err := fn(); err != nil {
				return fmt.Errorf("upgrade %s function migration failed: %w", u.Version, err)
			}
		}

		if _, err := DB.Exec("INSERT INTO schema_version (version) VALUES (?)", u.Version); err != nil {
			return fmt.Errorf("failed to record upgrade version %s: %w", u.Version, err)
		}

		log.Printf("[Upgrade] %s completed", u.Version)
	}

	// Fresh install database: no version records, write the latest version directly so next startup skips all upgrades
	var count int
	if err := DB.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count); err != nil {
		log.Printf("[Upgrade] Failed to query version records: %v", err)
	}
	if count == 0 {
		if _, err := DB.Exec("INSERT INTO schema_version (version) VALUES (?)", LatestVersion()); err != nil {
			return fmt.Errorf("failed to record fresh install version: %w", err)
		}
	}

	return nil
}
