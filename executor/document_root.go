package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DocumentRootPublic = "public"

func NormalizeDocumentRootSubdir(siteType, subdir string) (string, error) {
	subdir = strings.TrimSpace(subdir)
	if siteType != "php" || subdir == "" || subdir == "." {
		return "", nil
	}
	subdir = strings.Trim(subdir, "/\\")
	if subdir != DocumentRootPublic {
		return "", fmt.Errorf("Web入口目录只支持留空（项目根）或填写 public")
	}
	return subdir, nil
}

func EffectiveDocumentRoot(projectRoot, siteType, subdir string) string {
	cleanRoot := filepath.Clean(projectRoot)
	normalized, err := NormalizeDocumentRootSubdir(siteType, subdir)
	if err != nil || normalized == "" {
		return cleanRoot
	}
	return filepath.Join(cleanRoot, normalized)
}

func EnsureEffectiveDocumentRoot(projectRoot, siteType, subdir, systemUser string) (string, error) {
	documentRoot := EffectiveDocumentRoot(projectRoot, siteType, subdir)
	normalized, err := NormalizeDocumentRootSubdir(siteType, subdir)
	if err != nil {
		return "", err
	}
	if normalized == "" {
		return documentRoot, nil
	}
	if err := os.MkdirAll(documentRoot, 0755); err != nil {
		return "", fmt.Errorf("创建Web入口目录失败: %w", err)
	}
	if strings.TrimSpace(systemUser) != "" {
		if _, err := executeCommand("chown", "-R", siteOwner(systemUser), documentRoot); err != nil {
			return "", fmt.Errorf("设置Web入口目录权限失败: %w", err)
		}
	}
	return documentRoot, nil
}
