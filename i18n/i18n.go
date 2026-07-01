package i18n

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
)

const (
	DefaultLang = "zh-CN"
	English     = "en-US"
	CookieName  = "wp_panel_lang"
)

type P map[string]string

//go:embed locales/*.json
var localesFS embed.FS

var messages = loadMessages()

func NormalizeLang(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "en", "en-us", "en_us":
		return English
	case "zh", "zh-cn", "zh_cn", "cn":
		return DefaultLang
	default:
		return DefaultLang
	}
}

func LangFromRequest(r *http.Request) string {
	if r == nil {
		return DefaultLang
	}
	if lang := strings.TrimSpace(r.URL.Query().Get("lang")); lang != "" {
		return NormalizeLang(lang)
	}
	if cookie, err := r.Cookie(CookieName); err == nil {
		return NormalizeLang(cookie.Value)
	}
	return DefaultLang
}

func MaybeSetLanguageCookie(w http.ResponseWriter, r *http.Request) {
	if r == nil || w == nil {
		return
	}
	lang := strings.TrimSpace(r.URL.Query().Get("lang"))
	if lang == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    NormalizeLang(lang),
		Path:     "/",
		MaxAge:   60 * 60 * 24 * 365,
		HttpOnly: false,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func T(lang, key string, params ...P) string {
	value := lookup(NormalizeLang(lang), key)
	if value == "" && NormalizeLang(lang) != DefaultLang {
		value = lookup(DefaultLang, key)
	}
	if value == "" {
		return key
	}
	if len(params) > 0 {
		for name, replacement := range params[0] {
			value = strings.ReplaceAll(value, "{{"+name+"}}", replacement)
		}
	}
	return value
}

func TE(r *http.Request, key string, params ...P) string {
	return T(LangFromRequest(r), key, params...)
}

func FuncMap() template.FuncMap {
	return template.FuncMap{
		"t": T,
	}
}

func ExposedMessages(lang string, keys []string) map[string]string {
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		result[key] = T(lang, key)
	}
	return result
}

func MessagesJSON(lang string, keys []string) template.JS {
	data, err := json.Marshal(ExposedMessages(lang, keys))
	if err != nil {
		return "{}"
	}
	return template.JS(data)
}

func loadMessages() map[string]map[string]any {
	result := map[string]map[string]any{}
	for _, lang := range []string{DefaultLang, English} {
		data, err := localesFS.ReadFile("locales/" + lang + ".json")
		if err != nil {
			panic(err)
		}
		var locale map[string]any
		if err := json.Unmarshal(data, &locale); err != nil {
			panic(err)
		}
		result[lang] = locale
	}
	return result
}

func lookup(lang, key string) string {
	current, ok := messages[NormalizeLang(lang)]
	if !ok {
		current = messages[DefaultLang]
	}
	var value any = current
	for _, part := range strings.Split(key, ".") {
		nested, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = nested[part]
		if value == nil {
			return ""
		}
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}
