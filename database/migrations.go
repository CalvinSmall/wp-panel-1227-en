package database

var migrations = []string{
	// ============================================================
	// admin_users
	// ============================================================
	`CREATE TABLE IF NOT EXISTS admin_users (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		username      TEXT    NOT NULL UNIQUE,
		password_hash TEXT    NOT NULL,
		created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	// ============================================================
	// websites
	// ============================================================
	`CREATE TABLE IF NOT EXISTS websites (
		id                    INTEGER PRIMARY KEY AUTOINCREMENT,
		name                  TEXT    NOT NULL,
		domain                TEXT    NOT NULL UNIQUE,
		aliases               TEXT    DEFAULT '',
		status                TEXT    NOT NULL DEFAULT 'active',
		system_user           TEXT    NOT NULL,
		web_root              TEXT    NOT NULL,
		document_root_subdir  TEXT    NOT NULL DEFAULT '',
		log_dir               TEXT    NOT NULL,
		db_name               TEXT    NOT NULL,
		db_user               TEXT    NOT NULL,
		php_pool_path         TEXT    NOT NULL,
		nginx_conf_path       TEXT    NOT NULL,
		site_type             TEXT    NOT NULL DEFAULT 'wordpress',
		ssl_enabled           INTEGER NOT NULL DEFAULT 0,
		ssl_cert_path         TEXT    DEFAULT '',
		ssl_key_path          TEXT    DEFAULT '',
		ssl_expires_at        DATETIME,
		ssl_last_error        TEXT    NOT NULL DEFAULT '',
		ssl_export_enabled    INTEGER NOT NULL DEFAULT 0,
		template_version      TEXT    NOT NULL DEFAULT 'v1.0',
		access_log_mode       TEXT    NOT NULL DEFAULT 'error_only',
		fastcgi_cache_enabled INTEGER NOT NULL DEFAULT 0,
		fastcgi_cache_ttl     INTEGER NOT NULL DEFAULT 300,
		fastcgi_cache_key     TEXT    NOT NULL DEFAULT '',
		plugin_api_key        TEXT    NOT NULL DEFAULT '',
		monitoring_enabled    INTEGER NOT NULL DEFAULT 0,
		monitoring_interval   INTEGER NOT NULL DEFAULT 5,
		disable_wp_updates    INTEGER NOT NULL DEFAULT 0,
		disable_file_editing  INTEGER NOT NULL DEFAULT 0,
		xmlrpc_enabled        INTEGER NOT NULL DEFAULT 0,
		wp_debug_enabled      INTEGER NOT NULL DEFAULT 0,
		wp_post_revisions     INTEGER NOT NULL DEFAULT -1,
		wp_memory_limit       TEXT    NOT NULL DEFAULT '',
		file_lock_enabled     INTEGER NOT NULL DEFAULT 0,
		file_lock_enabled_at  TEXT    NOT NULL DEFAULT '',
		log_retention_days    INTEGER NOT NULL DEFAULT 7,
		cdn_realip_enabled    INTEGER NOT NULL DEFAULT 0,
		expires_at            DATETIME,
		created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE INDEX IF NOT EXISTS idx_websites_status ON websites(status)`,
	`CREATE INDEX IF NOT EXISTS idx_websites_domain ON websites(domain)`,

	// ============================================================
	// cron_jobs
	// ============================================================
	`CREATE TABLE IF NOT EXISTS cron_jobs (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		name            TEXT    NOT NULL,
		cron_expression TEXT    NOT NULL,
		command         TEXT    NOT NULL,
		site_id         INTEGER DEFAULT NULL,
		run_as_user     TEXT    DEFAULT '',
		task_type       TEXT    NOT NULL DEFAULT 'command',
		backup_mode     TEXT    NOT NULL DEFAULT 'incremental',
		keep_count      INTEGER NOT NULL DEFAULT 3,
		notify_fail     INTEGER NOT NULL DEFAULT 0,
		enabled         INTEGER NOT NULL DEFAULT 1,
		running         INTEGER NOT NULL DEFAULT 0,
		last_run_at     DATETIME,
		last_status     TEXT    DEFAULT '',
		last_output     TEXT    DEFAULT '',
		created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (site_id) REFERENCES websites(id) ON DELETE SET NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_cron_jobs_enabled ON cron_jobs(enabled)`,

	// ============================================================
	// monitoring_metrics
	// ============================================================
	`CREATE TABLE IF NOT EXISTS monitoring_metrics (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		cpu_percent        REAL,
		memory_percent     REAL,
		memory_used_bytes  INTEGER,
		memory_total_bytes INTEGER,
		disk_read_bytes    INTEGER,
		disk_write_bytes   INTEGER,
		load_avg_1         REAL,
		load_avg_5         REAL,
		load_avg_15        REAL,
		recorded_at        DATETIME NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_metrics_recorded ON monitoring_metrics(recorded_at)`,

	// ============================================================
	// firewall_bans
	// ============================================================
	`CREATE TABLE IF NOT EXISTS firewall_bans (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		ip_address  TEXT    NOT NULL,
		ban_level   INTEGER NOT NULL DEFAULT 2,
		reason      TEXT    DEFAULT '',
		source_jail TEXT    DEFAULT 'panel',
		banned_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		expires_at  DATETIME,
		unbanned_at DATETIME,
		ban_count   INTEGER NOT NULL DEFAULT 1,
		is_manual   INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_bans_ip ON firewall_bans(ip_address)`,
	`CREATE INDEX IF NOT EXISTS idx_bans_status ON firewall_bans(unbanned_at)`,

	// ============================================================
	// login_attempts
	// ============================================================
	`CREATE TABLE IF NOT EXISTS login_attempts (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		ip_address   TEXT    NOT NULL,
		attempt_type TEXT    NOT NULL,
		created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE INDEX IF NOT EXISTS idx_attempts_ip_type ON login_attempts(ip_address, attempt_type, created_at)`,

	// ============================================================
	// security_settings
	// ============================================================
	`CREATE TABLE IF NOT EXISTS security_settings (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		skey        TEXT    NOT NULL UNIQUE,
		svalue      TEXT    NOT NULL DEFAULT '',
		description TEXT    DEFAULT '',
		updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	// ============================================================
	// operation_logs
	// ============================================================
	`CREATE TABLE IF NOT EXISTS operation_logs (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		operation  TEXT    NOT NULL,
		target     TEXT    DEFAULT '',
		status     TEXT    NOT NULL DEFAULT 'success',
		message    TEXT    DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	// ============================================================
	// file_security_events
	// ============================================================
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

	// ============================================================
	// ssl_certificates
	// ============================================================
	`CREATE TABLE IF NOT EXISTS ssl_certificates (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id    INTEGER NOT NULL UNIQUE,
		domains    TEXT    NOT NULL,
		cert_path  TEXT    NOT NULL,
		key_path   TEXT    NOT NULL,
		issuer     TEXT    DEFAULT 'Let''s Encrypt',
		issued_at  DATETIME,
		expires_at DATETIME NOT NULL,
		auto_renew INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (site_id) REFERENCES websites(id) ON DELETE CASCADE
	)`,

	// ============================================================
	// template_versions
	// ============================================================
	`CREATE TABLE IF NOT EXISTS template_versions (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		template_type TEXT    NOT NULL,
		version       TEXT    NOT NULL,
		description   TEXT    DEFAULT '',
		is_active     INTEGER NOT NULL DEFAULT 1,
		created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(template_type, version)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_template_active ON template_versions(template_type, is_active)`,

	// ============================================================
	// cdn_realip_groups
	// ============================================================
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

	// ============================================================
	// seed: security_settings
	// ============================================================
	`INSERT OR IGNORE INTO security_settings (skey, svalue, description) VALUES
		('panel_title',              'WP Panel', 'Panel title (shown in sidebar and browser tab)'),
		('whitelist_ips',            '',         'Merged official and custom whitelist IPs/ranges'),
		('fail2ban_maxretry',        '5',        'Fail2ban trigger threshold'),
		('fail2ban_findtime',        '60',       'Fail2ban statistics time window (seconds)'),
		('fail2ban_bantime',         '600',      'Fail2ban first offense ban duration (seconds)'),
		('auto_whitelist_enabled',   'true',     'Automatically update official whitelist weekly'),
		('official_whitelist_ips',   '',         'Officially auto-fetched whitelist IPs/ranges'),
		('cloudflare_realip_ips',    '',         'Cloudflare Real IP official IP ranges'),
		('last_whitelist_update',    '',         'Last whitelist update time'),
		('rate_limit_enabled',       'true',     'Enable global rate limiting'),
		('rate_limit_rpm',           '60',       'Max requests per minute per IP'),
		('rate_limit_burst',         '300',      'Burst buffer allowance'),
		('bot_limit_enabled',        'false',    'Enable Bot UA unified rate limiting'),
		('bot_limit_rpm',            '30',       'Max requests per minute per site for bots'),
		('bot_limit_burst',          '20',       'Bot burst buffer allowance'),
		('googlebot_ips',            '',         'Googlebot official IP range cache'),
		('bingbot_ips',              '',         'Bingbot official IP range cache'),
		('wp_security_log_whitelist','',         'WordPress security log path whitelist'),
		('smtp_host',                '',         'SMTP server address'),
		('smtp_port',                '587',      'SMTP port'),
		('smtp_encryption',          'starttls', 'Encryption: starttls/ssl/none'),
		('smtp_user',                '',         'Sender email account'),
		('smtp_pass',                '',         'Sender email password/authorization code'),
		('admin_email',              '',         'Admin notification email'),
		('alert_cpu',                'true',     'CPU > 80% for 5 minutes alert'),
		('alert_memory',             'true',     'Available memory < 10% for 5 minutes alert'),
		('alert_disk',               'true',     'Disk > 90% alert'),
		('alert_service',            'true',     'Service process abnormal restart alert'),
		('alert_ssl',                'true',     'SSL certificate expiration alert'),
		('alert_backup',             'true',     'Database backup failure alert'),
		('alert_website_expiry',     'true',     'Website expiration alert'),
		('alert_remote_backup',      'false',    'Remote backup failure alert (requires remote backup enabled first)'),
		('alert_cron_fail',          'true',     'Scheduled task execution failure alert'),
		('alert_site',               'true',     'Website unavailable alert'),
		('alert_system_update',      'true',     'System available update alert'),
		('alert_panel_update',       'true',     'Panel new version alert'),
		('telemetry_enabled',        'true',     'Anonymous installation statistics (only reports machine ID and version)'),
		('telemetry_url',            '',         'Custom statistics reporting URL (leave empty for default)'),
		('github_proxy',             '',          'GitHub reverse proxy address, leave empty for direct connection'),
		('panel_auto_update_enabled','false',     'Enable panel auto-update'),
		('panel_auto_update_mode',   'patch_only','Panel auto-update mode: patch_only/all_stable'),
		('panel_auto_update_window', '03:00-05:00','Panel auto-update time window'),
		('panel_auto_update_release_delay_minutes','15','Panel auto-update release delay in minutes'),
		('panel_auto_update_signature_timeout_minutes','120','Panel auto-update signature wait timeout in minutes'),
		('panel_auto_update_last_target_version','','Panel auto-update last target version'),
		('panel_auto_update_last_check_at','','Panel auto-update last check time'),
		('panel_auto_update_last_attempt_at','','Panel auto-update last attempt time'),
		('panel_auto_update_last_status','','Panel auto-update last status'),
		('panel_auto_update_last_stage','','Panel auto-update last stage'),
		('panel_auto_update_last_error','','Panel auto-update last error'),
		('panel_auto_update_last_success_at','','Panel auto-update last success time'),
		('panel_auto_update_last_success_version','','Panel auto-update last success version'),
		('panel_auto_update_signature_wait_version','','Panel auto-update signature wait version'),
		('panel_auto_update_signature_wait_at','','Panel auto-update signature wait start time')`,

	// ============================================================
	// seed: template_versions
	// ============================================================
	`INSERT OR IGNORE INTO template_versions (template_type, version, description, is_active) VALUES
		('nginx_http',   'v1.0', 'HTTP default template',             1),
		('nginx_https',  'v1.0', 'HTTPS (with SSL) template',         1),
		('php_fpm_pool', 'v1.0', 'PHP-FPM Pool isolation template',   1)`,

	// ============================================================
	// seed: cdn_realip_groups

	// ============================================================
	`INSERT OR IGNORE INTO cdn_realip_groups (name, provider, header_name, ip_ranges, builtin, enabled, description) VALUES
		('Cloudflare', 'cloudflare', 'CF-Connecting-IP', '', 1, 1, 'Cloudflare official IP ranges, auto-fetched by panel'),
		('General CDN (compatible mode)', 'compatible', 'X-Forwarded-For', '', 1, 1, 'Skip source IP verification, trust X-Forwarded-For directly')`,

	// ============================================================
	// db_backups
	// ============================================================
	`CREATE TABLE IF NOT EXISTS db_backups (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id           INTEGER NOT NULL,
		filename          TEXT    NOT NULL,
		file_size         INTEGER DEFAULT 0,
		db_name           TEXT    NOT NULL,
		auto              INTEGER NOT NULL DEFAULT 0,
		transport_status  TEXT    DEFAULT 'local',
		transport_message TEXT    DEFAULT '',
		created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (site_id) REFERENCES websites(id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_backups_site ON db_backups(site_id, created_at)`,

	// ============================================================
	// backup_settings
	// ============================================================
	`CREATE TABLE IF NOT EXISTS backup_settings (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id    INTEGER NOT NULL UNIQUE,
		enabled    INTEGER NOT NULL DEFAULT 0,
		keep_count INTEGER NOT NULL DEFAULT 7,
		FOREIGN KEY (site_id) REFERENCES websites(id) ON DELETE CASCADE
	)`,

	// ============================================================
	// process_guard_incidents
	// ============================================================
	`CREATE TABLE IF NOT EXISTS process_guard_incidents (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		service    TEXT    NOT NULL,
		event      TEXT    NOT NULL,
		message    TEXT    DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE INDEX IF NOT EXISTS idx_guard_service ON process_guard_incidents(service, created_at)`,

	// ============================================================
	// alert_log
	// ============================================================
	`CREATE TABLE IF NOT EXISTS alert_log (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		alert_type TEXT    NOT NULL,
		level      TEXT    NOT NULL DEFAULT 'warning',
		message    TEXT    NOT NULL,
		resolved   INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`CREATE INDEX IF NOT EXISTS idx_alert_log_type ON alert_log(alert_type, created_at)`,

	// ============================================================
	// wp_extension_config — Default theme/plugin auto-install configuration
	// ============================================================
	`CREATE TABLE IF NOT EXISTS wp_extension_config (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		etype   TEXT    NOT NULL,
		slug    TEXT    NOT NULL,
		name    TEXT    NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1,
		UNIQUE(etype, slug)
	)`,
	`INSERT OR IGNORE INTO wp_extension_config (etype, slug, name, enabled) VALUES
		('theme',  'hello-elementor',   'Hello Elementor',  1),
		('theme',  'astra',             'Astra',            1),
		('theme',  'kadence',           'Kadence',          1),
		('theme',  'blocksy',           'Blocksy',          1),
		('plugin', 'elementor',         'Elementor',        1),
		('plugin', 'wordpress-seo',     'Yoast SEO',        1),
		('plugin', 'seo-by-rank-math',  'Rank Math SEO',    1),
		('plugin', 'woocommerce',       'WooCommerce',      1),
		('plugin', 'naibabiji-b2b-product-showcase', 'B2B Product Catalog', 1),
		('plugin', 'redis-cache',          'Redis Cache',      1)`,

	// Remote backup settings
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

	// ============================================================
	// ai_settings
	// ============================================================
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

	// ============================================================
	// ai_sessions
	// ============================================================
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

	// ============================================================
	// ai_messages
	// ============================================================
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
}
