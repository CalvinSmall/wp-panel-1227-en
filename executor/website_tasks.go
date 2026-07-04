package executor

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/naibabiji/wp-panel/config"
	"github.com/naibabiji/wp-panel/database"
)

type rollbackStep struct {
	desc string
	fn   func() error
}

func moveSiteLogDir(oldLogDir, newLogDir string) error {
	if oldLogDir == newLogDir {
		return nil
	}
	if _, err := os.Stat(oldLogDir); err != nil {
		return fmt.Errorf("failed to check old log directory: %w", err)
	}
	if info, err := os.Stat(newLogDir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("target log path already exists and is not a directory: %s", newLogDir)
		}
		entries, err := os.ReadDir(newLogDir)
		if err != nil {
			return fmt.Errorf("failed to read target log directory: %w", err)
		}
		if len(entries) > 0 {
			return fmt.Errorf("target log directory already exists and is not empty: %s", newLogDir)
		}
		if err := os.Remove(newLogDir); err != nil {
			return fmt.Errorf("failed to clean up empty target log directory: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to check target log directory: %w", err)
	}
	return os.Rename(oldLogDir, newLogDir)
}

func createSiteLogDir(logDir string) error {
	if strings.TrimSpace(logDir) == "" {
		return fmt.Errorf("log directory is empty")
	}
	if info, err := os.Lstat(logDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("log directory cannot be a symlink: %s", logDir)
		}
		if !info.IsDir() {
			return fmt.Errorf("log path already exists and is not a directory: %s", logDir)
		}
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return err
		}
	} else {
		return err
	}
	ensureSiteLogFiles(logDir)
	return nil
}

func managedSubpath(rootPath, targetPath, label string) (string, error) {
	rootPath = strings.TrimSpace(rootPath)
	targetPath = strings.TrimSpace(targetPath)
	if rootPath == "" || targetPath == "" {
		return "", fmt.Errorf("%s path is empty", label)
	}

	root := filepath.Clean(rootPath)
	target := filepath.Clean(targetPath)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", fmt.Errorf("%s path validation failed: %w", label, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s path is not within allowed directory: %s", label, targetPath)
	}
	return target, nil
}

func ensureCreateSiteResourcesAvailable(systemUser, webRoot, logDir, dbName, dbUser, phpPoolPath, nginxConfPath, nginxEnabledPath, phpSockPath string) error {
	db := database.GetDB()
	if db != nil {
		var domain string
		err := db.QueryRow(`
			SELECT domain
			FROM websites
			WHERE system_user = ?
			   OR web_root = ?
			   OR log_dir = ?
			   OR db_name = ?
			   OR db_user = ?
			   OR php_pool_path = ?
			   OR nginx_conf_path = ?
			LIMIT 1
		`, systemUser, webRoot, logDir, dbName, dbUser, phpPoolPath, nginxConfPath).Scan(&domain)
		if err == nil {
			return fmt.Errorf("internal resource is already used by site %s", domain)
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("check existing site resources: %w", err)
		}
	}

	if _, err := executeCommand("id", "-u", systemUser); err == nil {
		return fmt.Errorf("system user already exists: %s", systemUser)
	}

	for label, path := range map[string]string{
		"web root":            webRoot,
		"log dir":             logDir,
		"php-fpm pool":        phpPoolPath,
		"nginx config":        nginxConfPath,
		"nginx enabled link":  nginxEnabledPath,
		"php-fpm socket file": phpSockPath,
	} {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists: %s", label, path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("check %s: %w", label, err)
		}
	}

	return nil
}

func executeCreateSite(task *Task) TaskResult {
	payload, ok := task.Payload.(*CreateSitePayload)
	if !ok {
		return TaskResult{Success: false, Message: "Task parameter type error"}
	}

	var rollbacks []rollbackStep
	rollback := func() {
		for i := len(rollbacks) - 1; i >= 0; i-- {
			step := rollbacks[i]
			if err := step.fn(); err != nil {
				fmt.Fprintf(os.Stderr, "Rollback failed [%s]: %v\n", step.desc, err)
			}
		}
	}

	cfg := config.AppConfig
	domain := strings.ToLower(strings.TrimSpace(payload.Domain))
	siteName := buildSiteName(domain)

	dbPassword := payload.DBPassword
	if dbPassword == "" {
		dbPassword = generatePassword(24)
	}

	if !IsValidDomain(domain) {
		return TaskResult{Success: false, Message: "Invalid domain format: " + domain}
	}
	for _, alias := range payload.Aliases {
		if !IsValidDomain(strings.TrimSpace(alias)) {
			return TaskResult{Success: false, Message: "Invalid alias domain format: " + alias}
		}
	}

	systemUser := "wp_" + siteName
	if payload.SiteType == "php" {
		systemUser = "php_" + siteName
	}
	documentRootSubdir, err := NormalizeDocumentRootSubdir(payload.SiteType, payload.DocumentRootSubdir)
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	webRoot := filepath.Join(cfg.Paths.WWWRoot, domain)
	logDir := filepath.Join(cfg.Paths.WWWLogs, domain)
	dbName := "db_" + siteName
	dbUser := "user_" + siteName
	configBase := siteConfigBaseName(siteName)
	phpPoolPath := filepath.Join(cfg.Paths.PHPFPMPool, configBase+".conf")
	nginxConfPath := filepath.Join(cfg.Paths.NginxSitesAvailable, configBase+".conf")
	nginxEnabledPath := filepath.Join(cfg.Paths.NginxSitesEnabled, configBase+".conf")
	phpSockPath := filepath.Join(cfg.Paths.PHPFPMSock, configBase+".sock")
	if err := validateUnixSocketPath(phpSockPath); err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}

	if err := ensureCreateSiteResourcesAvailable(systemUser, webRoot, logDir, dbName, dbUser, phpPoolPath, nginxConfPath, nginxEnabledPath, phpSockPath); err != nil {
		log.Printf("Site resource name conflict domain=%s: %v", domain, err)
		return TaskResult{Success: false, Message: "Site resource name conflict: " + err.Error()}
	}

	// Step 1: Create system user
	if _, err := executeCommand("useradd", "-r", "-U", "-s", "/usr/sbin/nologin", "-M", "-d", "/nonexistent", systemUser); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			log.Printf("Failed to create system user: %v", err)
			return TaskResult{Success: false, Message: "Failed to create system user"}
		}
	}
	if err := ensureSitePrimaryGroup(systemUser); err != nil {
		log.Printf("Failed to create site user group: %v", err)
		return TaskResult{Success: false, Message: "Failed to create site user group"}
	}
	rollbacks = append(rollbacks, rollbackStep{"Delete system user " + systemUser, func() error {
		_, e := executeCommand("userdel", "-r", "-f", systemUser)
		return e
	}})

	// Step 2: Create directories
	for _, dir := range []string{webRoot, logDir} {
		if _, err := executeCommand("mkdir", "-p", dir); err != nil {
			rollback()
			log.Printf("Failed to create directory: %v", err)
			return TaskResult{Success: false, Message: "Failed to create directory"}
		}
	}
	ensureSiteLogFiles(logDir)
	rollbacks = append(rollbacks, rollbackStep{"Delete website directory " + webRoot, func() error {
		os.RemoveAll(webRoot)
		return nil
	}})
	rollbacks = append(rollbacks, rollbackStep{"Delete log directory " + logDir, func() error {
		os.RemoveAll(logDir)
		return nil
	}})
	documentRoot, err := EnsureEffectiveDocumentRoot(webRoot, payload.SiteType, documentRootSubdir, systemUser)
	if err != nil {
		rollback()
		log.Printf("Failed to prepare web entry directory: %v", err)
		return taskFailure("Failed to prepare web entry directory", err)
	}

	// Step 3: Deploy site files
	if payload.SiteType != "php" {
		wpPackagePath := cfg.Paths.WordPressPackage
		tmpDir := "/tmp/wp_deploy_" + siteName + "_" + generatePassword(8)
		if err := deployWordPress(wpPackagePath, webRoot, tmpDir); err != nil {
			rollback()
			log.Printf("WordPress deployment failed: %v", err)
			return TaskResult{Success: false, Message: "WordPress deployment failed"}
		}
	}

	// Step 4: Chown
	if _, err := executeCommand("chown", "-R", siteOwner(systemUser), webRoot); err != nil {
		rollback()
		log.Printf("Failed to set directory permissions: %v", err)
		return TaskResult{Success: false, Message: "Failed to set directory permissions"}
	}

	// Step 5: Create database
	if err := createMariaDBDatabase(dbName, dbUser, dbPassword, cfg); err != nil {
		rollback()
		log.Printf("Failed to create database: %v", err)
		return TaskResult{Success: false, Message: "Failed to create database"}
	}
	rollbacks = append(rollbacks, rollbackStep{"Delete database " + dbName, func() error {
		return dropMariaDBDatabase(dbName, dbUser, cfg)
	}})

	// Step 6: Generate wp-config.php (wordpress only)
	if payload.SiteType != "php" {
		if err := generateWPConfig(webRoot, domain, dbName, dbUser, dbPassword); err != nil {
			rollback()
			log.Printf("Failed to generate wp-config.php: %v", err)
			return TaskResult{Success: false, Message: "Failed to generate wp-config.php"}
		}
	}
	if err := HardenSiteSensitivePermissions(domain, webRoot, systemUser); err != nil {
		rollback()
		log.Printf("Failed to set site security permissions: %v", err)
		return TaskResult{Success: false, Message: "Failed to set site security permissions"}
	}

	// Step 7: Generate Nginx + PHP-FPM configs
	engine := NewTemplateEngine(cfg.Panel.BackupDir)

	allServerNames := buildServerNames(domain, payload.Aliases)

	phpData := &PHPFPMPoolData{
		Domain:     domain,
		PoolName:   configBase,
		SystemUser: systemUser,
		WebRoot:    webRoot,
		SocketPath: cfg.Paths.PHPFPMSock,
		SocketName: configBase,
	}
	phpConfig, err := engine.RenderPHPFPMPool(phpData)
	if err != nil {
		rollback()
		log.Printf("Failed to render PHP-FPM config: %v", err)
		return taskFailure("Failed to render PHP-FPM config", err)
	}
	if err := engine.ApplyPHPFPMPool(phpConfig, phpPoolPath, logDir); err != nil {
		rollback()
		log.Printf("Failed to apply PHP-FPM config: %v", err)
		return taskFailure("Failed to apply PHP-FPM config", err)
	}
	rollbacks = append(rollbacks, rollbackStep{"Delete PHP-FPM config " + phpPoolPath, func() error {
		os.Remove(phpPoolPath)
		exec.Command("systemctl", "reload", "php8.3-fpm").Run()
		return nil
	}})

	nginxData := &NginxSiteData{
		Domain:        domain,
		Aliases:       payload.Aliases,
		ServerNames:   allServerNames,
		WebRoot:       documentRoot,
		LogDir:        logDir,
		SystemUser:    systemUser,
		UseSSL:        false,
		PHPProxy:      "unix:" + phpSockPath,
		SiteType:      payload.SiteType,
		TemplateVer:   "v1.0",
		AccessLogMode: "error_only",
	}

	nginxConfig, err := engine.RenderNginxConfig(nginxData)
	if err != nil {
		rollback()
		log.Printf("Failed to render Nginx config: %v", err)
		return taskFailure("Failed to render Nginx config", err)
	}

	if err := engine.ApplyNginxConfig(nginxConfig, nginxConfPath, nginxEnabledPath); err != nil {
		rollback()
		log.Printf("Failed to apply Nginx config: %v", err)
		return taskFailure("Failed to apply Nginx config", err)
	}
	rollbacks = append(rollbacks, rollbackStep{"Delete Nginx config " + nginxConfPath, func() error {
		os.Remove(nginxEnabledPath)
		os.Remove(nginxConfPath)
		exec.Command("nginx", "-s", "reload").Run()
		return nil
	}})

	maskedPassword := maskPassword(dbPassword)

	certDir := filepath.Join(cfg.Paths.Certificates, domain)
	certPath := filepath.Join(certDir, "fullchain.pem")
	keyPath := filepath.Join(certDir, "privkey.pem")

	sslEnabled := 0
	var sslExpiry *time.Time
	sslWarning := ""
	if payload.SSLEnabled {
		if sslErr := os.MkdirAll(certDir, 0700); sslErr != nil {
			rollback()
			log.Printf("Failed to create SSL certificate directory: %v", sslErr)
			return TaskResult{Success: false, Message: "Failed to create SSL certificate directory"}
		}
		expiry, sslErr := obtainLegoCert(domain, strings.Join(payload.Aliases, "\n"), documentRoot, certDir)
		if sslErr != nil {
			log.Printf("Failed to obtain Let's Encrypt certificate: %v", sslErr)
			sslWarning = FriendlySSLError(sslErr)
			os.RemoveAll(certDir)
		} else {
			sslData := &NginxSiteData{
				Domain:        domain,
				Aliases:       payload.Aliases,
				ServerNames:   allServerNames,
				WebRoot:       documentRoot,
				LogDir:        logDir,
				SystemUser:    systemUser,
				UseSSL:        true,
				SSLCertPath:   certPath,
				SSLKeyPath:    keyPath,
				PHPProxy:      "unix:" + phpSockPath,
				SiteType:      payload.SiteType,
				TemplateVer:   "v1.0",
				AccessLogMode: "error_only",
			}

			httpsConfig, sslErr := engine.RenderNginxConfig(sslData)
			if sslErr != nil {
				log.Printf("Failed to render HTTPS config: %v", sslErr)
				sslWarning = "Failed to render HTTPS config: " + sslErr.Error()
				os.RemoveAll(certDir)
			} else if sslErr := engine.ApplyNginxConfig(httpsConfig, nginxConfPath, nginxEnabledPath); sslErr != nil {
				log.Printf("Failed to apply HTTPS config: %v", sslErr)
				os.RemoveAll(certDir)
				if restoreErr := engine.ApplyNginxConfig(nginxConfig, nginxConfPath, nginxEnabledPath); restoreErr != nil {
					rollback()
					log.Printf("Failed to restore HTTP config: %v", restoreErr)
					return taskFailure("Failed to apply HTTPS config and also failed to restore HTTP config", restoreErr)
				}
				sslWarning = "Failed to apply HTTPS config: " + sslErr.Error()
			} else {
				sslEnabled = 1
				sslExpiry = &expiry
				rollbacks = append(rollbacks, rollbackStep{"Delete SSL certificate directory " + certDir, func() error {
					os.RemoveAll(certDir)
					return nil
				}})
			}
		}
	}

	if payload.SiteType != "php" {
		if payload.CleanDefaults {
			removeDefaultPlugins(webRoot)
			log.Printf("Cleaned default plugins site=%s", domain)
		}
		if payload.RemoveUnusedThemes {
			removeUnusedThemes(webRoot)
			log.Printf("Removed unused default themes site=%s", domain)
		}
		if len(payload.InstallThemes) > 0 || len(payload.InstallPlugins) > 0 {
			installExtensions(webRoot, systemUser, payload.InstallThemes, payload.InstallPlugins)
			log.Printf("Installed extensions site=%s themes=%v plugins=%v", domain, payload.InstallThemes, payload.InstallPlugins)
		}
	}

	db := database.GetDB()
	insertResult, err := db.Exec(
		`INSERT INTO websites (name, domain, aliases, status, system_user, web_root, document_root_subdir, log_dir,
		 db_name, db_user, php_pool_path, nginx_conf_path, site_type, ssl_enabled, ssl_cert_path, ssl_key_path, ssl_expires_at, ssl_last_error, template_version, access_log_mode, expires_at)
		 VALUES (?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'v1.0', 'error_only', ?)`,
		siteName, domain, strings.Join(payload.Aliases, "\n"), systemUser,
		webRoot, documentRootSubdir, logDir, dbName, dbUser, phpPoolPath, nginxConfPath, payload.SiteType, sslEnabled,
		certPath, keyPath, sslExpiry, sslWarning, nilIfEmpty(payload.ExpiresAt),
	)
	if err != nil {
		rollback()
		log.Printf("Failed to write to database: %v", err)
		return TaskResult{Success: false, Message: "Failed to write to database"}
	}
	siteID, _ := insertResult.LastInsertId()
	if err := WriteSiteLogrotateConfig(domain, logDir, defaultSiteLogRetentionDays); err != nil {
		log.Printf("site logrotate config skipped after site create: %v", err)
	}
	if err := ReloadFail2ban(); err != nil {
		log.Printf("Fail2ban reload skipped after site create: %v", err)
	}

	sslMsg := ""
	if sslEnabled == 1 {
		sslMsg = fmt.Sprintf(", SSL enabled (expires: %s)", sslExpiry.Format("2006-01-02"))
	} else if sslWarning != "" {
		sslMsg = ", but SSL not enabled: " + sslWarning
	}

	return TaskResult{
		Success: true,
		Message: fmt.Sprintf("Website %s created successfully%s", domain, sslMsg),
		Data: map[string]interface{}{
			"domain":      domain,
			"id":          siteID,
			"db_name":     dbName,
			"db_user":     dbUser,
			"db_password": maskedPassword,
			"web_root":    webRoot,
			"system_user": systemUser,
			"ssl_enabled": sslEnabled == 1,
			"ssl_warning": sslWarning,
		},
	}
}

func executeDeleteSite(task *Task) TaskResult {
	payload, ok := task.Payload.(*DeleteSitePayload)
	if !ok {
		return TaskResult{Success: false, Message: "Task parameter type error"}
	}
	site := payload.Site
	cfg := config.AppConfig

	webRoot, err := managedSubpath(cfg.Paths.WWWRoot, site.WebRoot, "website directory")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	logDir, err := managedSubpath(cfg.Paths.WWWLogs, site.LogDir, "log directory")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	phpPoolPath, err := managedSubpath(cfg.Paths.PHPFPMPool, site.PHPPoolPath, "PHP-FPM config")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	nginxConfPath, err := managedSubpath(cfg.Paths.NginxSitesAvailable, site.NginxConfPath, "Nginx config")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	enabledPath := nginxEnabledPath(cfg, nginxConfPath, site.Domain)
	enabledPath, err = managedSubpath(cfg.Paths.NginxSitesEnabled, enabledPath, "Nginx enabled link")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	secretsDir, err := managedSubpath("/var/wp-panel/site-secrets", filepath.Join("/var/wp-panel/site-secrets", site.Domain), "site secrets directory")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	logrotatePath, err := managedSubpath("/etc/logrotate.d", filepath.Join("/etc/logrotate.d", "wppanel-"+site.Domain), "logrotate config")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	certDir, err := managedSubpath(cfg.Paths.Certificates, filepath.Join(cfg.Paths.Certificates, site.Domain), "certificate directory")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}

	if _, err := executeCommand("userdel", "-r", "-f", site.SystemUser); err != nil {
		fmt.Fprintf(os.Stderr, "Delete system user warning: %v\n", err)
	}

	os.RemoveAll(webRoot)
	os.RemoveAll(logDir)
	os.RemoveAll(secretsDir)

	// Clean up logrotate config
	os.Remove(logrotatePath)

	dbCleanupWarning := ""
	if err := dropMariaDBDatabase(site.DBName, site.DBUser, cfg); err != nil {
		log.Printf("Failed to delete database domain=%s db=%s: %v", site.Domain, site.DBName, err)
		dbCleanupWarning = ", but database cleanup failed, please check MariaDB and clean up manually"
	}

	os.Remove(phpPoolPath)
	os.Remove(enabledPath)
	os.Remove(nginxConfPath)

	exec.Command("nginx", "-s", "reload").Run()
	exec.Command("systemctl", "reload", "php8.3-fpm").Run()

	os.RemoveAll(certDir)

	db := database.GetDB()
	if _, err := db.Exec("DELETE FROM websites WHERE id = ?", site.ID); err != nil {
		return TaskResult{Success: false, Message: "Failed to clean database records: " + err.Error()}
	}

	return TaskResult{Success: true, Message: "Website " + site.Domain + " has been deleted" + dbCleanupWarning}
}

func executePauseSite(task *Task) TaskResult {
	payload, ok := task.Payload.(*PauseSitePayload)
	if !ok {
		return TaskResult{Success: false, Message: "Task parameter type error"}
	}
	site := payload.Site
	cfg := config.AppConfig

	nginxConfPath, err := managedSubpath(cfg.Paths.NginxSitesAvailable, site.NginxConfPath, "Nginx config")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	enabledPath := nginxEnabledPath(cfg, nginxConfPath, site.Domain)
	enabledPath, err = managedSubpath(cfg.Paths.NginxSitesEnabled, enabledPath, "Nginx enabled link")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	var removedEnabled bool
	if _, err := os.Lstat(enabledPath); err == nil {
		if err := os.Remove(enabledPath); err != nil {
			return TaskResult{Success: false, Message: "Failed to remove Nginx enabled link: " + err.Error()}
		}
		removedEnabled = true
	} else if !os.IsNotExist(err) {
		return TaskResult{Success: false, Message: "Failed to check Nginx enabled link: " + err.Error()}
	}

	if out, err := exec.Command("nginx", "-s", "reload").CombinedOutput(); err != nil {
		if removedEnabled {
			if restoreErr := os.Symlink(nginxConfPath, enabledPath); restoreErr != nil {
				log.Printf("Failed to restore Nginx enabled link after pause failure path=%s: %v", enabledPath, restoreErr)
			} else {
				exec.Command("nginx", "-s", "reload").Run()
			}
		}
		return TaskResult{Success: false, Message: "Nginx reload failed: " + string(out)}
	}

	db := database.GetDB()
	if _, err := db.Exec("UPDATE websites SET status = 'paused', updated_at = CURRENT_TIMESTAMP WHERE id = ?", site.ID); err != nil {
		return TaskResult{Success: false, Message: "Failed to update website status: " + err.Error()}
	}

	return TaskResult{Success: true, Message: "Website " + site.Domain + " has been paused"}
}

func executeEnableSite(task *Task) TaskResult {
	payload, ok := task.Payload.(*EnableSitePayload)
	if !ok {
		return TaskResult{Success: false, Message: "Task parameter type error"}
	}
	site := payload.Site
	cfg := config.AppConfig

	nginxConfPath, err := managedSubpath(cfg.Paths.NginxSitesAvailable, site.NginxConfPath, "Nginx config")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	enabledPath := nginxEnabledPath(cfg, nginxConfPath, site.Domain)
	enabledPath, err = managedSubpath(cfg.Paths.NginxSitesEnabled, enabledPath, "Nginx enabled link")
	if err != nil {
		return TaskResult{Success: false, Message: err.Error()}
	}
	oldTarget, hadOldLink := "", false
	if target, err := os.Readlink(enabledPath); err == nil {
		oldTarget = target
		hadOldLink = true
	}
	os.Remove(enabledPath)
	if err := os.Symlink(nginxConfPath, enabledPath); err != nil {
		log.Printf("Failed to create symlink: %v", err)
		return TaskResult{Success: false, Message: "Failed to create symlink"}
	}

	if out, err := exec.Command("nginx", "-s", "reload").CombinedOutput(); err != nil {
		_ = os.Remove(enabledPath)
		if hadOldLink {
			if restoreErr := os.Symlink(oldTarget, enabledPath); restoreErr != nil {
				log.Printf("Failed to restore Nginx enabled link after enable failure path=%s: %v", enabledPath, restoreErr)
			} else {
				exec.Command("nginx", "-s", "reload").Run()
			}
		}
		return TaskResult{Success: false, Message: "Nginx reload failed: " + string(out)}
	}

	db := database.GetDB()
	if _, err := db.Exec("UPDATE websites SET status = 'active', updated_at = CURRENT_TIMESTAMP WHERE id = ?", site.ID); err != nil {
		return TaskResult{Success: false, Message: "Failed to update website status: " + err.Error()}
	}

	return TaskResult{Success: true, Message: "Website " + site.Domain + " has been enabled"}
}

func executeUpdateDomains(task *Task) TaskResult {
	payload, ok := task.Payload.(*UpdateDomainsPayload)
	if !ok {
		return TaskResult{Success: false, Message: "Task parameter type error"}
	}

	site := payload.Site
	cfg := config.AppConfig

	domainChanged := false
	oldDomain := site.Domain
	newDomain := strings.TrimSpace(payload.NewDomain)
	newAliases := payload.Aliases

	// Validate alias domains
	for _, alias := range newAliases {
		if !IsValidDomain(strings.TrimSpace(alias)) {
			return TaskResult{Success: false, Message: "Invalid alias domain format: " + alias}
		}
	}

	if newDomain != "" && newDomain != oldDomain {
		newDomain = strings.ToLower(newDomain)
		if !IsValidDomain(newDomain) {
			return TaskResult{Success: false, Message: "Invalid new domain format: " + newDomain}
		}
		domainChanged = true
	} else {
		newDomain = oldDomain
	}

	var rollbacks []rollbackStep
	rollback := func() {
		for i := len(rollbacks) - 1; i >= 0; i-- {
			step := rollbacks[i]
			if e := step.fn(); e != nil {
				fmt.Fprintf(os.Stderr, "Rollback failed [%s]: %v\n", step.desc, e)
			}
		}
	}

	if domainChanged {
		oldWebRoot := site.WebRoot
		oldLogDir := site.LogDir
		oldNginxConf := site.NginxConfPath
		oldPHPPool := site.PHPPoolPath
		oldCertDir := filepath.Join(cfg.Paths.Certificates, oldDomain)
		oldEnabledLink := nginxEnabledPath(cfg, oldNginxConf, oldDomain)

		newWebRoot := filepath.Join(cfg.Paths.WWWRoot, newDomain)
		newLogDir := filepath.Join(cfg.Paths.WWWLogs, newDomain)
		newNginxConf := oldNginxConf
		newPHPPool := oldPHPPool
		newCertDir := filepath.Join(cfg.Paths.Certificates, newDomain)
		newEnabledLink := nginxEnabledPath(cfg, newNginxConf, newDomain)
		poolName := phpPoolName(newPHPPool, newDomain)
		if err := validateUnixSocketPath(phpSocketPath(cfg, newPHPPool, newDomain)); err != nil {
			return TaskResult{Success: false, Message: err.Error()}
		}
		os.Remove(oldEnabledLink)
		if newEnabledLink != oldEnabledLink {
			os.Remove(newEnabledLink)
		}

		nginxReload := func() { exec.Command("nginx", "-s", "reload").Run() }
		nginxRB := rollbackStep{"Restore Nginx config", func() error {
			os.Symlink(oldNginxConf, oldEnabledLink)
			nginxReload()
			return nil
		}}
		rollbacks = append(rollbacks, nginxRB)

		oldPoolContent, _ := os.ReadFile(oldPHPPool)
		engine := NewTemplateEngine(cfg.Panel.BackupDir)
		phpData := &PHPFPMPoolData{
			Domain:     newDomain,
			PoolName:   poolName,
			SystemUser: site.SystemUser,
			WebRoot:    newWebRoot,
			SocketPath: cfg.Paths.PHPFPMSock,
			SocketName: poolName,
		}
		phpConfig, err := engine.RenderPHPFPMPool(phpData)
		if err != nil {
			rollback()
			log.Printf("Failed to render PHP-FPM config: %v", err)
			return taskFailure("Failed to render PHP-FPM config", err)
		}
		if err := os.Rename(oldWebRoot, newWebRoot); err != nil {
			rollback()
			log.Printf("Failed to rename website directory: %v", err)
			return TaskResult{Success: false, Message: "Failed to rename website directory"}
		}
		rollbacks = append(rollbacks, rollbackStep{"Restore website directory " + oldWebRoot, func() error {
			return os.Rename(newWebRoot, oldWebRoot)
		}})

		logDirMoved := true
		if err := moveSiteLogDir(oldLogDir, newLogDir); err != nil {
			logDirMoved = false
			log.Printf("Failed to rename log directory, falling back to creating new log directory: %v", err)
			if createErr := createSiteLogDir(newLogDir); createErr != nil {
				rollback()
				log.Printf("Failed to create new log directory: %v", createErr)
				return TaskResult{Success: false, Message: "Failed to create new log directory"}
			}
		}
		if logDirMoved {
			rollbacks = append(rollbacks, rollbackStep{"Restore log directory " + oldLogDir, func() error {
				return os.Rename(newLogDir, oldLogDir)
			}})
		} else {
			rollbacks = append(rollbacks, rollbackStep{"Delete new log directory " + newLogDir, func() error {
				_ = os.Remove(filepath.Join(newLogDir, "access.log"))
				_ = os.Remove(filepath.Join(newLogDir, "error.log"))
				return os.Remove(newLogDir)
			}})
		}

		if err := engine.ApplyPHPFPMPool(phpConfig, newPHPPool, newLogDir); err != nil {
			rollback()
			log.Printf("Failed to apply PHP-FPM config: %v", err)
			return taskFailure("Failed to apply PHP-FPM config", err)
		}
		phpRB := rollbackStep{"Restore PHP-FPM Pool " + oldPHPPool, func() error {
			os.Remove(newPHPPool)
			os.WriteFile(oldPHPPool, oldPoolContent, 0644)
			exec.Command("systemctl", "reload", "php8.3-fpm").Run()
			return nil
		}}
		rollbacks = append(rollbacks, phpRB)

		if _, err := os.Stat(oldCertDir); err == nil {
			if err := os.Rename(oldCertDir, newCertDir); err != nil {
				rollback()
				log.Printf("Failed to rename SSL certificate directory: %v", err)
				return TaskResult{Success: false, Message: "Failed to rename SSL certificate directory"}
			}
			certRB := rollbackStep{"Restore SSL certificate directory", func() error {
				return os.Rename(newCertDir, oldCertDir)
			}}
			rollbacks = append(rollbacks, certRB)
		}

		site.WebRoot = newWebRoot
		site.LogDir = newLogDir
		site.NginxConfPath = newNginxConf

		site.PHPPoolPath = newPHPPool
		if site.SSLCertPath != "" {
			site.SSLCertPath = filepath.Join(newCertDir, "fullchain.pem")
			site.SSLKeyPath = filepath.Join(newCertDir, "privkey.pem")
		}

		aliasStr := strings.Join(newAliases, "\n")
		site.Domain = newDomain
		site.Aliases = aliasStr

		nginxData, err := nginxDataFromSiteChecked(site)
		if err != nil {
			rollback()
			return taskFailure("CDN real IP config is invalid", err)
		}

		nginxConfig, err := engine.RenderNginxConfig(nginxData)
		if err != nil {
			rollback()
			log.Printf("Failed to render Nginx config: %v", err)
			return taskFailure("Failed to render Nginx config", err)
		}

		if err := engine.ApplyNginxConfig(nginxConfig, newNginxConf, newEnabledLink); err != nil {
			rollback()
			log.Printf("Failed to apply Nginx config: %v", err)
			return taskFailure("Failed to apply Nginx config", err)
		}

		db := database.GetDB()
		_, err = db.Exec(`UPDATE websites SET domain = ?, aliases = ?, web_root = ?, log_dir = ?,
			nginx_conf_path = ?, php_pool_path = ?, ssl_cert_path = ?, ssl_key_path = ?,
			updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
			newDomain, aliasStr, newWebRoot, newLogDir,
			newNginxConf, newPHPPool, site.SSLCertPath, site.SSLKeyPath, site.ID)
		if err != nil {
			rollback()
			log.Printf("Failed to update database: %v", err)
			return TaskResult{Success: false, Message: "Failed to update database"}
		}

		msg := fmt.Sprintf("Primary domain changed from %s to %s", oldDomain, newDomain)
		if err := WriteSiteLogrotateConfig(oldDomain, oldLogDir, 0); err != nil {
			log.Printf("old site logrotate config cleanup skipped after domain update: %v", err)
		}
		if err := WriteSiteLogrotateConfig(newDomain, newLogDir, site.LogRetentionDays); err != nil {
			log.Printf("site logrotate config skipped after domain update: %v", err)
		}

		if site.SSLEnabled {
			msg += ". Please re-obtain SSL certificate to match the new domain"
		}
		return TaskResult{Success: true, Message: msg}
	}

	aliasStr := strings.Join(newAliases, "\n")
	site.Aliases = aliasStr

	engine := NewTemplateEngine(cfg.Panel.BackupDir)
	nginxData, err := nginxDataFromSiteChecked(site)
	if err != nil {
		return taskFailure("CDN real IP config is invalid", err)
	}

	nginxConfig, err := engine.RenderNginxConfig(nginxData)
	if err != nil {
		log.Printf("Failed to render Nginx config: %v", err)
		return taskFailure("Failed to render Nginx config", err)
	}

	if err := engine.ApplyNginxConfig(nginxConfig, site.NginxConfPath,
		nginxEnabledPath(cfg, site.NginxConfPath, newDomain)); err != nil {
		log.Printf("Failed to apply Nginx config: %v", err)
		return taskFailure("Failed to apply Nginx config", err)
	}

	db := database.GetDB()
	_, err = db.Exec(`UPDATE websites SET aliases = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, aliasStr, site.ID)
	if err != nil {
		log.Printf("Failed to update database: %v", err)
		return TaskResult{Success: false, Message: "Failed to update database"}
	}

	msg := "Aliases updated"
	if site.SSLEnabled {
		msg += ". If new aliases were added, please re-obtain SSL certificate to cover the new domains"
	}

	return TaskResult{Success: true, Message: msg}
}

func executeUnbanIP(task *Task) TaskResult {
	return TaskResult{Success: true, Message: "IP unbanning is not yet implemented"}
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func ReinstallWordPress(packagePath, webRoot, dbName, dbUser, systemUser string, cfg *config.Config,
	cleanDefaults, removeThemes bool, installThemes, installPlugins []string) error {
	webRoot, err := managedSubpath(cfg.Paths.WWWRoot, webRoot, "website directory")
	if err != nil {
		return err
	}

	tmpDir := "/tmp/wp_reinstall_" + dbName + "_" + generatePassword(8)
	tmpWebRoot := filepath.Join(filepath.Dir(webRoot), "."+filepath.Base(webRoot)+".reinstall_"+generatePassword(8))
	os.RemoveAll(tmpWebRoot)
	defer os.RemoveAll(tmpWebRoot)

	if err := os.MkdirAll(tmpWebRoot, 0755); err != nil {
		return fmt.Errorf("failed to create temporary website directory: %w", err)
	}
	if err := deployWordPress(packagePath, tmpWebRoot, tmpDir); err != nil {
		return fmt.Errorf("WordPress deployment failed: %w", err)
	}

	if err := dropMariaDBDatabase(dbName, dbUser, cfg); err != nil {
		return fmt.Errorf("failed to delete old database: %w", err)
	}

	dbPassword := generatePassword(24)
	if err := createMariaDBDatabase(dbName, dbUser, dbPassword, cfg); err != nil {
		return fmt.Errorf("failed to rebuild database: %w", err)
	}

	if err := generateWPConfig(tmpWebRoot, filepath.Base(webRoot), dbName, dbUser, dbPassword); err != nil {
		return fmt.Errorf("failed to generate wp-config.php: %w", err)
	}

	if _, err := executeCommand("chown", "-R", siteOwner(systemUser), tmpWebRoot); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to set temporary directory permissions: %v\n", err)
	}

	if err := os.RemoveAll(webRoot); err != nil {
		return fmt.Errorf("failed to clean up old website directory: %w", err)
	}
	if err := os.Rename(tmpWebRoot, webRoot); err != nil {
		return fmt.Errorf("failed to replace website directory: %w", err)
	}
	if err := HardenSiteSensitivePermissions(filepath.Base(webRoot), webRoot, systemUser); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to set security permissions: %v\n", err)
	}

	if cleanDefaults {
		removeDefaultPlugins(webRoot)
	}
	if removeThemes {
		removeUnusedThemes(webRoot)
	}
	if len(installThemes) > 0 || len(installPlugins) > 0 {
		installExtensions(webRoot, systemUser, installThemes, installPlugins)
	}

	return nil
}
