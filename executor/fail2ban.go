package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/naibabiji/wp-panel/database"
)

var syncMu sync.Mutex
var recordPersistBan = AddPersistBan
var manualAddNginxBan = AddNginxBan
var manualRemoveNginxBan = RemoveNginxBan
var syncReplaceNginxBannedIPs = ReplaceNginxBannedIPs

type fail2banConfigBackup struct {
	path    string
	data    []byte
	existed bool
}

func deployFail2ban(webWhitelistIPs, sshWhitelistIPs string, maxRetry, findTime, banTime int) error {
	jailDir := "/etc/fail2ban/jail.d"
	filterDir := "/etc/fail2ban/filter.d"
	actionDir := "/etc/fail2ban/action.d"
	os.MkdirAll(jailDir, 0755)
	os.MkdirAll(filterDir, 0755)
	os.MkdirAll(actionDir, 0755)

	ensureLogFiles()
	_ = EnsureNginxBannedIPsConfig()
	jailPath := filepath.Join(jailDir, "wppanel.conf")
	actionPath := filepath.Join(actionDir, "wppanel-nginx.conf")
	filterPath := filepath.Join(filterDir, "wppanel.conf")
	filter404Path := filepath.Join(filterDir, "wppanel-404.conf")
	backups, err := backupFail2banConfigFiles(jailPath, actionPath, filterPath, filter404Path)
	if err != nil {
		return err
	}
	rollbackDeploy := func(cause error) error {
		if restoreErr := restoreFail2banConfigFiles(backups); restoreErr != nil {
			return fmt.Errorf("%w; rollback fail2ban config files failed: %v", cause, restoreErr)
		}
		if reloadErr := reloadOrStartFail2ban(); reloadErr != nil {
			return fmt.Errorf("%w; fail2ban config files were rolled back, but reload failed: %v", cause, reloadErr)
		}
		return cause
	}

	webIgnoreIPs, err := buildFail2banIgnoreIPs(webWhitelistIPs)
	if err != nil {
		return err
	}
	sshIgnoreIPs, err := buildFail2banIgnoreIPs(sshWhitelistIPs)
	if err != nil {
		return err
	}

	if maxRetry <= 0 {
		maxRetry = 5
	}
	if findTime <= 0 {
		findTime = 60
	}
	if banTime <= 0 {
		banTime = 600
	}

	jailConfig := fmt.Sprintf(`# WP Panel Generated — DO NOT EDIT MANUALLY
[wppanel]
enabled = true
filter = wppanel
action = nftables-multiport[name=wppanel, port="http,https"]
         wppanel-nginx[name=wppanel]
logpath = /www/wwwlogs/*/access.log
          /www/wwwlogs/*/error.log
maxretry = %d
findtime = %d
bantime = %d
ignoreip = %s

[wppanel-404]
enabled = true
filter = wppanel-404
action = nftables-multiport[name=wppanel-404, port="http,https"]
         wppanel-nginx[name=wppanel-404]
logpath = /www/wwwlogs/*/access.log
maxretry = 30
findtime = 60
bantime = %d
ignoreip = %s

[wppanel-sshd]
enabled = true
filter = sshd
action = nftables-multiport[name=wppanel-sshd, port="ssh"]
logpath = /var/log/auth.log
maxretry = %d
findtime = %d
bantime = %d
ignoreip = %s
`, maxRetry, findTime, banTime, webIgnoreIPs, banTime, webIgnoreIPs, maxRetry, findTime, banTime, sshIgnoreIPs)

	if err := os.WriteFile(jailPath, []byte(jailConfig), 0644); err != nil {
		return rollbackDeploy(fmt.Errorf("Failed to write jail config: %w", err))
	}

	actionConfig := `# WP Panel Generated - DO NOT EDIT MANUALLY
[Definition]
actionban = /usr/local/bin/wp-panel --banip-nginx <ip> --record-fail2ban <ip> --ban-jail <name>
actionunban = /usr/local/bin/wp-panel --unbanip-nginx <ip>
`

	if err := os.WriteFile(actionPath, []byte(actionConfig), 0644); err != nil {
		return rollbackDeploy(fmt.Errorf("Failed to write nginx action config: %w", err))
	}

	filterConfig := `# WP Panel Generated — DO NOT EDIT MANUALLY
[Definition]
failregex = ^<HOST> .* "POST /wp-login\.php .*" .*$
            ^<HOST> .* "POST /xmlrpc\.php .*" 403 .*$
            ^<HOST> .* "POST //xmlrpc\.php .*" 403 .*$
            ^<HOST> .* ".*" 429 .*$
            ^<HOST> - - \[.*\] "(GET|POST) .*(\.env|\.git|config\.bak|wp-config\.php|\.sql|\.tar|\.gz|\.zip|\.old|\.swp|\.save|\.DS_Store).*" 404 .*$
ignoreregex =
`

	if err := os.WriteFile(filterPath, []byte(filterConfig), 0644); err != nil {
		return rollbackDeploy(fmt.Errorf("Failed to write filter config: %w", err))
	}

	filter404Config := `# WP Panel Generated — DO NOT EDIT MANUALLY
[Definition]
failregex = ^<HOST> - - \[.*\] ".*" 404 .*$
ignoreregex =
`

	if err := os.WriteFile(filter404Path, []byte(filter404Config), 0644); err != nil {
		return rollbackDeploy(fmt.Errorf("Failed to write 404 filter config: %w", err))
	}

	if err := reloadOrStartFail2ban(); err != nil {
		return rollbackDeploy(fmt.Errorf("Failed to reload fail2ban: %w", err))
	}
	return nil
}

func backupFail2banConfigFiles(paths ...string) ([]fail2banConfigBackup, error) {
	backups := make([]fail2banConfigBackup, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				backups = append(backups, fail2banConfigBackup{path: path})
				continue
			}
			return nil, fmt.Errorf("Failed to read Fail2ban config backup: %w", err)
		}
		backups = append(backups, fail2banConfigBackup{path: path, data: data, existed: true})
	}
	return backups, nil
}

func restoreFail2banConfigFiles(backups []fail2banConfigBackup) error {
	var errs []error
	for _, backup := range backups {
		if backup.existed {
			if err := os.WriteFile(backup.path, backup.data, 0644); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", backup.path, err))
			}
			continue
		}
		if err := os.Remove(backup.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("%s: %w", backup.path, err))
		}
	}
	return errors.Join(errs...)
}

func reloadOrStartFail2ban() error {
	if _, err := executeCommand("fail2ban-client", "reload"); err != nil {
		if _, activeErr := executeCommand("systemctl", "is-active", "--quiet", "fail2ban"); activeErr == nil {
			return err
		}
		if _, startErr := executeCommand("systemctl", "start", "fail2ban"); startErr != nil {
			return startErr
		}
	}
	return nil
}

func buildFail2banIgnoreIPs(whitelistIPs string) (string, error) {
	ignoreIPs := "127.0.0.1/8"
	if whitelistIPs == "" {
		return ignoreIPs, nil
	}
	for _, ip := range strings.Split(whitelistIPs, "\n") {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if strings.ContainsAny(ip, " \t\r") {
			return "", fmt.Errorf("Whitelist IP format invalid: %s", ip)
		}
		if strings.Contains(ip, "/") {
			if _, _, err := net.ParseCIDR(ip); err != nil {
				return "", fmt.Errorf("Whitelist IP format invalid: %s", ip)
			}
		} else if net.ParseIP(ip) == nil {
			return "", fmt.Errorf("Whitelist IP format invalid: %s", ip)
		}
		ignoreIPs += " " + ip
	}
	return ignoreIPs, nil
}

func ensureLogFiles() {
	hasLogs := false
	entries, err := os.ReadDir("/www/wwwlogs")
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			touch("/www/wwwlogs/" + e.Name() + "/access.log")
			touch("/www/wwwlogs/" + e.Name() + "/error.log")
			touch("/www/wwwlogs/" + e.Name() + "/wp-security.log")
			touch("/www/wwwlogs/" + e.Name() + "/php-error.log")
			touch("/www/wwwlogs/" + e.Name() + "/php-slow.log")
			hasLogs = true
		}
	}
	if !hasLogs {
		os.MkdirAll("/www/wwwlogs/_panel_placeholder", 0755)
		touch("/www/wwwlogs/_panel_placeholder/access.log")
		touch("/www/wwwlogs/_panel_placeholder/error.log")
		touch("/www/wwwlogs/_panel_placeholder/wp-security.log")
		touch("/www/wwwlogs/_panel_placeholder/php-error.log")
		touch("/www/wwwlogs/_panel_placeholder/php-slow.log")
	}
	touch("/var/log/auth.log")
}

func ensureSiteLogFiles(logDir string) {
	if strings.TrimSpace(logDir) == "" {
		return
	}
	touch(filepath.Join(logDir, "access.log"))
	touch(filepath.Join(logDir, "error.log"))
	touch(filepath.Join(logDir, "wp-security.log"))
	touch(filepath.Join(logDir, "php-error.log"))
	touch(filepath.Join(logDir, "php-slow.log"))
}

func touch(path string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		f.Close()
	}
}

func ReloadFail2ban() error {
	if _, err := executeCommand("fail2ban-client", "reload"); err != nil {
		if _, activeErr := executeCommand("systemctl", "is-active", "--quiet", "fail2ban"); activeErr == nil {
			return fmt.Errorf("reload fail2ban failed: %w", err)
		}
	}
	return nil
}

func executeRefreshWhitelist(task *Task) TaskResult {
	var allIPs []string
	var details []string

	if cfIPs, err := fetchCloudflareIPs(); err == nil {
		allIPs = append(allIPs, cfIPs...)
		details = append(details, fmt.Sprintf("Cloudflare: %d entries", len(cfIPs)))
		cacheCloudflareRealIPRanges(cfIPs)
		if err := DeployCloudflareRealIPConfig(cfIPs); err != nil {
			details = append(details, "Cloudflare Real IP: configuration failed")
		} else {
			details = append(details, "Cloudflare Real IP: updated")
		}
	} else {
		details = append(details, "Cloudflare: fetch failed")
	}
	if googleIPs, err := fetchGooglebotIPs(); err == nil {
		allIPs = append(allIPs, googleIPs...)
		details = append(details, fmt.Sprintf("Googlebot: %d entries", len(googleIPs)))
		cacheSearchBotIPRanges("googlebot_ips", googleIPs)
	} else {
		details = append(details, "Googlebot: fetch failed")
	}
	if bingIPs, err := fetchBingbotIPs(); err == nil {
		allIPs = append(allIPs, bingIPs...)
		details = append(details, fmt.Sprintf("Bingbot: %d entries", len(bingIPs)))
		cacheSearchBotIPRanges("bingbot_ips", bingIPs)
	} else {
		details = append(details, "Bingbot: fetch failed")
	}

	db := database.GetDB()
	db.Exec(`UPDATE security_settings SET svalue = ?, updated_at = CURRENT_TIMESTAMP WHERE skey = 'official_whitelist_ips'`,
		strings.Join(allIPs, "\n"))
	db.Exec(`UPDATE security_settings SET svalue = datetime('now'), updated_at = CURRENT_TIMESTAMP WHERE skey = 'last_whitelist_update'`)

	if err := ApplyFail2banSettings(); err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}

	return TaskResult{
		Success: true,
		Message: fmt.Sprintf("Fetched %d entries (%s)", len(allIPs), strings.Join(details, "；")),
	}
}

func cacheSearchBotIPRanges(key string, ips []string) {
	if database.GetDB() == nil {
		return
	}
	if key != "googlebot_ips" && key != "bingbot_ips" {
		return
	}
	database.GetDB().Exec(`INSERT INTO security_settings (skey, svalue, description, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(skey) DO UPDATE SET svalue = excluded.svalue, updated_at = excluded.updated_at`,
		key, strings.Join(ips, "\n"), key+"official IP range cache")
}

func ApplyFail2banSettings() error {
	db := database.GetDB()

	var officialIPs, customIPs, cdnRealIPIPs string
	var maxRetry, findTime, banTime string
	db.QueryRow(`SELECT svalue FROM security_settings WHERE skey = 'official_whitelist_ips'`).Scan(&officialIPs)
	db.QueryRow(`SELECT svalue FROM security_settings WHERE skey = 'whitelist_ips'`).Scan(&customIPs)
	cdnRealIPIPs = CombinedCDNRealIPRangesForFail2ban()
	db.QueryRow(`SELECT svalue FROM security_settings WHERE skey = 'fail2ban_maxretry'`).Scan(&maxRetry)
	db.QueryRow(`SELECT svalue FROM security_settings WHERE skey = 'fail2ban_findtime'`).Scan(&findTime)
	db.QueryRow(`SELECT svalue FROM security_settings WHERE skey = 'fail2ban_bantime'`).Scan(&banTime)

	baseIPs := strings.TrimSpace(officialIPs)
	if customIPs != "" {
		if baseIPs != "" {
			baseIPs += "\n"
		}
		baseIPs += customIPs
	}
	webIPs := baseIPs
	if cdnRealIPIPs != "" {
		if webIPs != "" {
			webIPs += "\n"
		}
		webIPs += cdnRealIPIPs
	}

	mr := parseIntOr(maxRetry, 5)
	ft := parseIntOr(findTime, 60)
	bt := parseIntOr(banTime, 600)

	if err := deployFail2ban(webIPs, baseIPs, mr, ft, bt); err != nil {
		return err
	}

	var autoEnabled string
	db.QueryRow(`SELECT svalue FROM security_settings WHERE skey = 'auto_whitelist_enabled'`).Scan(&autoEnabled)
	if autoEnabled == "false" {
		executeCommand("systemctl", "stop", "wppanel-whitelist.timer")
		executeCommand("systemctl", "disable", "wppanel-whitelist.timer")
	} else {
		DeployWhitelistTimer()
	}
	return nil
}

func SyncFail2banBans() {
	syncMu.Lock()
	defer syncMu.Unlock()

	ipJails := make(map[string]string)
	webBannedSet := make(map[string]bool)
	webJailStatusRead := false

	for _, jail := range []string{"wppanel", "wppanel-404", "wppanel-sshd"} {
		out, err := executeCommand("fail2ban-client", "status", jail)
		if err != nil || out == "" {
			continue
		}
		isWebJail := jail == "wppanel" || jail == "wppanel-404"
		if isWebJail {
			webJailStatusRead = true
		}
		for _, ip := range parseBannedIPs(out) {
			if _, exists := ipJails[ip]; !exists {
				ipJails[ip] = jail
			}
			if isWebJail {
				webBannedSet[ip] = true
			}
		}
	}

	bannedSet := make(map[string]bool, len(ipJails))
	for ip := range ipJails {
		bannedSet[ip] = true
	}

	nftablesSet := getNftablesPersistIPs()

	db := database.GetDB()
	now := time.Now()

	for ip, jail := range ipJails {
		var count int
		db.QueryRow("SELECT COUNT(*) FROM firewall_bans WHERE ip_address = ? AND unbanned_at IS NULL", ip).Scan(&count)
		if count > 0 {
			continue
		}

		prevBans, prevMaxLevel := countBanHistory(ip, now)
		banLevel := 2
		expiresVal := "datetime('now', '+600 seconds')"
		reason := "Fail2ban auto-ban"
		if jail == "wppanel-404" {
			reason = "404 flood detection"
		}
		if jail == "wppanel-sshd" {
			reason = "SSH brute force"
		}

		if prevMaxLevel >= 2 || prevBans > 0 {
			banLevel = 3
			expiresVal = "datetime('now', '+86400 seconds')"
			reason = "Fail2ban auto-ban (repeat violation within 24h, escalated to 24 hours)"
			if jail == "wppanel-404" {
				reason = "404 flood detection (repeat violation within 24h, escalated to 24 hours)"
			}
			if jail == "wppanel-sshd" {
				reason = "SSH brute force (repeat violation within 24h, escalated to 24 hours)"
			}

			l3Count := countLevel3(ip)
			if l3Count >= 2 {
				banLevel = 5
				expiresVal = "NULL"
				reason = "Fail2ban auto-ban (high risk: 3 serious violations, permanent ban)"
				if jail == "wppanel-404" {
					reason = "404 flood detection (high risk: 3 serious violations, permanent ban)"
				}
				if jail == "wppanel-sshd" {
					reason = "SSH brute force (high risk: 3 serious violations, permanent ban)"
				}
			}
		}

		db.Exec(
			`INSERT INTO firewall_bans (ip_address, ban_level, reason, source_jail, ban_count, expires_at)
			 VALUES (?, ?, ?, ?, ?, `+expiresVal+`)`,
			ip, banLevel, reason, jail, prevBans+1,
		)

		if banLevel >= 3 {
			AddPersistBan(ip)
		}
	}

	rows, err := db.Query("SELECT id, ip_address, ban_level, expires_at, is_manual, source_jail FROM firewall_bans WHERE unbanned_at IS NULL")
	if err != nil {
		return
	}
	defer rows.Close()

	var expiredIDs []int
	for rows.Next() {
		var id, level, isManual int
		var ip, jail string
		var expiresAt *time.Time
		if rows.Scan(&id, &ip, &level, &expiresAt, &isManual, &jail) != nil {
			continue
		}
		if bannedSet[ip] {
			if isWebBanSource(jail) {
				webBannedSet[ip] = true
			}
			continue
		}
		if isManual == 1 {
			if expiresAt != nil && !expiresAt.After(now) {
				RemovePersistBan(ip)
				expiredIDs = append(expiredIDs, id)
				continue
			}
			if level >= 3 {
				AddPersistBan(ip)
			}
			if isWebBanSource(jail) {
				webBannedSet[ip] = true
			}
			continue
		}
		if level >= 3 {
			if expiresAt == nil || expiresAt.After(now) {
				if nftablesSet != nil && nftablesSet[ip] {
					if isWebBanSource(jail) {
						webBannedSet[ip] = true
					}
					continue
				}
				if nftablesSet != nil && !nftablesSet[ip] {
					AddPersistBan(ip)
					if isWebBanSource(jail) {
						webBannedSet[ip] = true
					}
					continue
				}
				AddPersistBan(ip)
				if isWebBanSource(jail) {
					webBannedSet[ip] = true
				}
				continue
			}
			RemovePersistBan(ip)
		}
		expiredIDs = append(expiredIDs, id)
	}

	for _, id := range expiredIDs {
		db.Exec("UPDATE firewall_bans SET unbanned_at = datetime('now') WHERE id = ?", id)
	}
	if webJailStatusRead || len(webBannedSet) > 0 {
		_ = syncReplaceNginxBannedIPs(webBannedSet)
	}
}

func RecordFail2banBan(ip, jail string) error {
	db := database.GetDB()
	if db == nil {
		return fmt.Errorf("database not initialized")
	}

	ip = strings.TrimSpace(ip)
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("invalid IP: %s", ip)
	}

	jail = normalizeFail2banJail(jail)
	if jail == "" {
		jail = detectFail2banJail(ip)
		if jail == "" {
			jail = "wppanel"
		}
	}

	now := time.Now()
	prevBans, prevMaxLevel := countBanHistory(ip, now)

	banLevel := 2
	expiresVal := "datetime('now', '+600 seconds')"
	reason := "Fail2ban auto-ban"
	if jail == "wppanel-404" {
		reason = "404 flood detection"
	}

	if prevMaxLevel >= 2 || prevBans > 0 {
		banLevel = 3
		expiresVal = "datetime('now', '+86400 seconds')"
		reason = "Fail2ban auto-ban (repeat violation within 24h, escalated to 24 hours)"
		if jail == "wppanel-404" {
			reason = "404 flood detection (repeat violation within 24h, escalated to 24 hours)"
		}

		l3Count := countLevel3(ip)
		if l3Count >= 2 {
			banLevel = 5
			expiresVal = "NULL"
			reason = "Fail2ban auto-ban (high risk: 3 serious violations, permanent ban)"
			if jail == "wppanel-404" {
				reason = "404 flood detection (high risk: 3 serious violations, permanent ban)"
			}
		}
	}

	if _, err := db.Exec(
		`INSERT INTO firewall_bans (ip_address, ban_level, reason, source_jail, ban_count, expires_at)
		 VALUES (?, ?, ?, ?, ?, `+expiresVal+`)`,
		ip, banLevel, reason, jail, prevBans+1,
	); err != nil {
		return err
	}

	if banLevel >= 3 {
		recordPersistBan(ip)
	}
	return nil
}

func normalizeFail2banJail(jail string) string {
	switch strings.TrimSpace(jail) {
	case "wppanel", "wppanel-404", "wppanel-sshd":
		return strings.TrimSpace(jail)
	default:
		return ""
	}
}

func isWebBanSource(jail string) bool {
	return jail == "wppanel" || jail == "wppanel-404" || jail == "manual"
}

func detectFail2banJail(ip string) string {
	for _, jail := range []string{"wppanel", "wppanel-404"} {
		out, err := executeCommand("fail2ban-client", "status", jail)
		if err != nil {
			continue
		}
		for _, bannedIP := range parseBannedIPs(out) {
			if bannedIP == ip {
				return jail
			}
		}
	}
	return ""
}

func countBanHistory(ip string, since time.Time) (count int, maxLevel int) {
	db := database.GetDB()
	cutoff := since.Add(-24 * time.Hour).Format("2006-01-02 15:04:05")
	db.QueryRow(
		"SELECT COUNT(*), COALESCE(MAX(ban_level), 0) FROM firewall_bans WHERE ip_address = ? AND banned_at > ?",
		ip, cutoff,
	).Scan(&count, &maxLevel)
	return
}

func countLevel3(ip string) int {
	db := database.GetDB()
	cutoff := time.Now().Add(-30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	var c int
	db.QueryRow("SELECT COUNT(*) FROM firewall_bans WHERE ip_address = ? AND ban_level >= 3 AND banned_at > ?", ip, cutoff).Scan(&c)
	return c
}

func EnsurePersistNftables() {
	exec.Command("bash", "-c",
		`nft add table ip wppanel_persist 2>/dev/null
nft add chain ip wppanel_persist input { type filter hook input priority -1\; } 2>/dev/null
nft add set ip wppanel_persist banned_ips { type ipv4_addr\; } 2>/dev/null
nft list chain ip wppanel_persist input 2>/dev/null | grep -q "saddr @banned_ips drop" || nft add rule ip wppanel_persist input ip saddr @banned_ips drop
nft add set ip wppanel_persist ssh_limit { type ipv4_addr\; flags dynamic,timeout\; timeout 1m\; size 65535\; } 2>/dev/null
nft list chain ip wppanel_persist input 2>/dev/null | grep -q "tcp dport 22 ct state new" || nft add rule ip wppanel_persist input tcp dport 22 ct state new add @ssh_limit { ip saddr limit rate over 3/minute } drop`).Run()
}

func AddPersistBan(ip string) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return
	}
	if parsed := net.ParseIP(ip); parsed == nil {
		return
	}
	EnsurePersistNftables()
	exec.Command("nft", "add", "element", "ip", "wppanel_persist", "banned_ips", "{", ip, "}").Run()
}

func RemovePersistBan(ip string) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return
	}
	if parsed := net.ParseIP(ip); parsed == nil {
		return
	}
	exec.Command("nft", "delete", "element", "ip", "wppanel_persist", "banned_ips", "{", ip, "}").Run()
}

func getNftablesPersistIPs() map[string]bool {
	outBytes, err := exec.Command("bash", "-c",
		`nft list set ip wppanel_persist banned_ips 2>/dev/null | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}'`).CombinedOutput()
	out := strings.TrimSpace(string(outBytes))
	if err != nil || out == "" {
		return nil
	}
	ips := make(map[string]bool)
	for _, ip := range strings.Fields(out) {
		ips[strings.TrimSpace(ip)] = true
	}
	return ips
}

func parseBannedIPs(status string) []string {
	var ips []string
	for _, line := range strings.Split(status, "\n") {
		idx := strings.Index(line, "Banned IP list:")
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(line[idx+len("Banned IP list:"):])
		for _, ip := range strings.Fields(rest) {
			ip = strings.TrimSpace(ip)
			if ip != "" {
				ips = append(ips, ip)
			}
		}
	}
	return ips
}

func fetchCloudflareIPs() ([]string, error) {
	var ips []string
	for _, url := range []string{
		"https://www.cloudflare.com/ips-v4/",
		"https://www.cloudflare.com/ips-v6/",
	} {
		out, err := executeCommand("curl", "-s", "-f", "-L", url)
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					ips = append(ips, line)
				}
			}
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("Cannot fetch Cloudflare IP ranges")
	}
	return ips, nil
}

func fetchGooglebotIPs() ([]string, error) {
	out, err := executeCommand("curl", "-s", "-f", "-L", "https://developers.google.com/search/apis/ipranges/googlebot.json")
	if err != nil {
		return nil, err
	}
	var data struct {
		Prefixes []struct {
			IPv4Prefix string `json:"ipv4Prefix"`
			IPv6Prefix string `json:"ipv6Prefix"`
		} `json:"prefixes"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return nil, err
	}
	var ips []string
	for _, p := range data.Prefixes {
		if p.IPv4Prefix != "" {
			ips = append(ips, p.IPv4Prefix)
		}
		if p.IPv6Prefix != "" {
			ips = append(ips, p.IPv6Prefix)
		}
	}
	return ips, nil
}

func fetchBingbotIPs() ([]string, error) {
	out, err := executeCommand("curl", "-s", "-f", "-L", "https://www.bing.com/toolbox/bingbot.json")
	if err != nil {
		return nil, err
	}
	var data struct {
		Prefixes []struct {
			IPv4Prefix string `json:"ipv4Prefix"`
			IPv6Prefix string `json:"ipv6Prefix"`
		} `json:"prefixes"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return nil, err
	}
	var ips []string
	for _, p := range data.Prefixes {
		if p.IPv4Prefix != "" {
			ips = append(ips, p.IPv4Prefix)
		}
		if p.IPv6Prefix != "" {
			ips = append(ips, p.IPv6Prefix)
		}
	}
	return ips, nil
}

func executeManualBan(task *Task) TaskResult {
	payload, ok := task.Payload.(*ManualBanPayload)
	if !ok {
		return TaskResult{Success: false, Message: "Invalid task parameter type"}
	}

	ip := strings.TrimSpace(payload.IP)
	if ip == "" {
		return TaskResult{Success: false, Message: "IP address cannot be empty"}
	}

	if net.ParseIP(ip) == nil {
		return TaskResult{Success: false, Message: "IP address format invalid"}
	}

	db := database.GetDB()
	if db == nil {
		return TaskResult{Success: false, Message: "database not initialized"}
	}

	jail := "manual"
	banLevel := 2
	duration := 600
	if payload.Duration == 3600 {
		duration = 3600
		banLevel = 3
	} else if payload.Duration == 86400 {
		duration = 86400
		banLevel = 3
	} else if payload.Duration == 0 {
		duration = -1
		banLevel = 5
	}

	var expires interface{}
	if duration < 0 {
		expires = nil
	} else {
		expires = time.Now().Add(time.Duration(duration) * time.Second)
	}

	if err := manualAddNginxBan(ip); err != nil {
		return TaskResult{Success: false, Message: "Ban failed: " + err.Error()}
	}

	if _, err := db.Exec(
		`INSERT INTO firewall_bans (ip_address, ban_level, reason, source_jail, is_manual, ban_count, expires_at)
		 VALUES (?, ?, 'Admin manual ban', ?, 1, 1, ?)`,
		ip, banLevel, jail, expires,
	); err != nil {
		_ = manualRemoveNginxBan(ip)
		return TaskResult{Success: false, Message: "Failed to write ban record"}
	}

	if banLevel >= 3 {
		AddPersistBan(ip)
	}

	msg := fmt.Sprintf("IP %s has been banned", ip)
	if payload.Duration == 0 {
		msg += " (permanent)"
	} else if payload.Duration >= 3600 {
		msg += fmt.Sprintf(" (%d hours)", payload.Duration/3600)
	} else {
		msg += fmt.Sprintf(" (%d minutes)", payload.Duration/60)
	}

	return TaskResult{Success: true, Message: msg}
}

func parseIntOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func RunWhitelistRefresh() string {
	return executeRefreshWhitelist(&Task{ID: "cli-refresh", Type: TaskRefreshWhitelist}).Message
}

func DeployWhitelistTimer() {
	timerUnit := `[Unit]
Description=WP Panel Weekly Whitelist Refresh
Requires=wppanel-whitelist.service

[Timer]
OnCalendar=Mon *-*-* 04:00:00
Persistent=true

[Install]
WantedBy=timers.target
`

	serviceUnit := `[Unit]
Description=WP Panel Whitelist Refresh

[Service]
Type=oneshot
ExecStart=/usr/local/bin/wp-panel --refresh-whitelist --config=/www/server/panel/config.json
`

	os.WriteFile("/etc/systemd/system/wppanel-whitelist.timer", []byte(timerUnit), 0644)
	os.WriteFile("/etc/systemd/system/wppanel-whitelist.service", []byte(serviceUnit), 0644)
	executeCommand("systemctl", "daemon-reload")
	executeCommand("systemctl", "enable", "wppanel-whitelist.timer")
	executeCommand("systemctl", "start", "wppanel-whitelist.timer")
}

func UnbanAllIPs() string {
	db := database.GetDB()

	unbanned, _ := db.Exec("UPDATE firewall_bans SET unbanned_at = datetime('now') WHERE unbanned_at IS NULL")
	unbanCount := int64(0)
	if unbanned != nil {
		unbanCount, _ = unbanned.RowsAffected()
	}

	exec.Command("bash", "-c", "nft flush set ip wppanel_persist banned_ips 2>/dev/null; true").Run()
	_ = ReplaceNginxBannedIPs(map[string]bool{})

	for _, jail := range []string{"wppanel", "wppanel-404", "wppanel-sshd"} {
		out, err := executeCommand("fail2ban-client", "status", jail)
		if err == nil && out != "" {
			for _, ip := range parseBannedIPs(out) {
				executeCommand("fail2ban-client", "set", jail, "unbanip", ip)
			}
		}
	}

	return fmt.Sprintf("Cleared all ban rules, unbanned %d records", unbanCount)
}

func CleanExpiredBans() {
	db := database.GetDB()

	rows, err := db.Query(`SELECT id, ip_address, source_jail FROM firewall_bans
		WHERE unbanned_at IS NULL AND expires_at IS NOT NULL AND expires_at <= datetime('now')`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int
		var ip, jail string
		if rows.Scan(&id, &ip, &jail) != nil {
			continue
		}

		db.Exec("UPDATE firewall_bans SET unbanned_at = datetime('now') WHERE id = ?", id)

		if jail == "panel_scan" || jail == "panel" || jail == "manual" {
			RemovePersistBan(ip)
		}
		if isWebBanSource(jail) {
			_ = RemoveNginxBan(ip)
		}
	}
}
