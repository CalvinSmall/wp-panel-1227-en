package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// validMemoryLimit matches valid PHP memory limit values like 128M, 256M, 1G, 512K, or raw byte counts.
var validMemoryLimit = regexp.MustCompile(`^\d+[KMG]?$`)

type WPOptimizations struct {
	DisableUpdates     bool
	DisableFileEditing bool
	WPDebug            bool
	WPPostRevisions    int    // -1 = don't set, >=0 = define value
	WPMemoryLimit      string // empty = don't set, e.g. "128M"
}

func ApplyWPOptimizations(webRoot string, opts WPOptimizations) error {
	configPath := filepath.Join(webRoot, "wp-config.php")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}

	content := string(data)

	// Boolean constants: insert when enabled, remove when disabled
	content = applyBoolConstant(content, "AUTOMATIC_UPDATER_DISABLED", opts.DisableUpdates)
	content = applyBoolConstant(content, "DISALLOW_FILE_EDIT", opts.DisableFileEditing)

	// WP_DEBUG: write full debug trio when enabled, remove WP_DEBUG and related constants when disabled
	content = applyBoolConstant(content, "WP_DEBUG", opts.WPDebug)
	if opts.WPDebug {
		content = applyBoolConstant(content, "WP_DEBUG_LOG", true)
		content = applyBoolConstant(content, "WP_DEBUG_DISPLAY", false)
	} else {
		content = removeConstant(content, "WP_DEBUG_LOG")
		content = removeConstant(content, "WP_DEBUG_DISPLAY")
	}

	// WP_POST_REVISIONS: -1 = don't touch, >=0 = write numeric value
	if opts.WPPostRevisions >= 0 {
		content = applyIntConstant(content, "WP_POST_REVISIONS", opts.WPPostRevisions)
	} else {
		content = removeConstant(content, "WP_POST_REVISIONS")
	}

	// WP_MEMORY_LIMIT: empty string removes it; invalid format is rejected (prevents single-quote etc. from breaking php file)
	if opts.WPMemoryLimit != "" {
		if validMemoryLimit.MatchString(strings.ToUpper(opts.WPMemoryLimit)) {
			content = applyStringConstant(content, "WP_MEMORY_LIMIT", strings.ToUpper(opts.WPMemoryLimit))
		}
	} else {
		content = removeConstant(content, "WP_MEMORY_LIMIT")
	}

	return os.WriteFile(configPath, []byte(content), 0600)
}

func constPattern(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*define\s*\(\s*'` + regexp.QuoteMeta(name) + `'\s*,\s*[^)]+\)\s*;\s*\n?`)
}

func applyBoolConstant(content, name string, enable bool) string {
	re := constPattern(name)
	has := re.MatchString(content)

	if enable && !has {
		stmt := fmt.Sprintf("define('%s', %v);\n", name, true)
		return insertBeforeMarker(content, stmt)
	} else if !enable && has {
		return re.ReplaceAllString(content, "")
	}
	return content
}

func applyIntConstant(content, name string, value int) string {
	re := constPattern(name)
	has := re.MatchString(content)
	stmt := fmt.Sprintf("define('%s', %d);\n", name, value)

	if has {
		return re.ReplaceAllString(content, stmt)
	}
	return insertBeforeMarker(content, stmt)
}

func applyStringConstant(content, name, value string) string {
	re := constPattern(name)
	has := re.MatchString(content)
	stmt := fmt.Sprintf("define('%s', '%s');\n", name, value)

	if has {
		return re.ReplaceAllString(content, stmt)
	}
	return insertBeforeMarker(content, stmt)
}

func removeConstant(content, name string) string {
	return constPattern(name).ReplaceAllString(content, "")
}

func insertBeforeMarker(content, insertion string) string {
	marker := "/* That's all, stop editing!"
	idx := strings.Index(content, marker)
	if idx < 0 {
		marker = "require_once ABSPATH . 'wp-settings.php';"
		idx = strings.Index(content, marker)
	}
	if idx >= 0 {
		return content[:idx] + insertion + content[idx:]
	}

	idx = strings.LastIndex(content, "?>")
	if idx > 0 {
		if idx > 0 && content[idx-1] != '\n' {
			insertion = "\n" + insertion
		}
		return content[:idx] + insertion + content[idx:]
	}

	phpOpen := "<?php"
	idx = strings.Index(content, phpOpen)
	if idx >= 0 {
		lineBreak := strings.IndexAny(content[idx+len(phpOpen):], "\r\n")
		if lineBreak >= 0 {
			insertPos := idx + len(phpOpen) + lineBreak + 1
			return content[:insertPos] + insertion + content[insertPos:]
		}
		return content[:idx+len(phpOpen)] + "\n" + insertion + "\n" + content[idx+len(phpOpen):]
	}

	return content + "\n" + insertion
}
