package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/naibabiji/wp-panel/config"
	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/executor"
	"github.com/naibabiji/wp-panel/models"

	"github.com/gin-gonic/gin"
)

// canonical column list shared by all website queries.
const websiteCols = `id, name, domain, aliases, status, system_user, web_root, document_root_subdir, log_dir,
	db_name, db_user, php_pool_path, nginx_conf_path, site_type, ssl_enabled,
	ssl_cert_path, ssl_key_path, ssl_expires_at, ssl_last_error, ssl_export_enabled, template_version, access_log_mode,
	fastcgi_cache_enabled, fastcgi_cache_ttl, fastcgi_cache_key,
	monitoring_enabled, monitoring_interval, disable_wp_updates, disable_file_editing,
		xmlrpc_enabled, wp_debug_enabled, wp_post_revisions, wp_memory_limit,
		file_lock_enabled, log_retention_days, cdn_realip_enabled, expires_at, created_at, updated_at`

const fileLockBlockedMessage = "File lock is enabled for this site. Please disable file lock before performing this maintenance operation"

type siteLogFileInfo struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	Current    bool      `json:"current"`
	Compressed bool      `json:"compressed"`
}

// scanWebsite scans the canonical columns into a Website model.
// scanner is either row.Scan (for QueryRow) or rows.Scan (for Rows).
func scanWebsite(scanner func(dest ...interface{}) error) (*models.Website, error) {
	var w models.Website
	var aliases, status string
	var sslEnabled, sslExportEnabled, fCacheEnabled, monitoringEnabled int
	var monitoringInterval int
	var disableWPUpdates, disableFileEditing, xmlrpcEnabled int
	var wpDebugEnabled int
	var wpPostRevisions int
	var wpMemoryLimit string
	var fileLockEnabled int
	var logRetentionDays int
	var cdnRealIPEnabled int

	err := scanner(
		&w.ID, &w.Name, &w.Domain, &aliases, &status, &w.SystemUser,
		&w.WebRoot, &w.DocumentRootSubdir, &w.LogDir, &w.DBName, &w.DBUser, &w.PHPPoolPath,
		&w.NginxConfPath, &w.SiteType, &sslEnabled, &w.SSLCertPath, &w.SSLKeyPath,
		&w.SSLExpiresAt, &w.SSLLastError, &sslExportEnabled, &w.TemplateVersion, &w.AccessLogMode,
		&fCacheEnabled, &w.FCacheTTL, &w.FCacheKey,
		&monitoringEnabled, &monitoringInterval, &disableWPUpdates, &disableFileEditing,
		&xmlrpcEnabled, &wpDebugEnabled, &wpPostRevisions, &wpMemoryLimit,
		&fileLockEnabled, &logRetentionDays, &cdnRealIPEnabled, &w.ExpiresAt,
		&w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	w.Aliases = aliases
	w.Status = models.WebsiteStatus(status)
	w.SSLEnabled = sslEnabled == 1
	w.SSLExportEnabled = sslExportEnabled == 1
	w.FCacheEnabled = fCacheEnabled == 1
	w.MonitoringEnabled = monitoringEnabled == 1
	w.MonitoringInterval = monitoringInterval
	w.DisableWPUpdates = disableWPUpdates == 1
	w.DisableFileEditing = disableFileEditing == 1
	w.XMLRPCEnabled = xmlrpcEnabled == 1
	w.WPDebugEnabled = wpDebugEnabled == 1
	w.WPPostRevisions = wpPostRevisions
	w.WPMemoryLimit = wpMemoryLimit
	w.FileLockEnabled = fileLockEnabled == 1
	w.LogRetentionDays = logRetentionDays
	w.CDNRealIPEnabled = cdnRealIPEnabled == 1
	return &w, nil
}

type WebsiteHandler struct {
	DB *sql.DB
}

type sslPreflightDomain struct {
	Domain      string   `json:"domain"`
	Addresses   []string `json:"addresses"`
	Matched     bool     `json:"matched"`
	HasIPv6     bool     `json:"has_ipv6"`
	MatchedIPv6 bool     `json:"matched_ipv6"`
}

type sslPreflightResult struct {
	OK           bool                 `json:"ok"`
	Warnings     []string             `json:"warnings"`
	HardWarnings []string             `json:"hard_warnings"`
	Domains      []sslPreflightDomain `json:"domains"`
}

func normalizeWPSiteURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("URL must start with http:// or https://")
	}
	return value, nil
}

func localInterfaceIPs() map[string]bool {
	result := map[string]bool{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return result
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			result[ip.String()] = true
		}
	}
	return result
}

func uniqueRequestDomains(domain string, aliases []string) []string {
	seen := map[string]bool{}
	var domains []string
	for _, raw := range append([]string{domain}, aliases...) {
		d := strings.ToLower(strings.TrimSpace(raw))
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		domains = append(domains, d)
	}
	return domains
}

func runSSLPreflight(ctx context.Context, domain string, aliases []string) (sslPreflightResult, error) {
	domains := uniqueRequestDomains(domain, aliases)
	if len(domains) == 0 {
		return sslPreflightResult{}, fmt.Errorf("domain cannot be empty")
	}
	for _, domain := range domains {
		if !executor.IsValidDomain(domain) {
			return sslPreflightResult{}, fmt.Errorf("invalid domain format: %s", domain)
		}
	}

	localIPs := localInterfaceIPs()
	result := sslPreflightResult{}
	for _, domain := range domains {
		records, err := net.DefaultResolver.LookupIPAddr(ctx, domain)
		if err != nil || len(records) == 0 {
			msg := domain + " does not resolve to A/AAAA records. Let's Encrypt cannot access the verification file."
			if ctx.Err() != nil {
				msg = domain + " DNS resolution timed out. Please try again later or check your DNS service."
			}
			result.HardWarnings = append(result.HardWarnings, msg)
			result.Domains = append(result.Domains, sslPreflightDomain{Domain: domain})
			continue
		}

		item := sslPreflightDomain{Domain: domain}
		for _, record := range records {
			ip := record.IP
			ipText := ip.String()
			item.Addresses = append(item.Addresses, ipText)
			if ip.To4() == nil {
				item.HasIPv6 = true
			}
			if localIPs[ipText] {
				item.Matched = true
				if ip.To4() == nil {
					item.MatchedIPv6 = true
				}
			}
		}
		if !item.Matched {
			result.Warnings = append(result.Warnings, domain+" does not resolve to this server's network interface IP. If using CDN, please confirm the CDN is correctly pulling from the origin server and is not caching, rewriting, or intercepting /.well-known/acme-challenge/.")
		}
		if item.HasIPv6 && !item.MatchedIPv6 {
			result.Warnings = append(result.Warnings, domain+" has AAAA records but they do not match this server's IPv6 address. Let's Encrypt may access IPv6 and cause verification 404. Please remove incorrect AAAA records or configure the correct IPv6 address.")
		}
		result.Domains = append(result.Domains, item)
	}
	result.OK = len(result.Warnings) == 0 && len(result.HardWarnings) == 0
	return result, nil
}

func (h *WebsiteHandler) SSLPreflight(c *gin.Context) {
	var req models.CreateWebsiteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	result, err := runSSLPreflight(ctx, req.Domain, req.Aliases)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(result))
}

func (h *WebsiteHandler) List(c *gin.Context) {
	db := database.GetDB()
	rows, err := db.Query("SELECT " + websiteCols + " FROM websites ORDER BY created_at DESC")
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Query failed"))
		return
	}
	defer rows.Close()

	var websites []models.Website
	for rows.Next() {
		w, err := scanWebsite(rows.Scan)
		if err != nil {
			continue
		}
		websites = append(websites, *w)
	}
	if websites == nil {
		websites = []models.Website{}
	}

	type siteRow struct {
		models.Website
		AccessLogEnabled bool   `json:"access_log_enabled"`
		AccessLogMode    string `json:"access_log_mode"`
		FCacheEnabled    bool   `json:"fastcgi_cache_enabled"`
		BackupEnabled    bool   `json:"backup_enabled"`
	}
	result := make([]siteRow, len(websites))
	for i, w := range websites {
		result[i] = siteRow{
			Website:          w,
			AccessLogMode:    w.AccessLogMode,
			FCacheEnabled:    w.FCacheEnabled,
			AccessLogEnabled: w.AccessLogMode != "off",
		}
		var be int
		db.QueryRow("SELECT enabled FROM backup_settings WHERE site_id = ?", w.ID).Scan(&be)
		result[i].BackupEnabled = be == 1
	}

	c.JSON(http.StatusOK, models.SuccessResponse(result))
}

func (h *WebsiteHandler) Get(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	w, err := scanWebsite(database.GetDB().QueryRow(
		"SELECT "+websiteCols+" FROM websites WHERE id = ?", id,
	).Scan)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}
	if w.SiteType == "wordpress" {
		if prefix, err := executor.ReadWPTablePrefix(w.WebRoot); err == nil {
			w.TablePrefix = prefix
		}
	}
	executor.LoadWebsiteCDNRealIPGroups(w)

	c.JSON(http.StatusOK, models.SuccessResponse(w))
}

func isAliasConflicting(alias string, excludeID int) (bool, string) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" {
		return false, ""
	}
	rows, err := database.GetDB().Query(
		"SELECT domain, aliases FROM websites WHERE id != ?", excludeID)
	if err != nil {
		return false, ""
	}
	defer rows.Close()
	for rows.Next() {
		var domain, aliases string
		rows.Scan(&domain, &aliases)
		if alias == strings.ToLower(domain) {
			return true, domain
		}
		for _, a := range strings.Split(aliases, "\n") {
			if alias == strings.ToLower(strings.TrimSpace(a)) {
				return true, domain
			}
		}
	}
	return false, ""
}

func (h *WebsiteHandler) Create(c *gin.Context) {
	var req models.CreateWebsiteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	if strings.TrimSpace(req.Domain) == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("domain cannot be empty"))
		return
	}
	if conflict, target := isAliasConflicting(req.Domain, 0); conflict {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Domain "+req.Domain+" is already in use by site "+target))
		return
	}

	for _, alias := range req.Aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if alias == req.Domain {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("Alias cannot be the same as the primary domain"))
			return
		}
		if conflict, target := isAliasConflicting(alias, 0); conflict {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("Alias "+alias+" is already in use by site "+target))
			return
		}
	}

	siteType := req.SiteType
	if siteType != "php" {
		siteType = "wordpress"
	}
	documentRootSubdir, err := executor.NormalizeDocumentRootSubdir(siteType, req.DocumentRootSubdir)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
		return
	}
	if req.SSLEnabled {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		preflight, preflightErr := runSSLPreflight(ctx, req.Domain, req.Aliases)
		cancel()
		if preflightErr != nil {
			log.Printf("SSL preflight skipped domain=%s: %v", req.Domain, preflightErr)
		} else if !preflight.OK {
			log.Printf("SSL preflight risk domain=%s hard=%v warnings=%v", req.Domain, preflight.HardWarnings, preflight.Warnings)
		}
	}

	payload := &executor.CreateSitePayload{
		Domain:             req.Domain,
		Aliases:            req.Aliases,
		SSLEnabled:         req.SSLEnabled,
		DBPassword:         req.DBPassword,
		ExpiresAt:          req.ExpiresAt,
		SiteType:           siteType,
		DocumentRootSubdir: documentRootSubdir,
		CleanDefaults:      req.CleanDefaults,
		RemoveUnusedThemes: req.RemoveUnusedThemes,
		InstallThemes:      req.InstallThemes,
		InstallPlugins:     req.InstallPlugins,
	}

	task := executor.GlobalQueue.Enqueue(executor.TaskCreateSite, payload)
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(result.Data))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *WebsiteHandler) SetDocumentRoot(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}
	if site.SiteType != "php" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Only generic PHP websites support changing the web root directory"))
		return
	}

	var req struct {
		DocumentRootSubdir string `json:"document_root_subdir"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	documentRootSubdir, err := executor.NormalizeDocumentRootSubdir(site.SiteType, req.DocumentRootSubdir)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
		return
	}

	task := executor.GlobalQueue.Enqueue(executor.TaskSetDocumentRoot, &executor.SetDocumentRootPayload{
		Site: site, DocumentRootSubdir: documentRootSubdir,
	})
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": result.Message}))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *WebsiteHandler) Delete(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	payload := &executor.DeleteSitePayload{Site: site}
	task := executor.GlobalQueue.Enqueue(executor.TaskDeleteSite, payload)
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": result.Message}))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *WebsiteHandler) ToggleStatus(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	var req models.UpdateWebsiteStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	var taskType executor.TaskType
	switch req.Action {
	case "pause":
		taskType = executor.TaskPauseSite
	case "enable":
		taskType = executor.TaskEnableSite
	default:
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid operation"))
		return
	}

	var payload interface{}
	if taskType == executor.TaskPauseSite {
		payload = &executor.PauseSitePayload{Site: site}
	} else {
		payload = &executor.EnableSitePayload{Site: site}
	}

	task := executor.GlobalQueue.Enqueue(taskType, payload)
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": result.Message}))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *WebsiteHandler) EnableSSL(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	var req struct {
		Mode        string `json:"mode" binding:"required,oneof=auto manual"`
		Certificate string `json:"certificate"`
		PrivateKey  string `json:"private_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	if req.Mode == "manual" && (strings.TrimSpace(req.Certificate) == "" || strings.TrimSpace(req.PrivateKey) == "") {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Manual mode requires certificate content and private key"))
		return
	}

	task := executor.GlobalQueue.Enqueue(executor.TaskEnableSSL, &executor.EnableSSLPayload{
		Site: site, Mode: req.Mode, Certificate: req.Certificate, PrivateKey: req.PrivateKey,
	})
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": result.Message}))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *WebsiteHandler) RemoveSSL(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	if !site.SSLEnabled {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("SSL is not enabled for this site"))
		return
	}

	task := executor.GlobalQueue.Enqueue(executor.TaskRemoveSSL, &executor.RemoveSSLPayload{Site: site})
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": result.Message}))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *WebsiteHandler) DownloadSSLPackage(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}
	if !site.SSLEnabled {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("SSL is not enabled for this site"))
		return
	}

	zipData, filename, err := buildSSLCertificatePackage(site)
	if err != nil {
		c.JSON(sslDownloadStatus(err), models.ErrorResponse(err.Error()))
		return
	}

	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Data(http.StatusOK, "application/zip", zipData)
}

func (h *WebsiteHandler) SetSSLExport(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	enabled := 0
	if req.Enabled {
		enabled = 1
	}
	if _, err := database.GetDB().Exec(
		`UPDATE websites SET ssl_export_enabled = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		enabled, site.ID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to save SSL certificate export permission"))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "SSL certificate export permission saved"}))
}

type sslDownloadError struct {
	status  int
	message string
}

func (e sslDownloadError) Error() string {
	return e.message
}

func sslDownloadStatus(err error) int {
	if e, ok := err.(sslDownloadError); ok {
		return e.status
	}
	return http.StatusInternalServerError
}

func newSSLDownloadError(status int, message string) error {
	return sslDownloadError{status: status, message: message}
}

func buildSSLCertificatePackage(site *models.Website) ([]byte, string, error) {
	if site == nil {
		return nil, "", newSSLDownloadError(http.StatusNotFound, "Website not found")
	}
	if config.AppConfig == nil || strings.TrimSpace(config.AppConfig.Paths.Certificates) == "" {
		return nil, "", fmt.Errorf("Certificate directory not configured")
	}
	if !executor.IsValidDomain(site.Domain) {
		return nil, "", newSSLDownloadError(http.StatusBadRequest, "invalid website domain format")
	}

	certDir := filepath.Join(config.AppConfig.Paths.Certificates, site.Domain)
	certPath := filepath.Join(certDir, "fullchain.pem")
	keyPath := filepath.Join(certDir, "privkey.pem")
	if !sslPathWithin(config.AppConfig.Paths.Certificates, certPath) ||
		!sslPathWithin(config.AppConfig.Paths.Certificates, keyPath) {
		return nil, "", newSSLDownloadError(http.StatusForbidden, "Invalid certificate path")
	}

	certData, err := readSSLDownloadFile(certPath)
	if err != nil {
		return nil, "", err
	}
	keyData, err := readSSLDownloadFile(keyPath)
	if err != nil {
		return nil, "", err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if err := addZipFile(zw, "fullchain.pem", certData); err != nil {
		zw.Close()
		return nil, "", err
	}
	if err := addZipFile(zw, "privkey.pem", keyData); err != nil {
		zw.Close()
		return nil, "", err
	}
	if err := addZipFile(zw, "README.txt", []byte(sslCertificatePackageReadme(site))); err != nil {
		zw.Close()
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}

	filename := fmt.Sprintf("%s-ssl-cert-%s.zip", site.Domain, time.Now().Format("20060102"))
	return buf.Bytes(), filename, nil
}

func readSSLDownloadFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return nil, newSSLDownloadError(http.StatusNotFound, "certificate file not found. Please reapply for a certificate")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, newSSLDownloadError(http.StatusNotFound, "certificate file not found. Please reapply for a certificate")
	}
	return data, nil
}

func sslPathWithin(basePath, targetPath string) bool {
	baseAbs, err := filepath.Abs(filepath.Clean(basePath))
	if err != nil {
		return false
	}
	targetAbs, err := filepath.Abs(filepath.Clean(targetPath))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(baseAbs, targetAbs)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func addZipFile(zw *zip.Writer, name string, data []byte) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0600)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func sslCertificatePackageReadme(site *models.Website) string {
	aliases := strings.TrimSpace(site.Aliases)
	if aliases == "" {
		aliases = "None"
	}
	expiresAt := "Unknown"
	if site.SSLExpiresAt != nil {
		expiresAt = site.SSLExpiresAt.Format("2006-01-02")
	}

	return fmt.Sprintf(`WP Panel SSL Certificate Package

Site domain: %s
Additional domains:

%s
Certificate expires: %s

File description:

- fullchain.pem：Complete certificate chain. Upload to the CDN backend's“certificate” or “public key” field.
- privkey.pem：Private key. Upload to the CDN backend's“private key” field.

When uploading to Alibaba Cloud CDN, select“Custom upload (certificate + private key)”：
- Certificate (public key): Paste the contents of fullchain.pem.
- Private key: Paste the contents of privkey.pem.

Notes:

- Private keys are sensitive information. Do not send to unauthorized personnel or upload to untrusted locations.
- The CDN does not automatically sync origin certificates. After renewal, re-download the certificate package and upload to CDN.
- If the same site has multiple CDN domains (e.g., primary domain and www domain), you need to update them separately in the CDN backend.
`, site.Domain, aliases, expiresAt)
}

func sslCertificateExportPayload(site *models.Website) (gin.H, error) {
	if site == nil {
		return nil, newSSLDownloadError(http.StatusNotFound, "Website not found")
	}
	if !site.SSLEnabled {
		return nil, newSSLDownloadError(http.StatusBadRequest, "SSL is not enabled for this site")
	}
	if config.AppConfig == nil || strings.TrimSpace(config.AppConfig.Paths.Certificates) == "" {
		return nil, fmt.Errorf("Certificate directory not configured")
	}
	if !executor.IsValidDomain(site.Domain) {
		return nil, newSSLDownloadError(http.StatusBadRequest, "invalid website domain format")
	}

	certDir := filepath.Join(config.AppConfig.Paths.Certificates, site.Domain)
	certPath := filepath.Join(certDir, "fullchain.pem")
	keyPath := filepath.Join(certDir, "privkey.pem")
	if !sslPathWithin(config.AppConfig.Paths.Certificates, certPath) ||
		!sslPathWithin(config.AppConfig.Paths.Certificates, keyPath) {
		return nil, newSSLDownloadError(http.StatusForbidden, "Invalid certificate path")
	}
	certData, err := readSSLDownloadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyData, err := readSSLDownloadFile(keyPath)
	if err != nil {
		return nil, err
	}

	aliases := []string{}
	for _, raw := range strings.Split(site.Aliases, "\n") {
		alias := strings.TrimSpace(raw)
		if alias != "" {
			aliases = append(aliases, alias)
		}
	}

	return gin.H{
		"domain":      site.Domain,
		"aliases":     aliases,
		"expires_at":  site.SSLExpiresAt,
		"certificate": string(certData),
		"private_key": string(keyData),
	}, nil
}

func (h *WebsiteHandler) UpdateDomains(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	var req struct {
		NewDomain string   `json:"new_domain"`
		Aliases   []string `json:"aliases"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	targetDomain := strings.ToLower(strings.TrimSpace(req.NewDomain))
	if targetDomain == "" {
		targetDomain = site.Domain
	}

	if targetDomain != site.Domain {
		if conflict, existing := isAliasConflicting(targetDomain, site.ID); conflict {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("Domain "+targetDomain+" is already in use by site "+existing))
			return
		}
	}

	for _, alias := range req.Aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if alias == targetDomain {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("Alias cannot be the same as the primary domain"))
			return
		}
		if conflict, target := isAliasConflicting(alias, site.ID); conflict {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("Alias "+alias+" is already in use by site "+target))
			return
		}
	}

	task := executor.GlobalQueue.Enqueue(executor.TaskUpdateDomains, &executor.UpdateDomainsPayload{
		Site: site, NewDomain: targetDomain, Aliases: req.Aliases,
	})
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": result.Message}))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *WebsiteHandler) ChangeDBPassword(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}
	if site.FileLockEnabled {
		c.JSON(http.StatusLocked, models.ErrorResponse(fileLockBlockedMessage))
		return
	}

	var req struct {
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	task := executor.GlobalQueue.Enqueue(executor.TaskChangeDBPassword, &executor.ChangeDBPasswordPayload{
		Site: site, NewPassword: req.NewPassword,
	})
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(result.Data))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *WebsiteHandler) FixWPConfig(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}
	if site.FileLockEnabled {
		c.JSON(http.StatusLocked, models.ErrorResponse(fileLockBlockedMessage))
		return
	}

	var req struct {
		TablePrefix string `json:"table_prefix"`
	}
	c.ShouldBindJSON(&req)
	req.TablePrefix = strings.TrimSpace(req.TablePrefix)
	if req.TablePrefix != "" && !executor.IsValidWPTablePrefix(req.TablePrefix) {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Table prefix can only contain letters, numbers, and underscores, and cannot exceed 56 characters"))
		return
	}

	if err := executor.FixWPConfigCredentials(site.WebRoot, site.Domain, site.DBName, site.DBUser, req.TablePrefix); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(err.Error()))
		return
	}

	msg := "wp-config.php database name and username updated"
	if req.TablePrefix != "" {
		msg = "wp-config.php database name, username, and table prefix updated"
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": msg}))
}

func (h *WebsiteHandler) DetectDBTablePrefix(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	if site.SiteType != "wordpress" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Only WordPress sites support this feature"))
		return
	}

	cfg := config.AppConfig
	prefix, candidates, err := executor.DetectDBTablePrefix(site.DBName, cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Detection failed: "+err.Error()))
		return
	}

	// If the prefix in wp-config.php exists in the candidate list, it is recommended
	if site.WebRoot != "" {
		if configPrefix, err := executor.ReadWPTablePrefix(site.WebRoot); err == nil {
			for _, c := range candidates {
				if c == configPrefix {
					prefix = configPrefix
					break
				}
			}
		}
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"prefix":     prefix,
		"candidates": candidates,
	}))
}

func (h *WebsiteHandler) GetWPSiteURLs(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	if site.SiteType != "wordpress" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Only WordPress sites support this feature"))
		return
	}

	if site.TablePrefix == "" {
		if prefix, err := executor.ReadWPTablePrefix(site.WebRoot); err == nil {
			site.TablePrefix = prefix
		}
	}
	if site.TablePrefix == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("No table prefix detected. Please sync database information first"))
		return
	}

	cfg := config.AppConfig
	siteURL, homeURL, err := executor.ReadWPSiteURLs(site.DBName, site.TablePrefix, cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Read failed: "+err.Error()))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"siteurl": siteURL,
		"home":    homeURL,
	}))
}

func (h *WebsiteHandler) UpdateWPSiteURLs(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	if site.SiteType != "wordpress" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Only WordPress sites support this feature"))
		return
	}

	var req struct {
		SiteURL string `json:"siteurl"`
		HomeURL string `json:"home"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	req.SiteURL, err = normalizeWPSiteURL(req.SiteURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid siteurl format. Please use a full URL starting with http:// or https://"))
		return
	}
	req.HomeURL, err = normalizeWPSiteURL(req.HomeURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid home format. Please use a full URL starting with http:// or https://"))
		return
	}
	if req.SiteURL == "" && req.HomeURL == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Please fill in at least one URL"))
		return
	}

	if site.TablePrefix == "" {
		if prefix, err := executor.ReadWPTablePrefix(site.WebRoot); err == nil {
			site.TablePrefix = prefix
		}
	}
	if site.TablePrefix == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("No table prefix detected. Please sync database information first"))
		return
	}

	cfg := config.AppConfig
	if err := executor.UpdateWPSiteURLs(site.DBName, site.TablePrefix, req.SiteURL, req.HomeURL, cfg); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Update failed: "+err.Error()))
		return
	}

	// Asynchronously clear FastCGI and Redis Object Cache to prevent stale cache from returning old site URLs.
	executor.GoSafe(func() { executor.ClearWPSiteRuntimeCaches(site.ID, site.Domain, site.WebRoot) })

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Site URL updated"}))
}

func (h *WebsiteHandler) ViewLogs(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	logType := c.Query("type")
	if logType != "error" && logType != "access" && logType != "security" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid log type. Only error, access, or security are supported"))
		return
	}
	if logType == "security" && site.SiteType != "wordpress" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Only WordPress sites support security logs"))
		return
	}

	linesStr := c.DefaultQuery("lines", "200")
	lines, _ := strconv.Atoi(linesStr)
	if lines <= 0 || lines > 5000 {
		lines = 200
	}

	baseName, ok := siteLogBaseName(logType)
	if !ok {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid log type. Only error, access, or security are supported"))
		return
	}
	cleanPath, err := resolveSiteLogFile(site.LogDir, logType, baseName)
	if err != nil {
		c.JSON(http.StatusForbidden, models.ErrorResponse("Access to this path is forbidden"))
		return
	}
	if info, err := os.Lstat(cleanPath); err == nil && !info.Mode().IsRegular() {
		c.JSON(http.StatusForbidden, models.ErrorResponse("Access to this log file is forbidden"))
		return
	}

	content := tailFile(cleanPath, lines)
	if content == "" {
		if logType == "access" {
			content = "(No abnormal access logs. By default, only 4xx/5xx requests are logged. Normal access is not written to access.log)"
		} else if logType == "security" {
			content = "(No WordPress security logs. No abnormal path access detected)"
		} else {
			content = "(No error logs. Site is running normally)"
		}
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"log_type": logType, "content": content}))
}

func (h *WebsiteHandler) ListLogFiles(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	logType := c.Query("type")
	if logType != "error" && logType != "access" && logType != "security" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid log type. Only error, access, or security are supported"))
		return
	}
	if logType == "security" && site.SiteType != "wordpress" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Only WordPress sites support security logs"))
		return
	}

	files, err := listSiteLogFiles(site.LogDir, logType)
	if err != nil {
		c.JSON(http.StatusForbidden, models.ErrorResponse("Failed to read log file list"))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"log_type": logType, "files": files}))
}

func (h *WebsiteHandler) DownloadLogFile(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	logType := c.Query("type")
	if logType != "error" && logType != "access" && logType != "security" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid log type. Only error, access, or security are supported"))
		return
	}
	if logType == "security" && site.SiteType != "wordpress" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Only WordPress sites support security logs"))
		return
	}

	filename := strings.TrimSpace(c.Query("file"))
	cleanPath, err := resolveSiteLogFile(site.LogDir, logType, filename)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid log filename"))
		return
	}

	info, err := os.Lstat(cleanPath)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("log file not found"))
		return
	}
	if !info.Mode().IsRegular() {
		c.JSON(http.StatusForbidden, models.ErrorResponse("Downloading this log file is forbidden"))
		return
	}

	c.Header("Cache-Control", "no-store")
	c.FileAttachment(cleanPath, filename)
}

func (h *WebsiteHandler) ClearLogs(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	logType := c.Query("type")
	if logType != "error" && logType != "access" && logType != "security" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid log type. Only error, access, or security are supported"))
		return
	}
	if logType == "security" && site.SiteType != "wordpress" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Only WordPress sites support security logs"))
		return
	}

	baseName, ok := siteLogBaseName(logType)
	if !ok {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid log type. Only error, access, or security are supported"))
		return
	}
	cleanPath, err := resolveSiteLogFile(site.LogDir, logType, baseName)
	if err != nil {
		c.JSON(http.StatusForbidden, models.ErrorResponse("Access to this path is forbidden"))
		return
	}
	if info, err := os.Lstat(cleanPath); err == nil && !info.Mode().IsRegular() {
		c.JSON(http.StatusForbidden, models.ErrorResponse("Clearing this log file is forbidden"))
		return
	}

	if err := os.WriteFile(cleanPath, []byte{}, 0644); err != nil {
		log.Printf("Failed to clear logs path=%s: %v", cleanPath, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to clear logs"))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Logs cleared"}))
}

func tailFile(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	const bufSize = 4096
	info, _ := f.Stat()
	pos := info.Size()
	var chunks [][]byte
	total := 0
	for pos > 0 && total < n*bufSize {
		readSize := int64(bufSize)
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		b := make([]byte, readSize)
		f.ReadAt(b, pos)
		chunks = append(chunks, b)
		total += len(b)
	}
	var data []byte
	for i := len(chunks) - 1; i >= 0; i-- {
		data = append(data, chunks[i]...)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func siteLogBaseName(logType string) (string, bool) {
	switch logType {
	case "access":
		return "access.log", true
	case "error":
		return "error.log", true
	case "security":
		return "wp-security.log", true
	default:
		return "", false
	}
}

func isAllowedSiteLogFilename(logType, name string) bool {
	baseName, ok := siteLogBaseName(logType)
	if !ok || name == "" || name != filepath.Base(name) {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return false
	}
	if name == baseName {
		return true
	}

	if strings.HasPrefix(name, baseName+".") {
		suffix := strings.TrimPrefix(name, baseName+".")
		suffix = strings.TrimSuffix(suffix, ".gz")
		return suffix != "" && allDigits(suffix)
	}

	if strings.HasPrefix(name, baseName+"-") {
		suffix := strings.TrimPrefix(name, baseName+"-")
		suffix = strings.TrimSuffix(suffix, ".gz")
		_, err := time.Parse("2006-01-02", suffix)
		return err == nil
	}

	return false
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func resolveSiteLogFile(logDir, logType, filename string) (string, error) {
	if !isAllowedSiteLogFilename(logType, filename) {
		return "", fmt.Errorf("invalid log filename")
	}
	cleanLogDir := filepath.Clean(logDir)
	if cleanLogDir == "." || !filepath.IsAbs(cleanLogDir) {
		return "", fmt.Errorf("invalid log dir")
	}
	cleanPath := filepath.Clean(filepath.Join(cleanLogDir, filename))
	if filepath.Dir(cleanPath) != cleanLogDir {
		return "", fmt.Errorf("log path outside site log dir")
	}
	return cleanPath, nil
}

func listSiteLogFiles(logDir, logType string) ([]siteLogFileInfo, error) {
	baseName, ok := siteLogBaseName(logType)
	if !ok {
		return nil, fmt.Errorf("invalid log type")
	}
	cleanLogDir := filepath.Clean(logDir)
	if cleanLogDir == "." || !filepath.IsAbs(cleanLogDir) {
		return nil, fmt.Errorf("invalid log dir")
	}

	entries, err := os.ReadDir(cleanLogDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []siteLogFileInfo{}, nil
		}
		return nil, err
	}

	files := make([]siteLogFileInfo, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !isAllowedSiteLogFilename(logType, name) {
			continue
		}
		cleanPath, err := resolveSiteLogFile(cleanLogDir, logType, name)
		if err != nil {
			continue
		}
		info, err := os.Lstat(cleanPath)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, siteLogFileInfo{
			Name:       name,
			Size:       info.Size(),
			ModifiedAt: info.ModTime(),
			Current:    name == baseName,
			Compressed: strings.HasSuffix(name, ".gz"),
		})
	}

	sort.Slice(files, func(i, j int) bool {
		if files[i].Current != files[j].Current {
			return files[i].Current
		}
		if !files[i].ModifiedAt.Equal(files[j].ModifiedAt) {
			return files[i].ModifiedAt.After(files[j].ModifiedAt)
		}
		return files[i].Name < files[j].Name
	})

	return files, nil
}

func (h *WebsiteHandler) GetNginxCustom(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	nginxCustomDir := "/www/server/panel/nginx-custom"
	prePath := filepath.Join(nginxCustomDir, site.Domain+".pre.conf")
	mainPath := filepath.Join(nginxCustomDir, site.Domain+".conf")

	preContent, _ := os.ReadFile(prePath)
	content, _ := os.ReadFile(mainPath)

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"pre_content":        string(preContent),
		"content":            string(content),
		"access_log_enabled": site.AccessLogMode != "off",
		"access_log_mode":    site.AccessLogMode,
	}))
}

func (h *WebsiteHandler) SaveNginxCustom(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	var req struct {
		PreContent string `json:"pre_content"`
		Content    string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	task := executor.GlobalQueue.Enqueue(executor.TaskSaveNginxCustom, &executor.SaveNginxCustomPayload{
		Site: site, PreContent: req.PreContent, Content: req.Content,
	})
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": result.Message}))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *WebsiteHandler) SetAccessLogMode(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	var req struct {
		Mode string `json:"mode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	if req.Mode != "off" && req.Mode != "error_only" && req.Mode != "full" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid log mode"))
		return
	}

	task := executor.GlobalQueue.Enqueue(executor.TaskSetAccessLogMode, &executor.SetAccessLogModePayload{
		Site: site, Mode: req.Mode,
	})
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": result.Message}))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *WebsiteHandler) SetCDNRealIP(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}
	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	var req struct {
		Enabled  bool  `json:"enabled"`
		GroupIDs []int `json:"group_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	if req.Enabled && len(req.GroupIDs) == 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("At least one configuration group must be selected when enabling CDN real IP"))
		return
	}

	task := executor.GlobalQueue.Enqueue(executor.TaskSetCDNRealIP, &executor.SetCDNRealIPPayload{
		Site: site, Enabled: req.Enabled, GroupIDs: req.GroupIDs,
	})
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": result.Message}))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func getWebsiteByID(id int) *models.Website {
	w, err := scanWebsite(database.GetDB().QueryRow(
		"SELECT "+websiteCols+" FROM websites WHERE id = ?", id,
	).Scan)
	if err != nil {
		return nil
	}
	executor.LoadWebsiteCDNRealIPGroups(w)
	return w
}

func (h *WebsiteHandler) InstallPlugin(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}
	if site.FileLockEnabled {
		c.JSON(http.StatusLocked, models.ErrorResponse(fileLockBlockedMessage))
		return
	}
	domain, webRoot, systemUser := site.Domain, site.WebRoot, site.SystemUser

	src := "/www/server/panel/packages/wp-panel-optimizer.php"
	pluginDir := filepath.Join(webRoot, "wp-content", "plugins", "wp-panel-optimizer")
	dst := filepath.Join(pluginDir, "wp-panel-optimizer.php")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to create plugin directory"))
		return
	}

	srcData, err := os.ReadFile(src)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Plugin source file not found. Please upgrade the panel first"))
		return
	}
	if err := os.WriteFile(dst, srcData, 0644); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to write plugin file"))
		return
	}

	apiKey := executor.NewAPIKey()
	if _, err := database.GetDB().Exec("UPDATE websites SET plugin_api_key = ? WHERE id = ?", apiKey, id); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to save plugin key"))
		return
	}

	cfg := config.AppConfig
	panelURL := fmt.Sprintf("https://127.0.0.1:%d/%s", cfg.Panel.TLSPort, cfg.Panel.RandomSuffix)
	cfgJSON, _ := json.Marshal(map[string]string{
		"panel_url": panelURL,
		"api_key":   apiKey,
	})
	baseSecretsDir := "/var/wp-panel/site-secrets"
	secretsDir := filepath.Join(baseSecretsDir, domain)
	if err := os.MkdirAll(baseSecretsDir, 0711); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to create secrets directory"))
		return
	}
	if err := os.Chmod(baseSecretsDir, 0711); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to set secrets directory permissions"))
		return
	}
	if err := os.MkdirAll(secretsDir, 0700); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to create site secrets directory"))
		return
	}

	// Clean up configuration files from old path (before migration outside the web directory)
	os.Remove(filepath.Join(pluginDir, "wp-panel-config.json"))

	if err := os.WriteFile(filepath.Join(secretsDir, "wp-panel-config.json"), cfgJSON, 0600); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to write plugin secret"))
		return
	}

	executor.InstallPluginPermissions(domain, systemUser, pluginDir)

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"message":   "Plugin installed",
		"path":      "wp-content/plugins/wp-panel-optimizer/",
		"panel_url": panelURL,
	}))
}

func (h *WebsiteHandler) InstallPluginStatus(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	var domain, webRoot string
	err = database.GetDB().QueryRow("SELECT domain, web_root FROM websites WHERE id = ?", id).Scan(&domain, &webRoot)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	dst := filepath.Join(webRoot, "wp-content", "plugins", "wp-panel-optimizer", "wp-panel-optimizer.php")
	dstInfo, dstErr := os.Stat(dst)
	srcInfo, srcErr := os.Stat("/www/server/panel/packages/wp-panel-optimizer.php")

	status := "not_installed"
	if dstErr == nil {
		status = "installed"
		if srcErr == nil && dstInfo.ModTime().Before(srcInfo.ModTime()) {
			status = "update_available"
		}
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"status":      status,
		"plugin_path": "wp-content/plugins/wp-panel-optimizer/",
	}))
}

func (h *WebsiteHandler) UpdateCache(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
		TTL     int  `json:"ttl"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	if req.TTL < 10 {
		req.TTL = 300
	}
	if req.TTL > 86400 {
		req.TTL = 86400
	}

	enabled := 0
	if req.Enabled {
		enabled = 1
	}
	database.GetDB().Exec("UPDATE websites SET fastcgi_cache_enabled = ?, fastcgi_cache_ttl = ? WHERE id = ?", enabled, req.TTL, id)

	executor.GoSafe(func() {
		if err := executor.RegenerateSiteNginx(id); err != nil {
			log.Printf("Failed to refresh site Nginx configuration site=%d: %v", id, err)
		}
	})

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Cache settings updated"}))
}

func (h *WebsiteHandler) SaveWPOptimizations(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}
	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}
	if site.FileLockEnabled {
		c.JSON(http.StatusLocked, models.ErrorResponse(fileLockBlockedMessage))
		return
	}

	var req struct {
		FCacheEnabled      bool   `json:"fcache_enabled"`
		FCacheTTL          int    `json:"fcache_ttl"`
		DisableWPUpdates   bool   `json:"disable_wp_updates"`
		DisableFileEditing bool   `json:"disable_file_editing"`
		XMLRPCEnabled      bool   `json:"xmlrpc_enabled"`
		WPDebugEnabled     bool   `json:"wp_debug_enabled"`
		WPPostRevisions    int    `json:"wp_post_revisions"`
		WPMemoryLimit      string `json:"wp_memory_limit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	if req.FCacheTTL < 10 {
		req.FCacheTTL = 300
	}
	if req.FCacheTTL > 86400 {
		req.FCacheTTL = 86400
	}

	db := database.GetDB()

	// Check if FastCGI / XML-RPC configuration changed, decide whether to reload Nginx
	var domain string
	var oldFCacheEnabled, oldFCacheTTL, oldXMLRPCEnabled int
	db.QueryRow("SELECT domain, fastcgi_cache_enabled, fastcgi_cache_ttl, xmlrpc_enabled FROM websites WHERE id = ?", id).
		Scan(&domain, &oldFCacheEnabled, &oldFCacheTTL, &oldXMLRPCEnabled)

	fcEnabled := 0
	if req.FCacheEnabled {
		fcEnabled = 1
	}
	disableUpdates := 0
	if req.DisableWPUpdates {
		disableUpdates = 1
	}
	disableEditing := 0
	if req.DisableFileEditing {
		disableEditing = 1
	}
	xmlrpcEnabled := 0
	if req.XMLRPCEnabled {
		xmlrpcEnabled = 1
	}

	wpDebug := 0
	if req.WPDebugEnabled {
		wpDebug = 1
	}

	_, err = db.Exec(`UPDATE websites SET
		fastcgi_cache_enabled = ?, fastcgi_cache_ttl = ?,
		disable_wp_updates = ?, disable_file_editing = ?, xmlrpc_enabled = ?,
		wp_debug_enabled = ?, wp_post_revisions = ?, wp_memory_limit = ?
		WHERE id = ?`,
		fcEnabled, req.FCacheTTL, disableUpdates, disableEditing, xmlrpcEnabled,
		wpDebug, req.WPPostRevisions, req.WPMemoryLimit, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Save failed"))
		return
	}

	// Update wp-config.php
	var webRoot string
	db.QueryRow("SELECT web_root FROM websites WHERE id = ?", id).Scan(&webRoot)
	if webRoot != "" {
		opts := executor.WPOptimizations{
			DisableUpdates:     req.DisableWPUpdates,
			DisableFileEditing: req.DisableFileEditing,
			WPDebug:            req.WPDebugEnabled,
			WPPostRevisions:    req.WPPostRevisions,
			WPMemoryLimit:      req.WPMemoryLimit,
		}
		if err := executor.ApplyWPOptimizations(webRoot, opts); err != nil {
			log.Printf("ApplyWPOptimizations failed (site %d): %v", id, err)
		}
	}

	// Reload Nginx when FastCGI / XML-RPC configuration changes
	if oldFCacheEnabled != fcEnabled || oldFCacheTTL != req.FCacheTTL || oldXMLRPCEnabled != xmlrpcEnabled {
		executor.GoSafe(func() {
			if err := executor.RegenerateSiteNginx(id); err != nil {
				log.Printf("Failed to refresh site Nginx configuration site=%d: %v", id, err)
			}
		})
	}
	if domain != "" {
		recordHandlerOperationLog("wp_optimizations", domain, "success", wpOptimizationsLogMessage(req.FCacheEnabled, req.FCacheTTL, req.DisableWPUpdates, req.DisableFileEditing, req.XMLRPCEnabled, req.WPDebugEnabled, req.WPPostRevisions, req.WPMemoryLimit))
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Saved"}))
}

func (h *WebsiteHandler) SetFileLock(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}
	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}
	if site.SiteType != "wordpress" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Only WordPress sites support file lock"))
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	if site.FileLockEnabled == req.Enabled {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
			"message":           "File lock status unchanged",
			"file_lock_enabled": req.Enabled,
		}))
		return
	}

	task := executor.GlobalQueue.Enqueue(executor.TaskSetFileLock, &executor.SetFileLockPayload{
		Site:    site,
		Enabled: req.Enabled,
	})
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(result.Data))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *WebsiteHandler) SaveMonitoring(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	var req struct {
		Enabled  bool `json:"enabled"`
		Interval int  `json:"interval"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	if req.Interval < 1 {
		req.Interval = 5
	}
	enabled := 0
	if req.Enabled {
		enabled = 1
	}
	database.GetDB().Exec("UPDATE websites SET monitoring_enabled = ?, monitoring_interval = ? WHERE id = ?", enabled, req.Interval, id)
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Saved"}))
}

func (h *WebsiteHandler) ClearCache(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	executor.GoSafe(func() { executor.ClearSiteCache(id) })

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Cache cleared. Old cache will be automatically recycled within 60 minutes"}))
}

func (h *WebsiteHandler) ReinstallWordPress(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	var domain, webRoot, systemUser, dbName, dbUser, siteType string
	var fileLockEnabled int
	err = database.GetDB().QueryRow(
		"SELECT domain, web_root, system_user, db_name, db_user, site_type, file_lock_enabled FROM websites WHERE id = ?", id,
	).Scan(&domain, &webRoot, &systemUser, &dbName, &dbUser, &siteType, &fileLockEnabled)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	if siteType != "wordpress" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Only WordPress sites support reinstallation"))
		return
	}
	if fileLockEnabled == 1 {
		c.JSON(http.StatusLocked, models.ErrorResponse(fileLockBlockedMessage))
		return
	}

	cfg := config.AppConfig

	var req struct {
		CleanDefaults      bool     `json:"clean_defaults"`
		RemoveUnusedThemes bool     `json:"remove_unused_themes"`
		InstallThemes      []string `json:"install_themes"`
		InstallPlugins     []string `json:"install_plugins"`
	}
	c.ShouldBindJSON(&req)

	if err := executor.ReinstallWordPress(cfg.Paths.WordPressPackage, webRoot, dbName, dbUser, systemUser, cfg,
		req.CleanDefaults, req.RemoveUnusedThemes, req.InstallThemes, req.InstallPlugins); err != nil {
		log.Printf("WordPress reinstallation failed site=%d: %v", id, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(reinstallWordPressErrorMessage(err)))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"message": "WordPress reinstallation complete. Database and files have been reset to a fresh state",
	}))
}

func reinstallWordPressErrorMessage(err error) string {
	const prefix = "WordPress reinstallation failed"
	if err == nil {
		return prefix
	}
	stage := strings.TrimSpace(err.Error())
	if idx := strings.IndexAny(stage, ":："); idx >= 0 {
		stage = strings.TrimSpace(stage[:idx])
	}
	switch stage {
	case "Website directory path is empty",
		"Website directory path validation failed",
		"Website directory path is not within allowed directories",
		"Failed to create temporary website directory",
		"WordPress deployment failed",
		"Failed to delete old database",
		"Failed to rebuild database",
		"Failed to generate wp-config.php",
		"Failed to clean old website directory",
		"Failed to replace website directory":
		return prefix + "：" + stage
	default:
		return prefix
	}
}

// ============================================================
// CacheHelperHandler — WordPress Plugin API
// ============================================================

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}

type CacheHelperHandler struct{}

func pluginRequestHostAllowed(c *gin.Context) bool {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return false
	}
	return host == "127.0.0.1" || host == "::1"
}

func (h *CacheHelperHandler) checkAPIKey(domain string, c *gin.Context) bool {
	if !pluginRequestHostAllowed(c) {
		return false
	}
	key := c.GetHeader("X-WP-Panel-Key")
	if key == "" {
		return false
	}
	var storedKey string
	err := database.GetDB().QueryRow(
		"SELECT plugin_api_key FROM websites WHERE domain = ? OR (char(10) || aliases || char(10)) LIKE ('%' || char(10) || ? || char(10) || '%') ESCAPE '\\'",
		domain, escapeLike(domain),
	).Scan(&storedKey)
	if err != nil {
		return false
	}
	return storedKey != "" && key == storedKey
}

func (h *CacheHelperHandler) pluginSiteByDomain(domain string, c *gin.Context) (*models.Website, bool) {
	if !pluginRequestHostAllowed(c) {
		return nil, false
	}
	key := c.GetHeader("X-WP-Panel-Key")
	if key == "" {
		return nil, false
	}

	var siteID int
	var storedKey string
	err := database.GetDB().QueryRow(
		"SELECT id, plugin_api_key FROM websites WHERE domain = ? OR (char(10) || aliases || char(10)) LIKE ('%' || char(10) || ? || char(10) || '%') ESCAPE '\\'",
		domain, escapeLike(domain),
	).Scan(&siteID, &storedKey)
	if err != nil || storedKey == "" || key != storedKey {
		return nil, false
	}

	site := getWebsiteByID(siteID)
	return site, site != nil
}

func recordHandlerOperationLog(operation, target, status, message string) {
	if database.GetDB() == nil {
		return
	}
	_, _ = database.GetDB().Exec(
		"INSERT INTO operation_logs (operation, target, status, message) VALUES (?, ?, ?, ?)",
		operation, target, status, message,
	)
}

func (h *CacheHelperHandler) UpdateCacheSettings(c *gin.Context) {
	var req struct {
		Domain string `json:"domain"`
		TTL    int    `json:"ttl"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Domain == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	if !h.checkAPIKey(req.Domain, c) {
		c.JSON(http.StatusUnauthorized, models.ErrorResponse("Invalid API Key"))
		return
	}
	if req.TTL < 10 {
		req.TTL = 300
	}
	if req.TTL > 86400 {
		req.TTL = 86400
	}

	db := database.GetDB()
	_, err := db.Exec("UPDATE websites SET fastcgi_cache_ttl = ? WHERE (domain = ? OR (char(10) || aliases || char(10)) LIKE ('%' || char(10) || ? || char(10) || '%') ESCAPE '\\')", req.TTL, req.Domain, escapeLike(req.Domain))
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Update failed"))
		return
	}

	var siteID int
	db.QueryRow("SELECT id FROM websites WHERE (domain = ? OR (char(10) || aliases || char(10)) LIKE ('%' || char(10) || ? || char(10) || '%') ESCAPE '\\')", req.Domain, escapeLike(req.Domain)).Scan(&siteID)
	if siteID > 0 {
		executor.GoSafe(func() {
			if err := executor.RegenerateSiteNginx(siteID); err != nil {
				log.Printf("Failed to refresh site Nginx configuration site=%d: %v", siteID, err)
			}
		})
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "TTL updated", "ttl": req.TTL}))
}

func (h *CacheHelperHandler) ClearByDomain(c *gin.Context) {
	var req struct {
		Domain string `json:"domain"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Domain == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	if !h.checkAPIKey(req.Domain, c) {
		c.JSON(http.StatusUnauthorized, models.ErrorResponse("Invalid API Key"))
		return
	}

	var siteID int
	err := database.GetDB().QueryRow(
		"SELECT id FROM websites WHERE (domain = ? OR (char(10) || aliases || char(10)) LIKE ('%' || char(10) || ? || char(10) || '%') ESCAPE '\\')",
		req.Domain, escapeLike(req.Domain),
	).Scan(&siteID)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	executor.GoSafe(func() { executor.ClearSiteCache(siteID) })
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Cache cleared"}))
}

func (h *CacheHelperHandler) FindByDomain(c *gin.Context) {
	domain := c.Query("domain")
	if domain == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	if !h.checkAPIKey(domain, c) {
		c.JSON(http.StatusUnauthorized, models.ErrorResponse("Invalid API Key"))
		return
	}

	var siteID, fcacheEnabled, fcacheTTL, disableUpdates, disableEditing, xmlrpcEnabled, wpDebugEnabled, wpPostRevisions, fileLockEnabled int
	var wpMemoryLimit string
	err := database.GetDB().QueryRow(
		"SELECT id, fastcgi_cache_enabled, fastcgi_cache_ttl, disable_wp_updates, disable_file_editing, xmlrpc_enabled, wp_debug_enabled, wp_post_revisions, wp_memory_limit, file_lock_enabled FROM websites WHERE domain = ? OR (char(10) || aliases || char(10)) LIKE ('%' || char(10) || ? || char(10) || '%') ESCAPE '\\'",
		domain, escapeLike(domain),
	).Scan(&siteID, &fcacheEnabled, &fcacheTTL, &disableUpdates, &disableEditing, &xmlrpcEnabled, &wpDebugEnabled, &wpPostRevisions, &wpMemoryLimit, &fileLockEnabled)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"site_id":               siteID,
		"domain":                domain,
		"fastcgi_cache_enabled": fcacheEnabled == 1,
		"fastcgi_cache_ttl":     fcacheTTL,
		"disable_wp_updates":    disableUpdates == 1,
		"disable_file_editing":  disableEditing == 1,
		"xmlrpc_enabled":        xmlrpcEnabled == 1,
		"wp_debug_enabled":      wpDebugEnabled == 1,
		"wp_post_revisions":     wpPostRevisions,
		"wp_memory_limit":       wpMemoryLimit,
		"file_lock_enabled":     fileLockEnabled == 1,
	}))
}

func (h *CacheHelperHandler) ExportSSLCertificate(c *gin.Context) {
	domain := strings.ToLower(strings.TrimSpace(c.Query("domain")))
	if domain == "" || !executor.IsValidDomain(domain) {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	site, ok := h.pluginSiteByDomain(domain, c)
	if !ok {
		c.JSON(http.StatusUnauthorized, models.ErrorResponse("Invalid API Key"))
		return
	}
	if !site.SSLExportEnabled {
		recordHandlerOperationLog("ssl_certificate_export", site.Domain, "failed", "Plugin certificate export permission not enabled")
		c.JSON(http.StatusForbidden, models.ErrorResponse("SSL certificate export permission not enabled"))
		return
	}

	payload, err := sslCertificateExportPayload(site)
	if err != nil {
		recordHandlerOperationLog("ssl_certificate_export", site.Domain, "failed", err.Error())
		c.JSON(sslDownloadStatus(err), models.ErrorResponse(err.Error()))
		return
	}

	recordHandlerOperationLog("ssl_certificate_export", site.Domain, "success", "Plugin has read SSL certificate")
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.JSON(http.StatusOK, models.SuccessResponse(payload))
}

func (h *CacheHelperHandler) UpdateOptimizerSettings(c *gin.Context) {
	var req struct {
		Domain             string `json:"domain"`
		Enabled            bool   `json:"enabled"`
		TTL                int    `json:"ttl"`
		DisableWPUpdates   bool   `json:"disable_wp_updates"`
		DisableFileEditing bool   `json:"disable_file_editing"`
		WPDebugEnabled     bool   `json:"wp_debug_enabled"`
		WPPostRevisions    int    `json:"wp_post_revisions"`
		WPMemoryLimit      string `json:"wp_memory_limit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Domain == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	site, ok := h.pluginSiteByDomain(req.Domain, c)
	if !ok {
		c.JSON(http.StatusUnauthorized, models.ErrorResponse("Invalid API Key"))
		return
	}
	if site.FileLockEnabled {
		c.JSON(http.StatusLocked, models.ErrorResponse(fileLockBlockedMessage))
		return
	}
	if req.TTL < 10 {
		req.TTL = 300
	}
	if req.TTL > 86400 {
		req.TTL = 86400
	}

	db := database.GetDB()

	var oldFCacheEnabled, oldFCacheTTL int
	db.QueryRow("SELECT fastcgi_cache_enabled, fastcgi_cache_ttl FROM websites WHERE domain = ? OR (char(10) || aliases || char(10)) LIKE ('%' || char(10) || ? || char(10) || '%') ESCAPE '\\'", req.Domain, escapeLike(req.Domain)).
		Scan(&oldFCacheEnabled, &oldFCacheTTL)

	fcEnabled := 0
	if req.Enabled {
		fcEnabled = 1
	}
	disableUpdates := 0
	disableEditing := 0
	if req.DisableWPUpdates {
		disableUpdates = 1
	}
	if req.DisableFileEditing {
		disableEditing = 1
	}

	wpDebug2 := 0
	if req.WPDebugEnabled {
		wpDebug2 = 1
	}

	_, err := db.Exec(`UPDATE websites SET
		fastcgi_cache_enabled = ?, fastcgi_cache_ttl = ?,
		disable_wp_updates = ?, disable_file_editing = ?,
		wp_debug_enabled = ?, wp_post_revisions = ?, wp_memory_limit = ?
		WHERE domain = ? OR (char(10) || aliases || char(10)) LIKE ('%' || char(10) || ? || char(10) || '%') ESCAPE '\'`,
		fcEnabled, req.TTL, disableUpdates, disableEditing, wpDebug2, req.WPPostRevisions, req.WPMemoryLimit, req.Domain, escapeLike(req.Domain))
	if err != nil {
		log.Printf("UpdateOptimizerSettings DB Update failed (site %s): %v", req.Domain, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Save failed: "+err.Error()))
		return
	}

	// Update wp-config.php
	var webRoot string
	db.QueryRow("SELECT web_root FROM websites WHERE domain = ? OR (char(10) || aliases || char(10)) LIKE ('%' || char(10) || ? || char(10) || '%') ESCAPE '\\'", req.Domain, escapeLike(req.Domain)).Scan(&webRoot)
	if webRoot != "" {
		opts := executor.WPOptimizations{
			DisableUpdates:     req.DisableWPUpdates,
			DisableFileEditing: req.DisableFileEditing,
			WPDebug:            req.WPDebugEnabled,
			WPPostRevisions:    req.WPPostRevisions,
			WPMemoryLimit:      req.WPMemoryLimit,
		}
		if err := executor.ApplyWPOptimizations(webRoot, opts); err != nil {
			log.Printf("ApplyWPOptimizations failed (site %s): %v", req.Domain, err)
		}
	}

	// Reload Nginx when FastCGI configuration changes
	if oldFCacheEnabled != fcEnabled || oldFCacheTTL != req.TTL {
		var siteID int
		db.QueryRow("SELECT id FROM websites WHERE domain = ? OR (char(10) || aliases || char(10)) LIKE ('%' || char(10) || ? || char(10) || '%') ESCAPE '\\'", req.Domain, escapeLike(req.Domain)).Scan(&siteID)
		if siteID > 0 {
			executor.GoSafe(func() {
				if err := executor.RegenerateSiteNginx(siteID); err != nil {
					log.Printf("Failed to refresh site Nginx configuration site=%d: %v", siteID, err)
				}
			})
		}
	}
	recordHandlerOperationLog("wp_optimizations", req.Domain, "success", wpOptimizationsLogMessage(req.Enabled, req.TTL, req.DisableWPUpdates, req.DisableFileEditing, false, req.WPDebugEnabled, req.WPPostRevisions, req.WPMemoryLimit))

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Saved"}))
}

func wpOptimizationsLogMessage(fcacheEnabled bool, fcacheTTL int, disableUpdates, disableEditing, xmlrpcEnabled, wpDebugEnabled bool, postRevisions int, memoryLimit string) string {
	state := func(enabled bool) string {
		if enabled {
			return "on"
		}
		return "off"
	}
	parts := []string{
		"FastCGI cache=" + state(fcacheEnabled),
		fmt.Sprintf("Cache TTL=%d", fcacheTTL),
		"Disable updates=" + state(disableUpdates),
		"Disable file editing=" + state(disableEditing),
		"XML-RPC=" + state(xmlrpcEnabled),
		"WP_DEBUG=" + state(wpDebugEnabled),
		fmt.Sprintf("Post revisions=%d", postRevisions),
	}
	if strings.TrimSpace(memoryLimit) != "" {
		parts = append(parts, "PHP memory limit="+strings.TrimSpace(memoryLimit))
	}
	return strings.Join(parts, "；")
}

func (h *WebsiteHandler) SetLogRetention(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	var req struct {
		RetentionDays int `json:"retention_days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	if req.RetentionDays < 0 {
		req.RetentionDays = 0
	}

	if err := executor.WriteSiteLogrotateConfig(site.Domain, site.LogDir, req.RetentionDays); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to apply logrotate configuration"))
		return
	}

	db := database.GetDB()
	if _, err := db.Exec("UPDATE websites SET log_retention_days = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?", req.RetentionDays, id); err != nil {
		_ = executor.WriteSiteLogrotateConfig(site.Domain, site.LogDir, site.LogRetentionDays)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Save failed"))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Saved"}))
}

func (h *WebsiteHandler) UpdateExpiry(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid website ID"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("Website not found"))
		return
	}

	var req struct {
		ExpiresAt string `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	db := database.GetDB()
	req.ExpiresAt = strings.TrimSpace(req.ExpiresAt)
	var dbErr error
	if req.ExpiresAt == "" {
		_, dbErr = db.Exec("UPDATE websites SET expires_at = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = ?", id)
	} else {
		if _, err := time.Parse("2006-01-02", req.ExpiresAt); err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid date format. Please use YYYY-MM-DD"))
			return
		}
		_, dbErr = db.Exec("UPDATE websites SET expires_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?", req.ExpiresAt, id)
	}
	if dbErr != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Save failed"))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Saved"}))
}
