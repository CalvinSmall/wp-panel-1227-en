# WP Panel

WordPress server management panel for Debian 13 VPS environments, focused on site isolation, SSL, backups, security, and day-to-day WordPress hosting operations.

[![License](https://img.shields.io/badge/license-GPL--3.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](https://go.dev/)

---

## Official Sources

- Official website: <https://wp-panel.org>
- GitHub project: <https://github.com/naibabiji/wp-panel>

Other than `wp-panel.org` and this GitHub repository, no other domain names are official WP Panel websites or affiliated with this project.

---

## Positioning

Generic Linux panels are bloated, complex, and full of WordPress-irrelevant features.

WP Panel does one thing: **manage WordPress sites efficiently on a VPS**. No Docker, no mail systems, no FTP, no Java/Python/Node runtimes.

## Feature Modules

| Module | Description |
|--------|-------------|
| **Site Management** | One-click site creation (auto-creates isolated user/directory/Nginx/PHP-FPM/database), pause/enable/delete, reinstall WordPress |
| **SSL Certificates** | Let's Encrypt auto-request, auto-renew 30 days before expiry, manual replacement, self-signed certificates |
| **FastCGI Cache** | Nginx full-site static caching, companion WordPress plugin for one-click purge |
| **Security Defense** | Fail2ban + nftables dual-mechanism progressive banning, Cloudflare/Google/Bing official whitelists, global rate limiting, WordPress security event detection and log analysis |
| **Database Management** | MariaDB password changes, database backup/restore/upload restore/auto backup |
| **Scheduled Tasks** | Visual Cron management, WP Cron replacement, incremental file backup, system task viewer |
| **File Manager** | Upload/download/delete/rename/compress/decompress/cut/copy/paste/multi-select, chunked upload with resume |
| **Dashboard** | Real-time CPU/memory/disk/load monitoring, 24h/7d/15d historical trend charts |
| **AI Diagnostics** | One-click site health analysis, session-based follow-up questions and diagnostic records, focused on logs and service status clues |
| **Alert Notifications** | SMTP email alerts, independent on/off switches for CPU/memory/disk/service/SSL/site expiry/system update/panel update rules |
| **Software Management** | PHP/Nginx/MariaDB/Redis configuration changes, process guarding, log viewing |
| **Panel Security** | Random entry path + BasicAuth + Web dual authentication, bcrypt password hashing, login failure banning |
| **Version Updates** | In-panel one-click update checking, SHA256 + Ed25519 dual verification, auto-rollback on failure, configurable reverse proxy for China |
| **Suspicious Access Analysis** | WordPress site access log aggregation analysis, generates risk-level-based recommendations with suspicious IP/path evidence |
| **Bot Rate Limiting** | Bot-UA independent rate limiting (configurable RPM/Burst), reduces impact from scrapers and high-frequency script requests |
| **Remote Backup** | Site backup supports rsync/SSH and S3 object storage remote sync, optional local backup copy retention |

## One-Click Installation

```bash
apt-get update && apt-get install -y wget ca-certificates && wget -qO- https://raw.githubusercontent.com/naibabiji/wp-panel/main/install.sh | bash
```

**China servers**: When GitHub is inaccessible, use the China-optimized script:

```bash
apt-get update && apt-get install -y wget ca-certificates && wget -qO- https://gh.wp-panel.org/https://raw.githubusercontent.com/naibabiji/wp-panel/main/install-cn.sh | bash
```

After installation, the panel address and two-layer login credentials (BasicAuth + Web login) are displayed.

> Self-signed certificates will trigger a browser security warning on first access. Click "Advanced" → "Continue" to proceed.

## Security

**In short: as long as the login address and credentials are not leaked, no one can get in.**

The panel has four layers of defense:
- Layer 0: **Scan Defense** — Non-browser requests hitting port 8443 are immediately identified and blocked at the Nftables network layer for 30 days
- Layer 1: Random Entry Path — 8-character random hex path (16^8 ≈ 4.3 billion combinations), impossible for scanners to guess
- Layer 2: BasicAuth — Browser popup requesting username and password
- Layer 3: Web Login — Web form requiring panel login password

Only those passing all four layers can access the panel. Any single layer failing 5 times results in a ban.

---

More detailed security mechanisms:

**Access Protection**
- **Scan Defense**: Panel port 8443 automatically detects non-browser requests (curl, scripts, scanners), blocking at the Nftables network layer
- Random entry address (16^8 ≈ 4.3 billion combinations, combined with scan defense makes brute-force infeasible)
- BasicAuth + Web login dual authentication
- Pure HTTPS encrypted communication, panel only exposes port 8443
- API error messages do not leak internal paths or command output

**Brute-Force Prevention**
- Any authentication layer failing 5 consecutive times → Nftables network layer ban for 24 hours
- Progressive multi-level banning: 10 minutes → 24 hours → 30 days → permanent

**Site Isolation**
- Each website runs under an isolated system user and PHP-FPM Pool
- Each website uses an isolated MariaDB database
- One site's issues do not affect other sites

**WordPress-Specific Protection**
- Auto-detect and ban wp-login.php brute-force and xmlrpc.php malicious requests
- Sensitive file scanning (.env, .git, archives, etc.) → auto-ban
- 404 flood detection: 30 requests/60 seconds is considered directory scanning
- Nginx rejects HTTPS connections from unknown domains to prevent certificate information leakage
- Logged-in WordPress users are automatically exempt from rate limiting to avoid disrupting backend operations
- **WordPress Suspicious Access Analysis**: Aggregates `wp-security.log` and `error.log` with 30s cache, outputs high/medium/low-risk events, hit paths, suspicious IPs, and handling recommendations (analysis only by default, no auto-banning)
- **Runtime Security Monitoring**: Detects new suspicious PHP files and high-frequency access in `wp-content` runtime directory, generating file security event records
- **Bot Rate Limiting**: Provides independent `bot_limit_enabled`/`bot_limit_rpm`/`bot_limit_burst` policies; normal user traffic is unaffected, focusing on restricting high-frequency crawlers and scanning behavior

**AI Operations Diagnostics**
- Site AI diagnostics aggregate logs, PHP/database/service status, and runtime context, supporting session-based follow-up questions; read-only path analysis only, no file/database modifications or repair commands
- Useful for quickly identifying anomalous trends, suggesting reproduction steps, and creating traceable recommendation records

**Backup and Off-Site Archiving**
- Site backup tasks now support S3 object storage, with endpoint connectivity probing and callback testing; sync failures can retain local copies, with configurable restorative cleanup

**Update Security**
- Panel updates use SHA256 + Ed25519 dual verification; even if attackers compromise GitHub Releases, they cannot forge signatures
- Update failures automatically roll back to the old version, keeping the panel operational

**Code Transparency**
- 100% open source (GPL-3.0), auditable code
- No sensitive business data collection, anonymous statistics (version number only) can be turned off with one click in the panel
- Update checks only connect to GitHub, no other external services
- No Web Shell, no online code editing
- Passwords stored with bcrypt 12-round hashing, no plaintext stored
- Three rounds of AI security audits have fixed 44 potential issues

### Deep Security Analysis

- **[Install Script Security Transparency Report](security/wp-panel-install-security.md)** — Step-by-step breakdown of install.sh, addressing allegations of "password tampering, Nginx deletion, WordPress hacking"
- **[Runtime Security: Multi-Layer Protection Mechanism](security/wp-panel-runtime-security.md)** — Source-level analysis of six-layer defense-in-depth, update signature verification, and software vulnerability management

## Security Testing

White-hat hackers and security researchers are welcome to test this project. If you find a security vulnerability, please report it via:

- **Public feedback**: Submit a [GitHub Issue](https://github.com/naibabiji/wp-panel/issues), tag the title with `[Security]`
- **Private feedback**: Submit a Private Vulnerability Report through the GitHub Security tab
- Valid vulnerabilities will be credited to the reporter in Release Notes after a fix

## System Requirements

| Item | Requirement |
|------|-------------|
| Operating System | Debian 13 (Trixie) |
| CPU | 1 core or higher |
| Memory | 1 GB or higher (Swap auto-created if lower) |
| Architecture | x86_64 |

> Customized images from various cloud providers may cause unexpected issues. If you encounter difficulties installing, it is recommended to reinstall a clean Debian 13 using [bin456789/reinstall](https://github.com/bin456789/reinstall) and try again.

## Why These Technical Choices

**Why Debian 13?**

Debian is one of the most stable distributions in the server space. Trixie (Debian 13) was the latest stable release when panel development started, offering the latest kernel and newer package versions while maintaining Debian's traditionally conservative stability policy. Choosing this version means the panel can benefit from long-cycle security update support without users needing to frequently upgrade their system.

**Why PHP 8.3?**

WordPress officially recommends PHP 8.3 or higher. 8.3 has undergone the most extensive production validation in the WordPress ecosystem, with an active support cycle and continuous performance and security improvements. Locking the version means all users run the same PHP environment, issues are reproducible and debuggable, avoiding compatibility quirks caused by PHP version differences.

**Why MariaDB instead of MySQL?**

WordPress officially recommends MariaDB 10.6 or higher. The MariaDB versions included with Debian 12/13 both meet this requirement. Oracle MySQL carries license and feature restriction risks; MariaDB is a fully compatible GPL fork driven by the community.
Debian's built-in MariaDB LTS version provides security updates until 2028, with no need for third-party repositories.

**Why a custom-compiled Go binary instead of Docker/PM2?**

A single binary file, zero dependencies, managed by `systemd`. Uses only a dozen MB of memory, suitable for 1G small VPS. Does not share ports with Nginx, each providing HTTPS independently. No container layer, no runtime overhead.

## Runtime Components

All components are installed via the APT package manager; the panel does not compile anything itself:

| Component | Description |
|-----------|-------------|
| PHP 8.3 | Ondřej Surý source, isolated FPM Pools |
| MariaDB | Debian built-in LTS version |
| Nginx | Debian built-in stable version |
| Redis | Debian built-in |
| Fail2ban + nftables | Debian built-in |

## Technical Architecture

- **Backend**: Go + Gin web framework, SQLite (WAL mode), port 8443 (HTTPS/TLS)
- **Frontend**: HTML templates + TailwindCSS + Alpine.js + Chart.js
- **Distribution**: Single binary file (frontend resources embedded via `//go:embed`), approximately 20 MB
- **Security**: Panel is not coupled with Nginx reverse proxy, with independent TLS encryption

## SSH Management Commands

After installation, the panel provides a `wp` command-line tool:

| Command | Description |
|---------|-------------|
| `wp` | View panel information |
| `wp restart` | Restart the panel |
| `wp password` | One-click admin password reset |
| `wp info` | View version/port/entry path |
| `wp status` | View running status |
| `wp unban` | Clear all IP bans (emergency recovery when admin is mistakenly banned) |

## Panel Database Backup and Recovery

The panel uses SQLite for data storage, automatically backing up daily at 2:30 AM to `/www/server/panel/backups/panel-db/`, retaining the 7 most recent backups.

### When the Panel is Running Normally

On the "Panel Settings" page you can:
- Manually create backups
- Download backup files locally
- Restore from backup (auto-creates a safety backup before restoring, panel auto-restarts after restore)
- Delete backups

### Recovery Steps When the Panel Cannot Start

If the panel cannot start after database recovery, or the database is corrupted preventing panel operation, manually restore via SSH:

```bash
# 1. View available backups
ls -lh /www/server/panel/backups/panel-db/

# 2. Stop the panel
systemctl stop wp-panel

# 3. Back up the current corrupted database (just in case)
cp /www/server/panel/panel.db /www/server/panel/panel.db.broken

# 4. Replace the current database with the backup (use the actual backup filename)
cp /www/server/panel/backups/panel-db/panel_20260107_023000.db /www/server/panel/panel.db

# 5. Start the panel
systemctl start wp-panel

# 6. Check if it's working
systemctl status wp-panel
journalctl -u wp-panel -n 20
```

### Importing Backups After Reinstalling the Panel

If you need to completely reinstall the panel and restore data:

```bash
# 1. First save backup files to a safe location
cp -r /www/server/panel/backups/panel-db/ /root/panel-db-backup/

# 2. Reinstall the panel (choose "uninstall and reinstall", keeping site data)

# 3. Stop the panel after installation completes
systemctl stop wp-panel

# 4. Replace the new database with the backup
cp /root/panel-db-backup/panel_20260107_023000.db /www/server/panel/panel.db

# 5. Start the panel (auto-runs database upgrades)
systemctl start wp-panel
```

> **Note**: Older backups may lack newer database fields; the panel will automatically supplement them through the upgrade chain on startup.

## Project Structure

```
├── main.go               # Program entry point
├── config/               # Global configuration management
├── database/             # SQLite connection and migration
├── models/               # Data structures
├── router/               # Routing + page dispatch
├── middleware/            # BasicAuth / Session / CSRF / Login rate limiting
├── handlers/             # HTTP handlers
├── executor/             # Task executor
├── collector/            # System metrics collection
├── templates/            # HTML templates
├── static/               # JS
├── input.css             # TailwindCSS source file
├── install.sh            # One-click install script
├── install-cn.sh         # China-optimized install script
├── security/             # Security documentation
└── wp-panel-optimizer/   # WordPress companion plugin
```

## License

GPL-3.0
