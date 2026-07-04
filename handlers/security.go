package handlers

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/executor"
	"github.com/naibabiji/wp-panel/models"

	"github.com/gin-gonic/gin"
)

type SecurityHandler struct{}

var (
	applyFail2banSettings             = executor.ApplyFail2banSettings
	regenerateAllSitesNginx           = executor.RegenerateAllSitesNginx
	websiteIDsForCDNRealIPGroup       = executor.WebsiteIDsForCDNRealIPGroup
	restoreCDNRealIPGroupWithBindings = executor.RestoreCDNRealIPGroupWithBindings
)

func (h *SecurityHandler) GetSettings(c *gin.Context) {
	db := database.GetDB()
	rows, err := db.Query("SELECT id, skey, svalue, description, updated_at FROM security_settings")
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Query failed"))
		return
	}
	defer rows.Close()

	var settings []models.SecuritySetting
	for rows.Next() {
		var s models.SecuritySetting
		if err := rows.Scan(&s.ID, &s.Key, &s.Value, &s.Description, &s.UpdatedAt); err != nil {
			continue
		}
		settings = append(settings, s)
	}
	if settings == nil {
		settings = []models.SecuritySetting{}
	}

	c.JSON(http.StatusOK, models.SuccessResponse(settings))
}

func (h *SecurityHandler) UpdateSettings(c *gin.Context) {
	var raw map[string]interface{}
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	db := database.GetDB()

	normalized := make(map[string]string)
	for key, val := range raw {
		strVal, ok, err := normalizeSecuritySetting(key, val)
		if err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
			return
		}
		if !ok {
			continue
		}
		normalized[key] = strVal
	}

	var oldWPSecurityWhitelist string
	if newVal, ok := normalized["wp_security_log_whitelist"]; ok {
		_ = db.QueryRow("SELECT svalue FROM security_settings WHERE skey = 'wp_security_log_whitelist'").Scan(&oldWPSecurityWhitelist)
		if _, err := db.Exec("UPDATE security_settings SET svalue = ?, updated_at = CURRENT_TIMESTAMP WHERE skey = 'wp_security_log_whitelist'", newVal); err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to save security settings"))
			return
		}
		if err := executor.EnsureLogMap(); err != nil {
			_, _ = db.Exec("UPDATE security_settings SET svalue = ?, updated_at = CURRENT_TIMESTAMP WHERE skey = 'wp_security_log_whitelist'", oldWPSecurityWhitelist)
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to apply Nginx log rules. Whitelist settings have been rolled back: "+err.Error()))
			return
		}
		delete(normalized, "wp_security_log_whitelist")
	}

	for key, strVal := range normalized {
		if _, err := db.Exec("UPDATE security_settings SET svalue = ?, updated_at = CURRENT_TIMESTAMP WHERE skey = ?", strVal, key); err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to save security settings"))
			return
		}
	}

	if needsFail2banApply(normalized) {
		if err := applyFail2banSettings(); err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to apply Fail2ban configuration: "+err.Error()))
			return
		}
	}
	if needsRateLimitApply(normalized) {
		if err := applyRateLimit(); err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to apply Nginx rate limit configuration: "+err.Error()))
			return
		}
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Security settings updated"}))
}

func needsFail2banApply(settings map[string]string) bool {
	for _, key := range []string{"fail2ban_maxretry", "fail2ban_findtime", "fail2ban_bantime", "auto_whitelist_enabled", "whitelist_ips"} {
		if _, ok := settings[key]; ok {
			return true
		}
	}
	return false
}

func needsRateLimitApply(settings map[string]string) bool {
	for _, key := range []string{"rate_limit_enabled", "rate_limit_rpm", "rate_limit_burst", "bot_limit_enabled", "bot_limit_rpm", "bot_limit_burst"} {
		if _, ok := settings[key]; ok {
			return true
		}
	}
	return false
}

func (h *SecurityHandler) RefreshWhitelist(c *gin.Context) {
	executor.GoSafe(refreshOfficialWhitelist)

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Whitelist refresh task submitted"}))
}

func (h *SecurityHandler) ListCDNRealIPGroups(c *gin.Context) {
	groups, err := executor.ListCDNRealIPGroups()
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to query CDN configuration groups"))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(groups))
}

func (h *SecurityHandler) CreateCDNRealIPGroup(c *gin.Context) {
	var req struct {
		Name        string `json:"name"`
		HeaderName  string `json:"header_name"`
		IPRanges    string `json:"ip_ranges"`
		Enabled     *bool  `json:"enabled"`
		Description string `json:"description"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}

	name, header, ranges, enabled, desc, err := normalizeCDNRealIPGroupPayload(req.Name, req.HeaderName, req.IPRanges, req.Enabled, req.Description)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
		return
	}
	res, err := database.GetDB().Exec(`INSERT INTO cdn_realip_groups (name, provider, header_name, ip_ranges, builtin, enabled, description)
		VALUES (?, 'custom', ?, ?, 0, ?, ?)`, name, header, ranges, boolToInt(enabled), desc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to create CDN configuration group"))
		return
	}
	if err := applyFail2banSettings(); err != nil {
		id, idErr := res.LastInsertId()
		if idErr != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group created, but Fail2ban whitelist application failed and database rollback failed: "+idErr.Error()+"；original error: "+err.Error()))
			return
		}
		if _, rollbackErr := database.GetDB().Exec(`DELETE FROM cdn_realip_groups WHERE id = ? AND builtin = 0`, id); rollbackErr != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group created, but Fail2ban whitelist application failed and database rollback failed: "+rollbackErr.Error()+"；original error: "+err.Error()))
			return
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group was not created. Fail2ban whitelist application failed, rolled back: "+err.Error()))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "CDN configuration group created"}))
}

func (h *SecurityHandler) UpdateCDNRealIPGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid configuration group ID"))
		return
	}
	group, err := executor.GetCDNRealIPGroup(id)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("CDN configuration group not found"))
		return
	}
	if group.Builtin {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Built-in CDN configuration groups cannot be modified"))
		return
	}

	var req struct {
		Name        string `json:"name"`
		HeaderName  string `json:"header_name"`
		IPRanges    string `json:"ip_ranges"`
		Enabled     *bool  `json:"enabled"`
		Description string `json:"description"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid parameters"))
		return
	}
	name, header, ranges, enabled, desc, err := normalizeCDNRealIPGroupPayload(req.Name, req.HeaderName, req.IPRanges, req.Enabled, req.Description)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
		return
	}
	if _, err := database.GetDB().Exec(`UPDATE cdn_realip_groups
		SET name = ?, header_name = ?, ip_ranges = ?, enabled = ?, description = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, name, header, ranges, boolToInt(enabled), desc, id); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to save CDN configuration group"))
		return
	}
	if err := applyFail2banSettings(); err != nil {
		if _, rollbackErr := database.GetDB().Exec(`UPDATE cdn_realip_groups
			SET name = ?, header_name = ?, ip_ranges = ?, enabled = ?, description = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`, group.Name, group.HeaderName, group.IPRanges, boolToInt(group.Enabled), group.Description, id); rollbackErr != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group saved, but Fail2ban whitelist application failed and database rollback failed: "+rollbackErr.Error()+"；original error: "+err.Error()))
			return
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group did not take effect. Fail2ban whitelist application failed, rolled back: "+err.Error()))
		return
	}
	if err := regenerateAllSitesNginx(); err != nil {
		if _, rollbackErr := database.GetDB().Exec(`UPDATE cdn_realip_groups
			SET name = ?, header_name = ?, ip_ranges = ?, enabled = ?, description = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ?`, group.Name, group.HeaderName, group.IPRanges, boolToInt(group.Enabled), group.Description, id); rollbackErr != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group saved, but some site Nginx configuration update failed and database rollback failed: "+rollbackErr.Error()+"；original error: "+err.Error()))
			return
		}
		if rollbackErr := reapplyCDNRealIPRuntime(); rollbackErr != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group saved, but some site Nginx configuration update failed and rollback also failed: "+rollbackErr.Error()+"；original error: "+err.Error()))
			return
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group did not take effect. Some site Nginx configuration update failed, rolled back: "+err.Error()))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "CDN configuration group saved"}))
}

func (h *SecurityHandler) DeleteCDNRealIPGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Invalid configuration group ID"))
		return
	}
	group, err := executor.GetCDNRealIPGroup(id)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("CDN configuration group not found"))
		return
	}
	if group.Builtin {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("Built-in CDN configuration groups cannot be deleted"))
		return
	}
	boundWebsiteIDs, err := websiteIDsForCDNRealIPGroup(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to read CDN configuration group website bindings"))
		return
	}
	if _, err := database.GetDB().Exec(`DELETE FROM cdn_realip_groups WHERE id = ?`, id); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Failed to delete CDN configuration group"))
		return
	}
	if err := applyFail2banSettings(); err != nil {
		if restoreErr := restoreCDNRealIPGroupWithBindings(group, boundWebsiteIDs); restoreErr != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group deleted, but Fail2ban whitelist application failed and database rollback failed: "+restoreErr.Error()+"；original error: "+err.Error()))
			return
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group was not deleted. Fail2ban whitelist application failed, rolled back: "+err.Error()))
		return
	}
	if err := regenerateAllSitesNginx(); err != nil {
		if restoreErr := restoreCDNRealIPGroupWithBindings(group, boundWebsiteIDs); restoreErr != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group deleted, but some site Nginx configuration update failed and database rollback failed: "+restoreErr.Error()+"；original error: "+err.Error()))
			return
		}
		if rollbackErr := reapplyCDNRealIPRuntime(); rollbackErr != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group deleted, but some site Nginx configuration update failed and rollback also failed: "+rollbackErr.Error()+"；original error: "+err.Error()))
			return
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("CDN configuration group was not deleted. Some site Nginx configuration update failed, rolled back: "+err.Error()))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "CDN configuration group deleted"}))
}

func reapplyCDNRealIPRuntime() error {
	if err := applyFail2banSettings(); err != nil {
		return fmt.Errorf("Fail2ban rollback failed: %w", err)
	}
	if err := regenerateAllSitesNginx(); err != nil {
		return fmt.Errorf("Nginx rollback failed: %w", err)
	}
	return nil
}

func refreshOfficialWhitelist() {
	executor.GlobalQueue.Enqueue(executor.TaskRefreshWhitelist, nil)
}

func normalizeCDNRealIPGroupPayload(name, headerName, rawRanges string, enabled *bool, description string) (string, string, string, bool, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 50 || strings.ContainsAny(name, "\r\n\t") {
		return "", "", "", false, "", fmt.Errorf("Invalid CDN configuration group name format")
	}
	header, err := executor.NormalizeCDNRealIPHeader(headerName)
	if err != nil {
		return "", "", "", false, "", err
	}
	ranges, err := executor.NormalizeCDNRealIPRanges(rawRanges)
	if err != nil {
		return "", "", "", false, "", err
	}
	isEnabled := true
	if enabled != nil {
		isEnabled = *enabled
	}
	description = strings.TrimSpace(description)
	if len(description) > 200 {
		return "", "", "", false, "", fmt.Errorf("Description is too long")
	}
	return name, header, executor.JoinCDNRealIPRanges(ranges), isEnabled, description, nil
}

func applyRateLimit() error {
	return executor.ApplyRateLimitSettings()
}

func normalizeSecuritySetting(key string, val interface{}) (string, bool, error) {
	switch key {
	case "fail2ban_maxretry":
		return normalizeRange(key, val, 1, 20)
	case "fail2ban_findtime":
		return normalizeRange(key, val, 10, 3600)
	case "fail2ban_bantime":
		return normalizeRange(key, val, 60, 86400)
	case "rate_limit_rpm":
		return normalizeRange(key, val, 10, 600)
	case "rate_limit_burst":
		return normalizeRange(key, val, 5, 600)
	case "bot_limit_rpm":
		return normalizeRange(key, val, 5, 300)
	case "bot_limit_burst":
		return normalizeRange(key, val, 5, 300)
	case "auto_whitelist_enabled", "rate_limit_enabled", "bot_limit_enabled":
		v, err := normalizeBool(val)
		return v, true, err
	case "whitelist_ips":
		v, ok := val.(string)
		if !ok {
			return "", false, fmt.Errorf("Invalid whitelist format")
		}
		v = strings.TrimSpace(v)
		if err := validateWhitelistIPs(v); err != nil {
			return "", false, err
		}
		return v, true, nil
	case "wp_security_log_whitelist":
		v, ok := val.(string)
		if !ok {
			return "", false, fmt.Errorf("WordPress security log whitelist format invalid")
		}
		patterns, err := executor.NormalizeWPSecurityLogWhitelist(v)
		if err != nil {
			return "", false, err
		}
		return strings.Join(patterns, "\n"), true, nil
	default:
		return "", false, nil
	}
}

func normalizeRange(key string, val interface{}, min int, max int) (string, bool, error) {
	n, err := normalizeInt(val)
	if err != nil {
		return "", false, fmt.Errorf("%s must be a number", key)
	}
	if n < min || n > max {
		return "", false, fmt.Errorf("%s must be between %d and %d", key, min, max)
	}
	return strconv.Itoa(n), true, nil
}

func normalizeInt(val interface{}) (int, error) {
	switch v := val.(type) {
	case string:
		return strconv.Atoi(strings.TrimSpace(v))
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Errorf("invalid int")
		}
		return int(v), nil
	default:
		return 0, fmt.Errorf("invalid int")
	}
}

func normalizeBool(val interface{}) (string, error) {
	switch v := val.(type) {
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case string:
		v = strings.TrimSpace(v)
		if v == "true" || v == "false" {
			return v, nil
		}
	}
	return "", fmt.Errorf("Invalid toggle value")
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func validateWhitelistIPs(raw string) error {
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > 500 {
		return fmt.Errorf("Whitelist is too large")
	}
	for _, line := range lines {
		item := strings.TrimSpace(line)
		if item == "" {
			continue
		}
		if strings.ContainsAny(item, " \t\r") {
			return fmt.Errorf("Invalid whitelist entry: %s", item)
		}
		if strings.Contains(item, "/") {
			if _, _, err := net.ParseCIDR(item); err != nil {
				return fmt.Errorf("Invalid whitelist entry: %s", item)
			}
			continue
		}
		if net.ParseIP(item) == nil {
			return fmt.Errorf("Invalid whitelist entry: %s", item)
		}
	}
	return nil
}
