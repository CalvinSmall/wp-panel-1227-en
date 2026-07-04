package executor

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/naibabiji/wp-panel/config"
	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/models"
)

const cacheConfPath = "/etc/nginx/conf.d/wppanel-cache.conf"

func EnsureFastCGICacheConfig() {
	os.MkdirAll("/var/cache/nginx/fastcgi", 0755)
	content := `# WP Panel — FastCGI Cache
fastcgi_cache_path /var/cache/nginx/fastcgi levels=1:2 keys_zone=WP_CACHE:200m inactive=60m max_size=2g;
`
	os.WriteFile(cacheConfPath, []byte(content), 0644)
}

func EnsureCacheHelperPlugin(pluginFS embed.FS) {
	pkgDir := "/www/server/panel/packages"
	os.MkdirAll(pkgDir, 0755)
	dst := filepath.Join(pkgDir, "wp-panel-optimizer.php")

	data, err := pluginFS.ReadFile("wp-panel-optimizer/wp-panel-optimizer.php")
	if err != nil {
		return
	}

	// Only write to file when content changes to avoid rewriting on every startup which updates modification time
	if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, data) {
		return
	}

	os.WriteFile(dst, data, 0644)
}

// AutoDeployPluginUpdates scans all WordPress sites with companion plugins installed,
// and if plugin_api_key is non-empty and the site plugin version is behind the built-in version (determined by content/modification time comparison), automatically updates.
// Called on each panel startup for seamless plugin auto-upgrade.
func AutoDeployPluginUpdates(pluginFS embed.FS) {
	srcData, err := pluginFS.ReadFile("wp-panel-optimizer/wp-panel-optimizer.php")
	if err != nil {
		return
	}
	srcPath := "/www/server/panel/packages/wp-panel-optimizer.php"
	srcInfo, srcErr := os.Stat(srcPath)
	if srcErr != nil {
		return
	}

	db := database.GetDB()
	rows, err := db.Query(`SELECT id, web_root, system_user, domain, file_lock_enabled FROM websites
		WHERE site_type = 'wordpress' AND plugin_api_key != ''`)
	if err != nil {
		return
	}
	defer rows.Close()

	var updated int
	for rows.Next() {
		var id, fileLockEnabled int
		var webRoot, systemUser, domain string
		if err := rows.Scan(&id, &webRoot, &systemUser, &domain, &fileLockEnabled); err != nil {
			continue
		}
		if fileLockEnabled == 1 {
			continue
		}

		pluginDir := filepath.Join(webRoot, "wp-content", "plugins", "wp-panel-optimizer")
		dstPath := filepath.Join(pluginDir, "wp-panel-optimizer.php")

		// Prefer content comparison; skip if content matches — safest and avoids unnecessary system permission calls (chown/chmod)
		if dstData, err := os.ReadFile(dstPath); err == nil && bytes.Equal(dstData, srcData) {
			continue
		}

		// Fallback to modification time comparison (in case other logic depends on it)
		if dstInfo, err := os.Stat(dstPath); err == nil && !dstInfo.ModTime().Before(srcInfo.ModTime()) {
			continue
		}

		if err := os.MkdirAll(pluginDir, 0755); err != nil {
			log.Printf("[Plugin Auto-Update] Failed to create directory site=%d: %v", id, err)
			continue
		}
		if err := os.WriteFile(dstPath, srcData, 0644); err != nil {
			log.Printf("[Plugin Auto-Update] Write failed site=%d: %v", id, err)
			continue
		}
		InstallPluginPermissions(domain, systemUser, pluginDir)
		updated++
	}
	if updated > 0 {
		log.Printf("[Plugin Auto-Update] Updated companion plugins for %d sites", updated)
	}
}

func NewCacheKey() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func NewAPIKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func ClearSiteCache(siteID int) {
	db := database.GetDB()
	key := NewCacheKey()
	db.Exec("UPDATE websites SET fastcgi_cache_key = ? WHERE id = ?", key, siteID)
	if err := RegenerateSiteNginx(siteID); err != nil {
		log.Printf("Failed to refresh site Nginx config site=%d: %v", siteID, err)
	}
}

func ClearWPSiteRuntimeCaches(siteID int, domain, webRoot string) {
	ClearSiteCache(siteID)
	if err := ClearWPRedisObjectCache(domain, webRoot); err != nil {
		log.Printf("Failed to clear Redis Object Cache domain=%s: %v", domain, err)
	}
}

func ClearWPRedisObjectCache(domain, webRoot string) error {
	prefixes := redisObjectCachePrefixes(domain, webRoot)
	for _, prefix := range prefixes {
		if err := deleteRedisKeysByPrefix(prefix); err != nil {
			return err
		}
	}
	return nil
}

func redisObjectCachePrefixes(domain, webRoot string) []string {
	seen := make(map[string]bool)
	var prefixes []string
	add := func(prefix string) {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" || seen[prefix] {
			return
		}
		seen[prefix] = true
		prefixes = append(prefixes, prefix)
	}

	if strings.TrimSpace(webRoot) != "" {
		if data, err := os.ReadFile(filepath.Join(webRoot, "wp-config.php")); err == nil {
			content := string(data)
			add(extractWPConfigStringConstant(content, "WP_REDIS_PREFIX"))
			add(extractWPConfigStringConstant(content, "WP_CACHE_KEY_SALT"))
		}
	}
	add(wpCacheKeySalt(domain))
	return prefixes
}

func extractWPConfigStringConstant(content, name string) string {
	re := regexp.MustCompile(`(?m)^\s*define\s*\(\s*['"]` + regexp.QuoteMeta(name) + `['"]\s*,\s*['"]([^'"]*)['"]\s*\)\s*;`)
	matches := re.FindStringSubmatch(content)
	if len(matches) != 2 {
		return ""
	}
	return matches[1]
}

func deleteRedisKeysByPrefix(prefix string) error {
	keys, err := exec.Command("redis-cli", "--scan", "--pattern", prefix+"*").Output()
	if err != nil {
		return err
	}

	var batch []string
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		args := append([]string{"DEL"}, batch...)
		batch = nil
		return exec.Command("redis-cli", args...).Run()
	}
	for _, key := range strings.Split(string(keys), "\n") {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		batch = append(batch, key)
		if len(batch) >= 200 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func RegenerateSiteNginx(siteID int) error {
	db := database.GetDB()
	var domain, aliases, siteType, systemUser, webRoot, documentRootSubdir, logDir, accessLogMode, cacheKey, templateVer string
	var phpPoolPath, nginxConfPath string
	var sslEnabled, fCacheEnabled, xmlrpcEnabled, cdnRealIPEnabled int
	var fCacheTTL int
	var sslCertPath, sslKeyPath string

	err := db.QueryRow(
		`SELECT domain, aliases, site_type, system_user, web_root, document_root_subdir, log_dir, ssl_enabled,
		        access_log_mode, fastcgi_cache_enabled, fastcgi_cache_ttl, fastcgi_cache_key,
		        ssl_cert_path, ssl_key_path, template_version, xmlrpc_enabled, php_pool_path, nginx_conf_path, cdn_realip_enabled
		 FROM websites WHERE id = ?`, siteID,
	).Scan(&domain, &aliases, &siteType, &systemUser, &webRoot, &documentRootSubdir, &logDir, &sslEnabled, &accessLogMode, &fCacheEnabled, &fCacheTTL, &cacheKey, &sslCertPath, &sslKeyPath, &templateVer, &xmlrpcEnabled, &phpPoolPath, &nginxConfPath, &cdnRealIPEnabled)
	if err != nil || domain == "" {
		if err != nil {
			return fmt.Errorf("Failed to query site (site %d): %w", siteID, err)
		}
		return fmt.Errorf("Site domain is empty (site %d)", siteID)
	}

	if templateVer == "" {
		templateVer = "v1.0"
	}
	if cacheKey == "" {
		cacheKey = NewCacheKey()
		db.Exec("UPDATE websites SET fastcgi_cache_key = ? WHERE id = ?", cacheKey, siteID)
	}

	cfg := config.AppConfig
	engine := NewTemplateEngine(cfg.Panel.BackupDir)

	var aliasList []string
	if aliases != "" {
		aliasList = strings.Split(aliases, "\n")
	}

	data := &NginxSiteData{
		Domain:        domain,
		Aliases:       aliasList,
		ServerNames:   buildServerNames(domain, aliasList),
		WebRoot:       EffectiveDocumentRoot(webRoot, siteType, documentRootSubdir),
		LogDir:        logDir,
		SystemUser:    systemUser,
		SiteType:      siteType,
		PHPProxy:      "unix:" + phpSocketPath(cfg, phpPoolPath, domain),
		TemplateVer:   templateVer,
		AccessLogMode: accessLogMode,
		UseSSL:        sslEnabled == 1,
		FCacheEnabled: fCacheEnabled == 1,
		FCacheTTL:     fCacheTTL,
		FCacheKey:     cacheKey,
		XMLRPCEnabled: xmlrpcEnabled == 1,
	}
	if cdnRealIPEnabled == 1 {
		groups, _ := GetWebsiteCDNRealIPGroups(siteID)
		runtime, err := ResolveCDNRealIPRuntime(&models.Website{ID: siteID, CDNRealIPEnabled: true, CDNRealIPGroups: groups})
		if err != nil {
			return fmt.Errorf("CDN Real IP config invalid (site %d): %w", siteID, err)
		}
		if runtime.Enabled {
			data.CDNRealIPEnabled = true
			data.CDNRealIPHeader = runtime.HeaderName
			data.CDNRealIPRanges = runtime.IPRanges
			data.CDNRealIPCompat = runtime.Compatible
		}
	}
	if data.UseSSL {
		data.SSLCertPath = sslCertPath
		data.SSLKeyPath = sslKeyPath
	}

	config, err := engine.RenderNginxConfig(data)
	if err != nil {
		return fmt.Errorf("Failed to render Nginx config (site %d): %w", siteID, err)
	}

	if err := engine.ApplyNginxConfig(config, nginxConfPath, nginxEnabledPath(cfg, nginxConfPath, domain)); err != nil {
		return fmt.Errorf("Failed to apply Nginx config (site %d): %w", siteID, err)
	}
	return nil
}

// RegenerateAllSitesNginx rebuilds Nginx config for all sites, used for batch refresh after template updates.
func RegenerateAllSitesNginx() error {
	db := database.GetDB()
	rows, err := db.Query("SELECT id FROM websites")
	if err != nil {
		log.Printf("[Nginx-Rebuild] Failed to query site list: %v", err)
		return err
	}
	defer rows.Close()

	var failures []string
	for rows.Next() {
		var siteID int
		if err := rows.Scan(&siteID); err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if err := RegenerateSiteNginx(siteID); err != nil {
			log.Printf("[Nginx-Rebuild] Site %d update failed: %v", siteID, err)
			failures = append(failures, err.Error())
		}
	}
	if err := rows.Err(); err != nil {
		failures = append(failures, err.Error())
	}
	if len(failures) > 0 {
		return fmt.Errorf("Failed to update Nginx config for some sites: %s", strings.Join(failures, "; "))
	}
	log.Printf("[Nginx-Rebuild] Nginx config updated for all sites")
	return nil
}

// RegenerateAllSitesFPM rebuilds PHP-FPM pool config for all sites,
// used for batch refresh of old sites after open_basedir and other template changes.
func RegenerateAllSitesFPM() error {
	db := database.GetDB()
	rows, err := db.Query("SELECT id, domain, system_user, web_root, log_dir, php_pool_path FROM websites")
	if err != nil {
		log.Printf("[FPM-Rebuild] Failed to query site list: %v", err)
		return err
	}
	defer rows.Close()

	cfg := config.AppConfig
	engine := NewTemplateEngine(cfg.Panel.BackupDir)
	var failures []string

	for rows.Next() {
		var siteID int
		var domain, systemUser, webRoot, logDir, phpPoolPath string
		if err := rows.Scan(&siteID, &domain, &systemUser, &webRoot, &logDir, &phpPoolPath); err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if err := ensureSitePrimaryGroup(systemUser); err != nil {
			log.Printf("[FPM-Rebuild] %s: Site user group check failed: %v", domain, err)
			failures = append(failures, fmt.Sprintf("%s: %v", domain, err))
			continue
		}

		poolName := phpPoolName(phpPoolPath, domain)
		phpData := &PHPFPMPoolData{
			Domain:     domain,
			PoolName:   poolName,
			SystemUser: systemUser,
			WebRoot:    webRoot,
			SocketPath: cfg.Paths.PHPFPMSock,
			SocketName: poolName,
		}
		phpConfig, err := engine.RenderPHPFPMPool(phpData)
		if err != nil {
			log.Printf("[FPM-Rebuild] %s: Failed to render config: %v", domain, err)
			failures = append(failures, fmt.Sprintf("%s: %v", domain, err))
			continue
		}

		if err := engine.ApplyPHPFPMPool(phpConfig, phpPoolPath, logDir); err != nil {
			log.Printf("[FPM-Rebuild] %s: Failed to apply config: %v", domain, err)
			failures = append(failures, fmt.Sprintf("%s: %v", domain, err))
			continue
		}
	}
	log.Printf("[FPM-Rebuild] PHP-FPM pool config updated for all sites")
	if err := rows.Err(); err != nil {
		return err
	}
	if len(failures) > 0 {
		return fmt.Errorf("Failed to rebuild PHP-FPM Pool for some sites: %s", strings.Join(failures, "; "))
	}
	return nil
}
