package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/executor"
	"github.com/naibabiji/wp-panel/models"

	"github.com/gin-gonic/gin"
)

// aiDiagnosisMu prevents concurrent diagnoses for the same site within one process.
var aiDiagnosisMu sync.Map

type AIHandler struct{}

func (h *AIHandler) GetSettings(c *gin.Context) {
	settings, err := loadAISettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("读取 AI 设置失败"))
		return
	}
	settings.APIKey = ""
	settings.APIKeyMasked = maskAIKey(settings.APIKeyMasked)
	c.JSON(http.StatusOK, models.SuccessResponse(settings))
}

func (h *AIHandler) SaveSettings(c *gin.Context) {
	var req models.AISettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("参数错误"))
		return
	}
	settings, err := normalizeAISettingsRequest(req, true)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
		return
	}
	if err := saveAISettings(settings); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("保存 AI 设置失败"))
		return
	}
	settings.APIKey = ""
	settings.APIKeyMasked = maskAIKey(settings.APIKeyMasked)
	c.JSON(http.StatusOK, models.SuccessResponse(settings))
}

func (h *AIHandler) Test(c *gin.Context) {
	var req models.AITestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("参数错误"))
		return
	}
	settings, err := loadAISettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("读取 AI 设置失败"))
		return
	}
	if strings.TrimSpace(req.Provider) != "" || strings.TrimSpace(req.BaseURL) != "" || strings.TrimSpace(req.Model) != "" || strings.TrimSpace(req.APIKey) != "" || req.TimeoutSeconds > 0 {
		tmp := models.AISettingsRequest{
			Enabled:        true,
			Provider:       req.Provider,
			BaseURL:        req.BaseURL,
			Model:          req.Model,
			APIKey:         req.APIKey,
			TimeoutSeconds: req.TimeoutSeconds,
		}
		normalized, err := normalizeAISettingsRequest(tmp, false)
		if err != nil {
			c.JSON(http.StatusBadRequest, models.ErrorResponse(err.Error()))
			return
		}
		if normalized.APIKey == "" {
			normalized.APIKey = settings.APIKey
		}
		settings = normalized
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(settings.TimeoutSeconds)*time.Second)
	defer cancel()
	elapsed, msg, err := executor.TestAISettings(ctx, settings)
	if err != nil {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
			"ok":         false,
			"provider":   settings.Provider,
			"model":      settings.Model,
			"latency_ms": elapsed,
			"message":    aiUserError(err),
		}))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"ok":         true,
		"provider":   settings.Provider,
		"model":      settings.Model,
		"latency_ms": elapsed,
		"message":    msg,
	}))
}

func (h *AIHandler) Diagnose(c *gin.Context) {
	id, err := parseSiteID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("无效的网站ID"))
		return
	}
	var req models.AIDiagnoseRequest
	if err := c.ShouldBindJSON(&req); err != nil || !models.IsValidAIDiagnosisSymptom(req.Symptom) {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("诊断类型无效"))
		return
	}

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("网站不存在"))
		return
	}

	settings, err := loadAISettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("读取 AI 设置失败"))
		return
	}
	if !settings.Enabled {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("AI 诊断未启用，请先在面板设置中配置"))
		return
	}
	if strings.TrimSpace(settings.APIKey) == "" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("请先配置 AI API Key"))
		return
	}

	// Mark sessions left in 'running' by a previous process restart as failed.
	_, _ = database.GetDB().Exec(
		`UPDATE ai_sessions SET status = ?, error_message = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE site_id = ? AND status = ? AND updated_at <= datetime('now', '-10 minutes')`,
		models.AISessionFailed, "进程重启，会话已中断", site.ID, models.AISessionRunning,
	)

	// Prevent concurrent diagnoses for the same site within this process.
	if _, loaded := aiDiagnosisMu.LoadOrStore(site.ID, struct{}{}); loaded {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
			"status":  models.AISessionRunning,
			"message": "该网站已有 AI 诊断正在进行，请稍后刷新历史记录",
		}))
		return
	}
	defer aiDiagnosisMu.Delete(site.ID)

	// Also block if a running session exists from a different process.
	if running, ok := activeAISession(site.ID); ok {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
			"session_id": running.ID,
			"status":     running.Status,
			"message":    "该网站已有 AI 诊断正在进行，请稍后刷新历史记录",
		}))
		return
	}

	sessionID, err := createAISession(site.ID, req.Symptom)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("创建诊断记录失败"))
		return
	}
	updateAISessionStatus(sessionID, models.AISessionRunning, "")

	systemPrompt, userPrompt, err := executor.BuildAIDiagnosticPrompt(site, req.Symptom)
	if err != nil {
		failAISession(sessionID, err.Error(), len(userPrompt), 0)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("诊断上下文收集失败"))
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(settings.TimeoutSeconds)*time.Second)
	defer cancel()
	content, _, err := executor.CallAIChat(ctx, settings, systemPrompt, userPrompt)
	if err != nil {
		msg := aiUserError(err)
		failAISession(sessionID, msg, len(userPrompt), 0)
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
			"session_id":    sessionID,
			"status":        models.AISessionFailed,
			"error_message": msg,
		}))
		return
	}

	report, rawText, ok := executor.ParseAIReport(content)
	reportJSON := ""
	summary := ""
	riskLevel := ""
	if ok && report != nil {
		data, _ := json.Marshal(report)
		reportJSON = string(data)
		summary = strings.TrimSpace(report.Summary)
		riskLevel = strings.TrimSpace(report.RiskLevel)
	} else {
		rawText = content
		summary = excerpt(content, 500)
	}
	if summary == "" {
		summary = "AI 已返回诊断结果"
	}
	if err := completeAISession(sessionID, riskLevel, summary, reportJSON, rawText, len(userPrompt), len(content)); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("保存诊断结果失败"))
		return
	}
	pruneAISessions(site.ID, 20)

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"session_id": sessionID,
		"status":     models.AISessionCompleted,
		"report":     report,
		"raw_text":   rawText,
	}))
}

func (h *AIHandler) ListSessions(c *gin.Context) {
	id, err := parseSiteID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("无效的网站ID"))
		return
	}
	if getWebsiteByID(id) == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("网站不存在"))
		return
	}
	rows, err := database.GetDB().Query(`SELECT id, site_id, symptom, status, risk_level, summary, error_message, created_at, updated_at
		FROM ai_sessions WHERE site_id = ? ORDER BY created_at DESC LIMIT 10`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("查询 AI 诊断记录失败"))
		return
	}
	defer rows.Close()
	var items []models.AISessionSummary
	for rows.Next() {
		var item models.AISessionSummary
		var summary string
		if err := rows.Scan(&item.ID, &item.SiteID, &item.Symptom, &item.Status, &item.RiskLevel, &summary, &item.ErrorMessage, &item.CreatedAt, &item.UpdatedAt); err != nil {
			continue
		}
		item.SummaryExcerpt = excerpt(summary, 160)
		items = append(items, item)
	}
	if items == nil {
		items = []models.AISessionSummary{}
	}
	c.JSON(http.StatusOK, models.SuccessResponse(items))
}

func (h *AIHandler) GetSession(c *gin.Context) {
	siteID, err := parseSiteID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("无效的网站ID"))
		return
	}
	sessionID, err := parseSessionID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("无效的诊断记录ID"))
		return
	}
	var detail models.AISessionDetail
	var reportJSON string
	err = database.GetDB().QueryRow(`SELECT id, site_id, symptom, status, risk_level, summary, report_json, raw_text, error_message, prompt_chars, response_chars, created_at, updated_at
		FROM ai_sessions WHERE id = ? AND site_id = ?`, sessionID, siteID).Scan(
		&detail.ID, &detail.SiteID, &detail.Symptom, &detail.Status, &detail.RiskLevel,
		&detail.Summary, &reportJSON, &detail.RawText, &detail.ErrorMessage,
		&detail.PromptChars, &detail.ResponseChars, &detail.CreatedAt, &detail.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, models.ErrorResponse("诊断记录不存在"))
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("查询 AI 诊断记录失败"))
		return
	}
	if strings.TrimSpace(reportJSON) != "" {
		var report models.AIDiagnosticReport
		if json.Unmarshal([]byte(reportJSON), &report) == nil {
			detail.Report = &report
		}
	}
	c.JSON(http.StatusOK, models.SuccessResponse(detail))
}

func parseSiteID(c *gin.Context) (int, error) {
	return strconvAtoi(c.Param("id"))
}

func parseSessionID(c *gin.Context) (int, error) {
	return strconvAtoi(c.Param("session_id"))
}

func strconvAtoi(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New("empty id")
	}
	var id int
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return 0, errors.New("invalid id")
		}
		id = id*10 + int(ch-'0')
	}
	if id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func loadAISettings() (*models.AISettings, error) {
	db := database.GetDB()
	_, _ = db.Exec("INSERT OR IGNORE INTO ai_settings (id) VALUES (1)")
	var enabled int
	var settings models.AISettings
	err := db.QueryRow(`SELECT enabled, provider, base_url, model, api_key, timeout_seconds, created_at, updated_at
		FROM ai_settings WHERE id = 1`).Scan(
		&enabled, &settings.Provider, &settings.BaseURL, &settings.Model, &settings.APIKey,
		&settings.TimeoutSeconds, &settings.CreatedAt, &settings.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	settings.Enabled = enabled == 1
	settings.APIKeyMasked = settings.APIKey
	return &settings, nil
}

func normalizeAISettingsRequest(req models.AISettingsRequest, preserveExistingKey bool) (*models.AISettings, error) {
	provider := strings.TrimSpace(req.Provider)
	if provider == "" {
		provider = "deepseek"
	}
	if provider != "deepseek" && provider != "openai" && provider != "openai_compatible" {
		return nil, errors.New("AI 服务商无效")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if baseURL == "" {
		if provider == "deepseek" {
			baseURL = "https://api.deepseek.com"
		} else {
			baseURL = "https://api.openai.com/v1"
		}
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		if provider == "deepseek" {
			model = "deepseek-v4-pro"
		} else {
			model = "gpt-4.1-mini"
		}
	}
	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = 60
	}
	if timeout < 15 {
		timeout = 15
	}
	if timeout > 120 {
		timeout = 120
	}
	apiKey := strings.TrimSpace(req.APIKey)
	if preserveExistingKey && (apiKey == "" || strings.Contains(apiKey, "...")) {
		if current, err := loadAISettings(); err == nil {
			apiKey = current.APIKey
		}
	}
	return &models.AISettings{
		Enabled:        req.Enabled,
		Provider:       provider,
		BaseURL:        baseURL,
		Model:          model,
		APIKey:         apiKey,
		APIKeyMasked:   apiKey,
		TimeoutSeconds: timeout,
	}, nil
}

func saveAISettings(settings *models.AISettings) error {
	enabled := 0
	if settings.Enabled {
		enabled = 1
	}
	res, err := database.GetDB().Exec(`UPDATE ai_settings
		SET enabled = ?, provider = ?, base_url = ?, model = ?, api_key = ?, timeout_seconds = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = 1`, enabled, settings.Provider, settings.BaseURL, settings.Model, settings.APIKey, settings.TimeoutSeconds)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := database.GetDB().Exec("INSERT OR IGNORE INTO ai_settings (id) VALUES (1)"); err != nil {
			return err
		}
		_, err = database.GetDB().Exec(`UPDATE ai_settings
			SET enabled = ?, provider = ?, base_url = ?, model = ?, api_key = ?, timeout_seconds = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = 1`, enabled, settings.Provider, settings.BaseURL, settings.Model, settings.APIKey, settings.TimeoutSeconds)
		return err
	}
	return nil
}

func createAISession(siteID int, symptom string) (int, error) {
	res, err := database.GetDB().Exec(`INSERT INTO ai_sessions (site_id, symptom, status) VALUES (?, ?, ?)`,
		siteID, symptom, models.AISessionPending)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return int(id), err
}

func activeAISession(siteID int) (models.AISessionSummary, bool) {
	var item models.AISessionSummary
	err := database.GetDB().QueryRow(`SELECT id, site_id, symptom, status, risk_level, summary, error_message, created_at, updated_at
		FROM ai_sessions
		WHERE site_id = ? AND status = ? AND updated_at > datetime('now', '-10 minutes')
		ORDER BY updated_at DESC LIMIT 1`, siteID, models.AISessionRunning).Scan(
		&item.ID, &item.SiteID, &item.Symptom, &item.Status, &item.RiskLevel,
		&item.SummaryExcerpt, &item.ErrorMessage, &item.CreatedAt, &item.UpdatedAt,
	)
	return item, err == nil
}

func updateAISessionStatus(sessionID int, status, message string) {
	_, _ = database.GetDB().Exec(`UPDATE ai_sessions SET status = ?, error_message = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		status, message, sessionID)
}

func failAISession(sessionID int, message string, promptChars, responseChars int) {
	_, _ = database.GetDB().Exec(`UPDATE ai_sessions
		SET status = ?, error_message = ?, prompt_chars = ?, response_chars = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, models.AISessionFailed, message, promptChars, responseChars, sessionID)
}

func completeAISession(sessionID int, riskLevel, summary, reportJSON, rawText string, promptChars, responseChars int) error {
	_, err := database.GetDB().Exec(`UPDATE ai_sessions
		SET status = ?, risk_level = ?, summary = ?, report_json = ?, raw_text = ?, prompt_chars = ?, response_chars = ?, error_message = '', updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, models.AISessionCompleted, riskLevel, summary, reportJSON, rawText, promptChars, responseChars, sessionID)
	return err
}

func pruneAISessions(siteID, keep int) {
	_, _ = database.GetDB().Exec(`DELETE FROM ai_sessions
		WHERE site_id = ? AND id NOT IN (
			SELECT id FROM ai_sessions WHERE site_id = ? ORDER BY created_at DESC LIMIT ?
		)`, siteID, siteID, keep)
}

func maskAIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

func excerpt(s string, max int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

func aiUserError(err error) string {
	var providerErr *executor.AIProviderError
	if errors.As(err, &providerErr) {
		switch providerErr.Type {
		case "unauthorized":
			return "AI 服务认证失败，请检查 API Key 和模型权限"
		case "rate_limited":
			return "AI 服务返回请求过多或额度限制，请稍后重试或检查服务商后台"
		case "timeout":
			return "AI 服务请求超时，请稍后重试或调大超时时间"
		case "network_error":
			return "无法连接 AI 服务，请检查服务器网络或 Base URL"
		case "bad_response":
			return "AI 服务返回格式异常，请检查 Provider 是否兼容 OpenAI Chat Completions"
		case "empty_response":
			return "AI 服务返回空内容"
		}
	}
	if err == nil {
		return ""
	}
	return err.Error()
}
