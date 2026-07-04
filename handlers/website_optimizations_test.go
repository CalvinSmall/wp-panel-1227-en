package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/naibabiji/wp-panel/database"
)

func TestSaveWPOptimizationsRecordsOperationLog(t *testing.T) {
	setupWebsiteOptimizationsTestDB(t)

	router := gin.New()
	handler := &WebsiteHandler{}
	router.PUT("/api/websites/:id/wp-optimizations", handler.SaveWPOptimizations)

	body := `{
		"fcache_enabled": false,
		"fcache_ttl": 300,
		"disable_wp_updates": false,
		"disable_file_editing": false,
		"xmlrpc_enabled": false,
		"wp_debug_enabled": true,
		"wp_post_revisions": -1,
		"wp_memory_limit": ""
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/websites/1/wp-optimizations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var operation, target, message string
	err := database.GetDB().QueryRow(`SELECT operation, target, message FROM operation_logs ORDER BY id DESC LIMIT 1`).Scan(&operation, &target, &message)
	if err != nil {
		t.Fatalf("query operation log: %v", err)
	}
	if operation != "wp_optimizations" {
		t.Fatalf("operation = %q, want wp_optimizations", operation)
	}
	if target != "example.com" {
		t.Fatalf("target = %q, want example.com", target)
	}
	if !strings.Contains(message, "WP_DEBUG=on") {
		t.Fatalf("message missing WP_DEBUG state: %q", message)
	}
}

func setupWebsiteOptimizationsTestDB(t *testing.T) {
	t.Helper()
	oldDB := database.DB
	if err := database.Open(filepath.Join(t.TempDir(), "panel.db")); err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := database.RunMigrations(); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	t.Cleanup(func() {
		_ = database.Close()
		database.DB = oldDB
	})

	webRoot := t.TempDir()
	config := `<?php
define('DB_NAME', 'db_example');
define('DB_USER', 'user_example');
define('DB_PASSWORD', 'secret');
$table_prefix = 'wp_';
`
	if err := os.WriteFile(filepath.Join(webRoot, "wp-config.php"), []byte(config), 0600); err != nil {
		t.Fatalf("write wp-config.php: %v", err)
	}

	_, err := database.GetDB().Exec(`
		INSERT INTO websites (
			id, name, domain, aliases, status, system_user, web_root, log_dir,
			db_name, db_user, php_pool_path, nginx_conf_path, site_type
		) VALUES (
			1, 'example', 'example.com', '', 'active', 'wp_example', ?, '/www/wwwlogs/example.com',
			'db_example', 'user_example', '/etc/php/8.3/fpm/pool.d/example.conf', '/etc/nginx/sites-available/example.conf',
			'wordpress'
		)
	`, webRoot)
	if err != nil {
		t.Fatalf("insert website: %v", err)
	}
}
