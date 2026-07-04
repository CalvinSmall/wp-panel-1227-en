# WP Panel Install Script Security Transparency Report

> **Subtitle**: Step-by-step breakdown of `install.sh`, addressing allegations of "password tampering, Nginx deletion, WordPress hacking"

---

## 1. Preface: Conclusion First

Someone claimed in a GitHub Issue:

> "Server passwords were directly tampered with, wp all jumped to the periodic table, nginx was also deleted"

This is a **completely falsifiable allegation**. WP Panel's install scripts `install.sh` and `install-cn.sh` are both **open-source files** that anyone can open and read line by line. This article will break down the entire installation process into a dozen or so independent steps, explaining what each step does, why it's needed, and what operations it **cannot possibly** perform.

**Core conclusion first**:

| Allegation | Actual install script behavior | Can it achieve the alleged effect |
|------------|-------------------------------|-----------------------------------|
| Tampering with server root/SSH passwords | **Completely does not touch** `/etc/shadow`, `/etc/ssh/sshd_config`, or the system user framework | ❌ Impossible |
| Hacking WordPress (modifying files) | Only downloads official ZIP from `wordpress.org` for backup, **does not read, modify, or scan** existing sites | ❌ Impossible |
| Deleting Nginx | **Installs and enables** Nginx, uninstall also explicitly preserves site data | ❌ Impossible |

---

## 2. `install-cn.sh`: Just an "Entry Greeter"

Let's start with the simple one. The `install-cn.sh` that China users see is still very short, with core logic in just three lines:

```bash
export WP_PANEL_PREFER_CN_MIRROR=1    # Flag: prioritize China mirrors
bash install.sh --prefer-cn            # Call main script with China-priority parameter
```

If `install.sh` exists in the same directory, it runs the local file; otherwise, it tries `gh.wp-panel.org`, `jsDelivr`, and direct GitHub access in order from a fixed whitelist, continuing to try the next source if the content looks abnormal.

**Security points**:
- It does not execute any system operations, it's just a "**jump script**".
- You can save the fetched `install.sh` content locally for review before executing it.
- There is no situation where "two scripts each do their own thing with hidden malicious logic".

---

## 3. `install.sh` Overview: Understanding 1000 Lines of Script in One Table

Although `install.sh` is long, its structure is very clear. It can be divided into the following functional modules (line numbers may vary slightly by version; refer to the current source code):

| Module | Line Range | What It Does | Involves Password/Deletion Operations |
|--------|------------|--------------|---------------------------------------|
| Initialization and Parameter Parsing | 1–144 | Color output, log functions, parsing `--prefer-cn` and other parameters | ❌ No |
| System Kernel Optimization | 53–120 | TCP tuning, BBR, file descriptor limits | ❌ No |
| Debian / PHP Source Configuration | 150–282 | Selecting Debian mirror sources, adding PHP 8.3 source (USTC/SJTU/Official) | ❌ No |
| Uninstall/Cleanup Functions | 288–397 | Defining uninstall logic (not executed during installation) | ⚠️ Only executed during uninstall |
| Duplicate Installation Detection | 400–491 | Detecting if already installed, providing repair/uninstall options | ❌ No |
| Swap Configuration | 494–517 | Auto-creating 2GB Swap when memory ≤1GB | ❌ No |
| APT Base Component Installation | 519–569 | Installing nginx, mariadb, redis, fail2ban, PHP extensions, etc. | ❌ Installation, not deletion |
| systemd Process Guarding | 572–589 | Configuring auto-restart for nginx/php/mariadb/redis on crash | ❌ No |
| Nginx Base Configuration | 592–616 | Writing rate limiting and FastCGI cache configuration | ❌ No |
| Firewall Allow 8443 | 619–634 | nftables/ufw allowing panel port | ❌ Only opens one port |
| MariaDB Security Hardening | 637–680 | Setting root password, deleting empty users, deleting test database, disabling remote root | ⚠️ Sets MariaDB password, not system password |
| Directory Structure and Permissions | 683–691 | Creating `/www/server/panel` and other directories, permission 700 | ❌ No |
| Self-Signed SSL Certificate | 694–711 | Generating 2048-bit RSA certificate locally, valid for 10 years | ❌ Generated locally, no network connection |
| Download WordPress Package | 714–732 | Downloading latest official ZIP from wordpress.org | ❌ Downloaded for backup only |
| Generate Panel Security Credentials | 735–760 | Generating random passwords from /dev/urandom, bcrypt hash storage | ⚠️ Generates panel's own passwords |
| Write config.json | 763–826 | Writing panel configuration to JSON file, permission 600 | ⚠️ Writes config, no backdoor |
| Deploy Panel Binary | 829–877 | Downloading or copying Go-compiled binary to `/usr/local/bin` | ❌ No |
| Create systemd Service | 880–907 | Registering `wp-panel.service`, auto-start on boot | ❌ No |
| Port Detection and Output | 910–1013 | Checking if 8443 is listening, printing access address and password | ❌ Output only |

Now let's go through each section in detail.

---

## 4. Section-by-Section Breakdown: What Every Line Does

### 4.1 Permission Check and Duplicate Installation Detection (Lines 400–491)

```bash
if [[ $EUID -ne 0 ]]; then
    log_error "This script must be run with root privileges"
fi
```

**Why root is needed?** Because the panel needs to install system packages (nginx, mariadb), write to `/etc/nginx`, and manage system services. This is a common requirement for any server panel (including Baota, cPanel), not a special design of WP Panel.

**Security design highlights**:
- If a duplicate installation is detected, the script **stops to ask** instead of forcefully overwriting. Four options are provided: uninstall and reinstall / uninstall only / complete wipe / exit.
- If a previous interrupted installation is detected (with residual files), it also asks whether to continue repair, clean reinstall, or exit.
- **There is no silent overwrite or stealthy reinstall behavior.**

### 4.2 System Kernel Optimization (Lines 53–120)

```bash
cat > /etc/sysctl.d/99-wp-panel.conf << 'SYSCTLEOF'
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 8192
# ... TCP buffers, TIME-WAIT, Keepalive, BBR, etc.
SYSCTLEOF
```

This writes a set of **publicly standard Linux network tuning parameters**, common in all web server optimization guides. Then `sysctl --system` is executed to apply them.

- What's being changed are **network stack parameters**, not passwords.
- Single-core VPS will also intelligently skip BBR to avoid CPU contention.
- The file naming `99-wp-panel.conf` is for easy identification and future cleanup.

### 4.3 PHP 8.3 Source Configuration (Lines 150–282)

Debian 13's official repository has PHP version 8.4, but WordPress recommends PHP 8.3, which currently has better compatibility with the WordPress ecosystem. Therefore, Ondřej Surý's PHP source needs to be added to support PHP 8.3 installation. The script provides three fallback levels, with China mode prioritizing domestic mirrors:

1. **USTC Mirror**
2. **SJTU Mirror**
3. **Official Source** (`packages.sury.org`, final fallback)

```bash
# Download and install GPG public key (for verifying package signatures)
download_file "$PHP_KEY_URL" "$tmp_key" 20
dpkg -i "$tmp_key"

# Write apt source list
cat > /etc/apt/sources.list.d/php.sources << PHPSOURCESEOF
Types: deb
URIs: ${PHP_REPO_URL}
Suites: ${codename}
Components: main
Signed-By: ${keyring_file}
PHPSOURCESEOF
```

**Security points**:
- All PHP packages are installed via `apt`, protected by GPG signatures.
- The script first runs `apt-get update` and verifies whether `php8.3-cli` and `php8.3-fpm` have candidate versions; **if unavailable, it tries the next source**, only terminating if all fail.
- No remote code execution or password modification is involved.

### 4.4 Base Component Installation (Lines 547–569)

```bash
apt-get install -y \
    nginx \
    mariadb-server \
    redis-server \
    fail2ban \
    nftables \
    php8.3-fpm php8.3-mysql php8.3-curl ...
```

**This is installing software, not deleting it.** What's installed:
- **nginx**: Web server
- **mariadb-server**: Database
- **redis-server**: Cache
- **fail2ban**: Intrusion prevention (auto-bans brute-force IPs)
- **nftables**: Firewall framework
- **php8.3-***: PHP and common extensions

These are all standard packages from the Debian official repository, with publicly available versions and verifiable signatures.

In China mode, the script checks Nanjing University, USTC, Tsinghua, and Debian official sources in order before installing base components, while simultaneously writing `debian`, `debian-security`, and `debian-updates`. Each source runs `apt-get update` first, then checks whether key packages like `nginx`, `mariadb-server`, `redis-server`, `ca-certificates` have candidate versions; if domestic mirrors are unavailable or have incomplete sync, it will warn "Domestic mirror sync may be delayed, falling back to official source".

### 4.5 systemd Process Guarding (Lines 572–589)

```bash
for svc in nginx php8.3-fpm mariadb redis-server; do
    mkdir -p "/etc/systemd/system/${svc}.service.d"
    cat > "/etc/systemd/system/${svc}.service.d/wp-panel.conf" << SYSTEMDEOF
[Service]
Restart=always
RestartSec=5s
StartLimitIntervalSec=0
SYSTEMDEOF
done
```

Adds an **override configuration** for nginx, php-fpm, mariadb, and redis: if the process crashes unexpectedly, it auto-restarts after 5 seconds. This is a standard system stability operation that does not change any service's data or passwords.

### 4.6 Nginx Base Configuration (Lines 592–616)

The script writes two configuration files:

1. **`/etc/nginx/conf.d/wppanel-ratelimit.conf`** — Request rate limiting:
   - Logged-in WordPress users (with `wordpress_logged_in` cookie) are **not rate-limited**
   - Unlogged access is rate-limited to **60 requests/minute**
2. **`/etc/nginx/conf.d/wppanel-cache.conf`** — FastCGI cache path configuration

Then `nginx -t` is executed to test configuration validity, followed by `nginx -s reload` for a smooth reload.

**Security significance**: This is **hardening protection**, not destruction.

### 4.7 Firewall Allow 8443 (Lines 619–634)

```bash
# nftables
nft add rule inet filter input tcp dport 8443 accept

# ufw
ufw allow 8443/tcp
```

Only one thing is done: **allowing the panel's HTTPS management port `8443`**. It will not close ports 22 (SSH), 80, 443, etc. The panel's scan defense module will further harden firewall rules afterward.

### 4.8 MariaDB Security Hardening (Lines 637–680)

This is one of the **few places in the install script involving passwords**, but only MariaDB (database) is involved, not the system root password or SSH password.

```bash
# Generate 32-character random password
MYSQL_PASS=$(head -c 24 /dev/urandom | sha256sum | head -c 32)

# If MariaDB has no password set, set it
mysqladmin -u root password "${MYSQL_PASS}"

# Execute MariaDB official security hardening: delete empty users, delete test database, disable remote root
mysql -u root -p"${MYSQL_PASS}" -e "
    DELETE FROM mysql.user WHERE User='';
    DELETE FROM mysql.user WHERE User='root' AND Host!='localhost';
    DROP DATABASE IF EXISTS test;
    DELETE FROM mysql.db WHERE Db='test' OR Db='test\\_%';
    FLUSH PRIVILEGES;
"
```

**Security points**:
- MariaDB root password is randomly generated from `/dev/urandom`, **not a hardcoded generic password**.
- If MariaDB already has a password and can connect, the script **reuses the existing password** instead of forcibly changing it.
- **Remote root login** is disabled, allowing only localhost connections.
- **Completely does not touch** Linux system root password, `/etc/passwd`, `/etc/shadow`.

### 4.9 Directory Structure and File Permissions (Lines 683–691)

```bash
mkdir -p "$INSTALL_DIR"/{backups,packages,logs,certs}
mkdir -p /www/wwwroot
mkdir -p /www/wwwlogs
mkdir -p /www/server/certificates
chmod 700 "$INSTALL_DIR"       # Only owner can read/write/execute
```

Creates the standard directory structure. Key permission settings:
- Panel data directory `/www/server/panel` has permission `700`: only root can access.
- Subsequent `config.json` has permission `600`: only root can read/write.

### 4.10 Self-Signed SSL Certificate (Lines 694–711)

```bash
openssl req -x509 -nodes -days 3650 -newkey rsa:2048 \
    -keyout "$KEY_FILE" \
    -out "$CERT_FILE" \
    -subj "/C=CN/ST=Shanghai/L=Shanghai/O=WP Panel/OU=IT/CN=WP-Panel-SelfSigned" \
    -addext "subjectAltName=IP:127.0.0.1"
```

- **Generated locally on the server**, no connection to any external CA or API.
- 2048-bit RSA, valid for 10 years.
- Private key file permission `600`, certificate permission `644`.
- After installation, you can replace it with your own official certificate at any time; the panel supports configuring certificate paths.

### 4.11 Downloading Official WordPress Package (Lines 714–732)

```bash
download_file "https://wordpress.org/latest.zip" "$WP_ZIP_TMP" 60
```

- Download source is **`wordpress.org`** — WordPress's official website, not any third-party repository.
- What's downloaded is a ZIP file, **saved in `/www/server/panel/packages/`** for later use with "one-click site creation".
- If the download fails, the script will note "will use online download on first site creation", **not causing site creation to fail**.
- **Does not scan, read, or modify** any existing WordPress sites on the server.

### 4.12 Generating Panel Security Credentials (Lines 735–760)

This is the **most critical security design** in the install script, involving the generation of two-layer authentication passwords:

```bash
# Read true random numbers from /dev/urandom
PANEL_SUFFIX=$(head -c 20 /dev/urandom | sha256sum | head -c 8)
BASIC_PASS=$(head -c 12 /dev/urandom | base64 | head -c 16)
WEB_PASS=$(head -c 12 /dev/urandom | base64 | head -c 16)

# Use PHP or Python for bcrypt hashing (cost=12)
BASIC_HASH=$(php8.3 -r "echo password_hash('$BASIC_PASS', PASSWORD_BCRYPT, ['cost' => 12]);")
WEB_HASH=$(php8.3 -r "echo password_hash('$WEB_PASS', PASSWORD_BCRYPT, ['cost' => 12]);")
```

**Layer-by-layer analysis**:

1. **Entropy source**: `/dev/urandom` is the Linux kernel's cryptographically secure random number generator, not the pseudo-random function `rand()`.
2. **Password complexity**: 8-character random suffix + 16-character random BasicAuth password + 16-character random web password. Brute-force is infeasible.
3. **Storage method**: The config file only stores **bcrypt hashes** (starting with `$2a$12$`), **no plaintext passwords**.
   - bcrypt cost=12 means brute-forcing a password would take years.
   - Even if someone obtains your `config.json`, they **cannot reverse-engineer the original password**.
4. **Degradation protection**: If the server has neither PHP 8.3 nor Python 3 (extremely rare), the script writes a placeholder hash and notes "the panel will auto-reset the password on first startup". **It will not start with weak or empty passwords**.

### 4.13 Writing `config.json` (Lines 763–826)

All configuration is written to a single JSON file:

```json
{
  "panel": { "port": 8888, "tls_port": 8443, "random_suffix": "abc123de" },
  "mariadb": { "root_password": "xxx..." },
  "admin": { "username": "wpadmin", "password_hash": "$2a$12$..." },
  "basic_auth": { "username": "admin", "password_hash": "$2a$12$..." },
  "security": { "max_login_attempts": 5, "ban_duration_hours": 24 }
}
```

**Security points**:
- File permission `600`: only root can read.
- MariaDB root password is stored here, but the panel itself can only connect to MariaDB via **localhost + Unix Socket**, not exposed externally.
- **No hidden backdoor accounts, no hardcoded generic keys, no logic sending passwords to external servers**.

### 4.14 Deploying Panel Binary (Lines 829–877)

```bash
# Prioritize local binary in the same directory
cp "$SCRIPT_DIR/wp-panel" "$BIN_PATH"

# Otherwise download from GitHub Release
GITHUB_RELEASE="https://github.com/naibabiji/wp-panel/releases/latest/download/wp-panel"
```

- If you place the compiled `wp-panel` binary and install script in the same directory, the script **uses the local file directly** without downloading from the network.
- If downloading is needed, the source is **GitHub Releases** (formal releases of the open-source repository), optionally via `gh.wp-panel.org` China reverse proxy.
- After download, `chmod +x` grants execute permission, then moves to `/usr/local/bin/wp-panel`.

**How to verify binary safety?**
- WP Panel is an **open-source project**; you can clone the code yourself, audit the Go source, compile locally, and use.
- The install script provides a local deployment path, **completely supporting offline installation**.

### 4.15 Creating systemd Service (Lines 880–907)

```bash
cat > "$SERVICE_PATH" << SYSTEMDEOF
[Unit]
Description=WordPress Server Management Panel
After=network.target mariadb.service redis-server.service

[Service]
Type=simple
User=root
Group=root
ExecStart=$BIN_PATH --config=$CONFIG_FILE
Restart=always
RestartSec=5
LimitNOFILE=65536
SYSTEMDEOF
```

- Runs as `root` because the panel needs to manage Nginx configuration, restart services, and operate the file system. This is standard practice for server panels.
- `Restart=always`: If the panel process crashes, it auto-restarts after 5 seconds.
- Then `systemctl enable wp-panel` (auto-start on boot) and `systemctl start wp-panel` (start immediately) are executed.

### 4.16 Port Detection and Final Output (Lines 910–1013)

The script finally:
1. Checks if the `wp-panel` service is running normally
2. Checks if port 8443 is listening
3. **Prints the access address and two-layer passwords (displayed only once)**

```
Panel Address: https://<IP>:8443/<random-suffix>/
Layer 1 — BasicAuth (browser popup)  Username: admin / Password: xxxxxxxx
Layer 2 — Web Login (panel form)     Username: wpadmin / Password: xxxxxxxx
```

**Security points**:
- Passwords are **displayed only once in the terminal**, not saved to logs, not sent to any remote server.
- The script's mention of "anonymous installation statistics" includes only: machine anonymous identifier (SHA256 hash of `/etc/machine-id`) + panel version number. **No IP, domain, or password information is included.**

---

## 5. Direct Response to Three Allegations

### Allegation 1: "Server passwords were tampered with"

**Fact**: The install script **never reads or writes** any of the following files:

- `/etc/shadow` (Linux user passwords)
- `/etc/passwd` (user list)
- `/etc/ssh/sshd_config` (SSH configuration)
- `~/.ssh/authorized_keys` (SSH public keys)

The only passwords the script handles are:
1. **MariaDB root password** — randomly generated, used by the panel to manage the database, not the system root password.
2. **The panel's own two-layer login passwords** — randomly generated, stored with bcrypt hashing, completely unrelated to system passwords.

> Your SSH root password **does not change** before or after installation. If you find your password has been changed, please check: whether a weak password was brute-forced, whether keys were leaked elsewhere, or whether other suspicious software was installed.

### Allegation 2: "WP all jumped to the periodic table"

(Note: "Periodic table" refers to error pages or phishing content displayed when certain web pages are tampered with.)

**Fact**: The install script **completely does not touch** any existing files under `/www/wwwroot/`. The only WordPress-related operation in the script is:

```bash
download_file "https://wordpress.org/latest.zip" "$WP_ZIP_TMP" 60
```

- Downloads a ZIP from the **official wordpress.org website** to `/www/server/panel/packages/wordpress.zip`
- This is a **backup package** for subsequent "one-click new site creation"
- **Does not auto-extract to any existing directory**
- **Does not scan, modify, or delete** any existing site files

> If your WordPress site was tampered with, common causes include: using pirated plugins/themes, WordPress core or plugin vulnerabilities not updated promptly, or weak passwords being brute-forced. This is unrelated to the WP Panel install script.

### Allegation 3: "nginx was also deleted"

**Fact**: The install script **installs and enables** Nginx:

```bash
apt-get install -y nginx
systemctl start nginx
systemctl enable nginx
```

Subsequently, it writes rate limiting and cache configurations and executes `nginx -t` (config check) and `nginx -s reload` (smooth reload).

Even during **uninstallation** (`do_uninstall` function), the script explicitly preserves:

```bash
log_info "Panel has been uninstalled. The following content has been preserved:"
log_info "  - /www/wwwroot (website files)"
log_info "  - /www/wwwlogs (website logs)"
log_info "  - /www/server/certificates (SSL certificates)"
log_info "  - MariaDB databases"
log_info "  - System packages (nginx/php/mariadb/redis/fail2ban)"
```

Only when the user **manually selects "complete wipe" (purge)** will nginx and other packages be uninstalled — and this requires **interactive confirmation** with a `yes` input.

> If you find Nginx was deleted, please check: whether you manually ran uninstall or purge, or whether there are other management scripts/personnel operating on the server.

---

## 6. What You Can Verify Yourself

The value of open source is not just "code is public", but "you can verify it yourself". Here are several simple self-check methods:

### Method 1: Review the Install Script (No Execution Required)

```bash
# Download first, look before executing
curl -fsSL https://raw.githubusercontent.com/naibabiji/wp-panel/main/install.sh -o install.sh
# Open with a text editor, search for these keywords:
# passwd, shadow, ssh, rm -rf /etc/nginx, wget non-wordpress.org URLs
# You'll find: the above keywords either don't exist or appear in safe contexts.
```

### Method 2: Offline Local Installation

```bash
# 1. Clone source and compile locally
git clone https://github.com/naibabiji/wp-panel.git
cd wp-panel
go build -o wp-panel .

# 2. Place the compiled binary alongside install.sh
# 3. Disconnect from the internet, run bash install.sh
# The script will use the local wp-panel binary, downloading no external files.
```

### Method 3: Monitor the Installation Process

```bash
# Open another terminal window to monitor file changes in real-time
watch -n 1 'ls -la /etc/shadow; ls -la /www/wwwroot/'

# Simultaneously monitor network connections
ss -tpn

# You'll find: /etc/shadow timestamp unchanged, /www/wwwroot content unchanged,
# and no abnormal external network connections.
```

### Method 4: Check the Installed config.json

```bash
cat /www/server/panel/config.json
# Verify: no hardcoded generic passwords, no reporting endpoints to external servers,
# no suspicious remote command execution configuration.
```

---

## 7. Summary

WP Panel's `install.sh` is essentially an **automated server operations manual**: everything it does — installing Nginx, configuring MariaDB, tuning the kernel, generating random passwords, setting up the firewall — are standard operations that experienced system administrators would manually perform when setting up a WordPress server.

The benefit of open-sourcing all of this is: **there is no room for hidden operations**. Every allegation can be verified by reading the source code, comparing file hashes, and monitoring the installation process.

| Rumor | Truth |
|-------|-------|
| Tampering with server passwords | ❌ The script does not touch `/etc/shadow`, `/etc/ssh`, or any system authentication system |
| Hacking WordPress | ❌ Only downloads official backup packages from wordpress.org, does not touch existing sites |
| Deleting Nginx | ❌ Installs and enables Nginx, uninstall explicitly protects site data |

If you still have concerns, you are welcome to:
1. Open `install.sh` and search for the key line numbers mentioned in this article to verify yourself.
2. Run the installation offline in a local VM or container, observing the behavior at each step.
3. Follow the second article in this series: "After Installation, How WP Panel Protects Your Server Through Multi-Layer Mechanisms"

---

*This article is based on `install.sh` and `install-cn.sh` from the WP Panel open-source repository's commit history. All line numbers and code snippets are publicly verifiable.*
