package executor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/naibabiji/wp-panel/config"
	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/models"
)

const (
	aiMaxPromptChars      = 12000
	aiMaxLogCharsPerFile  = 4000
	aiMaxLinesPerLog      = 200
	aiMaxLogReadBytes     = 64 * 1024
	aiProviderMaxRetries  = 1
	aiMaxCodeSuspects     = 8
	aiMaxRecentPHPFiles   = 8
	aiCodeContextLines    = 3
	aiMaxCodeSnippetChars = 900
	aiMaxFollowupMessages = 10
	aiMaxFollowupChars    = 6000
)

var aiRunPHPLint = func(path string) (*ExecResult, error) {
	return Execute("php", "-l", path)
}

var aiReadWPDiagnosticOptions = ReadWPDiagnosticOptions

var aiProviderRetryDelay = 800 * time.Millisecond

var aiRunProcessList = func() ([]byte, error) {
	return exec.Command("ps", "-eo", "user:32,%cpu,%mem,comm").Output()
}

var aiHTTPProbeClient = &http.Client{
	Timeout: 5 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

var aiProbeHTTP = aiProbeHTTPURL

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
			Content json.RawMessage `json:"content"`
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
	DiagnosisProfile      map[string]interface{}  `json:"diagnosis_profile"`
	PanelContext          map[string]interface{}  `json:"panel_context"`
	SiteSummary           map[string]interface{}  `json:"site_summary"`
	LocalChecks           map[string]interface{}  `json:"local_checks"`
	RecentPanelOperations []map[string]string     `json:"recent_panel_operations"`
	Logs                  map[string]aiLogSnippet `json:"logs"`
	WPConfigSummary       map[string]interface{}  `json:"wp_config_summary"`
	DBCheck               map[string]interface{}  `json:"db_check"`
	ServiceChecks         map[string]interface{}  `json:"service_checks"`
	CurrentHTTPChecks     map[string]interface{}  `json:"current_http_checks"`
	CodeSuspects          map[string]interface{}  `json:"code_suspects"`
	PerformanceSummary    map[string]interface{}  `json:"performance_summary,omitempty"`
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
		return "", "", fmt.Errorf("website not found")
	}
	if !models.IsValidAIDiagnosisSymptom(symptom) {
		return "", "", fmt.Errorf("invalid diagnosis type")
	}

	ctx := aiDiagnosticContext{
		DiagnosisType:    symptom,
		DiagnosisLabel:   aiDiagnosisLabel(symptom),
		DiagnosisProfile: aiDiagnosisProfile(symptom),
		PanelContext:     aiPanelContext(),
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
		CurrentHTTPChecks:     aiCurrentHTTPChecks(site),
		CodeSuspects:          aiCodeSuspects(site),
		RecentPanelOperations: aiRecentPanelOperations(site.Domain, 20),
		Constraints: map[string]interface{}{
			"phase":                       "readonly_diagnosis",
			"no_write_actions":            true,
			"no_sql_execution":            true,
			"no_shell":                    true,
			"cache_recommendation_policy": aiCacheRecommendationPolicy(site),
		},
		OutputSchema: aiOutputSchema(),
	}
	if aiIsPerformanceSymptom(symptom) {
		ctx.PerformanceSummary = aiPerformanceSummary(site)
	}
	ctx.LocalChecks = aiLocalChecks(ctx)

	userPrompt, err = aiPromptWithinBudget(&ctx)
	if err != nil {
		return "", "", err
	}

	return aiSystemPrompt(), userPrompt, nil
}

func BuildAIFollowupPrompt(site *models.Website, session *models.AISessionDetail, messages []models.AIMessage, userMessage string) (systemPrompt, userPrompt string, err error) {
	if site == nil {
		return "", "", fmt.Errorf("website not found")
	}
	if session == nil || session.ID <= 0 {
		return "", "", fmt.Errorf("diagnostic session not found")
	}
	userMessage = strings.TrimSpace(userMessage)
	if userMessage == "" {
		return "", "", fmt.Errorf("follow-up message cannot be empty")
	}
	_, currentContext, err := BuildAIDiagnosticPrompt(site, session.Symptom)
	if err != nil {
		return "", "", err
	}
	ctx := map[string]interface{}{
		"mode":          "followup_diagnosis",
		"panel_context": aiPanelContext(),
		"original_session": map[string]interface{}{
			"id":            session.ID,
			"site_id":       session.SiteID,
			"symptom":       session.Symptom,
			"symptom_label": aiDiagnosisLabel(session.Symptom),
			"status":        session.Status,
			"risk_level":    session.RiskLevel,
			"summary":       session.Summary,
			"report":        session.Report,
			"raw_text":      aiTruncateRunes(session.RawText, 1800),
		},
		"recent_conversation":  aiFollowupMessagesForPrompt(messages),
		"latest_user_message":  userMessage,
		"current_site_context": json.RawMessage(currentContext),
		"constraints": map[string]interface{}{
			"phase":            "readonly_followup",
			"no_write_actions": true,
			"no_sql_execution": true,
			"no_shell":         true,
		},
		"response_rules": []string{
			"Respond directly to the user's latest feedback in English. Do not output JSON.",
			"First state whether the current rechecked status differs from the original diagnosis.",
			"If the user says they have completed an action, cross-reference current_site_context for new evidence; do not accept a user's claim of fixing something at face value.",
			"Only suggest real entries that exist in WP Panel; if no entry exists, explicitly state there is no direct entry available.",
			"Do not suggest shell commands, and do not claim to have modified files, databases, or services.",
			"Provide 1-3 of the most specific next troubleshooting or remediation suggestions.",
		},
	}
	data, err := aiMarshalMap(ctx)
	if err != nil {
		return "", "", err
	}
	userPrompt = string(data)
	if len(userPrompt) > aiMaxFollowupChars {
		ctx["recent_conversation"] = aiFollowupMessagesForPrompt(aiLimitAIMessages(messages, 4))
		ctx["original_session"].(map[string]interface{})["raw_text"] = ""
		data, err = aiMarshalMap(ctx)
		if err != nil {
			return "", "", err
		}
		userPrompt = string(data)
	}
	return aiFollowupSystemPrompt(), userPrompt, nil
}

func aiFollowupSystemPrompt() string {
	return strings.Join([]string{
		"You are the WP Panel WordPress diagnostic follow-up assistant, continuing to troubleshoot within the same AI diagnostic session.",
		"You must answer based on original_session, recent_conversation, latest_user_message, and current_site_context.",
		"current_site_context is the freshly collected current state for this round and takes priority over older conclusions and historical logs in original_session.",
		"Do not fabricate WP Panel entries that do not exist; only use real entries from panel_context.known_panel_entries and avoid forbidden_panel_entries.",
		"Do not suggest shell commands, do not claim to have performed fixes, and do not ask the user for passwords, API keys, SSL private keys, or database passwords.",
		"Respond naturally in concise English. Do not output JSON or use Markdown code blocks.",
	}, "\n")
}

func aiMarshalMap(ctx map[string]interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ctx); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func aiFollowupMessagesForPrompt(messages []models.AIMessage) []map[string]interface{} {
	messages = aiLimitAIMessages(messages, aiMaxFollowupMessages)
	out := make([]map[string]interface{}, 0, len(messages))
	for _, msg := range messages {
		out = append(out, map[string]interface{}{
			"role":       msg.Role,
			"content":    aiTruncateRunes(msg.Content, 1200),
			"created_at": msg.CreatedAt,
		})
	}
	return out
}

func aiLimitAIMessages(messages []models.AIMessage, max int) []models.AIMessage {
	if max <= 0 {
		return nil
	}
	if len(messages) <= max {
		return messages
	}
	return messages[len(messages)-max:]
}

func aiPromptWithinBudget(ctx *aiDiagnosticContext) (string, error) {
	return aiPromptWithinBudgetLimit(ctx, aiMaxPromptChars)
}

func aiPromptWithinBudgetLimit(ctx *aiDiagnosticContext, limit int) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("AI diagnostic context is empty")
	}
	marshal := func() (string, error) {
		data, err := aiMarshalDiagnosticContext(*ctx)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	fits := func(prompt string) bool {
		return limit <= 0 || len(prompt) <= limit
	}
	prompt, err := marshal()
	if err != nil || fits(prompt) {
		return prompt, err
	}

	type budgetStep struct {
		note string
		run  func()
	}
	steps := []budgetStep{
		{
			note: fmt.Sprintf("Prompt exceeds %d characters; log snippets further truncated.", limit),
			run:  func() { aiShrinkLogs(ctx.Logs, 1200) },
		},
		{
			note: "Compressed low-risk code snippets and recent PHP file list to control context length.",
			run: func() {
				aiTrimCodeSuspectSnippets(ctx.CodeSuspects, false, 320)
				aiLimitRecentPHPFiles(ctx.CodeSuspects, 4)
			},
		},
		{
			note: "Reduced recent panel operations record count.",
			run:  func() { ctx.RecentPanelOperations = aiLimitStringMapSlice(ctx.RecentPanelOperations, 8) },
		},
		{
			note: "Compressed debug.log summary and low-priority logs.",
			run: func() {
				aiTrimDebugLog(ctx.CodeSuspects, 30, 1200)
				aiClearLogLines(ctx.Logs, []string{"wp_security", "access_5xx"}, "Log snippet not sent due to context budget limit")
			},
		},
		{
			note: "Further compressed code suspects, keeping only highest priority evidence.",
			run: func() {
				aiLimitCodeSuspects(ctx.CodeSuspects, 4)
				aiTrimCodeSuspectSnippets(ctx.CodeSuspects, true, 260)
				ctx.RecentPanelOperations = aiLimitStringMapSlice(ctx.RecentPanelOperations, 4)
			},
		},
		{
			note: "Cleared all log bodies, keeping only log status and local check results.",
			run:  func() { aiClearAllLogLines(ctx.Logs, "Log body not sent due to context budget limit") },
		},
		{
			note: "Removed low-priority code list and debug.log body, keeping only code suspect summary.",
			run: func() {
				aiLimitRecentPHPFiles(ctx.CodeSuspects, 0)
				aiTrimDebugLog(ctx.CodeSuspects, 0, 0)
				aiTrimCodeSuspectSnippets(ctx.CodeSuspects, true, 0)
			},
		},
		{
			note: "Used compact output schema and panel context to meet context budget.",
			run: func() {
				ctx.PanelContext = map[string]interface{}{
					"product_name": "WP Panel",
					"scope":        "WordPress-dedicated server management panel; do not reference other panel entries.",
				}
				ctx.OutputSchema = aiCompactOutputSchema()
				ctx.RecentPanelOperations = nil
			},
		},
	}

	for _, step := range steps {
		ctx.PromptNotes = append(ctx.PromptNotes, step.note)
		step.run()
		prompt, err = marshal()
		if err != nil || fits(prompt) {
			return prompt, err
		}
	}
	return prompt, nil
}

func aiMarshalDiagnosticContext(ctx aiDiagnosticContext) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ctx); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func CallAIChat(ctx context.Context, settings *models.AISettings, systemPrompt, userPrompt string) (string, int64, error) {
	if settings == nil {
		return "", 0, &AIProviderError{Type: "bad_config", Message: "AI settings not found"}
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
		return "", 0, &AIProviderError{Type: "bad_config", Message: "Model cannot be empty"}
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
	var lastErr error
	for attempt := 0; attempt <= aiProviderMaxRetries; attempt++ {
		resp, err := client.Do(req)
		elapsed := time.Since(start).Milliseconds()
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout") {
				return "", elapsed, &AIProviderError{Type: "timeout", Message: "AI service request timed out"}
			}
			return "", elapsed, &AIProviderError{Type: "network_error", Message: "Cannot connect to AI service: " + err.Error()}
		}

		respData, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", elapsed, aiHTTPError(resp.StatusCode, respData)
		}

		content, err := aiExtractChatContent(respData)
		if err != nil {
			lastErr = err
			if aiShouldRetryProviderResponse(err) && attempt < aiProviderMaxRetries && aiSleepWithContext(ctx, aiProviderRetryDelay) == nil {
				req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
				if err != nil {
					return "", elapsed, &AIProviderError{Type: "bad_config", Message: err.Error()}
				}
				req.Header.Set("Content-Type", "application/json")
				if strings.TrimSpace(settings.APIKey) != "" {
					req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(settings.APIKey))
				}
				continue
			}
			return "", elapsed, err
		}
		if strings.TrimSpace(content) == "" {
			lastErr = &AIProviderError{Type: "empty_response", Message: "AI service returned empty content"}
			if attempt < aiProviderMaxRetries && aiSleepWithContext(ctx, aiProviderRetryDelay) == nil {
				req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
				if err != nil {
					return "", elapsed, &AIProviderError{Type: "bad_config", Message: err.Error()}
				}
				req.Header.Set("Content-Type", "application/json")
				if strings.TrimSpace(settings.APIKey) != "" {
					req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(settings.APIKey))
				}
				continue
			}
			return "", elapsed, lastErr
		}
		return content, elapsed, nil
	}
	return "", time.Since(start).Milliseconds(), lastErr
}

func aiShouldRetryProviderResponse(err error) bool {
	var providerErr *AIProviderError
	if errors.As(err, &providerErr) {
		return providerErr.Type == "bad_response" || providerErr.Type == "empty_response"
	}
	return false
}

func aiSleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func TestAISettings(ctx context.Context, settings *models.AISettings) (int64, string, error) {
	system := "You are the WP Panel AI connection test assistant."
	user := `Please return only JSON: {"ok":true}`
	content, elapsed, err := CallAIChat(ctx, settings, system, user)
	if err != nil {
		return elapsed, "", err
	}
	return elapsed, content, nil
}

func ParseAIReport(content string) (*models.AIDiagnosticReport, string, bool) {
	raw := strings.TrimSpace(content)
	// Try direct parse first (model followed instructions).
	if report, ok := aiParseReportJSON([]byte(raw)); ok {
		return report, raw, true
	}
	// Extract the outermost JSON object in case the model wrapped it in markdown fences
	// or added preamble/postamble text.
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		if report, ok := aiParseReportJSON([]byte(raw[start : end+1])); ok {
			return report, raw, true
		}
	}
	return nil, raw, false
}

func aiParseReportJSON(data []byte) (*models.AIDiagnosticReport, bool) {
	var report models.AIDiagnosticReport
	if err := json.Unmarshal(data, &report); err == nil {
		return &report, true
	}
	if report, ok := aiParseFlexibleReportJSON(data); ok {
		return report, true
	}
	return nil, false
}

func aiParseFlexibleReportJSON(data []byte) (*models.AIDiagnosticReport, bool) {
	var payload struct {
		Summary                 string            `json:"summary"`
		RiskLevel               string            `json:"risk_level"`
		LikelyCauses            []json.RawMessage `json:"likely_causes"`
		RecommendedActions      []json.RawMessage `json:"recommended_actions"`
		NeedsMoreInfo           bool              `json:"needs_more_info"`
		UserFriendlyExplanation string            `json:"user_friendly_explanation"`
		Metadata                map[string]string `json:"metadata,omitempty"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, false
	}
	report := &models.AIDiagnosticReport{
		Summary:                 strings.TrimSpace(payload.Summary),
		RiskLevel:               strings.TrimSpace(payload.RiskLevel),
		NeedsMoreInfo:           payload.NeedsMoreInfo,
		UserFriendlyExplanation: strings.TrimSpace(payload.UserFriendlyExplanation),
		Metadata:                payload.Metadata,
	}
	for _, rawCause := range payload.LikelyCauses {
		var cause struct {
			Title      string          `json:"title"`
			Confidence string          `json:"confidence"`
			Evidence   json.RawMessage `json:"evidence"`
		}
		if err := json.Unmarshal(rawCause, &cause); err != nil {
			continue
		}
		report.LikelyCauses = append(report.LikelyCauses, models.AILikelyCause{
			Title:      strings.TrimSpace(cause.Title),
			Confidence: strings.TrimSpace(cause.Confidence),
			Evidence:   aiFlexibleStringList(cause.Evidence),
		})
	}
	for _, rawAction := range payload.RecommendedActions {
		var action struct {
			Label           string          `json:"label"`
			Description     string          `json:"description"`
			Risk            string          `json:"risk"`
			ManualSteps     json.RawMessage `json:"manual_steps"`
			PanelActionHint string          `json:"panel_action_hint"`
		}
		if err := json.Unmarshal(rawAction, &action); err != nil {
			continue
		}
		report.RecommendedActions = append(report.RecommendedActions, models.AIAction{
			Label:           strings.TrimSpace(action.Label),
			Description:     strings.TrimSpace(action.Description),
			Risk:            strings.TrimSpace(action.Risk),
			ManualSteps:     aiFlexibleStringList(action.ManualSteps),
			PanelActionHint: strings.TrimSpace(action.PanelActionHint),
		})
	}
	return report, report.Summary != "" || report.UserFriendlyExplanation != "" || len(report.LikelyCauses) > 0 || len(report.RecommendedActions) > 0
}

func aiFlexibleStringList(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return []string{}
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return aiCleanStringList(list)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return aiCleanStringList([]string{text})
	}
	var generic []interface{}
	if err := json.Unmarshal(raw, &generic); err == nil {
		out := make([]string, 0, len(generic))
		for _, item := range generic {
			switch value := item.(type) {
			case string:
				out = append(out, value)
			default:
				if data, err := json.Marshal(value); err == nil {
					out = append(out, string(data))
				}
			}
		}
		return aiCleanStringList(out)
	}
	return []string{}
}

func aiCleanStringList(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	if out == nil {
		return []string{}
	}
	return out
}

func aiExtractChatContent(data []byte) (string, error) {
	var parsed aiChatResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		if content, ok := aiExtractSSEContent(data); ok && strings.TrimSpace(content) != "" {
			return strings.TrimSpace(content), nil
		}
		preview := aiResponsePreview(data, 240)
		if preview != "" {
			return "", &AIProviderError{Type: "bad_response", Message: "AI service returned invalid JSON, response snippet: " + preview}
		}
		return "", &AIProviderError{Type: "bad_response", Message: "AI service returned invalid JSON"}
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", &AIProviderError{Type: "provider_error", Message: parsed.Error.Message}
	}
	if len(parsed.Choices) > 0 {
		content, err := aiContentToText(parsed.Choices[0].Message.Content)
		if err == nil && strings.TrimSpace(content) != "" {
			return strings.TrimSpace(content), nil
		}
	}

	content, ok := aiExtractFallbackContent(data)
	if ok && strings.TrimSpace(content) != "" {
		return strings.TrimSpace(content), nil
	}
	return "", &AIProviderError{Type: "bad_response", Message: "No usable text content found in AI service response"}
}

func aiExtractSSEContent(data []byte) (string, bool) {
	raw := strings.TrimSpace(string(data))
	if !strings.Contains(raw, "data:") {
		return "", false
	}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		content, err := aiExtractChatChunkContent([]byte(payload))
		if err == nil && strings.TrimSpace(content) != "" {
			out = append(out, content)
		}
	}
	if len(out) == 0 {
		return "", true
	}
	return strings.Join(out, ""), true
}

func aiExtractChatChunkContent(data []byte) (string, error) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content json.RawMessage `json:"content"`
			} `json:"delta"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return "", err
	}
	if len(chunk.Choices) == 0 {
		return "", fmt.Errorf("missing choices")
	}
	if content, err := aiContentToText(chunk.Choices[0].Delta.Content); err == nil && strings.TrimSpace(content) != "" {
		return content, nil
	}
	return aiContentToText(chunk.Choices[0].Message.Content)
}

func aiContentToText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}

	var parts []map[string]interface{}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out []string
		for _, part := range parts {
			if value, ok := part["text"].(string); ok && strings.TrimSpace(value) != "" {
				out = append(out, value)
				continue
			}
			if value, ok := part["content"].(string); ok && strings.TrimSpace(value) != "" {
				out = append(out, value)
			}
		}
		return strings.Join(out, "\n"), nil
	}
	return "", fmt.Errorf("unsupported content")
}

func aiExtractFallbackContent(data []byte) (string, bool) {
	var payload map[string]interface{}
	if json.Unmarshal(data, &payload) != nil {
		return "", false
	}
	if text, ok := payload["output_text"].(string); ok {
		return text, true
	}
	if text, ok := payload["content"].(string); ok {
		return text, true
	}
	if parts, ok := payload["content"].([]interface{}); ok {
		var out []string
		for _, item := range parts {
			part, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		return strings.Join(out, "\n"), len(out) > 0
	}
	return "", false
}

func aiResponsePreview(data []byte, maxRunes int) string {
	preview := strings.TrimSpace(string(data))
	if preview == "" {
		return ""
	}
	preview = strings.Join(strings.Fields(preview), " ")
	runes := []rune(preview)
	if maxRunes > 0 && len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "..."
	}
	return preview
}

func aiSystemPrompt() string {
	return strings.Join([]string{
		"You are the WP Panel WordPress site-level diagnostic assistant. You may only analyze the site summary, log summaries, and check results provided in the input.",
		"You must answer based on WP Panel's actual product capabilities. You must not reference or fabricate menus, buttons, paths, or operations from BT Panel, aaPanel, 1Panel, cPanel, Plesk, or any other panel.",
		"If suggesting the administrator operate in the panel, only use WP Panel entries listed in panel_context.known_panel_entries from the user prompt; if no corresponding entry exists, explicitly state 'WP Panel currently has no direct entry for this; please use file management or manual processing'.",
		"Do not fabricate non-existent entries like 'Dashboard -> Monitoring Settings' or 'Enable site resource monitoring'. WP Panel's dashboard only shows resource charts and site resource rankings; the 'Website Monitoring' in site details only performs HTTP availability monitoring, not a performance resource collection toggle.",
		"service_checks only indicates whether the site config file, website directory, and log directory exist; it does not mean Nginx, PHP-FPM, or MariaDB services are running. Do not describe it as 'service is not down'. To confirm service status, suggest the administrator check the service status on the 'Software Management' page.",
		"current_http_checks are real-time HTTP probes initiated during this diagnosis and take priority over historical access_5xx logs. If current_http_checks.home and wp_admin are not 5xx, do not claim 'current site is 500'; only state that historical logs previously showed 5xx but current probes did not reproduce it.",
		"code_suspects are evidence from read-only scans of currently active themes, active plugins, and a few high-value files by the panel. If code_suspects has high severity with file line numbers, prioritize it as a likely cause; do not ask the user to manually check the same evidence again.",
		"code_suspects with context=conditional_block for die/wp_die/exit indicates they are inside conditional code blocks, usually only low-priority clues; unless logs or request conditions directly correspond, do not write them as primary causes.",
		"When diagnosis_profile.profile=performance, prioritize analyzing server load, site PHP-FPM resource usage, WP Panel FastCGI cache status, active plugin structure, and cache plugin conflicts in performance_summary; do not default performance issues to 500 or service downtime.",
		"If site_summary.fastcgi_cache_enabled=true, do not suggest installing or enabling WordPress page cache plugins such as WP Super Cache, W3 Total Cache, WP Fastest Cache, Cache Enabler, WP Rocket page caching, etc.; instead suggest verifying FastCGI cache hits, cleanup mechanisms, bypass rules, and troubleshooting object cache, theme/plugin code, database queries, image resources, and external HTTP requests.",
		"recent_panel_operations are panel operation audit trails, not fault cause conclusions. Only when operation type, time, and log evidence directly correspond can they be considered as likely causes; do not state CDN real IP, SSL, backup, or other recent operations without direct evidence as causes.",
		"Do not claim to have modified the server. Do not suggest arbitrary shell commands. Do not output operations requiring root privileges.",
		"Do not ask the user for passwords, API keys, SSL private keys, or panel database.",
		"Provide evidence for each conclusion; reduce confidence when uncertain.",
		"Return a JSON object in English. Fields must include summary, risk_level, likely_causes, recommended_actions, needs_more_info, user_friendly_explanation.",
		"Do not include Markdown code blocks, and do not output text other than JSON.",
	}, "\n")
}

func aiPanelContext() map[string]interface{} {
	return map[string]interface{}{
		"product_name": "WP Panel",
		"scope":        "WordPress-dedicated server management panel, not BT Panel, 1Panel, cPanel, or a generic Linux panel.",
		"answer_rules": []string{
			"Recommended operations must use WP Panel's actual page and button names.",
			"Do not write 'Log in to BT Panel', 'Go to BT site settings', 'Software Store', or other panel-specific text.",
			"Do not write 'Dashboard -> Monitoring Settings' or 'Enable site resource monitoring'; WP Panel does not have this entry.",
			"'Website Monitoring' in site details is only for periodic HTTP availability checks and alerts, not a server resource monitoring toggle.",
			"When unsure whether WP Panel has a certain entry, do not guess the menu path.",
			"Do not suggest executing shell commands; Phase 1 only provides diagnostic and manual fix suggestions.",
		},
		"forbidden_panel_entries": []string{
			"Dashboard -> Monitoring Settings",
			"WP Panel Dashboard -> Monitoring Settings",
			"Enable site resource monitoring",
			"Enable resource monitoring",
		},
		"known_panel_entries": []map[string]string{
			{
				"page":        "Dashboard",
				"entry":       "Dashboard",
				"description": "View server CPU, memory, load trends, and site resource rankings; you can only view here, not enable resource monitoring.",
			},
			{
				"page":        "Website Management",
				"entry":       "Website Management -> Site -> Details",
				"description": "View website basic info, database, SSL, Nginx custom config, WordPress optimization, and website logs.",
			},
			{
				"page":        "AI Diagnostics",
				"entry":       "AI Diagnostics -> Select site and issue type -> Start diagnosis",
				"description": "Initiate site-level read-only diagnostics, view structured results and historical diagnosis records.",
			},
			{
				"page":        "Site Details",
				"entry":       "Site Details -> Basic Info -> File Management",
				"description": "Enter the site's web root directory to view or edit wp-config.php, themes, plugins, and other site files.",
			},
			{
				"page":        "Site Details",
				"entry":       "Site Details -> Database -> Sync Database Info",
				"description": "Use the panel-recorded database name and username to sync DB_NAME, DB_USER in wp-config.php, and optionally sync table prefix.",
			},
			{
				"page":        "Site Details",
				"entry":       "Site Details -> Database -> Change Password",
				"description": "Change the site's database user password; WordPress sites need to sync wp-config.php.",
			},
			{
				"page":        "Site Details",
				"entry":       "Site Details -> Database -> Change Site URL",
				"description": "Modify siteurl and home in the WordPress database.",
			},
			{
				"page":        "Site Details",
				"entry":       "Site Details -> Website Logs",
				"description": "View access logs, error logs, and WordPress security logs.",
			},
			{
				"page":        "Site Details",
				"entry":       "Site Details -> Website Monitoring",
				"description": "Enable or disable HTTP availability periodic checks and anomaly alerts for this site; not a resource monitoring or performance data collection toggle.",
			},
			{
				"page":        "Site Details",
				"entry":       "Site Details -> Nginx Custom Config",
				"description": "Edit pre.conf and .conf custom snippets, which take effect after panel validation.",
			},
			{
				"page":        "Site Details",
				"entry":       "Site Details -> WordPress Optimization",
				"description": "Manage WP_DEBUG, XML-RPC, FastCGI cache, and other site optimization items.",
			},
			{
				"page":        "File Management",
				"entry":       "File Management -> Select site",
				"description": "Browse, upload, edit, and delete files in the site root directory; do not operate across sites.",
			},
			{
				"page":        "Software Management",
				"entry":       "Software Management",
				"description": "View Nginx, PHP-FPM, MariaDB, Redis, and other base service status; diagnostic reports can only suggest the administrator check, do not claim to have completed runtime status checks.",
			},
		},
	}
}

func aiDiagnosisLabel(symptom string) string {
	switch symptom {
	case models.AIDiagnosisSite500:
		return "Site 500 / White Screen"
	case models.AIDiagnosisWPAdminDown:
		return "Backend inaccessible"
	case models.AIDiagnosisSSLFailure:
		return "SSL failure"
	case models.AIDiagnosisDBConnection:
		return "Database connection issue"
	case models.AIDiagnosisCacheIssue:
		return "Cache anomaly"
	case models.AIDiagnosisPerformance:
		return "Slow website"
	default:
		return symptom
	}
}

func aiDiagnosisProfile(symptom string) map[string]interface{} {
	if aiIsPerformanceSymptom(symptom) {
		return map[string]interface{}{
			"profile": "performance",
			"focus": []string{
				"Whether overall server CPU, memory, and load are abnormal",
				"Current site or co-hosted sites' PHP-FPM resource usage",
				"Whether WP Panel's Nginx FastCGI cache is enabled and possibly missing hits",
				"Whether WordPress cache plugins, optimization plugins, page builders, or heavy plugins cause conflicts or excessive overhead",
			},
			"answer_rules": []string{
				"First distinguish between server resource bottlenecks, co-hosted site resource contention, and current site optimization issues, then give recommendations.",
				"Without performance data, suggest the user check existing resource charts on the 'Dashboard' or view logs at 'Site Details -> Website Logs'; do not suggest enabling non-existent resource monitoring settings.",
				"Cache anomaly should focus on analyzing FastCGI cache, WordPress cache plugins, and multi-layer cache conflicts; slow website should focus on resources, plugins, themes, and cache hit rates.",
				"If fastcgi_cache_enabled=true, do not suggest installing or enabling WordPress page cache plugins; if a page cache plugin already exists, only suggest checking if it overlaps with FastCGI cache and disabling its page cache feature as needed.",
			},
		}
	}
	return map[string]interface{}{
		"profile": "availability",
		"focus": []string{
			"Whether requests return 500, 502, 503, 504 or the backend is inaccessible",
			"wp-config.php, database connection, PHP fatal, theme/plugin code, and Nginx/PHP log evidence",
			"Whether SSL or database connection issues directly cause site unavailability",
		},
	}
}

func aiIsPerformanceSymptom(symptom string) bool {
	return symptom == models.AIDiagnosisCacheIssue || symptom == models.AIDiagnosisPerformance
}

func aiCacheRecommendationPolicy(site *models.Website) map[string]interface{} {
	if site != nil && site.FCacheEnabled {
		return map[string]interface{}{
			"fastcgi_cache_enabled": true,
			"rule":                  "When WP Panel FastCGI cache is enabled, do not suggest installing or enabling WordPress page cache plugins.",
			"avoid_recommending": []string{
				"WP Super Cache page caching",
				"W3 Total Cache page caching",
				"WP Fastest Cache page caching",
				"Cache Enabler page caching",
				"WP Rocket page caching",
				"Any additional WordPress full-page/static HTML page caching",
			},
			"prefer_recommending": []string{
				"Verify whether X-FastCGI-Cache is hitting",
				"Check FastCGI cache TTL, cleanup mechanisms, and bypass rules",
				"Check whether login state, backend, shopping cart, checkout pages, and other dynamic paths bypass the cache",
				"Troubleshoot object cache, slow queries, heavy plugins, theme code, image resources, CDN, and external HTTP requests",
			},
		}
	}
	return map[string]interface{}{
		"fastcgi_cache_enabled": false,
		"rule":                  "When WP Panel FastCGI cache is not enabled, prefer suggesting WP Panel's FastCGI cache; do not default to requiring multiple page cache plugins stacked together.",
	}
}

func aiOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"summary":    "string",
		"risk_level": "low|medium|high",
		"likely_causes": []map[string]interface{}{{
			"title":      "string",
			"confidence": "low|medium|high",
			"evidence":   []string{"string, must be a string array even if there is only one piece of evidence"},
		}},
		"recommended_actions": []map[string]interface{}{{
			"label":             "string",
			"description":       "string",
			"risk":              "low|medium|high",
			"manual_steps":      []string{"string, must be a string array even if there is only one step"},
			"panel_action_hint": "string, must be a real WP Panel entry; empty string if no entry exists",
		}},
		"needs_more_info":           false,
		"user_friendly_explanation": "string",
	}
}

func aiCompactOutputSchema() map[string]interface{} {
	return map[string]interface{}{
		"summary":                   "string",
		"risk_level":                "low|medium|high",
		"likely_causes":             []string{"title, confidence, evidence[]"},
		"recommended_actions":       []string{"label, description, risk, manual_steps[], panel_action_hint"},
		"needs_more_info":           false,
		"user_friendly_explanation": "string",
	}
}

func aiReadLogSnippet(logDir, filename string) aiLogSnippet {
	path := filepath.Join(logDir, filename)
	if _, err := os.Stat(path); err != nil {
		return aiLogSnippet{Source: filename, Status: "not_found", Message: "Log unreadable or not found"}
	}
	if !aiPathWithin(logDir, path) {
		return aiLogSnippet{Source: filename, Status: "forbidden", Message: "Log path escape detected"}
	}
	lines, truncated, err := aiTailInterestingLines(path, aiMaxLinesPerLog, aiMaxLogCharsPerFile, false)
	if err != nil {
		return aiLogSnippet{Source: filename, Status: "not_found", Message: "Log unreadable or not found"}
	}
	return aiLogSnippet{Source: filename, Status: "ok", Lines: lines, Truncated: truncated}
}

func aiReadAccess5xxSnippet(logDir string) aiLogSnippet {
	path := filepath.Join(logDir, "access.log")
	if _, err := os.Stat(path); err != nil {
		return aiLogSnippet{Source: "access.log", Status: "not_found", Message: "Access log unreadable or not found"}
	}
	if !aiPathWithin(logDir, path) {
		return aiLogSnippet{Source: "access.log", Status: "forbidden", Message: "Log path escape detected"}
	}
	lines, truncated, err := aiTailInterestingLines(path, aiMaxLinesPerLog, aiMaxLogCharsPerFile, true)
	if err != nil {
		return aiLogSnippet{Source: "access.log", Status: "not_found", Message: "Access log unreadable or not found"}
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
		result["message"] = "Non-WordPress site, wp-config.php not checked"
		return result
	}
	path := filepath.Join(site.WebRoot, "wp-config.php")
	if !aiPathWithin(site.WebRoot, path) {
		result["message"] = "wp-config.php path escape detected"
		return result
	}
	data, err := os.ReadFile(path)
	if err != nil {
		result["message"] = "wp-config.php not found or unreadable"
		return result
	}
	text := string(data)
	result["checked"] = true
	result["exists"] = true
	result["php_syntax_check"] = aiWPConfigSyntaxCheck(site, path)
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

func aiWPConfigSyntaxCheck(site *models.Website, path string) map[string]interface{} {
	result := map[string]interface{}{
		"checked": false,
		"ok":      false,
	}
	if site == nil || site.SiteType != "wordpress" {
		result["message"] = "Non-WordPress site, wp-config.php syntax not checked"
		return result
	}
	if !aiPathWithin(site.WebRoot, path) {
		result["message"] = "wp-config.php path escape detected"
		return result
	}
	if _, err := os.Stat(path); err != nil {
		result["message"] = "wp-config.php not found or unreadable"
		return result
	}

	lintResult, err := aiRunPHPLint(path)
	output := aiSanitizeWPConfigLintOutput(aiLintOutput(lintResult), path)
	if err != nil {
		if output == "" {
			result["message"] = "php -l check unavailable: " + err.Error()
			return result
		}
		result["checked"] = true
		result["message"] = "wp-config.php PHP syntax check failed"
		result["output"] = output
		return result
	}

	result["checked"] = true
	result["ok"] = true
	result["message"] = "wp-config.php PHP syntax check passed"
	if output != "" {
		result["output"] = output
	}
	return result
}

func aiLintOutput(result *ExecResult) string {
	if result == nil {
		return ""
	}
	parts := []string{}
	if strings.TrimSpace(result.Stdout) != "" {
		parts = append(parts, strings.TrimSpace(result.Stdout))
	}
	if strings.TrimSpace(result.Stderr) != "" {
		parts = append(parts, strings.TrimSpace(result.Stderr))
	}
	return strings.Join(parts, "\n")
}

func aiSanitizeWPConfigLintOutput(output, path string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	output = strings.ReplaceAll(output, path, "wp-config.php")
	output = strings.ReplaceAll(output, filepath.Base(path), "wp-config.php")
	return aiTruncateRunes(output, 1000)
}

func aiSanitizeFileOutput(output, webRoot, path string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	output = strings.ReplaceAll(output, path, aiRelPath(webRoot, path))
	output = strings.ReplaceAll(output, webRoot, "<site_root>")
	return aiTruncateRunes(output, 1000)
}

func aiTruncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "..."
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
		result["message"] = "Non-WordPress site, database not checked"
		return result
	}
	if config.AppConfig == nil {
		result["message"] = "Panel config not initialized"
		return result
	}
	prefix, err := ReadWPTablePrefix(site.WebRoot)
	if err != nil {
		result["message"] = "Unable to read table prefix: " + err.Error()
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

func aiCurrentHTTPChecks(site *models.Website) map[string]interface{} {
	result := map[string]interface{}{"checked": false}
	if site == nil {
		result["message"] = "Website not found, HTTP probe not executed"
		return result
	}
	baseURL, err := aiSiteProbeBaseURL(site)
	if err != nil {
		result["message"] = err.Error()
		return result
	}
	result["checked"] = true
	result["base_url"] = baseURL
	result["home"] = aiProbeHTTP(aiJoinURLPath(baseURL, "/"))
	if site.SiteType == "wordpress" {
		result["wp_admin"] = aiProbeHTTP(aiJoinURLPath(baseURL, "/wp-admin/"))
	}
	result["note"] = "This field is a real-time HTTP probe during this diagnosis; access_5xx is a historical log snippet and cannot alone prove the current site is still returning 500."
	return result
}

func aiSiteProbeBaseURL(site *models.Website) (string, error) {
	domain := strings.TrimSpace(site.Domain)
	if domain == "" {
		return "", fmt.Errorf("Website domain is empty, HTTP probe not executed")
	}
	if strings.Contains(domain, "://") {
		parsed, err := url.Parse(domain)
		if err != nil || parsed.Host == "" {
			return "", fmt.Errorf("Website domain format is abnormal, HTTP probe not executed")
		}
		domain = parsed.Host
	}
	if strings.ContainsAny(domain, "/?#") {
		return "", fmt.Errorf("Website domain contains path or query parameters, HTTP probe not executed")
	}
	scheme := "http"
	if site.SSLEnabled {
		scheme = "https"
	}
	return scheme + "://" + domain, nil
}

func aiJoinURLPath(baseURL, path string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	parsed.Path = path
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func aiProbeHTTPURL(target string) map[string]interface{} {
	result := map[string]interface{}{
		"checked": false,
		"url":     target,
	}
	resp, method, err := aiDoHTTPProbe(target, http.MethodHead)
	if err == nil && resp != nil && resp.StatusCode == http.StatusMethodNotAllowed {
		_ = resp.Body.Close()
		resp, method, err = aiDoHTTPProbe(target, http.MethodGet)
	}
	if err != nil {
		result["error"] = err.Error()
		return result
	}
	if resp == nil {
		result["error"] = "HTTP probe did not return a response"
		return result
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	result["checked"] = true
	result["method"] = method
	result["status_code"] = resp.StatusCode
	result["status"] = resp.Status
	result["status_family"] = aiHTTPStatusFamily(resp.StatusCode)
	result["is_5xx"] = resp.StatusCode >= 500 && resp.StatusCode <= 599
	result["is_currently_available_signal"] = resp.StatusCode > 0 && resp.StatusCode < 500
	if location := strings.TrimSpace(resp.Header.Get("Location")); location != "" {
		result["redirect_location"] = aiTruncateRunes(location, 200)
	}
	return result
}

func aiDoHTTPProbe(target, method string) (*http.Response, string, error) {
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		return nil, method, err
	}
	req.Header.Set("User-Agent", "WP Panel AI Diagnostics")
	req.Header.Set("Accept", "text/html,*/*;q=0.8")
	resp, err := aiHTTPProbeClient.Do(req)
	return resp, method, err
}

func aiHTTPStatusFamily(statusCode int) string {
	switch {
	case statusCode >= 100 && statusCode <= 199:
		return "1xx"
	case statusCode >= 200 && statusCode <= 299:
		return "2xx"
	case statusCode >= 300 && statusCode <= 399:
		return "3xx"
	case statusCode >= 400 && statusCode <= 499:
		return "4xx"
	case statusCode >= 500 && statusCode <= 599:
		return "5xx"
	default:
		return "unknown"
	}
}

func aiPerformanceSummary(site *models.Website) map[string]interface{} {
	result := map[string]interface{}{
		"checked": true,
		"panel_optimization": map[string]interface{}{
			"fastcgi_cache_enabled": site != nil && site.FCacheEnabled,
			"fastcgi_cache_ttl":     0,
			"wp_memory_limit":       "",
			"monitoring_enabled":    site != nil && site.MonitoringEnabled,
			"access_log_mode":       "",
		},
		"server_resource_summary": aiServerResourceSummary(),
		"site_resource_summary":   aiSiteResourceSummary(site),
		"wordpress_structure":     aiWPPerformanceStructure(site),
		"limitations": []string{
			"Current access.log uses combined format without request_time; cannot prove single request latency from access logs alone.",
			"Current disk IO collection is still a placeholder value; cannot treat high disk IO as a confirmed cause.",
		},
	}
	if site != nil {
		result["panel_optimization"] = map[string]interface{}{
			"fastcgi_cache_enabled": site.FCacheEnabled,
			"fastcgi_cache_ttl":     site.FCacheTTL,
			"wp_memory_limit":       site.WPMemoryLimit,
			"monitoring_enabled":    site.MonitoringEnabled,
			"access_log_mode":       site.AccessLogMode,
		}
	}
	return result
}

func aiServerResourceSummary() map[string]interface{} {
	cores := runtime.NumCPU()
	result := map[string]interface{}{
		"checked":   false,
		"cpu_cores": cores,
		"current": map[string]interface{}{
			"load":   aiCurrentLoadSummary(cores),
			"memory": aiCurrentMemorySummary(),
		},
	}
	db := database.GetDB()
	if db == nil {
		result["message"] = "Panel database not initialized, cannot read historical monitoring metrics"
		return result
	}
	result["checked"] = true
	result["latest_metric"] = aiLatestMonitoringMetric(db, cores)
	result["windows"] = map[string]interface{}{
		"15m": aiMonitoringWindow(db, "-15 minutes", cores),
		"1h":  aiMonitoringWindow(db, "-1 hour", cores),
		"24h": aiMonitoringWindow(db, "-24 hours", cores),
	}
	return result
}

func aiLatestMonitoringMetric(db *sql.DB, cores int) map[string]interface{} {
	item := map[string]interface{}{"available": false}
	var cpu, memory, load1, load5, load15 sql.NullFloat64
	var recordedAt string
	err := db.QueryRow(`SELECT cpu_percent, memory_percent, load_avg_1, load_avg_5, load_avg_15, recorded_at FROM monitoring_metrics ORDER BY recorded_at DESC LIMIT 1`).Scan(&cpu, &memory, &load1, &load5, &load15, &recordedAt)
	if err != nil {
		item["message"] = "No historical monitoring data available"
		return item
	}
	item["available"] = true
	item["recorded_at"] = recordedAt
	item["cpu_percent"] = aiRoundFloat(aiNullFloat(cpu))
	item["memory_percent"] = aiRoundFloat(aiNullFloat(memory))
	item["load_avg_1"] = aiRoundFloat(aiNullFloat(load1))
	item["load_avg_5"] = aiRoundFloat(aiNullFloat(load5))
	item["load_avg_15"] = aiRoundFloat(aiNullFloat(load15))
	item["high_load"] = aiNullFloat(load1) >= float64(cores) || aiNullFloat(load5) >= float64(cores)*0.8
	return item
}

func aiMonitoringWindow(db *sql.DB, modifier string, cores int) map[string]interface{} {
	item := map[string]interface{}{"available": false}
	var count int
	var avgCPU, maxCPU, avgMemory, maxMemory, avgLoad1, maxLoad1, avgLoad5, maxLoad5 sql.NullFloat64
	err := db.QueryRow(`SELECT COUNT(*), AVG(cpu_percent), MAX(cpu_percent), AVG(memory_percent), MAX(memory_percent), AVG(load_avg_1), MAX(load_avg_1), AVG(load_avg_5), MAX(load_avg_5)
		FROM monitoring_metrics WHERE recorded_at >= datetime('now', ?)`, modifier).Scan(&count, &avgCPU, &maxCPU, &avgMemory, &maxMemory, &avgLoad1, &maxLoad1, &avgLoad5, &maxLoad5)
	if err != nil || count == 0 {
		item["sample_count"] = count
		item["message"] = "No monitoring data available for this time window"
		return item
	}
	item["available"] = true
	item["sample_count"] = count
	item["avg_cpu_percent"] = aiRoundFloat(aiNullFloat(avgCPU))
	item["max_cpu_percent"] = aiRoundFloat(aiNullFloat(maxCPU))
	item["avg_memory_percent"] = aiRoundFloat(aiNullFloat(avgMemory))
	item["max_memory_percent"] = aiRoundFloat(aiNullFloat(maxMemory))
	item["avg_load_1"] = aiRoundFloat(aiNullFloat(avgLoad1))
	item["max_load_1"] = aiRoundFloat(aiNullFloat(maxLoad1))
	item["avg_load_5"] = aiRoundFloat(aiNullFloat(avgLoad5))
	item["max_load_5"] = aiRoundFloat(aiNullFloat(maxLoad5))
	item["high_resource_pressure"] = aiNullFloat(maxCPU) >= 90 || aiNullFloat(maxMemory) >= 90 || aiNullFloat(maxLoad1) >= float64(cores) || aiNullFloat(avgLoad5) >= float64(cores)*0.8
	return item
}

func aiCurrentLoadSummary(cores int) map[string]interface{} {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return map[string]interface{}{"available": false, "message": "Cannot read /proc/loadavg"}
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return map[string]interface{}{"available": false, "message": "loadavg format is abnormal"}
	}
	load1, _ := strconv.ParseFloat(fields[0], 64)
	load5, _ := strconv.ParseFloat(fields[1], 64)
	load15, _ := strconv.ParseFloat(fields[2], 64)
	return map[string]interface{}{
		"available":   true,
		"load_avg_1":  aiRoundFloat(load1),
		"load_avg_5":  aiRoundFloat(load5),
		"load_avg_15": aiRoundFloat(load15),
		"high_load":   load1 >= float64(cores) || load5 >= float64(cores)*0.8,
	}
}

func aiCurrentMemorySummary() map[string]interface{} {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return map[string]interface{}{"available": false, "message": "Cannot read /proc/meminfo"}
	}
	var total, available int64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, _ := strconv.ParseInt(fields[1], 10, 64)
		value *= 1024
		switch fields[0] {
		case "MemTotal:":
			total = value
		case "MemAvailable:":
			available = value
		}
	}
	if total <= 0 {
		return map[string]interface{}{"available": false, "message": "Memory info format is abnormal"}
	}
	used := total - available
	percent := float64(used) / float64(total) * 100
	return map[string]interface{}{
		"available":          true,
		"memory_used_bytes":  used,
		"memory_total_bytes": total,
		"memory_percent":     aiRoundFloat(percent),
		"high_memory":        percent >= 90,
	}
}

type aiPHPSiteResource struct {
	User      string
	SiteID    int
	Domain    string
	CPU       float64
	Mem       float64
	ProcCount int
}

func aiSiteResourceSummary(site *models.Website) map[string]interface{} {
	result := map[string]interface{}{
		"checked": false,
	}
	db := database.GetDB()
	if db == nil {
		result["message"] = "Panel database not initialized, cannot read site resource usage"
		return result
	}
	result["active_site_count"] = aiWebsiteCount(db, "active")
	result["total_site_count"] = aiWebsiteCount(db, "")
	resources, err := aiCollectPHPSiteResources(db)
	if err != nil {
		result["message"] = err.Error()
		return result
	}
	result["checked"] = true
	result["top_php_fpm_sites"] = aiTopSiteResources(resources, 5)
	if site != nil {
		result["current_site"] = aiFindSiteResource(resources, site.SystemUser, site.Domain)
	}
	return result
}

func aiWebsiteCount(db *sql.DB, status string) int {
	var count int
	if status == "" {
		_ = db.QueryRow("SELECT COUNT(*) FROM websites").Scan(&count)
		return count
	}
	_ = db.QueryRow("SELECT COUNT(*) FROM websites WHERE status = ?", status).Scan(&count)
	return count
}

func aiCollectPHPSiteResources(db *sql.DB) ([]aiPHPSiteResource, error) {
	out, err := aiRunProcessList()
	if err != nil {
		return nil, fmt.Errorf("Failed to read PHP-FPM process resources: %v", err)
	}
	agg := map[string]*aiPHPSiteResource{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		user := fields[0]
		if !strings.HasPrefix(user, "wp_") && !strings.HasPrefix(user, "php_") {
			continue
		}
		comm := fields[3]
		if !strings.HasPrefix(comm, "php-fpm") {
			continue
		}
		cpu, _ := strconv.ParseFloat(fields[1], 64)
		mem, _ := strconv.ParseFloat(fields[2], 64)
		item := agg[user]
		if item == nil {
			item = &aiPHPSiteResource{User: user}
			agg[user] = item
		}
		item.CPU += cpu
		item.Mem += mem
		item.ProcCount++
	}
	result := make([]aiPHPSiteResource, 0, len(agg))
	for _, item := range agg {
		_ = db.QueryRow("SELECT id, domain FROM websites WHERE system_user = ?", item.User).Scan(&item.SiteID, &item.Domain)
		if item.Domain == "" {
			continue
		}
		item.CPU = aiRoundFloat(item.CPU)
		item.Mem = aiRoundFloat(item.Mem)
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CPU == result[j].CPU {
			return result[i].Mem > result[j].Mem
		}
		return result[i].CPU > result[j].CPU
	})
	return result, nil
}

func aiTopSiteResources(resources []aiPHPSiteResource, limit int) []map[string]interface{} {
	if limit <= 0 || len(resources) == 0 {
		return []map[string]interface{}{}
	}
	if len(resources) < limit {
		limit = len(resources)
	}
	out := make([]map[string]interface{}, 0, limit)
	for _, item := range resources[:limit] {
		out = append(out, aiSiteResourceMap(item))
	}
	return out
}

func aiFindSiteResource(resources []aiPHPSiteResource, systemUser, domain string) map[string]interface{} {
	for _, item := range resources {
		if item.User == systemUser || (domain != "" && item.Domain == domain) {
			found := aiSiteResourceMap(item)
			found["found"] = true
			return found
		}
	}
	return map[string]interface{}{
		"found":   false,
		"domain":  domain,
		"message": "No active php-fpm processes found for this site; there may be no requests at the moment, or the process sample did not hit.",
	}
}

func aiSiteResourceMap(item aiPHPSiteResource) map[string]interface{} {
	return map[string]interface{}{
		"site_id":     item.SiteID,
		"domain":      item.Domain,
		"cpu_percent": item.CPU,
		"mem_percent": item.Mem,
		"proc_count":  item.ProcCount,
	}
}

func aiWPPerformanceStructure(site *models.Website) map[string]interface{} {
	result := map[string]interface{}{"checked": false}
	if site == nil || site.SiteType != "wordpress" {
		result["message"] = "Non-WordPress site, plugin structure not checked"
		return result
	}
	prefix, err := ReadWPTablePrefix(site.WebRoot)
	if err != nil {
		result["message"] = "Unable to read table prefix: " + err.Error()
		return result
	}
	if config.AppConfig == nil {
		result["message"] = "Panel config not initialized"
		return result
	}
	opts, err := aiReadWPDiagnosticOptions(site.DBName, prefix, config.AppConfig)
	if err != nil {
		result["message"] = err.Error()
		return result
	}
	activePlugins := aiParseActivePlugins(opts["active_plugins"])
	pageCachePlugins := aiClassifyPlugins(activePlugins, aiPageCachePluginPatterns())
	objectCachePlugins := aiClassifyPlugins(activePlugins, aiObjectCachePluginPatterns())
	assetOptimizationPlugins := aiClassifyPlugins(activePlugins, aiAssetOptimizationPluginPatterns())
	builderPlugins := aiClassifyPlugins(activePlugins, aiBuilderPluginPatterns())
	heavyPlugins := aiClassifyPlugins(activePlugins, aiHeavyPluginPatterns())
	result["checked"] = true
	result["active_theme"] = map[string]string{
		"template":   strings.TrimSpace(opts["template"]),
		"stylesheet": strings.TrimSpace(opts["stylesheet"]),
	}
	result["active_plugin_count"] = len(activePlugins)
	result["page_cache_plugins"] = pageCachePlugins
	result["object_cache_plugins"] = objectCachePlugins
	result["asset_optimization_plugins"] = assetOptimizationPlugins
	result["builder_plugins"] = builderPlugins
	result["heavy_plugins"] = heavyPlugins
	result["multiple_page_cache_plugins"] = len(pageCachePlugins) > 1
	result["potential_fastcgi_page_cache_overlap"] = site.FCacheEnabled && len(pageCachePlugins) > 0
	result["asset_optimization_overlap"] = len(assetOptimizationPlugins) > 1
	result["many_active_plugins"] = len(activePlugins) >= 25
	return result
}

func aiClassifyPlugins(activePlugins []string, patterns map[string]string) []map[string]string {
	var result []map[string]string
	for _, plugin := range activePlugins {
		normalized := strings.ToLower(filepath.ToSlash(plugin))
		for needle, label := range patterns {
			if strings.Contains(normalized, needle) {
				result = append(result, map[string]string{
					"plugin": plugin,
					"label":  label,
				})
				break
			}
		}
	}
	if result == nil {
		return []map[string]string{}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i]["plugin"] < result[j]["plugin"]
	})
	return result
}

func aiPageCachePluginPatterns() map[string]string {
	return map[string]string{
		"litespeed-cache/":  "LiteSpeed Cache",
		"wp-rocket/":        "WP Rocket",
		"w3-total-cache/":   "W3 Total Cache",
		"wp-super-cache/":   "WP Super Cache",
		"cache-enabler/":    "Cache Enabler",
		"wp-fastest-cache/": "WP Fastest Cache",
		"breeze/":           "Breeze",
		"sg-cachepress/":    "SiteGround Optimizer",
	}
}

func aiObjectCachePluginPatterns() map[string]string {
	return map[string]string{
		"redis-cache/":        "Redis Object Cache",
		"memcached/":          "Memcached",
		"object-cache-pro/":   "Object Cache Pro",
		"wp-redis/":           "WP Redis",
		"wp-redis-cache/":     "WP Redis Cache",
		"redis-object-cache/": "Redis Object Cache",
	}
}

func aiAssetOptimizationPluginPatterns() map[string]string {
	return map[string]string{
		"autoptimize/":          "Autoptimize",
		"wp-rocket/":            "WP Rocket",
		"litespeed-cache/":      "LiteSpeed Cache",
		"w3-total-cache/":       "W3 Total Cache",
		"perfmatters/":          "Perfmatters",
		"asset-cleanup/":        "Asset CleanUp",
		"wp-optimize/":          "WP-Optimize",
		"fast-velocity-minify/": "Fast Velocity Minify",
	}
}

func aiBuilderPluginPatterns() map[string]string {
	return map[string]string{
		"elementor/":         "Elementor",
		"elementor-pro/":     "Elementor Pro",
		"js_composer/":       "WPBakery Page Builder",
		"oxygen/":            "Oxygen Builder",
		"bricks/":            "Bricks",
		"bb-plugin/":         "Beaver Builder",
		"divi-builder/":      "Divi Builder",
		"siteorigin-panels/": "SiteOrigin Page Builder",
	}
}

func aiHeavyPluginPatterns() map[string]string {
	return map[string]string{
		"woocommerce/":             "WooCommerce",
		"wordfence/":               "Wordfence",
		"wordfence-security/":      "Wordfence Security",
		"updraftplus/":             "UpdraftPlus",
		"all-in-one-wp-migration/": "All-in-One WP Migration",
		"duplicator/":              "Duplicator",
	}
}

func aiNullFloat(value sql.NullFloat64) float64 {
	if !value.Valid {
		return 0
	}
	return value.Float64
}

func aiRoundFloat(value float64) float64 {
	return math.Round(value*10) / 10
}

func aiCodeSuspects(site *models.Website) map[string]interface{} {
	result := map[string]interface{}{
		"checked":  false,
		"suspects": []map[string]interface{}{},
	}
	if site == nil || site.SiteType != "wordpress" {
		result["message"] = "Non-WordPress site, theme or plugin code not scanned"
		return result
	}
	if !aiDirExists(site.WebRoot) {
		result["message"] = "Website root directory does not exist or is unreadable"
		return result
	}
	result["checked"] = true

	prefix, err := ReadWPTablePrefix(site.WebRoot)
	if err != nil {
		result["wp_options_error"] = "Unable to read table prefix: " + err.Error()
	} else if config.AppConfig == nil {
		result["wp_options_error"] = "Panel config not initialized"
	} else if opts, err := aiReadWPDiagnosticOptions(site.DBName, prefix, config.AppConfig); err != nil {
		result["wp_options_error"] = err.Error()
	} else {
		templateName := strings.TrimSpace(opts["template"])
		stylesheetName := strings.TrimSpace(opts["stylesheet"])
		activePlugins := aiParseActivePlugins(opts["active_plugins"])
		result["active_theme"] = map[string]interface{}{
			"template":   templateName,
			"stylesheet": stylesheetName,
		}
		result["active_plugins"] = activePlugins
		suspects := result["suspects"].([]map[string]interface{})
		suspects = append(suspects, aiScanActiveTheme(site.WebRoot, stylesheetName)...)
		suspects = append(suspects, aiScanActivePlugins(site.WebRoot, activePlugins)...)
		aiSortCodeSuspects(suspects)
		if len(suspects) > aiMaxCodeSuspects {
			suspects = suspects[:aiMaxCodeSuspects]
			result["suspects_truncated"] = true
		}
		result["suspects"] = suspects
	}

	result["debug_log"] = aiDebugLogSummary(site.WebRoot)
	result["recent_php_files"] = aiRecentPHPFiles(site.WebRoot)
	return result
}

func aiScanActiveTheme(webRoot, stylesheetName string) []map[string]interface{} {
	if strings.TrimSpace(stylesheetName) == "" {
		return []map[string]interface{}{aiCodeSuspect("theme", "", 0, "active_theme_missing", "high", "WordPress current theme stylesheet is empty", "")}
	}
	if !aiSafeThemeName(stylesheetName) {
		return []map[string]interface{}{aiCodeSuspect("theme", "", 0, "active_theme_invalid", "high", "WordPress current theme stylesheet contains unsafe path characters", "")}
	}
	themeDir := filepath.Join(webRoot, "wp-content", "themes", stylesheetName)
	relThemeDir := filepath.ToSlash(filepath.Join("wp-content", "themes", stylesheetName))
	if !aiPathWithin(webRoot, themeDir) || !aiDirExists(themeDir) {
		return []map[string]interface{}{aiCodeSuspect("theme", relThemeDir, 0, "active_theme_not_found", "high", "Currently active theme directory does not exist or is unreadable", "")}
	}
	functionsPath := filepath.Join(themeDir, "functions.php")
	relFunctions := filepath.ToSlash(filepath.Join(relThemeDir, "functions.php"))
	if !aiFileExists(functionsPath) {
		return []map[string]interface{}{aiCodeSuspect("theme", relFunctions, 0, "functions_php_missing", "medium", "Current theme has no functions.php; this is not necessarily an error, but code suspects cannot be scanned from this file", "")}
	}
	return aiScanPHPFileForSuspects(webRoot, functionsPath, "active_theme_functions")
}

func aiScanActivePlugins(webRoot string, plugins []string) []map[string]interface{} {
	var suspects []map[string]interface{}
	for _, plugin := range plugins {
		if !aiSafePluginPath(plugin) {
			suspects = append(suspects, aiCodeSuspect("plugin", plugin, 0, "active_plugin_invalid", "high", "Unsafe plugin path found in active_plugins", ""))
			continue
		}
		path := filepath.Join(webRoot, "wp-content", "plugins", filepath.FromSlash(plugin))
		rel := filepath.ToSlash(filepath.Join("wp-content", "plugins", filepath.FromSlash(plugin)))
		if !aiPathWithin(webRoot, path) || !aiFileExists(path) {
			suspects = append(suspects, aiCodeSuspect("plugin", rel, 0, "active_plugin_not_found", "high", "Active plugin file does not exist or is unreadable", ""))
			continue
		}
		fileSuspects := aiScanPHPFileForSuspects(webRoot, path, "active_plugin_main")
		suspects = append(suspects, fileSuspects...)
	}
	aiSortCodeSuspects(suspects)
	if len(suspects) > aiMaxCodeSuspects {
		return suspects[:aiMaxCodeSuspects]
	}
	return suspects
}

func aiSortCodeSuspects(suspects []map[string]interface{}) {
	priority := func(item map[string]interface{}) int {
		switch item["severity"] {
		case "high":
			return 0
		case "medium":
			return 1
		case "low":
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(suspects, func(i, j int) bool {
		left := priority(suspects[i])
		right := priority(suspects[j])
		if left != right {
			return left < right
		}
		return aiSuspectLine(suspects[i]) < aiSuspectLine(suspects[j])
	})
}

func aiSuspectLine(item map[string]interface{}) int {
	line, ok := item["line"].(int)
	if !ok {
		return 0
	}
	return line
}

func aiScanPHPFileForSuspects(webRoot, path, scope string) []map[string]interface{} {
	if !aiPathWithin(webRoot, path) || !aiFileExists(path) {
		return nil
	}
	var suspects []map[string]interface{}
	rel := aiRelPath(webRoot, path)
	if lintResult, err := aiRunPHPLint(path); err != nil {
		output := aiSanitizeFileOutput(aiLintOutput(lintResult), webRoot, path)
		if output != "" {
			suspects = append(suspects, aiCodeSuspect(scope, rel, 0, "php_syntax_error", "high", output, ""))
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return suspects
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	braceDepth := 0
	for i, line := range lines {
		depthBeforeLine := braceDepth
		pattern, severity, reason, conditional, ok := aiSuspiciousPHPLine(line, depthBeforeLine)
		if ok {
			suspect := aiCodeSuspect(scope, rel, i+1, pattern, severity, reason, aiSnippetAround(lines, i, aiCodeContextLines))
			if conditional {
				suspect["context"] = "conditional_block"
			}
			suspects = append(suspects, suspect)
			if len(suspects) >= aiMaxCodeSuspects {
				break
			}
		}
		braceDepth += aiBraceDelta(line)
		if braceDepth < 0 {
			braceDepth = 0
		}
	}
	return suspects
}

func aiSuspiciousPHPLine(line string, braceDepth int) (pattern, severity, reason string, conditional bool, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "#") {
		return "", "", "", false, false
	}
	if regexp.MustCompile(`^\s*wp_die\s*\(`).MatchString(trimmed) {
		if braceDepth == 0 {
			return "wp_die(", "high", "Top-level wp_die call found in file, may directly terminate WordPress request and cause white screen/500", false, true
		}
		return "wp_die(", "low", "wp_die call found inside code block, usually requires specific conditions to trigger; low-priority clue only", true, true
	}
	if regexp.MustCompile(`^\s*die\s*\(`).MatchString(trimmed) {
		if braceDepth == 0 {
			return "die(", "high", "Top-level die call found in file, may directly terminate PHP request", false, true
		}
		return "die(", "low", "die call found inside code block, usually requires specific conditions to trigger; low-priority clue only", true, true
	}
	if regexp.MustCompile(`^\s*exit\s*(\(|;)`).MatchString(trimmed) {
		if braceDepth == 0 {
			return "exit", "high", "Top-level exit call found in file, may directly terminate PHP request", false, true
		}
		return "exit", "low", "exit call found inside code block, usually requires specific conditions to trigger; low-priority clue only", true, true
	}
	checks := []struct {
		re       *regexp.Regexp
		pattern  string
		severity string
		reason   string
	}{
		{regexp.MustCompile(`trigger_error\s*\([^,\)]*,\s*E_USER_ERROR\s*\)`), "trigger_error(E_USER_ERROR)", "medium", "E_USER_ERROR found in code, may trigger fatal error under certain conditions"},
		{regexp.MustCompile(`throw\s+new\s+(Error|Exception|RuntimeException)\b`), "throw new", "medium", "Exception/error throw found in code, if uncaught may cause 500"},
		{regexp.MustCompile(`\b(require|require_once|include|include_once)\s*\(?\s*['"][^'"]+['"]`), "include/require", "low", "include/require found in code, may cause error if target file is missing; needs verification with specific path"},
	}
	for _, check := range checks {
		if check.re.MatchString(trimmed) {
			return check.pattern, check.severity, check.reason, braceDepth > 0, true
		}
	}
	return "", "", "", false, false
}

func aiBraceDelta(line string) int {
	stripped := aiStripPHPLineForBraceCount(line)
	return strings.Count(stripped, "{") - strings.Count(stripped, "}")
}

func aiStripPHPLineForBraceCount(line string) string {
	var out strings.Builder
	var quote rune
	escaped := false
	for _, ch := range line {
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			continue
		}
		out.WriteRune(ch)
	}
	stripped := out.String()
	if idx := strings.Index(stripped, "//"); idx >= 0 {
		stripped = stripped[:idx]
	}
	if idx := strings.Index(stripped, "#"); idx >= 0 {
		stripped = stripped[:idx]
	}
	return stripped
}

func aiCodeSuspect(scope, relPath string, line int, pattern, severity, reason, snippet string) map[string]interface{} {
	item := map[string]interface{}{
		"scope":    scope,
		"file":     filepath.ToSlash(relPath),
		"pattern":  pattern,
		"severity": severity,
		"reason":   reason,
	}
	if line > 0 {
		item["line"] = line
	}
	if strings.TrimSpace(snippet) != "" {
		item["snippet"] = aiTruncateRunes(snippet, aiMaxCodeSnippetChars)
	}
	return item
}

func aiSnippetAround(lines []string, index, contextLines int) string {
	start := index - contextLines
	if start < 0 {
		start = 0
	}
	end := index + contextLines + 1
	if end > len(lines) {
		end = len(lines)
	}
	var out []string
	for i := start; i < end; i++ {
		out = append(out, fmt.Sprintf("%d: %s", i+1, lines[i]))
	}
	return strings.Join(out, "\n")
}

func aiDebugLogSummary(webRoot string) map[string]interface{} {
	path := filepath.Join(webRoot, "wp-content", "debug.log")
	result := map[string]interface{}{
		"checked": true,
		"exists":  false,
		"file":    "wp-content/debug.log",
	}
	if !aiPathWithin(webRoot, path) || !aiFileExists(path) {
		result["message"] = "debug.log does not exist or is unreadable"
		return result
	}
	lines, truncated, err := aiTailInterestingLines(path, 80, 2500, false)
	if err != nil {
		result["message"] = "debug.log is unreadable"
		return result
	}
	result["exists"] = true
	result["lines"] = lines
	result["truncated"] = truncated
	return result
}

func aiRecentPHPFiles(webRoot string) []map[string]interface{} {
	base := filepath.Join(webRoot, "wp-content")
	if !aiPathWithin(webRoot, base) || !aiDirExists(base) {
		return []map[string]interface{}{}
	}
	type fileInfo struct {
		path    string
		modTime time.Time
		size    int64
	}
	var files []fileInfo
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "uploads" || name == "cache" || name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) > 500 {
			return filepath.SkipAll
		}
		if !strings.EqualFold(filepath.Ext(path), ".php") || !aiPathWithin(webRoot, path) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		files = append(files, fileInfo{path: path, modTime: info.ModTime(), size: info.Size()})
		return nil
	})
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})
	if len(files) > aiMaxRecentPHPFiles {
		files = files[:aiMaxRecentPHPFiles]
	}
	result := make([]map[string]interface{}, 0, len(files))
	for _, item := range files {
		result = append(result, map[string]interface{}{
			"file":     aiRelPath(webRoot, item.path),
			"modified": item.modTime.Format(time.RFC3339),
			"size":     item.size,
		})
	}
	return result
}

func aiParseActivePlugins(serialized string) []string {
	re := regexp.MustCompile(`s:\d+:"([^"]+\.php)"`)
	matches := re.FindAllStringSubmatch(serialized, -1)
	plugins := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		plugin := filepath.ToSlash(strings.TrimSpace(match[1]))
		if plugin == "" || seen[plugin] {
			continue
		}
		seen[plugin] = true
		plugins = append(plugins, plugin)
	}
	return plugins
}

func aiSafeThemeName(name string) bool {
	return regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(name) && !strings.Contains(name, "..")
}

func aiSafePluginPath(plugin string) bool {
	if plugin == "" || strings.Contains(plugin, "..") || strings.HasPrefix(plugin, "/") || strings.HasPrefix(plugin, "\\") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(plugin)))
	if clean != plugin || !strings.HasSuffix(clean, ".php") {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(part) {
			return false
		}
	}
	return true
}

func aiRelPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
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
			"operation":       operation,
			"operation_label": aiOperationLabel(operation),
			"target":          target,
			"status":          status,
			"message":         message,
			"created_at":      createdAt,
		})
	}
	if result == nil {
		return []map[string]string{}
	}
	return result
}

func aiOperationLabel(operation string) string {
	switch operation {
	case "wp_optimizations":
		return "WordPress optimization settings"
	case "set_cdn_realip":
		return "CDN real IP settings"
	case "set_access_log_mode":
		return "Access log settings"
	case "save_nginx_custom":
		return "Nginx custom config"
	case "change_db_password":
		return "Database password change"
	case "update_domains":
		return "Domain settings"
	case "enable_ssl":
		return "Enable SSL"
	case "remove_ssl":
		return "Remove SSL"
	case "create_backup":
		return "Create backup"
	case "restore_backup":
		return "Restore backup"
	case "create_site":
		return "Create website"
	case "delete_site":
		return "Delete website"
	case "pause_site":
		return "Pause website"
	case "enable_site":
		return "Enable website"
	case "ssl_certificate_export":
		return "SSL certificate export"
	default:
		return operation
	}
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

func aiClearLogLines(logs map[string]aiLogSnippet, keys []string, message string) {
	for _, key := range keys {
		item, ok := logs[key]
		if !ok {
			continue
		}
		item.Lines = nil
		item.Truncated = true
		item.Message = message
		logs[key] = item
	}
}

func aiClearAllLogLines(logs map[string]aiLogSnippet, message string) {
	keys := make([]string, 0, len(logs))
	for key := range logs {
		keys = append(keys, key)
	}
	aiClearLogLines(logs, keys, message)
}

func aiLimitStringMapSlice(items []map[string]string, max int) []map[string]string {
	if max <= 0 {
		return nil
	}
	if len(items) <= max {
		return items
	}
	return items[:max]
}

func aiCodeSuspectItems(code map[string]interface{}) []map[string]interface{} {
	if code == nil {
		return nil
	}
	items, ok := code["suspects"].([]map[string]interface{})
	if ok {
		return items
	}
	generic, ok := code["suspects"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(generic))
	for _, item := range generic {
		if m, ok := item.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out
}

func aiSetCodeSuspectItems(code map[string]interface{}, items []map[string]interface{}) {
	if code != nil {
		code["suspects"] = items
	}
}

func aiTrimCodeSuspectSnippets(code map[string]interface{}, includeHigh bool, maxChars int) {
	items := aiCodeSuspectItems(code)
	for _, item := range items {
		severity, _ := item["severity"].(string)
		if severity == "high" && !includeHigh {
			continue
		}
		if maxChars <= 0 {
			delete(item, "snippet")
			continue
		}
		if snippet, ok := item["snippet"].(string); ok {
			item["snippet"] = aiTruncateRunes(snippet, maxChars)
		}
	}
	aiSetCodeSuspectItems(code, items)
}

func aiLimitCodeSuspects(code map[string]interface{}, max int) {
	if max <= 0 {
		aiSetCodeSuspectItems(code, []map[string]interface{}{})
		return
	}
	items := aiCodeSuspectItems(code)
	if len(items) <= max {
		return
	}
	priority := func(item map[string]interface{}) int {
		switch item["severity"] {
		case "high":
			return 0
		case "medium":
			return 1
		case "low":
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return priority(items[i]) < priority(items[j])
	})
	aiSetCodeSuspectItems(code, items[:max])
	if code != nil {
		code["suspects_truncated"] = true
	}
}

func aiLimitRecentPHPFiles(code map[string]interface{}, max int) {
	if code == nil {
		return
	}
	if max <= 0 {
		delete(code, "recent_php_files")
		return
	}
	if files, ok := code["recent_php_files"].([]map[string]interface{}); ok && len(files) > max {
		code["recent_php_files"] = files[:max]
	}
}

func aiTrimDebugLog(code map[string]interface{}, maxLines, maxChars int) {
	if code == nil {
		return
	}
	debugLog, ok := code["debug_log"].(map[string]interface{})
	if !ok {
		return
	}
	if maxLines <= 0 || maxChars <= 0 {
		delete(debugLog, "lines")
		debugLog["truncated"] = true
		debugLog["message"] = "debug.log body not sent due to context budget limit"
		return
	}
	lines, ok := debugLog["lines"].([]string)
	if !ok {
		return
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		debugLog["truncated"] = true
	}
	total := 0
	start := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		if total+len(lines[i]) > maxChars {
			break
		}
		total += len(lines[i])
		start = i
	}
	debugLog["lines"] = lines[start:]
	if start > 0 {
		debugLog["truncated"] = true
	}
}

func aiChatEndpoint(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", fmt.Errorf("Base URL cannot be empty")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("Base URL format is invalid")
	}
	if u.User != nil {
		return "", fmt.Errorf("Base URL cannot contain username or password")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("Base URL only supports http or https")
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
