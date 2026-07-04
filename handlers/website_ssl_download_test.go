package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/naibabiji/wp-panel/config"
	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/models"
)

func setupWebsiteSSLDownloadTest(t *testing.T) (string, *gin.Engine) {
	t.Helper()

	gin.SetMode(gin.TestMode)

	_ = database.Close()
	if err := database.Open(filepath.Join(t.TempDir(), "panel.db")); err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	if err := database.RunMigrations(); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	certRoot := t.TempDir()
	oldConfig := config.AppConfig
	config.AppConfig = &config.Config{Paths: config.PathsConfig{Certificates: certRoot}}
	t.Cleanup(func() { config.AppConfig = oldConfig })

	router := gin.New()
	handler := &WebsiteHandler{}
	router.GET("/api/websites/:id/ssl/download", handler.DownloadSSLPackage)
	router.PUT("/api/websites/:id/ssl/export", handler.SetSSLExport)
	cacheHelper := &CacheHelperHandler{}
	router.GET("/api/sites/ssl/export", cacheHelper.ExportSSLCertificate)

	return certRoot, router
}

func insertSSLDownloadWebsite(t *testing.T, domain string, sslEnabled bool) int64 {
	t.Helper()

	sslValue := 0
	var expiresAt interface{} = nil
	if sslEnabled {
		sslValue = 1
		expiresAt = time.Now().AddDate(0, 2, 0)
	}

	result, err := database.GetDB().Exec(`
		INSERT INTO websites (
			name, domain, aliases, status, system_user, web_root, log_dir,
			db_name, db_user, php_pool_path, nginx_conf_path, site_type,
			ssl_enabled, ssl_cert_path, ssl_key_path, ssl_expires_at
		) VALUES (?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?, 'wordpress', ?, ?, ?, ?)`,
		domain, domain, "www."+domain, "wp_test", "/www/wwwroot/"+domain,
		"/www/wwwlogs/"+domain, "db", "dbuser", "/etc/php/pool.conf",
		"/etc/nginx/sites-available/"+domain+".conf", sslValue,
		"/tmp/ignored/fullchain.pem", "/tmp/ignored/privkey.pem", expiresAt,
	)
	if err != nil {
		t.Fatalf("insert website: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

func writeTestCertificateFiles(t *testing.T, certRoot, domain string) {
	t.Helper()

	certDir := filepath.Join(certRoot, domain)
	if err := os.MkdirAll(certDir, 0700); err != nil {
		t.Fatalf("mkdir cert dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "fullchain.pem"), []byte("CERTIFICATE DATA"), 0644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "privkey.pem"), []byte("PRIVATE KEY DATA"), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

func performSSLDownload(router *gin.Engine, siteID int64) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/websites/"+strconvFormatInt(siteID)+"/ssl/download", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func performSSLExportToggle(router *gin.Engine, siteID int64, enabled bool) *httptest.ResponseRecorder {
	body := `{"enabled":false}`
	if enabled {
		body = `{"enabled":true}`
	}
	req := httptest.NewRequest(http.MethodPut, "/api/websites/"+strconvFormatInt(siteID)+"/ssl/export", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func performPluginSSLExport(router *gin.Engine, domain, apiKey, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/sites/ssl/export?domain="+domain, nil)
	req.RemoteAddr = remoteAddr
	if apiKey != "" {
		req.Header.Set("X-WP-Panel-Key", apiKey)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func strconvFormatInt(id int64) string {
	return strconv.FormatInt(id, 10)
}

func TestDownloadSSLPackageRejectsDisabledSSL(t *testing.T) {
	_, router := setupWebsiteSSLDownloadTest(t)
	id := insertSSLDownloadWebsite(t, "example.com", false)

	rec := performSSLDownload(router, id)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "SSL is not enabled for this site") {
		t.Fatalf("body = %q, want SSL disabled message", rec.Body.String())
	}
}

func TestDownloadSSLPackageMissingWebsite(t *testing.T) {
	_, router := setupWebsiteSSLDownloadTest(t)

	rec := performSSLDownload(router, 999)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestDownloadSSLPackageMissingFiles(t *testing.T) {
	_, router := setupWebsiteSSLDownloadTest(t)
	id := insertSSLDownloadWebsite(t, "example.com", true)

	rec := performSSLDownload(router, id)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if !strings.Contains(rec.Body.String(), "certificate file not found") {
		t.Fatalf("body = %q, want missing certificate message", rec.Body.String())
	}
}

func TestDownloadSSLPackageZipContents(t *testing.T) {
	certRoot, router := setupWebsiteSSLDownloadTest(t)
	domain := "example.com"
	id := insertSSLDownloadWebsite(t, domain, true)
	writeTestCertificateFiles(t, certRoot, domain)

	rec := performSSLDownload(router, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", contentType)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, domain+"-ssl-cert-") || !strings.Contains(cd, ".zip") {
		t.Fatalf("Content-Disposition = %q, want certificate zip filename", cd)
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}

	got := map[string]string{}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "/") || strings.Contains(f.Name, "\\") || filepath.IsAbs(f.Name) {
			t.Fatalf("zip entry has unsafe path: %q", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read zip entry %s: %v", f.Name, err)
		}
		got[f.Name] = string(data)
	}

	for _, name := range []string{"fullchain.pem", "privkey.pem", "README.txt"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("zip missing %s; entries=%v", name, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("zip entries = %d, want 3; entries=%v", len(got), got)
	}
	if got["fullchain.pem"] != "CERTIFICATE DATA" {
		t.Fatalf("fullchain.pem = %q", got["fullchain.pem"])
	}
	if got["privkey.pem"] != "PRIVATE KEY DATA" {
		t.Fatalf("privkey.pem = %q", got["privkey.pem"])
	}
	if strings.Contains(got["README.txt"], certRoot) {
		t.Fatalf("README leaks certificate root: %q", got["README.txt"])
	}
}

func TestSetSSLExportTogglesFlag(t *testing.T) {
	_, router := setupWebsiteSSLDownloadTest(t)
	id := insertSSLDownloadWebsite(t, "example.com", true)

	rec := performSSLExportToggle(router, id, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"success":true`) || !strings.Contains(rec.Body.String(), "SSL certificate export permission saved") {
		t.Fatalf("body = %q, want success message", rec.Body.String())
	}

	var got int
	if err := database.GetDB().QueryRow("SELECT ssl_export_enabled FROM websites WHERE id = ?", id).Scan(&got); err != nil {
		t.Fatalf("query ssl_export_enabled: %v", err)
	}
	if got != 1 {
		t.Fatalf("ssl_export_enabled = %d, want 1", got)
	}

	rec = performSSLExportToggle(router, id, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if err := database.GetDB().QueryRow("SELECT ssl_export_enabled FROM websites WHERE id = ?", id).Scan(&got); err != nil {
		t.Fatalf("query disabled ssl_export_enabled: %v", err)
	}
	if got != 0 {
		t.Fatalf("disabled ssl_export_enabled = %d, want 0", got)
	}
}

func TestPluginSSLCertificateExportSecurity(t *testing.T) {
	certRoot, router := setupWebsiteSSLDownloadTest(t)
	domain := "example.com"
	id := insertSSLDownloadWebsite(t, domain, true)
	writeTestCertificateFiles(t, certRoot, domain)
	if _, err := database.GetDB().Exec("UPDATE websites SET plugin_api_key = ? WHERE id = ?", "secret", id); err != nil {
		t.Fatalf("set plugin key: %v", err)
	}

	rec := performPluginSSLExport(router, domain, "secret", "127.0.0.1:12345")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("default disabled status = %d, want %d; body=%q", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	rec = performPluginSSLExport(router, domain, "secret", "203.0.113.10:12345")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote status = %d, want %d; body=%q", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	rec = performPluginSSLExport(router, domain, "wrong", "127.0.0.1:12345")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key status = %d, want %d; body=%q", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	if _, err := database.GetDB().Exec("UPDATE websites SET ssl_export_enabled = 1 WHERE id = ?", id); err != nil {
		t.Fatalf("enable ssl export: %v", err)
	}

	rec = performPluginSSLExport(router, domain, "secret", "127.0.0.1:12345")
	if rec.Code != http.StatusOK {
		t.Fatalf("enabled status = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if cache := rec.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cache)
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Domain      string   `json:"domain"`
			Aliases     []string `json:"aliases"`
			Certificate string   `json:"certificate"`
			PrivateKey  string   `json:"private_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("success = false; body=%q", rec.Body.String())
	}
	if resp.Data.Domain != domain {
		t.Fatalf("domain = %q, want %q", resp.Data.Domain, domain)
	}
	if resp.Data.Certificate != "CERTIFICATE DATA" {
		t.Fatalf("certificate = %q", resp.Data.Certificate)
	}
	if resp.Data.PrivateKey != "PRIVATE KEY DATA" {
		t.Fatalf("private key = %q", resp.Data.PrivateKey)
	}

	var logCount int
	if err := database.GetDB().QueryRow("SELECT COUNT(*) FROM operation_logs WHERE operation = 'ssl_certificate_export' AND target = ?", domain).Scan(&logCount); err != nil {
		t.Fatalf("query operation logs: %v", err)
	}
	if logCount == 0 {
		t.Fatal("expected ssl_certificate_export operation log")
	}

	rec = performSSLExportToggle(router, id, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable export status = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}
	rec = performPluginSSLExport(router, domain, "secret", "127.0.0.1:12345")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled after enabled status = %d, want %d; body=%q", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

func TestPluginSSLCertificateExportRejectsCrossSiteKey(t *testing.T) {
	certRoot, router := setupWebsiteSSLDownloadTest(t)
	siteA := insertSSLDownloadWebsite(t, "a.example.com", true)
	siteB := insertSSLDownloadWebsite(t, "b.example.com", true)
	writeTestCertificateFiles(t, certRoot, "a.example.com")
	writeTestCertificateFiles(t, certRoot, "b.example.com")
	if _, err := database.GetDB().Exec("UPDATE websites SET plugin_api_key = ?, ssl_export_enabled = 1 WHERE id = ?", "key-a", siteA); err != nil {
		t.Fatalf("set site a key: %v", err)
	}
	if _, err := database.GetDB().Exec("UPDATE websites SET plugin_api_key = ?, ssl_export_enabled = 1 WHERE id = ?", "key-b", siteB); err != nil {
		t.Fatalf("set site b key: %v", err)
	}

	rec := performPluginSSLExport(router, "b.example.com", "key-a", "127.0.0.1:12345")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cross-site key status = %d, want %d; body=%q", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestSSLCertificateExportPayloadValidatesSiteDomain(t *testing.T) {
	certRoot, _ := setupWebsiteSSLDownloadTest(t)
	site := &models.Website{
		Domain:     "../example.com",
		SSLEnabled: true,
	}
	writeTestCertificateFiles(t, certRoot, "example.com")

	_, err := sslCertificateExportPayload(site)
	if err == nil {
		t.Fatal("sslCertificateExportPayload error = nil, want invalid domain error")
	}
	if !strings.Contains(err.Error(), "invalid website domain format") {
		t.Fatalf("error = %q, want invalid domain message", err.Error())
	}
}
