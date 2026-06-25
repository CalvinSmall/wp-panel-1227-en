package handlers

import (
	"testing"

	"github.com/naibabiji/wp-panel/models"
)

func TestNormalizeAISettingsDefaultsDeepSeekV4Pro(t *testing.T) {
	settings, err := normalizeAISettingsRequest(models.AISettingsRequest{
		Enabled: true,
	}, false)
	if err != nil {
		t.Fatalf("normalizeAISettingsRequest() error = %v", err)
	}
	if settings.Provider != "deepseek" {
		t.Fatalf("provider = %q, want deepseek", settings.Provider)
	}
	if settings.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("base url = %q, want DeepSeek base url", settings.BaseURL)
	}
	if settings.Model != "deepseek-v4-pro" {
		t.Fatalf("model = %q, want deepseek-v4-pro", settings.Model)
	}
	if settings.TimeoutSeconds != 60 {
		t.Fatalf("timeout = %d, want 60", settings.TimeoutSeconds)
	}
}

func TestNormalizeAISettingsRejectsUnknownProvider(t *testing.T) {
	_, err := normalizeAISettingsRequest(models.AISettingsRequest{Provider: "bad"}, false)
	if err == nil {
		t.Fatal("expected unknown provider to be rejected")
	}
}

func TestMaskAIKey(t *testing.T) {
	if got := maskAIKey("sk-1234567890"); got != "sk-1...7890" {
		t.Fatalf("maskAIKey() = %q", got)
	}
	if got := maskAIKey(""); got != "" {
		t.Fatalf("empty mask = %q, want empty", got)
	}
}
