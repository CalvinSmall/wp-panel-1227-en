# After Installation, How WP Panel Protects Your Server Through Multi-Layer Mechanisms

> **Subtitle**: Addressing "Will the panel secretly change passwords or plant trojans?" and "Is the panel secure when server passwords aren't leaked?"

---

## 1. Preface

In the first article, we proved: WP Panel's install script is transparent — it won't tamper with your server passwords, hack WordPress, or delete Nginx. But after installation, new questions naturally arise:

- **After the panel is running, will it secretly change my password?**
- **The panel has auto-updates — could it be hijacked to plant trojans?**
- **My SSH password hasn't leaked — is the panel itself secure enough?**

This article will break down all of WP Panel's post-installation runtime behavior from a **source code perspective**, explaining how each layer of protection works, and why — when server passwords aren't leaked — it's nearly impossible for attackers to compromise your server through the panel.

---

## 2. Will the Panel Secretly Change Passwords?

### 2.1 Password Storage: Even the Panel Itself Can't See Plaintext

WP Panel uses a two-layer authentication system:

| Layer | Purpose | Storage Method |
|-------|---------|----------------|
| Layer 1 — BasicAuth | Browser popup, intercepts first layer of scanning | bcrypt hash stored in `config.json` |
| Layer 2 — Web Login | Panel form login | bcrypt hash stored in SQLite database |

**What is bcrypt?** It's a **one-way password hashing algorithm**. Your entered password is transformed into a string starting with `$2a$12$` — this process is **irreversible** — even if someone obtains the database and config file, they **cannot reverse-engineer your original password**.

When verifying login, the panel does only one thing:

```go
bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(yourPassword))
```

Match → pass; no match → reject. **The panel never saves, never prints, never transmits your plaintext password.**

### 2.2 Under What Circumstances Are Passwords Changed?

The only code paths that can modify passwords are the following **three**, all requiring **your manual trigger**:

| Method | Trigger Condition | Who Can Execute |
|--------|------------------|-----------------|
| Panel settings page change | Login to panel → Settings → Change Password | Administrator who knows the current password |
| CLI one-click reset | Server SSH: `wp password` | Server root user |
| CLI password-only change | Server SSH: `wp-panel --passwd="newpassword"` | Server root user |

**Why does entering `wp password` actually execute a different set of commands?**

`wp` is a **command wrapper script** created during panel installation (stored at `/usr/local/bin/wp`), designed to package complex underlying commands into easy-to-remember daily commands. The mapping is as follows:

| Your Command | Actual Underlying Command | Effect Difference |
|-------------|--------------------------|-------------------|
| `wp password` | `wp-panel --reset-admin` | **Resets both username and password** (username reverts to `wpadmin`, password randomly generated) |
| `wp-panel --passwd="xxx"` | `wp-panel --passwd="xxx"` | **Changes password only, retains current username** |

In other words, `wp password` is more suitable for "I'm locked out, one-click recovery" emergencies; while `wp-panel --passwd="xxx"` is for "I know the current username and just want to change the password" scenarios. Both exist — one is a human-friendly shortcut, the other a low-level interface for precise control.

**Key conclusions**:
- No scheduled task automatically changes passwords at midnight.
- No remote command can remotely modify your password.
- There is no "backdoor password" or "generic key".
- The panel **does not send your password or password hash to any server**.

### 2.3 If Passwords Were Really Changed, More Likely Causes...

If your server password was modified after installing the panel, the investigation order should be:

1. **Were SSH keys leaked** — Check `~/.ssh/authorized_keys` for unknown public keys
2. **Were weak passwords used** — Is the server root password in a dictionary
3. **Was other software installed** — Are there suspicious processes outside the panel
4. **Was the cloud provider console compromised** — Password resets via VNC/console leave no traces

> There is **not a single line** in the panel code that modifies `/etc/shadow`, `/etc/passwd`, or SSH configuration. This can be verified through full-text search.

---

## 3. Will the Panel Plant Viruses or Trojans?

This is the most critical and reasonable concern. A program running in the background with auto-update capability theoretically does carry exploitation risk. We need to analyze this from two dimensions: **update mechanism** and **runtime constraints**.

### 3.1 Auto-Update Mechanism: Triple Independent Verification

WP Panel's update functionality is implemented in `handlers/update.go`, with the following flow:

```
User clicks "Update Panel" → Download new binary → SHA256 verification → Ed25519 signature verification → Preflight check → Backup old version → Replace → Restart
```

#### First Layer: SHA256 Integrity Verification

After download, the panel also downloads a `.sha256` file containing the correct hash. The panel recomputes the downloaded file's SHA256 and **terminates immediately if it doesn't match**.

```go
if err := verifySHA256(newBinary, shaFile); err != nil {
    fail(http.StatusInternalServerError, "Verification failed")
    return
}
```

This ensures the file was not tampered with or corrupted during transmission.

#### Second Layer: Ed25519 Digital Signature

SHA256 only guarantees file integrity, but cannot prove file origin. Therefore, the panel also introduces **Ed25519 asymmetric signatures**:

- **Public key** is hardcoded in the panel source code (`releasePubKeyHex = "ee8ec641..."`)
- **Private key** is held offline by the author, not on GitHub, CI, or any server
- During each release, the author signs the `.sha256` file with the private key, generating `.sha256.sig`
- The panel verifies the signature with the built-in public key; **verification failure terminates the update**

```go
if err := verifyEd25519(shaFile, sigFile); err != nil {
    fail(http.StatusInternalServerError, "Signature verification failed")
    return
}
```

**What does this mean?** Even if an attacker hijacks GitHub Releases and uploads a malicious binary, since they don't have the author's private key, they **cannot generate a valid Ed25519 signature**, and the panel will refuse installation.

#### Third Layer: Preflight Check

Even after passing the first two verifications, the panel runs a **preflight** before replacement:

```go
if err := preflightBinary(newBinary); err != nil {
    fail(http.StatusInternalServerError, "New version preflight failed")
    return
}
```

The preflight runs the new binary's `--info` parameter to confirm it can start normally and won't crash immediately. If the new binary was tampered into a non-executable garbage file, this step will catch it.

#### Backup and Rollback

Only after all verifications above pass does the panel perform replacement, and **always backs up first**:

```go
backupPath := versionedBackupPath(h.CurrentVersion)  // /usr/local/bin/wp-panel.bak.v1.2.3.20240101-120000
if err := copyFile(installPath, backupPath, 0755); err != nil {
    fail(http.StatusInternalServerError, "Failed to backup old version")
    return
}
```

If permission setting fails after replacement, the panel **auto-rolls back**:

```go
if err := os.Chmod(installPath, 0755); err != nil {
    if rbErr := copyFile(backupPath, installPath, 0755); rbErr != nil {
        fail(http.StatusInternalServerError, "Permission setting failed after replacement, and auto-rollback failed")
        return
    }
    fail(http.StatusInternalServerError, "Permission setting failed after replacement, rolled back")
    return
}
```

### 3.2 The Panel Has No "Remote Code Execution" Capability

WP Panel is written in **Go** and compiled into a **static binary**, which means:

- It doesn't depend on runtime interpreters (like PHP, Python, Node.js) and cannot be "injected with scripts"
- It has no `eval()`, `system()` or similar functions that can execute arbitrary strings
- All system command invocations go through `executor/commander.go`'s **whitelist mechanism**

How strict is the whitelist? Here are some representative commands (the complete whitelist contains 20+ system commands):

| Allowed Commands | Allowed Parameters |
|-----------------|-------------------|
| `systemctl` | start, stop, reload, restart, enable, disable... |
| `nginx` | -t, -s, -c |
| `mysql` | -u, -p, -e, -h, -P |
| `wget` | -q, -O, -T, -t (and URLs must be HTTPS + whitelist domains) |
| `curl` | -s, -o, -f, -L, -X, -H, -d (and URLs must be HTTPS + whitelist domains) |
| `unzip` | -o, -q, -d |
| `fail2ban-client` | set, unban, status, banip... |

**Dangerous characters are globally filtered**: `;`, `|`, `&`, `` ` ``, `$`, `<`, `>` and any characters that could compose shell injection are directly rejected.

```go
func hasUnsafeArgs(binary string, args []string) bool {
    for _, arg := range args {
        if strings.ContainsAny(arg, ";|&`$<>") {
            return true
        }
        // wget/curl URLs must be HTTPS and from whitelist domains
    }
}
```

### 3.3 File Management Has a "Cage"

The panel provides a file manager, but all file operations are restricted within **website root directories**:

```go
func isPathWithin(basePath, targetPath string) bool {
    // Resolve symlinks to prevent directory escapes via soft links
    base, _ := filepath.EvalSymlinks(filepath.Clean(basePath))
    target, _ := resolvePathForAccess(targetPath)
    
    // Calculate relative path, ensure target is within base
    rel, _ := filepath.Rel(base, target)
    return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
```

**Security points**:
- Even if you log in through the panel, you **cannot access** sensitive paths like `/etc/shadow`, `/root/.ssh/`, `/www/server/panel/config.json`
- Upload, download, delete, decompress and other operations all go through `isPathWithin()` checks; **unauthorized access directly returns 403**
- Symlinks are resolved to real paths by `EvalSymlinks`, **cannot be bypassed via soft links**

### 3.3.1 File Lock

File Lock is a "file write protection enhancement" enabled at the website level. It requires WordPress sites to have `site_type = wordpress`, with `file_lock_enabled` and `file_lock_enabled_at` written to the database `websites` table.

#### Write Scope During Lock (Runtime)

When lock is enabled, path writes must pass both the `isPathWithin()` root directory check and additional rules:

- Allowed writes: Runtime data under `/www/wwwroot/<site>/wp-content/...`
- Forbidden writes:
  - `wp-content/plugins`
  - `wp-content/themes`
  - `wp-content/mu-plugins`
  - `wp-content/upgrade`
  - `wp-content/upgrade-temp-backup`
- Forbidden config file modifications: `wp-config.php`, `.user.ini`, `.htaccess` (site root), and `php.ini`, `wordfence-waf.php`
- Default forbidden: creating/modifying PHP executable files (`.php`, `.phtml`, `.phar`)
- File lock settings reject sites where `wp-config.php` already has `DISALLOW_FILE_MODS = false`, avoiding conflicts with panel behavior

#### File Lock Direct API Interception

When locked, the following maintenance endpoints return `423 (Locked)`:

- `POST /websites/:id/db-password`
- `POST /websites/:id/fix-wp-config`
- `POST /websites/:id/install-plugin`
- `POST /websites/:id/wp-optimizations`
- `POST /websites/:id/reinstall`
- `POST /api/cache/helper/optimizer-settings`

Returned text matches frontend text:

> This site has file lock enabled. Please disable file lock before performing this maintenance operation.

#### File Manager Integration

In locked state, file manager write actions (upload, upload chunk completion, delete, rename, create directory, compress, decompress, copy, move, archive import, etc.) all go through `checkFileLockWrite`.

When the target write path doesn't satisfy rules, it returns:

> This site has file lock enabled. Only runtime data directories under wp-content are allowed for writes, and PHP executable files are forbidden.

This means: file lock is an **additional write behavior restriction** beyond "path isolation", not a replacement.

#### Code Layer Implementation

- When enabled, writes a managed block (`WP Panel File Lock`) to `wp-config.php`
  - `define('DISALLOW_FILE_MODS', true);`
  - `define('FS_METHOD', 'direct');`
- When disabled, removes the managed block and restores the normal permission model
- Enable/disable also triggers website permission refresh, verifying critical paths (e.g., `wp-config.php`, `wp-content/{plugins,themes,mu-plugins}`) have no suspicious symlinks

### 3.4 Download Sources Are Locked Down

All download-related areas in the panel (WordPress core, plugins, themes, update packages) have URLs restricted to **whitelisted domains**:

```go
func isAllowedDownloadURL(raw string) bool {
    u, err := url.Parse(raw)
    if err != nil || u.Scheme != "https" || u.User != nil {
        return false
    }
    switch strings.ToLower(u.Hostname()) {
    case "wordpress.org", "downloads.wordpress.org", "api.wordpress.org",
         "www.cloudflare.com", "developers.google.com", "www.bing.com":
        return true
    default:
        return false
    }
}
```

- Must use **HTTPS**
- No username/password embedded in URLs (`u.User != nil`)
- Domains not on the whitelist are **all rejected**

---

## 4. When Server Passwords Aren't Leaked, How Secure Is the Panel?

Assuming your SSH root password, keys, and cloud provider console haven't been leaked, attackers can only access your server over the network. WP Panel builds **six layers of defense-in-depth** for this scenario.

### 4.1 Covert Layer: Making Attackers Unable to Find the Door

#### Random Entry Path

During installation, the panel generates an **8-character random suffix** (e.g., `a3b9c2d1`), making the panel address:

```
https://your-ip:8443/a3b9c2d1/login
```

Without this suffix, access directly returns 404. For attackers trying to brute-force this path:

- The suffix is generated from SHA256 hash, each character has 16 possibilities (0-9, a-f)
- Total 8-character combinations = 16^8 ≈ **4.3 billion**
- Pure math: at 10,000 scans per second, about 5 days to exhaust — but scan defense bans IPs for 30 days on the first non-browser request, making actual attacks infeasible.

#### 8443 Non-Standard Port

The panel uses port 8443 instead of common 80/443/8080/8888, which itself filters out 99% of internet batch scanners.

#### Scan Defense: Non-Browser Immediately Banned for 30 Days

If someone attempts to scan port 8443 with a script, the panel's first defense is not login verification, but **`middleware/scan_defense.go`**:

```go
func ScanDefense(db *sql.DB, randomSuffix string) gin.HandlerFunc {
    return func(c *gin.Context) {
        path := c.Request.URL.Path
        // If path doesn't start with random suffix
        if !strings.HasPrefix(path, legitPrefix) {
            // Check if User-Agent is a browser
            if !isBrowserLike(c) {
                // Not a browser? Directly ban IP for 720 hours (30 days)
                banScanIP(db, c.ClientIP(), "High-risk scan: non-browser characteristic probe on panel port", 720)
                c.AbortWithStatus(http.StatusForbidden)
                return
            }
        }
        c.Next()
    }
}
```

**What does this mean?** If attackers use Nmap, Dirbuster, or Python scripts to scan your panel port, and the User-Agent lacks `Mozilla`, `Chrome`, `Safari` or other browser identifiers, **the IP is immediately written to the firewall blacklist and banned for 30 days**.

### 4.2 Authentication Layer: Two Password Doors

Even if attackers know your random entry path, they still need to break through two consecutive authentication layers:

**Layer 1 — BasicAuth (Browser Popup)**
- Username/password compared using bcrypt
- 5 consecutive failures → IP banned for 24 hours
- Failure records written to database, synced to system firewall by fail2ban

**Layer 2 — Web Login (Panel Form)**
- Independent second set of username/password
- Also uses bcrypt comparison
- Also has 5-failure ban mechanism
- Session only granted after passing

**Why two layers?** Even if BasicAuth's password leaks for some reason (e.g., browser saved the password and someone next to you saw it), attackers still need a second password to enter the panel. This isn't over-engineering — it's **defense-in-depth**.

### 4.3 Session Layer: Stealing the Cookie Is Useless

After successful login, the panel issues a Session Cookie:

```go
http.SetCookie(c.Writer, &http.Cookie{
    Name:     "wp_session",
    Value:    session.Token,     // UUID random string
    MaxAge:   1800,              // 30 minutes
    Path:     "/",
    HttpOnly: true,              // JavaScript cannot read
    Secure:   true,              // HTTPS only transmission
    SameSite: http.SameSiteLaxMode,
})
```

**Security features**:
- **HttpOnly**: Even if the website has XSS vulnerabilities, JavaScript cannot read this Cookie
- **Secure**: Only transmitted via HTTPS encryption, cannot be intercepted in plaintext by man-in-the-middle
- **Sliding renewal**: Validity automatically extended by 30 minutes on each valid page access
- **Server-side storage**: Sessions stored in panel memory, **not persisted to disk**; all sessions expire after panel restart, requiring re-login

Additionally, all **write operations** (modifying configuration, deleting files, creating websites, etc.) must also carry a **CSRF Token**:

```go
func CSRF() gin.HandlerFunc {
    return func(c *gin.Context) {
        // Read X-CSRF-Token from Header
        headerToken := c.GetHeader("X-CSRF-Token")
        // Read csrf_token from Cookie
        cookieToken, _ := c.Cookie("csrf_token")
        // Both must match
        if headerToken != cookieToken {
            c.AbortWithStatusJSON(http.StatusForbidden, ...)
            return
        }
        c.Next()
    }
}
```

This prevents **cross-site request forgery attacks** — attackers can trick you into clicking malicious links but cannot forge a correct CSRF Token.

### 4.4 Transport Layer: Encryption + Secure Response Headers

The panel enforces **HTTPS** (port 8443) and sends the following security response headers:

| Response Header | Purpose |
|----------------|---------|
| `Strict-Transport-Security: max-age=31536000; includeSubDomains` | Tells browser to use HTTPS only for the next year |
| `X-Frame-Options: DENY` | Prevents page from being embedded in iframes, preventing clickjacking |
| `X-Content-Type-Options: nosniff` | Prevents browser from guessing file types |
| `Referrer-Policy: no-referrer` | Does not leak source page address |

### 4.5 Operation Layer: Even Inside the Panel, Can't Do Harm

In an extreme scenario: attacker passes all authentication and enters the panel. What can they do?

**File Management**: Can only operate website files under `/www/wwwroot/` and the `/www/server/panel/backups` backup directory, **cannot access** system configuration files, other users' data, or the panel's own configuration.

**Command Execution**: The panel has no "terminal" function; all system operations are executed through wrapped APIs, with the underlying layer going through `executor/commander.go`'s whitelist. **There is no entry point for arbitrary command input**.

**Database**: The panel itself uses SQLite for configuration and logs; MariaDB management is executed through the command-line `mysql` client (with `MYSQL_PWD` environment variable), and the MariaDB root password only exists in `config.json` (permission 600). Attackers through the panel can only manage **databases corresponding to websites**, not directly obtain the MariaDB root password.

### 4.6 Network Layer: Firewall + Rate Limiting + Intrusion Detection

**Nginx Rate Limiting**:
- Unlogged WordPress users rate-limited to **60 requests/minute**
- Logged-in users are not rate-limited (to avoid disrupting normal users)

**fail2ban Integration**:
- SSH brute-force → auto-ban
- Panel login failure → auto-ban
- 404 scanning → auto-ban

**nftables Firewall**:
- The panel's own scan defense and manual bans are directly written to nftables, blocking connections at the system level

---

## 5. Is the Software Installed by the Panel Itself Secure? — Software-Level Security Protection

The previous sections proved the panel itself won't do harm. But another reasonable concern is: **the software the panel installs (Nginx, MariaDB, Redis, PHP-FPM, etc.) might have vulnerabilities — has the panel done anything about this?**

To answer this question, we first need to understand a core concept —

### 5.1 Plain Language: Why "Updating Software" Equals "Fixing Vulnerabilities"

Imagine your server is a house, and each running piece of software (Nginx, PHP, database, etc.) is a door or window. **Software vulnerabilities are gaps in windows that aren't fully closed** — hackers sneak into your house through these gaps.

The key point is:

> **Hackers know about vulnerabilities, and so do software vendors. Every "update" the vendor releases is fixing these discovered doors and windows.**

An analogy:

| Everyday Scenario | Server Scenario |
|-------------------|-----------------|
| A smart door lock from a certain brand is found to have a security defect | Nginx running on your server is found to have a remote code execution vulnerability (CVE) |
| Vendor releases new firmware fixing the defect | Debian/Nginx official releases new version fixing the vulnerability |
| You update door lock firmware → door is secure again | You run `apt upgrade nginx` → vulnerability is patched |
| You never update → thief knows this model has a defect, specifically targets your lock | You don't update → hackers scan the entire internet for this version of Nginx, easily compromise |

**The truth is: most hacked websites aren't because hackers are so skilled, but because site owners didn't update software promptly.** The 2023 Wordfence report showed that over 60% of compromised WordPress sites had known, unpatched vulnerabilities — in other words, updating on time would have prevented the hack.

Understanding this basic logic, you can clearly see what WP Panel does.

### 5.2 What Exactly Is the Panel's "System Update"?

When you open WP Panel's "System Update" page, the panel performs this check in the background:

```
Asking the system: "Are there new versions for all installed software (nginx, php, mariadb, redis...)?"
```

The system answers:
- No new versions → displays "System is up to date"
- New versions available → lists updatable software and version numbers, e.g.:
  - nginx 1.24.0 → 1.26.0 (fixed 2 security issues)
  - php8.3-fpm 8.3.6 → 8.3.8 (fixed 1 security issue)
  - mariadb-server 10.11.6 → 10.11.8

Technically, the panel's underlying call is Debian's `apt list --upgradable` command (source `handlers/system_update.go`), which queries each package's latest version from the Debian official software repository — exactly the same principle as your phone's App Store checking for app updates.

### 5.3 One-Click Update: No Command Line Knowledge Needed

The traditional approach: SSH into server → type `apt update` → type `apt upgrade -y` → watch the screen wait for results.

WP Panel turns these three steps into **one button** (source `handlers/system_update.go:56-80`). You just need to open the panel, click "System Update", and the panel automatically completes all operations. For site owners unfamiliar with the command line, **no Linux knowledge is needed to keep the server secure**.

### 5.4 Automatic Alerts: You Forget, the Panel Remembers

This is the most critical and easily overlooked capability. Humans forget; the panel doesn't.

WP Panel's alert system (`executor/alert_monitor.go`) automatically checks every 24 hours:

- **System software update alerts**: Are there new versions for nginx, php, mariadb, redis on the current server? If so, it notifies you.
- **Panel self-update alerts**: Does WP Panel itself have a new version? If so, it also notifies you.

If you've configured email notifications, you'll receive emails like:

> Warning: Your server has 12 available security updates:
> nginx, mariadb-server, php8.3-fpm, php8.3-cli, redis-server, openssl, libssl3...

**This means:** You don't need to proactively check CVE databases for "does my nginx version have vulnerabilities" — the Debian security team has already checked for you, put fixes into apt updates, and the panel tells you "updates available", you just click one button.

### 5.5 Real-World Example: If a Log4j-Level Vulnerability Hit Your Server Components

At the end of 2021, the Log4j vulnerability (Log4Shell) was exposed, affecting millions of servers worldwide. Affected companies had to find all impacted systems and update within **hours** — one day late could mean compromise.

If a similarly severe vulnerability hit your server components (like Nginx or PHP), WP Panel's process would be:

```
Vulnerability disclosed → Debian security team releases fix package → Panel alert system detects available update →
→ Sends email/Webhook notification → You open panel → Click "System Update" → Vulnerability fixed
```

Compare with no panel:
```
Vulnerability disclosed → You have no idea → Weeks later your site is hacked → You finally discover
```

**The difference lies in "knowing updates exist" and "how simple the update operation is."**

### 5.6 Process Guard (ProcessGuard): Auto-Restarts Crashed Software

Beyond vulnerabilities, software also has **runtime stability** issues. The panel's ProcessGuard (`executor/process_guard.go`) checks whether the following six critical services are alive every 30 seconds:

| Service | If It Crashes |
|---------|--------------|
| Nginx | Website inaccessible → ProcessGuard auto-restarts |
| PHP-FPM | Website white screen/errors → ProcessGuard auto-restarts |
| MariaDB | Database unavailable → ProcessGuard auto-restarts |
| Redis | Cache unavailable, website slows down → ProcessGuard auto-restarts |
| nftables | Firewall unavailable → ProcessGuard auto-restarts |
| Fail2ban | Brute-force protection unavailable → ProcessGuard auto-restarts |

For beginners, **you don't even need to know what these services are called** — the panel silently guards them in the background. You can see each service's status on the panel's "System Guard" page: green = normal, red = auto-restarted.

### 5.7 Software Versions Transparently Visible

The panel displays each installed software's exact version number on the "Software Management" page (`handlers/software.go`). If a serious vulnerability is ever discovered, you can immediately check whether your server is affected:

- CVE announcement says "Versions before Nginx 1.24.0 are affected" → Check panel: mine is 1.26.0 → Not affected, relax
- CVE announcement says "PHP 8.3.0-8.3.7 affected" → Check panel: mine is 8.3.1 → Affected → Update immediately with one click

No need to type commands to check versions, no need to remember software paths — see everything on one page.

### 5.8 Are These Software Sources Reliable?

All software installed by WP Panel comes from the **Debian official repository** or **Ondřej Surý PHP official source** (source `install.sh:549-568`), not packaged by the panel itself:

```
apt-get install -y nginx mariadb-server redis-server fail2ban nftables php8.3-fpm ...
```

- **Debian official repository** is maintained by the Debian security team, with each package having a GPG digital signature to ensure no tampering
- **Ondřej Surý PHP source** is the most authoritative third-party PHP source in the Debian ecosystem, also with GPG signature verification

The panel's role is not "providing software" but "helping you manage these official software packages and notifying you at the first sign of security updates."

### 5.9 One-Sentence Summary

| Your Concern | Actual Situation |
|-------------|-----------------|
| "What if the Nginx installed by the panel has vulnerabilities?" | Vulnerability exists → Debian official releases update → Panel auto-detects → Sends email/Webhook notification → You click one-click update → Vulnerability fixed. You don't need to be technical. |
| "I don't know when to update" | The panel helps you know. Auto-checks every 24 hours, notifies you when updates are available. |
| "I don't know how to update" | The panel helps you operate. One button, no command line needed. |
| "What if software crashes" | The panel helps you restart. Auto-recovers within 30 seconds, you might not even notice. |

WP Panel doesn't introduce new vulnerabilities to software — it installs software from official sources, with each package having signature verification. What the panel does is **let you know about vulnerabilities faster and fix them more simply than manual management**.

---

## 6. Common Attack Scenario Simulation

To better understand security intuitively, we simulate several common attack methods:

### Scenario 1: Brute-Forcing Panel Login

**Attacker's approach**: Dictionary attack on `https://IP:8443/random-suffix/login`

**Result**:
- Don't know random suffix → Access `/` directly gets 404, triggers scan defense, IP banned for 30 days
- Know random suffix but not BasicAuth password → 5 failures → IP banned for 24 hours
- Break through BasicAuth but don't know web password → Another 5 failures → continue banned for 24 hours
- At 1 attempt per second, cracking a 16-character random password would take **hundreds of millions of years**

**Conclusion**: Pure network brute-forcing is **infeasible**.

### Scenario 2: SQL Injection

**Attacker's approach**: Enter `' OR '1'='1` in login form

**Result**: Panel uses parameterized queries:

```go
db.QueryRow("SELECT password_hash FROM admin_users WHERE username = ?", req.Username)
```

User input is treated as **plain text parameters**, not parsed as SQL statements.

**Conclusion**: SQL injection is **infeasible**.

### Scenario 3: Path Traversal (Download `/etc/passwd`)

**Attacker's approach**: Access `../../../etc/passwd` through file management API

**Result**: `isPathWithin()` function calculates relative paths, finds target escapes website root directory, directly returns **403 path unauthorized**.

**Conclusion**: Path traversal is **infeasible**.

### Scenario 4: Command Injection (Write `; rm -rf /` in domain input)

**Attacker's approach**: Enter malicious domain when creating a website

**Result**: All system command operations go through `executor/commander.go` filtering:
- Commands must be on the whitelist
- Parameters cannot contain `;|&\`$<>`
- Patterns like `bash -c` that can execute arbitrary strings **simply don't exist**

**Conclusion**: Command injection is **infeasible**.

### Scenario 5: XSS (Cross-Site Scripting)

**Attacker's approach**: Change panel title to `<script>alert(1)</script>` in "Panel Settings"

**Result**:
- Backend templates use Go's `html/template`, **auto-escaping** HTML special characters; `<script>` is rendered as plain text, not executable script
- Frontend data transmitted via API JSON, browser renders as text
- Even if frontend is bypassed, Cookie is HttpOnly, JS cannot read Session

**Conclusion**: XSS cannot steal login credentials.

---

## 7. Does the Panel Really Not "Secretly Contact External Services"?

### 7.1 Telemetry Reporting: Transparent Content, Disablable

The panel sends a "heartbeat" to `stats.wp-panel.org` every 24 hours, but the content is only:

```json
{
  "anonymous_id": "a1b2c3d4e5f67890",  // First 16 bytes of SHA256 of /etc/machine-id
  "version": "1.0.0"                     // Panel version number
}
```

**Does not include**: IP addresses, domain names, website counts, passwords, any business data.

**Disablable**: Turn off "anonymous statistics" in the panel's security settings, or set `telemetry_enabled` to `false` in the database.

### 7.2 Alert Notifications: Only Sent to You

The panel's alerts (CPU high, SSL expiry, backup failure, etc.) are only sent to your **proactively configured** email or Webhook. If you haven't configured SMTP or Webhook, alerts are only recorded in the local database, **not sent externally**.

### 7.3 Update Check: Only Visits GitHub

When the panel checks for updates, it only visits `api.github.com` to get Release information. It **does not download** any update files unless you **manually click "Update Now"** in the panel.

---

## 8. What You Can Verify

If you're still concerned, you can audit through the following methods:

### 8.1 Check Panel Network Connections

```bash
# View what network connections wp-panel process has established
ss -tpn | grep wp-panel

# Or view real-time network activity
lsof -i -a -c wp-panel
```

Normal should only show:
- Local port 8443 HTTPS listening
- Occasional connections to `api.github.com` (checking updates)
- If telemetry is enabled, once daily to `stats.wp-panel.org`

**Will not show**: Connections to unfamiliar IPs, uploading large amounts of data, persistent abnormal connections.

### 8.2 Check System Scheduled Tasks

```bash
# View cron tasks created by the panel
cat /etc/cron.d/wp_panel_cron

# View system-level scheduled tasks
crontab -l
ls /etc/cron.d/
```

The panel only writes cron files when you **proactively create scheduled tasks**, with content completely transparent and readable.

### 8.3 Verify Binary File Integrity

```bash
# Calculate current panel's SHA256
sha256sum /usr/local/bin/wp-panel

# Compare with checksum from GitHub Release
# https://github.com/naibabiji/wp-panel/releases
```

### 8.4 View Panel Operation Logs

The panel's **background task queue** (backups, SSL renewal, firewall bans, scheduled task execution, etc.) is recorded in the `operation_logs` table, viewable in "Panel Settings → Operation Logs". Additionally, login attempts, system alerts, etc. also have independent record tables.

### 8.5 Disable Telemetry

```bash
# Enter SQLite database
sqlite3 /www/server/panel/panel.db

# Disable telemetry
UPDATE security_settings SET svalue = 'false' WHERE skey = 'telemetry_enabled';
.quit

# Restart panel
systemctl restart wp-panel
```

---

## 9. Summary

| Concern | Fact |
|---------|------|
| Will the panel secretly change passwords? | ❌ **No**. Password changes only through panel settings page (requires known password) or server CLI (requires root). No automatic password change mechanism. |
| Will auto-update plant trojans? | ❌ **Impossible**. Updates require triple verification: SHA256 + Ed25519 signature + preflight; source must be GitHub Releases. |
| Is the panel secure when server isn't leaked? | ✅ **Very secure**. Six-layer defense-in-depth: covert layer + dual authentication + Session/CSRF + HTTPS + operation isolation + firewall. |
| Will the panel secretly transmit data? | ❌ **No**. Telemetry contains only anonymous ID + version number, disablable; no other hidden network behavior. |

WP Panel's security design follows one core principle: **Defense in Depth**. No single defense line, but layered accumulation — attackers need to break through random path, browser detection, BasicAuth, web login, Session, CSRF, path isolation, command whitelist, firewall in sequence... Each layer is extremely difficult to bypass, and together they form an extremely high security threshold.

More importantly, all code is **open source and auditable**. Any claim that "the panel is insecure" should be specific to a certain line of code, a certain function, a certain network connection. Vague claims of "feeling insecure" cannot stand against verifiable source code.

---

*This article is based on the Go source code from the WP Panel open-source repository. All referenced code snippets and file paths are publicly verifiable.*
