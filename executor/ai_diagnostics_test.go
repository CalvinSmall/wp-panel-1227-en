package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naibabiji/wp-panel/models"
)

func TestParseAIReportJSON(t *testing.T) {
	report, raw, ok := ParseAIReport(`{"summary":"发现 PHP Fatal","risk_level":"high","likely_causes":[],"recommended_actions":[],"needs_more_info":false,"user_friendly_explanation":"请查看错误日志"}`)
	if !ok {
		t.Fatalf("ParseAIReport() ok = false, raw=%q", raw)
	}
	if report.Summary != "发现 PHP Fatal" || report.RiskLevel != "high" {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func TestParseAIReportFallbackRawText(t *testing.T) {
	report, raw, ok := ParseAIReport("not json")
	if ok || report != nil {
		t.Fatalf("expected parse failure, got ok=%v report=%#v", ok, report)
	}
	if raw != "not json" {
		t.Fatalf("raw = %q", raw)
	}
}

func TestBuildAIDiagnosticPromptRedactsWPSecrets(t *testing.T) {
	root := t.TempDir()
	logDir := t.TempDir()
	config := `<?php
define('DB_NAME', 'db_example');
define('DB_USER', 'user_example');
define('DB_PASSWORD', 'super-secret');
define('AUTH_KEY', 'auth-secret');
$table_prefix = 'wp_';
define('WP_DEBUG', true);
`
	if err := os.WriteFile(filepath.Join(root, "wp-config.php"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "php-error.log"), []byte("PHP Fatal error: test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	site := &models.Website{
		ID:            1,
		Domain:        "example.com",
		SiteType:      "wordpress",
		WebRoot:       root,
		LogDir:        logDir,
		DBName:        "db_example",
		DBUser:        "user_example",
		PHPPoolPath:   filepath.Join(root, "pool.conf"),
		NginxConfPath: filepath.Join(root, "nginx.conf"),
	}
	_, prompt, err := BuildAIDiagnosticPrompt(site, models.AIDiagnosisSite500)
	if err != nil {
		t.Fatalf("BuildAIDiagnosticPrompt() error = %v", err)
	}
	if strings.Contains(prompt, "super-secret") || strings.Contains(prompt, "auth-secret") {
		t.Fatalf("prompt leaked secret:\n%s", prompt)
	}
	if !strings.Contains(prompt, "PHP Fatal error") {
		t.Fatalf("prompt missing log evidence:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"contains_db_password": "redacted"`) {
		t.Fatalf("prompt missing redaction marker:\n%s", prompt)
	}
}

func TestAIReadLogSnippetDistinguishesMissingAndSymlinkEscape(t *testing.T) {
	logDir := t.TempDir()
	missing := aiReadLogSnippet(logDir, "missing.log")
	if missing.Status != "not_found" {
		t.Fatalf("missing log status = %q, want not_found", missing.Status)
	}

	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("secret\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(logDir, "error.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	got := aiReadLogSnippet(logDir, "error.log")
	if got.Status != "forbidden" {
		t.Fatalf("symlink escape status = %q, want forbidden", got.Status)
	}
}

func TestAITailInterestingLinesKeepsNewestLinesWhenCapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "php-error.log")
	content := strings.Join([]string{
		"old line 1 with enough text",
		"old line 2 with enough text",
		"PHP Fatal error: old plugin failure",
		"recent context before fatal",
		"PHP Fatal error: latest plugin failure",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	lines, truncated, err := aiTailInterestingLines(path, 5, 70, false)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("expected truncation")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "latest plugin failure") {
		t.Fatalf("expected latest fatal to be kept, got:\n%s", joined)
	}
	if strings.Contains(joined, "old line 1") {
		t.Fatalf("expected oldest line to be dropped, got:\n%s", joined)
	}
}
