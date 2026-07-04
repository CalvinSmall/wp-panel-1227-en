package handlers

import (
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/executor"
	"github.com/naibabiji/wp-panel/models"

	"github.com/gin-gonic/gin"
)

type AlertHandler struct{}

func (h *AlertHandler) GetSettings(c *gin.Context) {
	db := database.GetDB()
	rows, err := db.Query("SELECT id, skey, svalue, description, updated_at FROM security_settings WHERE skey LIKE 'alert_%' OR skey LIKE 'smtp_%' OR skey = 'admin_email' OR skey LIKE 'webhook_%'")
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("query failed"))
		return
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var id int
		var key, val, desc, updated string
		rows.Scan(&id, &key, &val, &desc, &updated)
		settings[key] = val
	}
	c.JSON(http.StatusOK, models.SuccessResponse(settings))
}

func (h *AlertHandler) SaveSettings(c *gin.Context) {
	var raw map[string]interface{}
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("invalid parameters"))
		return
	}

	db := database.GetDB()
	for key, val := range raw {
		strVal, ok, err := normalizeAlertSetting(key, val)
		if err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
			return
		}
		if !ok {
			continue
		}
		if _, err := db.Exec("INSERT INTO security_settings (skey, svalue, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP) ON CONFLICT(skey) DO UPDATE SET svalue = excluded.svalue, updated_at = excluded.updated_at", key, strVal); err != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to save alert settings"))
			return
		}
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Saved"}))
}

func normalizeAlertSetting(key string, val interface{}) (string, bool, error) {
	switch key {
	case "smtp_host":
		v, err := normalizePlainString(val, 300, key)
		if err != nil {
			return "", false, err
		}
		if strings.EqualFold(v, "true") || strings.EqualFold(v, "false") {
			return "", false, fmt.Errorf("incorrect SMTP server address")
		}
		return v, true, nil
	case "smtp_user", "smtp_pass":
		v, err := normalizePlainString(val, 300, key)
		return v, true, err
	case "smtp_port":
		v, err := normalizePlainString(val, 10, key)
		if err != nil {
			return "", false, err
		}
		port, err := strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return "", false, fmt.Errorf("incorrect SMTP port")
		}
		return v, true, nil
	case "smtp_encryption":
		v, err := normalizePlainString(val, 20, key)
		if err != nil {
			return "", false, err
		}
		if v != "starttls" && v != "ssl" && v != "none" {
			return "", false, fmt.Errorf("incorrect SMTP encryption method")
		}
		return v, true, nil
	case "admin_email":
		v, err := normalizePlainString(val, 300, key)
		if err != nil {
			return "", false, err
		}
		if v != "" {
			if _, err := mail.ParseAddress(v); err != nil {
				return "", false, fmt.Errorf("incorrect admin email format")
			}
		}
		return v, true, nil
	case "webhook_enabled":
		v, err := normalizeBool(val)
		return v, true, err
	case "webhook_channel":
		v, err := normalizePlainString(val, 30, key)
		if err != nil {
			return "", false, err
		}
		switch v {
		case "wecom", "dingtalk", "feishu", "serverchan", "bark", "custom":
			return v, true, nil
		default:
			return "", false, fmt.Errorf("incorrect webhook channel")
		}
	case "webhook_url":
		v, err := normalizePlainString(val, 1000, key)
		if err != nil {
			return "", false, err
		}
		if v != "" {
			u, err := url.Parse(v)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return "", false, fmt.Errorf("incorrect webhook URL format")
			}
		}
		return v, true, nil
	case "alert_cpu", "alert_memory", "alert_disk", "alert_service", "alert_ssl",
		"alert_backup", "alert_website_expiry", "alert_remote_backup", "alert_cron_fail", "alert_site", "alert_system_update", "alert_panel_update":
		v, err := normalizeBool(val)
		return v, true, err
	default:
		return "", false, nil
	}
}

func normalizePlainString(val interface{}, maxLen int, field string) (string, error) {
	v, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("incorrect %s format", field)
	}
	v = strings.TrimSpace(v)
	if len(v) > maxLen || strings.ContainsAny(v, "\x00\r\n") {
		return "", fmt.Errorf("incorrect %s format", field)
	}
	return v, nil
}

func (h *AlertHandler) TestSMTP(c *gin.Context) {
	var req struct {
		Email string `json:"email"`
	}
	c.ShouldBindJSON(&req)
	if req.Email == "" {
		cfg := executor.GetSMTPConfig()
		if cfg != nil {
			req.Email = cfg.AdminEmail
		}
	}
	if req.Email == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("please enter a test email"))
		return
	}
	if err := executor.TestSMTP(req.Email); err != nil {
		log.Printf("SMTP test send failed: %v", err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to send"))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Test email sent to " + req.Email}))
}

func (h *AlertHandler) TestWebhook(c *gin.Context) {
	var req struct {
		Channel string `json:"channel"`
		URL     string `json:"url"`
	}
	c.ShouldBindJSON(&req)
	if req.Channel == "" || req.URL == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("please specify channel and Webhook URL"))
		return
	}
	if err := executor.TestWebhook(req.Channel, req.URL); err != nil {
		log.Printf("Webhook test send failed: %v", err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to send"))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Test message sent"}))
}

func (h *AlertHandler) GetLog(c *gin.Context) {
	db := database.GetDB()
	rows, err := db.Query("SELECT id, alert_type, level, message, resolved, created_at FROM alert_log ORDER BY id DESC LIMIT 30")
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("query failed"))
		return
	}
	defer rows.Close()

	type logEntry struct {
		ID        int    `json:"id"`
		AlertType string `json:"alert_type"`
		Level     string `json:"level"`
		Message   string `json:"message"`
		Resolved  bool   `json:"resolved"`
		CreatedAt string `json:"created_at"`
	}
	var logs []logEntry
	for rows.Next() {
		var e logEntry
		var r int
		if rows.Scan(&e.ID, &e.AlertType, &e.Level, &e.Message, &r, &e.CreatedAt) == nil {
			e.Resolved = r == 1
			logs = append(logs, e)
		}
	}
	if logs == nil {
		logs = []logEntry{}
	}
	c.JSON(http.StatusOK, models.SuccessResponse(logs))
}
