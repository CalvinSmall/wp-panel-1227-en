package executor

import (
	"path/filepath"
	"testing"

	"github.com/naibabiji/wp-panel/config"
)

func TestBuildSiteNameAvoidsSeparatorCollisions(t *testing.T) {
	first := buildSiteName("ab.com")
	second := buildSiteName("a-b.com")
	if first == second {
		t.Fatalf("expected distinct resource names, got %q", first)
	}
}

func TestBuildSiteNameNormalizesEquivalentDomains(t *testing.T) {
	first := buildSiteName("Example.COM")
	second := buildSiteName(" example.com. ")
	if first != second {
		t.Fatalf("expected normalized domains to match, got %q and %q", first, second)
	}
}

func TestBuildSiteNameFitsSystemUserLimit(t *testing.T) {
	name := buildSiteName("this-is-a-very-long-domain-name-that-should-be-truncated.example.com")
	if len(name) > 27 {
		t.Fatalf("site name %q length = %d, want <= 27", name, len(name))
	}
	if len("php_"+name) > 32 {
		t.Fatalf("php user name length = %d, want <= 32", len("php_"+name))
	}
}

func TestShortResourcePathsFitLongDomain(t *testing.T) {
	domain := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.ccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc.example.com"
	siteName := buildSiteName(domain)
	cfg := &config.Config{}
	cfg.Paths.PHPFPMSock = "/run/php"

	phpPoolPath := filepath.Join("/etc/php/8.3/fpm/pool.d", siteConfigBaseName(siteName)+".conf")
	if len(filepath.Base(phpPoolPath)) > 255 {
		t.Fatalf("php pool filename length = %d, want <= 255", len(filepath.Base(phpPoolPath)))
	}
	if err := validateUnixSocketPath(phpSocketPath(cfg, phpPoolPath, domain)); err != nil {
		t.Fatal(err)
	}
}

func TestPHPPoolNamePreservesLegacyDomainPool(t *testing.T) {
	got := phpPoolName("/etc/php/8.3/fpm/pool.d/example.com.conf", "new.example.com")
	if got != "example.com" {
		t.Fatalf("expected legacy pool name to be preserved, got %q", got)
	}
}

func TestNginxEnabledPathUsesStoredConfigFilename(t *testing.T) {
	cfg := &config.Config{}
	cfg.Paths.NginxSitesEnabled = "/etc/nginx/sites-enabled"

	got := nginxEnabledPath(cfg, "/etc/nginx/sites-available/vps17_top_0ea8c86a.conf", "vps17.top")
	want := filepath.Join("/etc/nginx/sites-enabled", "vps17_top_0ea8c86a.conf")
	if got != want {
		t.Fatalf("nginxEnabledPath() = %q, want %q", got, want)
	}
}

func TestIsValidDomainRejectsLongLabel(t *testing.T) {
	domain := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com"
	if IsValidDomain(domain) {
		t.Fatalf("expected long domain label to be rejected")
	}
}
