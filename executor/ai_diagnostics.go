package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/naibabiji/wp-panel/config"
	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/models"
)

const (
	aiMaxPromptChars     = 12000
	aiMaxLogCharsPerFile = 4000
	aiMaxLinesPerLog     = 200
	aiMaxLogReadBytes    = 64 * 1024
)

type AIProviderError struct {
	Type       string
	StatusCode int
	Message    string
}

func (e *AIProviderError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Type
}

type aiChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type aiChatRequest struct {
	Model       string          `json:"model"`
	Messages    []aiChatMessage `json:"messages"`
	Temperature float64         `json:"temperature"`
	Stream      bool            `json:"stream"`
}

type aiChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

type aiDiagnosticContext struct {
	DiagnosisType         string                  `json:"diagnosis_type"`
	DiagnosisLabel        string                  `json:"diagnosis_label"`
	SiteSummary           map[string]interface{}  `json:"site_summary"`
	LocalChecks           map[string]interface{}  `json:"local_checks"`
	RecentPanelOperations []map[string]string     `json:"recent_panel_operations"`
	Logs                  map[string]aiLogSnippet `json:"logs"`
	WPConfigSummary       map[string]interface{}  `json:"wp_config_summary"`
	DBCheck               map[string]interface{}  `json:"db_check"`
	ServiceChecks         map[string]interface{}  `json:"service_checks"`
	Constraints           map[string]interface{}  `json:"constraints"`
	OutputSchema          map[string]interface{}  `json:"output_schema"`
	PromptNotes           []string                `json:"prompt_notes,omitempty"`
}

type aiLogSnippet struct {
	Source    string   `json:"source"`
	Status    string   `json:"status"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
	Message   string   `json:"message,omitempty"`
}

func BuildAIDiagnosticPrompt(site *models.Website, symptom string) (systemPrompt, userPrompt string, err error) {
	if site == nil {
		return "", "", fmt.Errorf("网站不存在")
	}
	if !models.IsValidAIDiagnosisSymptom(symptom) {
		return "", "", fmt.Errorf("诊断类型无效")
	}

	ctx := aiDiagnosticContext{
		DiagnosisType:  symptom,
		DiagnosisLabel: aiDiagnosisLabel(symptom),
		SiteSummary: map[string]interface{}{
			"domain":                site.Domain,
			"aliases":               site.Aliases,
			"site_type":             site.SiteType,
			"status":                site.Status,
			"ssl_enabled":           site.SSLEnabled,
			"ssl_last_error":        site.SSLLastError,
			"fastcgi_cache_enabled": site.FCacheEnabled,
			"fastcgi_cache_ttl":     site.FCacheTTL,
			"monitoring_enabled":    site.MonitoringEnabled,
			"wp_debug_enabled":      site.WPDebugEnabled,
			"xmlrpc_enabled":        site.XMLRPCEnabled,
			"access_log_mode":       site.AccessLogMode,
		},
		Logs: map[string]aiLogSnippet{
			"nginx_error": aiReadLogSnippet(site.LogDir, "error.log"),
			"php_error":   aiReadLogSnippet(site.LogDir, "php-error.log"),
			"wp_security": aiReadLogSnippet(site.LogDir, "wp-security.log"),
			"access_5xx":  aiReadAccess5xxSnippet(site.LogDir),
		},
		WPConfigSummary:       aiWPConfigSummary(site),
		DBCheck:               aiDBCheck(site),
		ServiceChecks:         aiServiceChecks(site),
		RecentPanelOperations: aiRecentPanelOperations(site.Domain, 20),
		Constraints: map[string]interface{}{
			"phase":            "readonly_diagnosis",
			"no_write_actions": true,
			"no_sql_execution": true,
			"no_shell":         true,
		},
		OutputSchema: aiOutputSchema(),
	}
	ctx.LocalChecks = aiLocalChecks(ctx)

	userBytes, err := json.MarshalIndent(ctx, "", "  ")
	if err != nil {
		return "", "", err
	}
	userPrompt = string(userBytes)
	if len(userPrompt) > aiMaxPromptChars {
		ctx.PromptNotes = append(ctx.PromptNotes, fmt.Sprintf("Prompt 超过 %d 字符，日志片段已进一步截断。", aiMaxPromptChars))
		aiShrinkLogs(ctx.Logs, 1200)
		userBytes, _ = json.MarshalIndent(ctx, "", "  ")
		userPrompt = string(userBytes)
	}
	if len(userPrompt) > aiMaxPromptChars {
		ctx.PromptNotes = append(ctx.PromptNotes, "Prompt 仍超过预算，低优先级日志已清空。")
		for _, key := range []string{"wp_security", "access_5xx"} {
			item := ctx.Logs[key]
			item.Lines = nil
			item.Truncated = true
			item.Message = "因上下文预算限制未发送该日志片段"
			ctx.Logs[key] = item
		}
		userBytes, _ = json.MarshalIndent(ctx, "", "  ")
		userPrompt = string(userBytes)
	}

	return aiSystemPrompt(), userPrompt, nil
}

func CallAIChat(ctx context.Context, settings *models.AISettings, systemPrompt, userPrompt string) (string, int64, error) {
	if settings == nil {
		return "", 0, &AIProviderError{Type: "bad_config", Message: "AI 设置不存在"}
	}
	endpoint, err := aiChatEndpoint(settings.BaseURL)
	if err != nil {
		return "", 0, &AIProviderError{Type: "bad_config", Message: err.Error()}
	}
	reqBody := aiChatRequest{
		Model: strings.TrimSpace(settings.Model),
		Messages: []aiChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature: 0.2,
		Stream:      false,
	}
	if reqBody.Model == "" {
		return "", 0, &AIProviderError{Type: "bad_config", Message: "模型不能为空"}
	}
	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, &AIProviderError{Type: "bad_config", Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(settings.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(settings.APIKey))
	}

	timeout := time.Duration(settings.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout") {
			return "", elapsed, &AIProviderError{Type: "timeout", Message: "AI 服务请求超时"}
		}
		return "", elapsed, &AIProviderError{Type: "network_error", Message: "无法连接 AI 服务: " + err.Error()}
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", elapsed, aiHTTPError(resp.StatusCode, respData)
	}

	var parsed aiChatResponse
	if err := json.Unmarshal(respData, &parsed); err != nil {
		return "", elapsed, &AIProviderError{Type: "bad_response", Message: "AI 服务返回格式异常"}
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", elapsed, &AIProviderError{Type: "provider_error", Message: parsed.Error.Message}
	}
	if len(parsed.Choices) == 0 {
		return "", elapsed, &AIProviderError{Type: "bad_response", Message: "AI 服务未返回 choices"}
	}
	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if content == "" {
		return "", elapsed, &AIProviderError{Type: "empty_response", Message: "AI 服务返回空内容"}
	}
	return content, elapsed, nil
}

func TestAISettings(ctx context.Context, settings *models.AISettings) (int64, string, error) {
	system := "你是 WP Panel 的 AI 连接测试助手。"
	user := `请只返回 JSON：{"ok":true}`
	content, elapsed, err := CallAIChat(ctx, settings, system, user)
	if err != nil {
		return elapsed, "", err
	}
	return elapsed, content, nil
}

func ParseAIReport(content string) (*models.AIDiagnosticReport, string, bool) {
	raw := strings.TrimSpace(content)
	var report models.AIDiagnosticReport
	// Try direct parse first (model followed instructions).
	if err := json.Unmarshal([]byte(raw), &report); err == nil {
		return &report, raw, true
	}
	// Extract the outermost JSON object in case the model wrapped it in markdown fences
	// or added preamble/postamble text.
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(raw[start:end+1]), &report); err == nil {
			return &report, raw, true
		}
	}
	return nil, raw, false
}

func aiSystemPrompt() string {
	return strings.Join([]string{
		"你是 WP Panel 的 WordPress 站点级诊断助手。你只能分析输入中的站点摘要、日志摘要和检查结果。",
		"不要声称已经修改服务器。不要建议任意 shell 命令。不要输出需要 root 权限的操作。",
		"不要要求用户提供密码、API Key、SSL 私钥或面板数据库。",
		"对每个结论给出证据，不确定时降低置信度。",
		"请用中文返回 JSON 对象，字段必须包含 summary、risk_level、likely_causes、recommended_actions、needs_more_info、user_friendly_explanation。",
		"不要包含 Markdown 代码块，不要输出 JSON 以外的文字。",
	}, "\n")
}

func aiDiagnosisLabel(symptom string) string {
	switch symptom {
	case models.AIDiagnosisSite500:
		return "网站 500 / 白屏"
	case models.AIDiagnosisWPAdminDown:
		return "后台打不开"
	case models.AIDiagnosisSSLFailure:
		return "SSL 失败"
	case models.AIDiagnosisDBConnection:
		return "数据库连接问题"
	case models.AIDiagnosisCacheIssue:
		return "缓存异常"
	case models.AIDiagnosisPerformance:
		return "网站速度慢"
	default:
		return symptom
	}
}

func aiOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"summary":    "string",
		"risk_level": "low|medium|high",
		"likely_causes": []map[string]interface{}{{
			"title":      "string",
			"confidence": "low|medium|high",
			"evidence":   []string{"string"},
		}},
		"recommended_actions": []map[string]interface{}{{
			"label":             "string",
			"description":       "string",
			"risk":              "low|medium|high",
			"manual_steps":      []string{"string"},
			"panel_action_hint": "string",
		}},
		"needs_more_info":           false,
		"user_friendly_explanation": "string",
	}
}

func aiReadLogSnippet(logDir, filename string) aiLogSnippet {
	path := filepath.Join(logDir, filename)
	if _, err := os.Stat(path); err != nil {
		return aiLogSnippet{Source: filename, Status: "not_found", Message: "日志不可读或不存在"}
	}
	if !aiPathWithin(logDir, path) {
		return aiLogSnippet{Source: filename, Status: "forbidden", Message: "日志路径越界"}
	}
	lines, truncated, err := aiTailInterestingLines(path, aiMaxLinesPerLog, aiMaxLogCharsPerFile, false)
	if err != nil {
		return aiLogSnippet{Source: filename, Status: "not_found", Message: "日志不可读或不存在"}
	}
	return aiLogSnippet{Source: filename, Status: "ok", Lines: lines, Truncated: truncated}
}

func aiReadAccess5xxSnippet(logDir string) aiLogSnippet {
	path := filepath.Join(logDir, "access.log")
	if _, err := os.Stat(path); err != nil {
		return aiLogSnippet{Source: "access.log", Status: "not_found", Message: "访问日志不可读或不存在"}
	}
	if !aiPathWithin(logDir, path) {
		return aiLogSnippet{Source: "access.log", Status: "forbidden", Message: "日志路径越界"}
	}
	lines, truncated, err := aiTailInterestingLines(path, aiMaxLinesPerLog, aiMaxLogCharsPerFile, true)
	if err != nil {
		return aiLogSnippet{Source: "access.log", Status: "not_found", Message: "访问日志不可读或不存在"}
	}
	return aiLogSnippet{Source: "access.log", Status: "ok", Lines: lines, Truncated: truncated}
}

func aiTailInterestingLines(path string, maxLines, maxChars int, only5xx bool) ([]string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return nil, false, fmt.Errorf("invalid log file")
	}
	size := info.Size()
	readSize := int64(aiMaxLogReadBytes)
	if size < readSize {
		readSize = size
	}
	buf := make([]byte, readSize)
	if readSize > 0 {
		if _, err := f.ReadAt(buf, size-readSize); err != nil && err != io.EOF {
			return nil, false, err
		}
	}
	rawLines := strings.Split(strings.ReplaceAll(string(buf), "\r\n", "\n"), "\n")
	keywords := []string{"Fatal error", "Parse error", "Allowed memory size", "Call to undefined", "Class not found", "permission denied", "Primary script unknown", "database", "Connection refused", "upstream", " 500 ", " 502 ", " 503 ", " 504 "}
	selectedIndexes := map[int]bool{}
	seen := map[string]bool{}
	allowLine := func(line string) bool {
		if !only5xx {
			return true
		}
		return strings.Contains(line, " 500 ") || strings.Contains(line, " 502 ") || strings.Contains(line, " 503 ") || strings.Contains(line, " 504 ")
	}
	add := func(index int, line string) {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			return
		}
		if !allowLine(line) {
			return
		}
		seen[line] = true
		selectedIndexes[index] = true
	}
	for index, line := range rawLines {
		for _, kw := range keywords {
			if strings.Contains(line, kw) {
				add(index, line)
				break
			}
		}
	}
	for i := len(rawLines) - 1; i >= 0 && len(selectedIndexes) < maxLines; i-- {
		add(i, rawLines[i])
	}
	var selected []string
	for i, line := range rawLines {
		if selectedIndexes[i] {
			selected = append(selected, strings.TrimSpace(line))
		}
	}
	if len(selected) > maxLines {
		selected = selected[len(selected)-maxLines:]
	}
	// Cap from the tail to preserve the most recent lines.
	total := 0
	capStart := len(selected)
	for i := len(selected) - 1; i >= 0; i-- {
		if total+len(selected[i]) > maxChars {
			break
		}
		total += len(selected[i])
		capStart = i
	}
	return selected[capStart:], size > readSize || capStart > 0, nil
}

func aiPathWithin(basePath, targetPath string) bool {
	return isPathWithinRoot(basePath, targetPath)
}

func aiWPConfigSummary(site *models.Website) map[string]interface{} {
	result := map[string]interface{}{
		"checked": false,
		"exists":  false,
	}
	if site == nil || site.SiteType != "wordpress" {
		result["message"] = "非 WordPress 站点，未检查 wp-config.php"
		return result
	}
	path := filepath.Join(site.WebRoot, "wp-config.php")
	if !aiPathWithin(site.WebRoot, path) {
		result["message"] = "wp-config.php 路径越界"
		return result
	}
	data, err := os.ReadFile(path)
	if err != nil {
		result["message"] = "wp-config.php 不存在或不可读"
		return result
	}
	text := string(data)
	result["checked"] = true
	result["exists"] = true
	dbName := aiExtractWPConstant(text, "DB_NAME")
	dbUser := aiExtractWPConstant(text, "DB_USER")
	dbHost := aiExtractWPConstant(text, "DB_HOST")
	result["db_name_matches_panel"] = dbName == site.DBName
	result["db_user_matches_panel"] = dbUser == site.DBUser
	result["db_host"] = dbHost
	if prefix, err := ReadWPTablePrefix(site.WebRoot); err == nil {
		result["table_prefix"] = prefix
	} else {
		result["table_prefix_error"] = err.Error()
	}
	result["wp_debug_enabled"] = regexp.MustCompile(`(?i)define\(\s*['"]WP_DEBUG['"]\s*,\s*true\s*\)`).MatchString(text)
	result["contains_db_password"] = "redacted"
	result["contains_auth_salts"] = "redacted"
	return result
}

func aiExtractWPConstant(content, name string) string {
	pattern := fmt.Sprintf(`define\(\s*['"]%s['"]\s*,\s*['"]([^'"]*)['"]\s*\)`, regexp.QuoteMeta(name))
	m := regexp.MustCompile(pattern).FindStringSubmatch(content)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

func aiDBCheck(site *models.Website) map[string]interface{} {
	result := map[string]interface{}{"checked": false}
	if site == nil || site.SiteType != "wordpress" {
		result["message"] = "非 WordPress 站点，未检查数据库"
		return result
	}
	if config.AppConfig == nil {
		result["message"] = "面板配置未初始化"
		return result
	}
	prefix, err := ReadWPTablePrefix(site.WebRoot)
	if err != nil {
		result["message"] = "未能读取表前缀: " + err.Error()
		return result
	}
	result["checked"] = true
	result["table_prefix"] = prefix
	siteURL, homeURL, err := ReadWPSiteURLs(site.DBName, prefix, config.AppConfig)
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
		return result
	}
	result["ok"] = true
	result["siteurl"] = siteURL
	result["home"] = homeURL
	return result
}

func aiServiceChecks(site *models.Website) map[string]interface{} {
	result := map[string]interface{}{}
	if site == nil {
		return result
	}
	result["nginx_conf_exists"] = aiFileExists(site.NginxConfPath)
	result["php_pool_exists"] = aiFileExists(site.PHPPoolPath)
	result["web_root_exists"] = aiDirExists(site.WebRoot)
	result["log_dir_exists"] = aiDirExists(site.LogDir)
	return result
}

func aiFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func aiDirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func aiRecentPanelOperations(domain string, limit int) []map[string]string {
	db := database.GetDB()
	if db == nil || domain == "" {
		return []map[string]string{}
	}
	rows, err := db.Query(`SELECT operation, target, status, message, created_at
		FROM operation_logs
		WHERE target = ?
		ORDER BY created_at DESC
		LIMIT ?`, domain, limit)
	if err != nil {
		return []map[string]string{}
	}
	defer rows.Close()
	var result []map[string]string
	for rows.Next() {
		var operation, target, status, message, createdAt string
		if err := rows.Scan(&operation, &target, &status, &message, &createdAt); err != nil {
			continue
		}
		result = append(result, map[string]string{
			"operation":  operation,
			"target":     target,
			"status":     status,
			"message":    message,
			"created_at": createdAt,
		})
	}
	if result == nil {
		return []map[string]string{}
	}
	return result
}

func aiLocalChecks(ctx aiDiagnosticContext) map[string]interface{} {
	all := strings.ToLower(aiJoinedLogs(ctx.Logs))
	hits := []string{}
	check := func(label, needle string) {
		if strings.Contains(all, strings.ToLower(needle)) {
			hits = append(hits, label)
		}
	}
	check("PHP Fatal error", "Fatal error")
	check("PHP Parse error", "Parse error")
	check("PHP memory exhausted", "Allowed memory size")
	check("Undefined function", "Call to undefined")
	check("Class not found", "Class not found")
	check("Permission denied", "permission denied")
	check("Nginx Primary script unknown", "Primary script unknown")
	check("Database related error", "database")
	check("Nginx upstream error", "upstream")
	return map[string]interface{}{
		"rule_hits": hits,
		"has_hits":  len(hits) > 0,
	}
}

func aiJoinedLogs(logs map[string]aiLogSnippet) string {
	var b strings.Builder
	for _, item := range logs {
		for _, line := range item.Lines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func aiShrinkLogs(logs map[string]aiLogSnippet, maxChars int) {
	for key, item := range logs {
		if len(item.Lines) == 0 {
			continue
		}
		total := 0
		start := len(item.Lines)
		for i := len(item.Lines) - 1; i >= 0; i-- {
			if total+len(item.Lines[i]) > maxChars {
				break
			}
			total += len(item.Lines[i])
			start = i
		}
		if start > 0 {
			item.Truncated = true
			item.Lines = item.Lines[start:]
		}
		logs[key] = item
	}
}

func aiChatEndpoint(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", fmt.Errorf("Base URL 不能为空")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("Base URL 格式无效")
	}
	if u.User != nil {
		return "", fmt.Errorf("Base URL 不能包含用户名或密码")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("Base URL 仅支持 http 或 https")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(u.Path, "/chat/completions") {
		return u.String(), nil
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	return u.String(), nil
}

func aiHTTPError(status int, data []byte) error {
	msg := strings.TrimSpace(string(data))
	var parsed aiChatResponse
	if json.Unmarshal(data, &parsed) == nil && parsed.Error != nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", status)
	}
	errType := "provider_error"
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		errType = "unauthorized"
	case http.StatusTooManyRequests:
		errType = "rate_limited"
	default:
		if status >= 500 {
			errType = "provider_error"
		}
	}
	return &AIProviderError{Type: errType, StatusCode: status, Message: msg}
}
