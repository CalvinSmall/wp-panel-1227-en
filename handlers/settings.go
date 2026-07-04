package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/naibabiji/wp-panel/config"
	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/models"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

type SettingsHandler struct{}

func (h *SettingsHandler) GetSettings(c *gin.Context) {
	db := database.GetDB()
	var username string
	db.QueryRow("SELECT username FROM admin_users LIMIT 1").Scan(&username)

	basicAuthUser := readConfigValue("basic_auth", "username")

	var panelTitle string
	db.QueryRow("SELECT svalue FROM security_settings WHERE skey = 'panel_title'").Scan(&panelTitle)
	if panelTitle == "" {
		panelTitle = "WP Panel"
	}

	var githubProxy string
	db.QueryRow("SELECT svalue FROM security_settings WHERE skey = 'github_proxy'").Scan(&githubProxy)
	autoUpdate := map[string]string{}
	for _, key := range []string{
		"panel_auto_update_enabled", "panel_auto_update_mode", "panel_auto_update_window",
		"panel_auto_update_release_delay_minutes", "panel_auto_update_signature_timeout_minutes",
		"panel_auto_update_last_target_version", "panel_auto_update_last_attempt_at",
		"panel_auto_update_last_status", "panel_auto_update_last_stage", "panel_auto_update_last_error",
		"panel_auto_update_last_success_at", "panel_auto_update_last_success_version",
	} {
		var v string
		db.QueryRow("SELECT svalue FROM security_settings WHERE skey = ?", key).Scan(&v)
		autoUpdate[key] = v
	}

	timezone := getTimezone()
	hostname := getHostname()
	ntpSynced, ntpServer := getNTPSyncStatus()

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"username":          username,
		"basic_auth_user":   basicAuthUser,
		"panel_title":       panelTitle,
		"github_proxy":      githubProxy,
		"timezone":          timezone,
		"hostname":          hostname,
		"ntp_synced":        ntpSynced,
		"ntp_server":        ntpServer,
		"server_time":       time.Now().UnixMilli(),
		"panel_auto_update": autoUpdate,
	}))
}

func (h *SettingsHandler) UpdateSettings(c *gin.Context) {
	var req struct {
		PanelTitle                *string `json:"panel_title"`
		Username                  *string `json:"username"`
		BasicAuthUser             *string `json:"basic_auth_user"`
		OldPassword               *string `json:"old_password"`
		NewPassword               *string `json:"new_password"`
		BasicAuthPw               *string `json:"basic_auth_password"`
		Timezone                  *string `json:"timezone"`
		Hostname                  *string `json:"hostname"`
		NtpSync                   *bool   `json:"ntp_sync"`
		GithubProxy               *string `json:"github_proxy"`
		PanelAutoUpdateEnabled    *string `json:"panel_auto_update_enabled"`
		PanelAutoUpdateMode       *string `json:"panel_auto_update_mode"`
		PanelAutoUpdateWindow     *string `json:"panel_auto_update_window"`
		PanelAutoUpdateDelay      *string `json:"panel_auto_update_release_delay_minutes"`
		PanelAutoUpdateSigTimeout *string `json:"panel_auto_update_signature_timeout_minutes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("invalid parameters"))
		return
	}

	db := database.GetDB()

	if req.PanelTitle != nil && *req.PanelTitle != "" {
		_, err := db.Exec("UPDATE security_settings SET svalue = ?, updated_at = CURRENT_TIMESTAMP WHERE skey = 'panel_title'", *req.PanelTitle)
		if err != nil {
			_, _ = db.Exec("INSERT INTO security_settings (skey, svalue, description) VALUES ('panel_title', ?, 'Panel title')", *req.PanelTitle)
		}
	}

	if req.Username != nil && *req.Username != "" {
		if _, err := db.Exec("UPDATE admin_users SET username = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1", *req.Username); err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to update username"))
			return
		}
	}

	if req.BasicAuthUser != nil && *req.BasicAuthUser != "" {
		if err := updateConfigValue("basic_auth", "username", *req.BasicAuthUser); err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to update BasicAuth username"))
			return
		}
		config.AppConfig.BasicAuth.Username = *req.BasicAuthUser
	}

	if req.NewPassword != nil && *req.NewPassword != "" {
		if req.OldPassword == nil || *req.OldPassword == "" {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("please enter current password"))
			return
		}
		if len(*req.NewPassword) < 8 {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("new password must be at least 8 characters"))
			return
		}
		var currentHash string
		err := db.QueryRow("SELECT password_hash FROM admin_users LIMIT 1").Scan(&currentHash)
		if err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to query user"))
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(currentHash), []byte(*req.OldPassword)); err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("current password is incorrect"))
			return
		}
		newHash, err := bcrypt.GenerateFromPassword([]byte(*req.NewPassword), 12)
		if err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("password encryption failed"))
			return
		}
		_, err = db.Exec("UPDATE admin_users SET password_hash = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1", string(newHash))
		if err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to update password"))
			return
		}
	}

	if req.BasicAuthPw != nil && *req.BasicAuthPw != "" {
		if len(*req.BasicAuthPw) < 8 {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("BasicAuth password must be at least 8 characters"))
			return
		}
		newHash, err := bcrypt.GenerateFromPassword([]byte(*req.BasicAuthPw), 12)
		if err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("password encryption failed"))
			return
		}
		if err := updateConfigValue("basic_auth", "password_hash", string(newHash)); err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to update BasicAuth password"))
			return
		}
		config.AppConfig.BasicAuth.PasswordHash = string(newHash)
	}

	var tzRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_/+\\-]+(/[A-Za-z][A-Za-z0-9_/+\\-]+)*$`)

	if req.Timezone != nil && *req.Timezone != "" {
		tz := strings.TrimSpace(*req.Timezone)
		if !tzRe.MatchString(tz) {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("invalid timezone"))
			return
		}
		if err := exec.Command("timedatectl", "set-timezone", tz).Run(); err != nil {
			log.Printf("Failed to set timezone (%s): %v", tz, err)
		}
	}

	var hostRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?$`)

	if req.Hostname != nil && *req.Hostname != "" {
		host := strings.TrimSpace(*req.Hostname)
		if !hostRe.MatchString(host) || len(host) > 253 || strings.HasPrefix(host, "-") || strings.HasSuffix(host, "-") {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("invalid hostname"))
			return
		}
		exec.Command("hostnamectl", "set-hostname", host).Run()
	}

	if req.NtpSync != nil && *req.NtpSync {
		exec.Command("bash", "-c", "timedatectl set-ntp true 2>/dev/null; systemctl restart systemd-timesyncd 2>/dev/null; ntpdate -u pool.ntp.org 2>/dev/null || true").Run()
	}

	if req.GithubProxy != nil {
		proxy := strings.TrimSpace(*req.GithubProxy)
		proxy = strings.TrimRight(proxy, "/")
		if proxy != "" && !strings.HasPrefix(proxy, "https://") {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("proxy URL must start with https://"))
			return
		}
		_, err := db.Exec("UPDATE security_settings SET svalue = ?, updated_at = CURRENT_TIMESTAMP WHERE skey = 'github_proxy'", proxy)
		if err != nil {
			_, _ = db.Exec("INSERT INTO security_settings (skey, svalue, description) VALUES ('github_proxy', ?, 'GitHub proxy address')", proxy)
		}
	}

	if req.PanelAutoUpdateEnabled != nil {
		v := strings.TrimSpace(*req.PanelAutoUpdateEnabled)
		if v != "true" && v != "false" {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("auto-update toggle parameter error"))
			return
		}
		saveSecuritySetting("panel_auto_update_enabled", v)
	}
	if req.PanelAutoUpdateMode != nil {
		v := strings.TrimSpace(*req.PanelAutoUpdateMode)
		if v != "patch_only" && v != "all_stable" {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("auto-update mode parameter error"))
			return
		}
		saveSecuritySetting("panel_auto_update_mode", v)
	}
	if req.PanelAutoUpdateWindow != nil {
		v := strings.TrimSpace(*req.PanelAutoUpdateWindow)
		if !regexp.MustCompile(`^\d{2}:\d{2}-\d{2}:\d{2}$`).MatchString(v) {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("auto-update time window format should be HH:MM-HH:MM"))
			return
		}
		saveSecuritySetting("panel_auto_update_window", v)
	}
	if req.PanelAutoUpdateDelay != nil {
		if !saveMinuteSetting(c, "panel_auto_update_release_delay_minutes", *req.PanelAutoUpdateDelay, 1, 1440) {
			return
		}
	}
	if req.PanelAutoUpdateSigTimeout != nil {
		if !saveMinuteSetting(c, "panel_auto_update_signature_timeout_minutes", *req.PanelAutoUpdateSigTimeout, 5, 1440) {
			return
		}
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Settings updated"}))
}

func saveSecuritySetting(key, value string) {
	db := database.GetDB()
	_, _ = db.Exec(`INSERT INTO security_settings (skey, svalue, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(skey) DO UPDATE SET svalue = excluded.svalue, updated_at = excluded.updated_at`, key, value)
}

func saveMinuteSetting(c *gin.Context, key, raw string, min, max int) bool {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v < min || v > max {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(fmt.Sprintf("minutes must be between %d-%d", min, max)))
		return false
	}
	saveSecuritySetting(key, strconv.Itoa(v))
	return true
}

func (h *SettingsHandler) TestProxy(c *gin.Context) {
	proxy := strings.TrimRight(strings.TrimSpace(c.Query("url")), "/")
	if proxy == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("please provide a proxy URL"))
		return
	}
	if !strings.HasPrefix(proxy, "https://") {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("proxy URL must start with https://"))
		return
	}

	testURL := proxy + "/https://api.github.com/repos/naibabiji/wp-panel/releases/latest"
	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	resp, err := client.Get(testURL)
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
			"ok":      false,
			"error":   fmt.Sprintf("Connection failed: %v", err),
			"latency": elapsed,
		}))
		return
	}
	resp.Body.Close()

	if resp.StatusCode == 200 {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
			"ok":      true,
			"latency": elapsed,
		}))
	} else {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
			"ok":      false,
			"error":   fmt.Sprintf("HTTP %d", resp.StatusCode),
			"latency": elapsed,
		}))
	}
}

func (h *SettingsHandler) GetOperationLogs(c *gin.Context) {
	db := database.GetDB()

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	perPage := 30

	var total int
	db.QueryRow("SELECT COUNT(*) FROM operation_logs").Scan(&total)

	offset := (page - 1) * perPage
	rows, err := db.Query(
		`SELECT id, operation, target, status, message, created_at
		 FROM operation_logs ORDER BY created_at DESC LIMIT ? OFFSET ?`, perPage, offset,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("query failed"))
		return
	}
	defer rows.Close()

	var logs []models.OperationLog
	for rows.Next() {
		var l models.OperationLog
		if err := rows.Scan(&l.ID, &l.Operation, &l.Target, &l.Status, &l.Message, &l.CreatedAt); err != nil {
			continue
		}
		logs = append(logs, l)
	}
	if logs == nil {
		logs = []models.OperationLog{}
	}

	totalPages := (total + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"data":        logs,
		"total":       total,
		"page":        page,
		"per_page":    perPage,
		"total_pages": totalPages,
	}))
}

func GetPanelTitle() string {
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

func readConfigValue(section, key string) string {
	data, err := os.ReadFile("/www/server/panel/config.json")
	if err != nil {
		return ""
	}
	var cfg map[string]map[string]interface{}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	if sec, ok := cfg[section]; ok {
		if v, ok := sec[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func getNTPSyncStatus() (bool, string) {
	out, _ := exec.Command("bash", "-c", "timedatectl show --property=NTP --value 2>/dev/null").CombinedOutput()
	synced := strings.TrimSpace(string(out)) == "yes"
	server := "pool.ntp.org"
	return synced, server
}

func getTimezone() string {
	out, _ := exec.Command("bash", "-c", "timedatectl show --property=Timezone --value 2>/dev/null").CombinedOutput()
	tz := strings.TrimSpace(string(out))
	if tz == "" {
		if data, err := os.ReadFile("/etc/timezone"); err == nil {
			tz = strings.TrimSpace(string(data))
		}
	}
	return tz
}

func getHostname() string {
	out, _ := exec.Command("bash", "-c", "hostnamectl hostname 2>/dev/null || hostname").CombinedOutput()
	return strings.TrimSpace(string(out))
}

// ============================================================
// WordPress Package Management
// ============================================================

func (h *SettingsHandler) GetWPPackage(c *gin.Context) {
	cfg := config.AppConfig
	pkgPath := cfg.Paths.WordPressPackage

	info, err := os.Stat(pkgPath)
	if err != nil {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
			"available": false,
			"path":      pkgPath,
		}))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"available":  true,
		"path":       pkgPath,
		"size":       info.Size(),
		"size_text":  formatFileSize(info.Size()),
		"updated_at": info.ModTime().Format("2006-01-02 15:04:05"),
	}))
}

func (h *SettingsHandler) UploadWPPackage(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("please select a file"))
		return
	}

	// Validate file extension
	name := strings.ToLower(file.Filename)
	if !strings.HasSuffix(name, ".zip") {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("only .zip packages are supported"))
		return
	}

	// Limit file size (WordPress packages are usually 25-30MB, max 100MB)
	if file.Size > 100*1024*1024 {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("file too large, max 100MB"))
		return
	}

	cfg := config.AppConfig
	pkgPath := cfg.Paths.WordPressPackage
	pkgDir := filepath.Dir(pkgPath)

	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to create directory"))
		return
	}

	// Write to temp file first, replace after validation
	tmpPath := pkgPath + ".upload_tmp"
	if err := c.SaveUploadedFile(file, tmpPath); err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to save file"))
		return
	}

	// Basic validation: use unzip -t to test archive integrity
	if out, err := exec.Command("unzip", "-t", tmpPath).CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		log.Printf("Uploaded ZIP validation failed: %s, %v", string(out), err)
		c.JSON(http.StatusBadRequest, models.ErrorResponse("file validation failed, not a valid ZIP archive"))
		return
	}

	// Replace old file
	os.Remove(pkgPath)
	if err := os.Rename(tmpPath, pkgPath); err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to replace package"))
		return
	}

	log.Printf("WordPress package updated via upload: %s (%s)", pkgPath, formatFileSize(file.Size))
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"message": "Package uploaded successfully",
	}))
}

func (h *SettingsHandler) DownloadWPPackage(c *gin.Context) {
	cfg := config.AppConfig
	pkgPath := cfg.Paths.WordPressPackage
	pkgDir := filepath.Dir(pkgPath)

	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to create directory"))
		return
	}

	tmpPath := pkgPath + ".download_tmp"
	if out, err := exec.Command("wget", "-q", "-T", "30", "-t", "3", "-O", tmpPath,
		"https://wordpress.org/latest.zip").CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		log.Printf("Online download of WordPress package failed: %s, %v", string(out), err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("download failed, please check server network or upload package manually"))
		return
	}

	// Validate downloaded file
	if info, err := os.Stat(tmpPath); err != nil || info.Size() == 0 {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("downloaded file is invalid"))
		return
	}

	// Validate ZIP integrity
	if out, err := exec.Command("unzip", "-t", tmpPath).CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		log.Printf("Downloaded ZIP validation failed: %s, %v", string(out), err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("downloaded file validation failed, please retry or upload manually"))
		return
	}

	// Replace old file
	os.Remove(pkgPath)
	if err := os.Rename(tmpPath, pkgPath); err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to replace package"))
		return
	}

	// Update file timestamp to current time (wget preserves remote Last-Modified by default, so mtime doesn't reflect actual download time)
	os.Chtimes(pkgPath, time.Now(), time.Now())

	info, _ := os.Stat(pkgPath)
	sizeText := ""
	if info != nil {
		sizeText = formatFileSize(info.Size())
	}

	log.Printf("WordPress package updated via online download: %s (%s)", pkgPath, sizeText)
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"message": "Package downloaded successfully",
	}))
}

func (h *SettingsHandler) DeleteWPPackage(c *gin.Context) {
	cfg := config.AppConfig
	pkgPath := cfg.Paths.WordPressPackage

	if err := os.Remove(pkgPath); err != nil && !os.IsNotExist(err) {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to delete"))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"message": "Package deleted",
	}))
}

func formatFileSize(size int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case size >= GB:
		return fmt.Sprintf("%.1f GB", float64(size)/float64(GB))
	case size >= MB:
		return fmt.Sprintf("%.1f MB", float64(size)/float64(MB))
	case size >= KB:
		return fmt.Sprintf("%.1f KB", float64(size)/float64(KB))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

func updateConfigValue(section, key, value string) error {
	configPath := "/www/server/panel/config.json"
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read config file")
	}
	var cfg map[string]map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse config file")
	}
	sec, ok := cfg[section]
	if !ok {
		return fmt.Errorf("config section %s not found", section)
	}
	sec[key] = value
	newData, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize config")
	}
	if err := os.WriteFile(configPath, newData, 0600); err != nil {
		return fmt.Errorf("failed to write config file")
	}
	return nil
}

// ============================================================
// Panel Database Backup Management
// ============================================================

func (h *SettingsHandler) GetDBBackups(c *gin.Context) {
	cfg := config.AppConfig
	backupDir := filepath.Join(cfg.Panel.BackupDir, "panel-db")
	backups, err := database.ListDBBackups(backupDir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to query backup list"))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(backups))
}

func (h *SettingsHandler) CreateDBBackup(c *gin.Context) {
	cfg := config.AppConfig
	backupDir := filepath.Join(cfg.Panel.BackupDir, "panel-db")

	path, err := database.BackupDatabase(backupDir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("backup failed: "+err.Error()))
		return
	}

	// Verify backup integrity
	if verr := database.VerifyDBBackup(path); verr != nil {
		os.Remove(path)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("backup verification failed: "+verr.Error()))
		return
	}

	database.CleanupOldDBBackups(backupDir, 7)

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"message": "Database backup complete",
	}))
}

func (h *SettingsHandler) RestoreDBBackup(c *gin.Context) {
	var req struct {
		Filename string `json:"filename"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Filename == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("please select a backup to restore"))
		return
	}

	cfg := config.AppConfig
	backupDir := filepath.Join(cfg.Panel.BackupDir, "panel-db")

	backupPath, err := database.RestoreDBBackupPath(backupDir, req.Filename)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
		return
	}

	// Verify backup file integrity
	if verr := database.VerifyDBBackup(backupPath); verr != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("backup file verification failed, cannot restore: "+verr.Error()))
		return
	}

	dbPath := cfg.SQLite.Path

	// Create a safety backup (current running database) for rollback
	safeBackup, safeErr := database.BackupDatabase(backupDir)
	if safeErr != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("safety backup before restore failed: "+safeErr.Error()))
		return
	}

	// Write restore script: atomic replace (cp to .tmp then mv) -> clean WAL/SHM -> restart; rollback to safety backup if cp/mv fails.
	// Escape single quotes in paths to prevent shell injection
	sb := strings.ReplaceAll(safeBackup, "'", "'\\''")
	bp := strings.ReplaceAll(backupPath, "'", "'\\''")
	dp := strings.ReplaceAll(dbPath, "'", "'\\''")

	script := "#!/bin/bash\n" +
		"sleep 1\n" +
		"rm -f '" + dp + "'.tmp\n" +
		// Atomic replace: copy to .tmp first, then mv (mv is atomic on same filesystem)
		"cp -f '" + bp + "' '" + dp + "'.tmp && " +
		"mv -f '" + dp + "'.tmp '" + dp + "'\n" +
		"restore_status=$?\n" +
		"rm -f '" + dp + "'.tmp\n" +
		"if [ $restore_status -ne 0 ]; then\n" +
		// cp/mv failed -> rollback to safety backup
		"  echo 'DB restore cp/mv failed, rolling back...' >&2\n" +
		"  cp -f '" + sb + "' '" + dp + "'\n" +
		"fi\n" +
		"rm -f '" + dp + "-wal' '" + dp + "-shm'\n" +
		"systemctl restart wp-panel\n" +
		"rm -f /tmp/wp-panel-db-restore.sh\n"

	scriptPath := "/tmp/wp-panel-db-restore.sh"
	if err := os.WriteFile(scriptPath, []byte(script), 0700); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to create restore script"))
		return
	}

	// Run async via script
	if err := exec.Command("bash", scriptPath).Start(); err != nil {
		os.Remove(scriptPath)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to start restore script: "+err.Error()))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"message": "Database restoring, panel will restart automatically. If restart fails, safety backup at " + filepath.Base(safeBackup),
	}))
}

func (h *SettingsHandler) DeleteDBBackup(c *gin.Context) {
	filename := c.Query("filename")
	if filename == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("please specify a filename"))
		return
	}

	cfg := config.AppConfig
	backupDir := filepath.Join(cfg.Panel.BackupDir, "panel-db")

	fullPath, err := database.RestoreDBBackupPath(backupDir, filename)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
		return
	}

	if err := os.Remove(fullPath); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to delete"))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Backup deleted"}))
}

func (h *SettingsHandler) DownloadDBBackup(c *gin.Context) {
	filename := c.Param("filename")

	cfg := config.AppConfig
	backupDir := filepath.Join(cfg.Panel.BackupDir, "panel-db")

	fullPath, err := database.RestoreDBBackupPath(backupDir, filename)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
		return
	}

	c.FileAttachment(fullPath, filename)
}
