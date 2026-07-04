package executor

import (
	"crypto/tls"
	"fmt"
	"html"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/naibabiji/wp-panel/database"
)

type alertRule struct {
	key               string
	checkFn           func() (firing bool, msg string)
	thresholdDuration time.Duration
	pendingSince      time.Time
	lastFired         time.Time
	firing            bool
	lastAlertMsg      string
}

type alertManager struct {
	mu     sync.Mutex
	rules  []*alertRule
	stopCh chan struct{}
}

var (
	alertMgr            = &alertManager{stopCh: make(chan struct{})}
	panelCurrentVersion string
)

func StartAlertMonitor(currentVersion string) {
	panelCurrentVersion = currentVersion
	alertMgr.rules = []*alertRule{
		{key: "alert_cpu", checkFn: checkCPU, thresholdDuration: 5 * time.Minute},
		{key: "alert_memory", checkFn: checkMemory, thresholdDuration: 5 * time.Minute},
		{key: "alert_disk", checkFn: checkDisk},
		{key: "alert_service", checkFn: checkService},
		{key: "alert_ssl", checkFn: checkSSL},
		{key: "alert_backup", checkFn: checkBackup},
		{key: "alert_website_expiry", checkFn: checkWebsiteExpiry},
		{key: "alert_remote_backup", checkFn: checkRemoteBackup},
		{key: "alert_cron_fail", checkFn: checkCronFail},
		{key: "alert_site", checkFn: checkSites},
		{key: "alert_system_update", checkFn: checkSystemUpdate},
		{key: "alert_panel_update", checkFn: checkPanelUpdate},
	}
	go alertMgr.loop()
}

func (m *alertManager) loop() {
	// Initial check without sending (warm up)
	time.Sleep(30 * time.Second)
	m.runChecks()

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.runChecks()
		case <-m.stopCh:
			return
		}
	}
}

func (m *alertManager) runChecks() {
	// Site monitoring serial curl calls may take a long time (multiple sites + timeouts),
	// execute before the global lock to avoid blocking CPU/memory/disk and other alert rules.
	sitePreChecked := false
	var siteFiring bool
	var siteMsg string
	if isRuleEnabled("alert_site") {
		siteFiring, siteMsg = checkSites()
		sitePreChecked = true
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	cfg := GetSMTPConfig()
	hasSMTP := cfg != nil && cfg.Host != "" && cfg.AdminEmail != ""

	wCfg := GetWebhookConfig()
	hasWebhook := wCfg != nil && wCfg.Enabled == "true" && wCfg.URL != ""

	for _, r := range m.rules {
		if !isRuleEnabled(r.key) {
			r.firing = false
			r.pendingSince = time.Time{}
			continue
		}

		var instantFiring bool
		var msg string
		if r.key == "alert_site" && sitePreChecked {
			instantFiring, msg = siteFiring, siteMsg
		} else {
			instantFiring, msg = r.checkFn()
		}
		now := time.Now()
		firing := r.sustainedFiring(instantFiring, now)
		if firing && !r.firing {
			// Transition: normal → alert
			r.firing = true
			r.lastFired = now
			r.lastAlertMsg = msg
			logAlert(r.key, "critical", msg)
			if hasSMTP {
				go SendMail("", getPanelTitle()+" Alert — "+alertLabel(r.key), formatEmailHTML(alertLabel(r.key), msg, getEmailTip(r.key, false), true))
			}
			if hasWebhook {
				go SendWebhook(getPanelTitle()+" Alert — "+alertLabel(r.key), msg)
			}
		} else if !firing && r.firing {
			// Transition: alert → normal
			r.firing = false
			recoveryDetail := buildRecoveryDetail(r)
			logAlert(r.key, "info", recoveryDetail)
			database.GetDB().Exec("UPDATE alert_log SET resolved = 1 WHERE alert_type = ? AND resolved = 0", r.key)
			// Instant alerts (no threshold) send recovery notification directly; threshold alerts wait 5 minutes for debounce
			sendRecovery := time.Since(r.lastFired) > 5*time.Minute || r.thresholdDuration <= 0
			if hasSMTP && sendRecovery {
				go SendMail("", getPanelTitle()+" Recovery Notice", formatEmailHTML(alertLabel(r.key)+" has recovered to normal", recoveryDetail, getEmailTip(r.key, true), false))
			}
			if hasWebhook && sendRecovery {
				go SendWebhook(getPanelTitle()+" Recovery Notice", recoveryDetail)
			}
		} else if firing && r.firing {
			r.lastAlertMsg = msg
			// Continuous alert — re-send on each rule's interval.
			if time.Since(r.lastFired) > alertResendInterval(r.key) {
				r.lastFired = time.Now()
				logAlert(r.key, "critical", msg)
				if hasSMTP {
					go SendMail("", getPanelTitle()+" Alert — "+alertLabel(r.key)+" (ongoing)", formatEmailHTML(alertLabel(r.key)+" (ongoing)", msg, getEmailTip(r.key, false), true))
				}
				if hasWebhook {
					go SendWebhook(getPanelTitle()+" Alert — "+alertLabel(r.key)+" (ongoing)", msg)
				}
			}
		}
	}
}

func (r *alertRule) sustainedFiring(instantFiring bool, now time.Time) bool {
	if r.thresholdDuration <= 0 {
		if !instantFiring {
			r.pendingSince = time.Time{}
		}
		return instantFiring
	}
	if !instantFiring {
		r.pendingSince = time.Time{}
		return false
	}
	if r.pendingSince.IsZero() {
		r.pendingSince = now
		return false
	}
	return now.Sub(r.pendingSince) >= r.thresholdDuration
}

func alertResendInterval(key string) time.Duration {
	if key == "alert_system_update" || key == "alert_panel_update" {
		return 24 * time.Hour
	}
	return 30 * time.Minute
}

func isRuleEnabled(key string) bool {
	var v string
	database.GetDB().QueryRow("SELECT svalue FROM security_settings WHERE skey = ?", key).Scan(&v)
	return v != "false"
}

func alertLabel(key string) string {
	switch key {
	case "alert_cpu":
		return "CPU high load"
	case "alert_memory":
		return "Insufficient available memory"
	case "alert_disk":
		return "Insufficient disk space"
	case "alert_service":
		return "Service process abnormal"
	case "alert_ssl":
		return "SSL certificate expiring soon"
	case "alert_backup":
		return "Database backup failed"
	case "alert_website_expiry":
		return "Website expiring soon"
	case "alert_remote_backup":
		return "Remote backup failed"
	case "alert_cron_fail":
		return "Scheduled task execution failed"
	case "alert_site":
		return "Website unavailable"
	case "alert_system_update":
		return "System has available updates"
	case "alert_panel_update":
		return "Panel has a new version"
	}
	return key
}

func logAlert(alertType, level, message string) {
	db := database.GetDB()
	if db == nil {
		return
	}
	db.Exec("INSERT INTO alert_log (alert_type, level, message) VALUES (?, ?, ?)", alertType, level, message)
	// Keep last 30
	db.Exec("DELETE FROM alert_log WHERE id NOT IN (SELECT id FROM alert_log ORDER BY id DESC LIMIT 30)")
}

func getEmailTip(key string, isRecovery bool) string {
	switch key {
	case "alert_cpu":
		return "Tip: Sustained high CPU load may indicate traffic growth or attack. Please log in to the panel to view real-time trend charts."
	case "alert_memory":
		return "Tip: Low memory may be caused by high PHP-FPM or Redis usage, or excessive crawler requests. Please log in to the panel to view access logs and investigate abnormal traffic."
	case "alert_disk":
		return "Tip: Cleaning up old backup files and logs usually frees up significant space quickly, which is more practical than upgrading the disk."
	case "alert_service":
		if isRecovery {
			return "Tip: After the issue is resolved, it is recommended to review the logs. Understanding the root cause helps prevent recurrence."
		}
		return "Tip: Services will automatically attempt to restart. If alerts repeat, please log in to the panel to view the relevant logs and investigate the root cause."
	case "alert_ssl":
		if isRecovery {
			return "Tip: It is recommended to mark the next expiration date on your calendar. Renewing 30 days in advance is more relaxed."
		}
		return "Tip: Certificate expiration will cause browser 'Not Secure' warnings, affecting visitor trust and SEO. Please renew as soon as possible."
	case "alert_backup":
		return "Tip: Develop a good habit of regularly backing up your website. Data security is better with backups."
	case "alert_website_expiry":
		if isRecovery {
			return "Tip: Develop a good habit of regularly backing up your website. Data security is better with backups."
		}
		return "Tip: Please remind website users to renew or back up data promptly. After expiration, the website will be inaccessible."
	case "alert_remote_backup":
		return "Tip: Develop a good habit of regularly backing up your website. Data security is better with backups."
	case "alert_cron_fail":
		if isRecovery {
			return "Tip: After the issue is resolved, it is recommended to review the logs. Understanding the root cause helps prevent recurrence."
		}
		return "Tip: Scheduled task failures may be caused by script errors or insufficient resources. Please check the execution logs to locate the cause."
	case "alert_site":
		if isRecovery {
			return "Tip: Please confirm the website is accessible normally and inform website users of this incident."
		}
		return "Tip: Please promptly check server status, domain resolution, and website program functionality to avoid prolonged downtime affecting user operations."
	case "alert_system_update":
		if isRecovery {
			return "Tip: It is recommended to keep the system updated regularly. This is the simplest and most effective way to maintain server security."
		}
		return "Tip: Please log in to the panel settings page and execute the system update as soon as possible. Security updates typically fix known vulnerabilities; delaying updates increases attack risk."
	case "alert_panel_update":
		if isRecovery {
			return "Tip: After the panel is updated, it is recommended to briefly check key pages such as the website list, backups, and scheduled tasks."
		}
		return "Tip: Please update the panel promptly to avoid accumulating too many changes when upgrading across multiple versions, which increases upgrade risk."
	}
	return ""
}

func extractDomains(msg string) string {
	parts := strings.Split(msg, ";")
	var domains []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if idx := strings.Index(p, " "); idx > 0 {
			domains = append(domains, p[:idx])
		}
	}
	return strings.Join(domains, ", ")
}

func buildRecoveryDetail(r *alertRule) string {
	if r.key == "alert_site" && r.lastAlertMsg != "" {
		domains := extractDomains(r.lastAlertMsg)
		if domains != "" {
			return domains + " has recovered to normal"
		}
	}
	if r.key == "alert_system_update" {
		return "All system packages have been updated and are now at the latest version"
	}
	if r.key == "alert_panel_update" {
		return "Panel has been updated to the latest version"
	}
	return alertLabel(r.key) + " has recovered to normal"
}

func formatEmailHTML(title, detail, tip string, isAlert bool) string {
	icon := "ℹ️"
	titleColor := "#1976d2"
	if isAlert {
		icon = "⚠️"
		titleColor = "#d32f2f"
	}
	panelTitle := html.EscapeString(getPanelTitle())
	detail = html.EscapeString(detail)
	tip = html.EscapeString(tip)

	var b strings.Builder
	b.WriteString(`<!DOCTYPE html>
<html>
<head><meta charset="UTF-8"></head>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', 'Helvetica Neue', sans-serif; max-width: 560px; margin: 0 auto; padding: 24px; color: #333;">
`)
	fmt.Fprintf(&b, `<h2 style="color: %s; margin: 0 0 16px 0; font-size: 18px;">%s %s</h2>`+"\n", titleColor, icon, title)
	fmt.Fprintf(&b, `<p style="font-size: 15px; line-height: 1.7; margin: 0 0 24px 0; color: #444;">%s</p>`+"\n", detail)
	if tip != "" {
		b.WriteString(`<hr style="border: none; border-top: 1px solid #e0e0e0; margin: 24px 0;">` + "\n")
		fmt.Fprintf(&b, `<p style="font-size: 13px; line-height: 1.6; color: #888; margin: 0;">%s</p>`+"\n", tip)
	}
	fmt.Fprintf(&b, `<p style="font-size: 12px; color: #aaa; margin: 20px 0 0 0;">— From %s Panel</p>`+"\n", panelTitle)
	b.WriteString(`</body>
</html>`)
	return b.String()
}

// --- Checkers ---

func checkCPU() (bool, string) {
	db := database.GetDB()
	var cpu, ts string
	db.QueryRow("SELECT cpu_percent, recorded_at FROM monitoring_metrics ORDER BY id DESC LIMIT 1").Scan(&cpu, &ts)
	v, _ := strconv.ParseFloat(cpu, 64)
	if v > 80 {
		return true, fmt.Sprintf("CPU usage %.1f%% (threshold 80%%), at %s", v, toLocalTime(ts))
	}
	return false, ""
}

func checkMemory() (bool, string) {
	db := database.GetDB()
	var mem, ts string
	db.QueryRow("SELECT memory_percent, recorded_at FROM monitoring_metrics ORDER BY id DESC LIMIT 1").Scan(&mem, &ts)
	v, _ := strconv.ParseFloat(mem, 64)
	if v > 90 {
		return true, fmt.Sprintf("Available memory below 10%% (current usage %.1f%%), at %s", v, toLocalTime(ts))
	}
	return false, ""
}

func toLocalTime(dbTime string) string {
	layouts := []string{
		"2006-01-02 15:04:05",
		time.RFC3339,
	}
	for _, layout := range layouts {
		t, err := time.Parse(layout, dbTime)
		if err == nil {
			return t.Local().Format("2006-01-02 15:04:05")
		}
	}
	return dbTime
}

func checkDisk() (bool, string) {
	out, err := exec.Command("df", "-h", "/").Output()
	if err != nil {
		return false, ""
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) < 2 {
		return false, ""
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 5 {
		return false, ""
	}
	useStr := strings.TrimSuffix(fields[4], "%")
	use, _ := strconv.Atoi(useStr)
	if use > 90 {
		return true, fmt.Sprintf("Disk usage %d%% (threshold 90%%), remaining %s", use, fields[3])
	}
	return false, ""
}

func checkService() (bool, string) {
	svcs := GetGuardStatus()
	var msgs []string
	for _, s := range svcs {
		if !s.Running && !s.Paused && s.Restarts > 0 {
			msgs = append(msgs, fmt.Sprintf("%s abnormal (auto-restarted %d times, most recent: %s)", s.Name, s.Restarts, s.LastIncident))
		}
	}
	if len(msgs) > 0 {
		return true, strings.Join(msgs, "; ")
	}
	return false, ""
}

func checkSSL() (bool, string) {
	db := database.GetDB()
	// Include certificates expired within the last 7 days to avoid silently ignoring already expired ones
	rows, err := db.Query(`SELECT domain, ssl_expires_at FROM websites WHERE ssl_enabled = 1 AND ssl_expires_at > datetime('now', '-7 days')`)
	if err != nil {
		return false, ""
	}
	defer rows.Close()

	var msgs []string
	now := time.Now()
	for rows.Next() {
		var domain string
		var expiresAt time.Time
		if rows.Scan(&domain, &expiresAt) != nil {
			continue
		}
		days := int(expiresAt.Sub(now).Hours() / 24)
		if days < 0 {
			msgs = append(msgs, fmt.Sprintf("%s certificate expired %d days ago", domain, -days))
		} else if days <= 14 {
			msgs = append(msgs, fmt.Sprintf("%s certificate expires in %d days", domain, days))
		}
	}
	if len(msgs) > 0 {
		return true, strings.Join(msgs, "; ")
	}
	return false, ""
}

func checkBackup() (bool, string) {
	db := database.GetDB()
	rows, err := db.Query(`SELECT w.domain FROM backup_settings bs
		JOIN websites w ON w.id = bs.site_id
		WHERE bs.enabled = 1
		AND EXISTS (
			SELECT 1 FROM db_backups b
			WHERE b.site_id = bs.site_id AND b.auto = 1
		)
		AND NOT EXISTS (
			SELECT 1 FROM db_backups b
			WHERE b.site_id = bs.site_id AND b.auto = 1
			AND b.created_at > datetime('now', '-1 day', '-5 minutes')
		)
		ORDER BY w.domain`)
	if err != nil {
		return false, ""
	}
	defer rows.Close()

	var domains []string
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil {
			domains = append(domains, d)
		}
	}
	if len(domains) > 0 {
		return true, strings.Join(domains, ", ") + " has no successful automatic backup in the last 24 hours"
	}
	return false, ""
}

func checkWebsiteExpiry() (bool, string) {
	db := database.GetDB()
	rows, err := db.Query(`SELECT domain, expires_at FROM websites WHERE expires_at IS NOT NULL AND expires_at > datetime('now')`)
	if err != nil {
		return false, ""
	}
	defer rows.Close()

	var msgs []string
	now := time.Now()
	milestones := map[int]bool{14: true, 7: true, 3: true, 1: true}

	for rows.Next() {
		var domain string
		var expiresAt time.Time
		if rows.Scan(&domain, &expiresAt) != nil {
			continue
		}
		days := int(expiresAt.Sub(now).Hours() / 24)
		if !milestones[days] {
			continue
		}
		// Check if this domain has already been alerted today
		var alerted int
		db.QueryRow(`SELECT COUNT(*) FROM alert_log
			WHERE alert_type = 'alert_website_expiry' AND message LIKE ? AND created_at > datetime('now', '-24 hours')`,
			domain+"%").Scan(&alerted)
		if alerted > 0 {
			continue
		}
		msgs = append(msgs, fmt.Sprintf("%s expires in %d days", domain, days))
	}
	if len(msgs) > 0 {
		return true, strings.Join(msgs, "; ")
	}
	return false, ""
}

func checkRemoteBackup() (bool, string) {
	db := database.GetDB()
	var enabled int
	db.QueryRow("SELECT enabled FROM remote_backup_settings WHERE id = 1").Scan(&enabled)
	if enabled == 0 {
		return false, ""
	}

	// Check if there are any failed sync records in the last hour
	var failCount int
	db.QueryRow(`SELECT COUNT(*) FROM operation_logs
		WHERE operation = 'Remote backup' AND message LIKE 'Remote sync failed%'
		AND created_at > datetime('now', '-1 hour')`).Scan(&failCount)
	if failCount > 0 {
		return true, fmt.Sprintf("%d remote backup sync failures in the last hour", failCount)
	}
	return false, ""
}

func checkCronFail() (bool, string) {
	db := database.GetDB()
	rows, err := db.Query(`SELECT name FROM cron_jobs
		WHERE enabled = 1 AND notify_fail = 1 AND running = 0
		AND last_status = 'failed' AND last_run_at > datetime('now', '-1 hour')`)
	if err != nil {
		return false, ""
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			names = append(names, "'"+name+"'")
		}
	}
	if len(names) > 0 {
		return true, "Scheduled task " + strings.Join(names, ", ") + " execution failed"
	}
	return false, ""
}

const siteFailureAlertThreshold = 2

var siteLastCheck = make(map[string]time.Time)
var siteFailureMessages = make(map[string]string)
var siteFailureCounts = make(map[string]int)

func checkSites() (bool, string) {
	db := database.GetDB()
	rows, err := db.Query(`SELECT id, domain, ssl_enabled, monitoring_interval FROM websites WHERE status = 'active' AND monitoring_enabled = 1`)
	if err != nil {
		return false, ""
	}
	defer rows.Close()

	type siteInfo struct {
		id       string
		domain   string
		ssl      int
		interval int
	}
	var sites []siteInfo
	seen := make(map[string]bool)
	for rows.Next() {
		var s siteInfo
		if rows.Scan(&s.id, &s.domain, &s.ssl, &s.interval) != nil {
			continue
		}
		seen[s.id] = true
		if s.interval <= 0 {
			s.interval = 5
		}
		sites = append(sites, s)
	}

	for id := range siteFailureMessages {
		if !seen[id] {
			delete(siteFailureMessages, id)
			delete(siteFailureCounts, id)
		}
	}
	if len(siteLastCheck) > 100 {
		for id := range siteLastCheck {
			if !seen[id] {
				delete(siteLastCheck, id)
			}
		}
	}

	type checkTarget struct {
		id     string
		domain string
		url    string
	}
	var toCheck []checkTarget
	var msgs []string
	for _, s := range sites {
		if last, ok := siteLastCheck[s.id]; ok && time.Since(last) < time.Duration(s.interval)*time.Minute {
			if msg, ok := siteFailureMessages[s.id]; ok && siteFailureCounts[s.id] >= siteFailureAlertThreshold {
				msgs = append(msgs, msg)
			}
			continue
		}
		siteLastCheck[s.id] = time.Now()
		proto := "http"
		if s.ssl == 1 {
			proto = "https"
		}
		url := proto + "://" + s.domain + "/?wp_hc=" + strconv.FormatInt(time.Now().Unix(), 10)
		toCheck = append(toCheck, checkTarget{id: s.id, domain: s.domain, url: url})
	}

	if len(toCheck) == 0 {
		if len(msgs) > 0 {
			return true, strings.Join(msgs, "; ")
		}
		return false, ""
	}

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	type result struct {
		id     string
		domain string
		code   int
		err    error
	}
	resultCh := make(chan result, len(toCheck))
	for _, t := range toCheck {
		go func(t checkTarget) {
			resp, err := httpClient.Get(t.url)
			if err != nil {
				resultCh <- result{id: t.id, domain: t.domain, err: err}
				return
			}
			resp.Body.Close()
			resultCh <- result{id: t.id, domain: t.domain, code: resp.StatusCode}
		}(t)
	}

	for range toCheck {
		r := <-resultCh
		if r.err != nil {
			msg := fmt.Sprintf("%s unreachable (%v)", r.domain, r.err)
			siteFailureMessages[r.id] = msg
			siteFailureCounts[r.id]++
			if siteFailureCounts[r.id] >= siteFailureAlertThreshold {
				msgs = append(msgs, msg)
			}
		} else if r.code < 200 || r.code >= 400 {
			msg := fmt.Sprintf("%s returned %d", r.domain, r.code)
			siteFailureMessages[r.id] = msg
			siteFailureCounts[r.id]++
			if siteFailureCounts[r.id] >= siteFailureAlertThreshold {
				msgs = append(msgs, msg)
			}
		} else {
			delete(siteFailureMessages, r.id)
			delete(siteFailureCounts, r.id)
		}
	}

	if len(msgs) > 0 {
		return true, strings.Join(msgs, "; ")
	}
	return false, ""
}

var sysUpdateCache struct {
	mu     sync.Mutex
	lastAt time.Time
	names  []string
}

var panelUpdateCache struct {
	mu      sync.Mutex
	lastAt  time.Time
	latest  string
	message string
}

func ClearSystemUpdateAlertCache() {
	sysUpdateCache.mu.Lock()
	sysUpdateCache.lastAt = time.Time{}
	sysUpdateCache.names = nil
	sysUpdateCache.mu.Unlock()
}

func ClearPanelUpdateAlertCache() {
	panelUpdateCache.mu.Lock()
	panelUpdateCache.lastAt = time.Time{}
	panelUpdateCache.latest = ""
	panelUpdateCache.message = ""
	panelUpdateCache.mu.Unlock()
}

func checkSystemUpdate() (bool, string) {
	sysUpdateCache.mu.Lock()
	if time.Since(sysUpdateCache.lastAt) < 24*time.Hour {
		names := sysUpdateCache.names
		sysUpdateCache.mu.Unlock()
		if len(names) > 0 {
			return true, fmt.Sprintf("System has %d available updates: %s", len(names), strings.Join(names, ", "))
		}
		return false, ""
	}
	sysUpdateCache.mu.Unlock()

	out, err := exec.Command("bash", "-c", "apt list --upgradable 2>/dev/null").Output()
	if err != nil {
		return false, ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var names []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Listing...") {
			continue
		}
		parts := strings.SplitN(line, "/", 2)
		if len(parts) > 0 {
			names = append(names, parts[0])
		}
	}

	sysUpdateCache.mu.Lock()
	sysUpdateCache.lastAt = time.Now()
	sysUpdateCache.names = names
	sysUpdateCache.mu.Unlock()

	if len(names) > 0 {
		return true, fmt.Sprintf("System has %d available updates: %s", len(names), strings.Join(names, ", "))
	}
	return false, ""
}

func checkPanelUpdate() (bool, string) {
	if panelCurrentVersion == "" || panelCurrentVersion == "dev" {
		return false, ""
	}

	panelUpdateCache.mu.Lock()
	if time.Since(panelUpdateCache.lastAt) < 24*time.Hour {
		msg := panelUpdateCache.message
		panelUpdateCache.mu.Unlock()
		return msg != "", msg
	}
	panelUpdateCache.mu.Unlock()

	latest, err := FetchLatestPanelRelease("")
	if err != nil || latest == nil || latest.TagName == "" {
		return false, ""
	}

	msg := ""
	if CompareVersions(latest.TagName, panelCurrentVersion) > 0 {
		msg = fmt.Sprintf("Panel has new version %s available, current version %s. It is recommended to update from the panel settings page as soon as possible to avoid upgrading across multiple versions.", latest.TagName, panelCurrentVersion)
	}

	panelUpdateCache.mu.Lock()
	panelUpdateCache.lastAt = time.Now()
	panelUpdateCache.latest = latest.TagName
	panelUpdateCache.message = msg
	panelUpdateCache.mu.Unlock()

	return msg != "", msg
}

func getPanelTitle() string {
	db := database.GetDB()
	if db == nil {
		return "WP Panel"
	}
	var title string
	db.QueryRow("SELECT svalue FROM security_settings WHERE skey = 'panel_title'").Scan(&title)
	if title == "" {
		return "WP Panel"
	}
	return title
}
