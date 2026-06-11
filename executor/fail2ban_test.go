package executor

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/naibabiji/wp-panel/database"
)

func TestRecordFail2banBanKeepsRepeatedHistory(t *testing.T) {
	openTestDB(t)
	oldRecordPersistBan := recordPersistBan
	recordPersistBan = func(string) {}
	t.Cleanup(func() { recordPersistBan = oldRecordPersistBan })

	ip := "203.0.113.77"
	if err := RecordFail2banBan(ip, "wppanel-404"); err != nil {
		t.Fatalf("first record failed: %v", err)
	}
	if err := RecordFail2banBan(ip, "wppanel-404"); err != nil {
		t.Fatalf("second record failed: %v", err)
	}

	rows, err := database.GetDB().Query(
		`SELECT ban_level, source_jail, ban_count FROM firewall_bans
		 WHERE ip_address = ? ORDER BY id`, ip,
	)
	if err != nil {
		t.Fatalf("query records: %v", err)
	}
	defer rows.Close()

	var levels, counts []int
	var jails []string
	for rows.Next() {
		var level, count int
		var jail string
		if err := rows.Scan(&level, &jail, &count); err != nil {
			t.Fatalf("scan record: %v", err)
		}
		levels = append(levels, level)
		jails = append(jails, jail)
		counts = append(counts, count)
	}
	if len(levels) != 2 {
		t.Fatalf("expected two history records, got %d", len(levels))
	}
	if levels[0] != 2 || levels[1] != 3 {
		t.Fatalf("expected levels [2 3], got %v", levels)
	}
	if counts[0] != 1 || counts[1] != 2 {
		t.Fatalf("expected ban counts [1 2], got %v", counts)
	}
	for _, jail := range jails {
		if jail != "wppanel-404" {
			t.Fatalf("expected jail wppanel-404, got %q", jail)
		}
	}
}

func TestNginxTemplateErrorOnlyAccessLog(t *testing.T) {
	engine := NewTemplateEngine(t.TempDir())
	config, err := engine.RenderNginxConfig(&NginxSiteData{
		Domain:        "example.com",
		ServerNames:   "example.com",
		WebRoot:       "/www/wwwroot/example.com",
		PHPProxy:      "unix:/run/php/example.sock",
		TemplateVer:   "v1.0",
		AccessLogMode: "error_only",
	})
	if err != nil {
		t.Fatalf("render nginx config: %v", err)
	}
	if !strings.Contains(config, `access_log /www/wwwlogs/example.com/access.log combined if=$wp_loggable;`) {
		t.Fatalf("expected error-only access log in config:\n%s", config)
	}
	if strings.Contains(config, "access_log off;") {
		t.Fatalf("did not expect access_log off in error-only config:\n%s", config)
	}
}

func TestNginxTemplateIncludesFastCGIHeaderBuffers(t *testing.T) {
	engine := NewTemplateEngine(t.TempDir())
	config, err := engine.RenderNginxConfig(&NginxSiteData{
		Domain:        "example.com",
		ServerNames:   "example.com",
		WebRoot:       "/www/wwwroot/example.com",
		PHPProxy:      "unix:/run/php/example.sock",
		TemplateVer:   "v1.0",
		AccessLogMode: "full",
	})
	if err != nil {
		t.Fatalf("render nginx config: %v", err)
	}

	for _, directive := range []string{
		"fastcgi_buffer_size 128k;",
		"fastcgi_buffers 8 128k;",
		"fastcgi_busy_buffers_size 256k;",
	} {
		if !strings.Contains(config, directive) {
			t.Fatalf("expected %q in config:\n%s", directive, config)
		}
	}
}

func openTestDB(t *testing.T) {
	t.Helper()

	if database.DB != nil {
		_ = database.Close()
		database.DB = nil
	}
	dbPath := filepath.Join(t.TempDir(), "wp-panel-test.db")
	if err := database.Open(dbPath); err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		_ = database.Close()
		database.DB = nil
	})
	if err := database.RunMigrations(); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
}
