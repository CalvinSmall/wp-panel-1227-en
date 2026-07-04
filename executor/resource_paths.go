package executor

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/naibabiji/wp-panel/config"
)

const maxUnixSocketPathLen = 107

func siteConfigBaseName(siteName string) string {
	siteName = strings.TrimSpace(siteName)
	if siteName == "" {
		return "site"
	}
	return siteName
}

func pathBaseWithoutExt(path, fallback string) string {
	base := strings.TrimSpace(filepath.Base(path))
	if base == "." || base == string(filepath.Separator) {
		base = ""
	}
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if base == "" {
		base = fallback
	}
	return base
}

func phpPoolName(phpPoolPath, domain string) string {
	return pathBaseWithoutExt(phpPoolPath, buildSiteName(domain))
}

func phpSocketPath(cfg *config.Config, phpPoolPath, domain string) string {
	return filepath.Join(cfg.Paths.PHPFPMSock, phpPoolName(phpPoolPath, domain)+".sock")
}

func nginxEnabledPath(cfg *config.Config, nginxConfPath, domain string) string {
	confName := strings.TrimSpace(filepath.Base(nginxConfPath))
	if confName == "" || confName == "." || confName == string(filepath.Separator) {
		confName = buildSiteName(domain) + ".conf"
	}
	return filepath.Join(cfg.Paths.NginxSitesEnabled, confName)
}

func validateUnixSocketPath(path string) error {
	if len([]byte(path)) > maxUnixSocketPathLen {
		return fmt.Errorf("PHP-FPM socket path too long: %s", path)
	}
	return nil
}
