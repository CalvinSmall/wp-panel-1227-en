package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/naibabiji/wp-panel/database"
)

const nginxRealIPPath = "/etc/nginx/conf.d/wppanel-realip.conf"

func EnsureCloudflareRealIPConfig() error {
	exists := false
	if _, err := os.Stat(nginxRealIPPath); err == nil {
		exists = true
		if strings.TrimSpace(cachedCloudflareRealIPRanges()) != "" {
			return nil
		}
	}
	cfIPs, err := fetchCloudflareIPs()
	if err != nil {
		return err
	}
	cacheCloudflareRealIPRanges(cfIPs)
	if exists {
		return nil
	}
	return DeployCloudflareRealIPConfig(cfIPs)
}

func DeployCloudflareRealIPConfig(cfIPs []string) error {
	if len(cfIPs) == 0 {
		return fmt.Errorf("cloudflare IP list is empty")
	}
	if err := os.MkdirAll(filepath.Dir(nginxRealIPPath), 0755); err != nil {
		return err
	}

	content := renderCloudflareRealIPConfig(cfIPs)
	if old, err := os.ReadFile(nginxRealIPPath); err == nil && string(old) == content {
		return nil
	}
	oldContent, oldErr := os.ReadFile(nginxRealIPPath)
	hadOld := oldErr == nil

	tmpPath := nginxRealIPPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, nginxRealIPPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if out, err := executeCommand("nginx", "-t"); err != nil {
		if hadOld {
			_ = os.WriteFile(nginxRealIPPath, oldContent, 0644)
		} else {
			_ = os.Remove(nginxRealIPPath)
		}
		return fmt.Errorf("nginx -t failed: %s", out)
	}
	if out, err := executeCommand("nginx", "-s", "reload"); err != nil {
		return fmt.Errorf("nginx reload failed: %s", out)
	}
	return nil
}

func cacheCloudflareRealIPRanges(cfIPs []string) {
	if database.GetDB() == nil {
		return
	}
	database.GetDB().Exec(`UPDATE security_settings SET svalue = ?, updated_at = CURRENT_TIMESTAMP WHERE skey = 'cloudflare_realip_ips'`, strings.Join(cfIPs, "\n"))
}

func renderCloudflareRealIPConfig(cfIPs []string) string {
	seen := make(map[string]bool)
	var ips []string
	for _, ip := range cfIPs {
		ip = strings.TrimSpace(ip)
		if ip == "" || seen[ip] || !isValidIPOrCIDR(ip) {
			continue
		}
		seen[ip] = true
		ips = append(ips, ip)
	}
	sort.Strings(ips)

	var b strings.Builder
	b.WriteString("# WP Panel Generated - Cloudflare real client IP\n")
	b.WriteString("# Trust only official Cloudflare proxy ranges.\n")
	for _, ip := range ips {
		b.WriteString("set_real_ip_from ")
		b.WriteString(ip)
		b.WriteString(";\n")
	}
	b.WriteString("real_ip_header CF-Connecting-IP;\n")
	b.WriteString("real_ip_recursive on;\n")
	return b.String()
}
