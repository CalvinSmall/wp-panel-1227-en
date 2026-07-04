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
		return "", fmt.Errorf("Web entry directory only supports empty (project root) or 'public'")
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
		return "", fmt.Errorf("Failed to create Web entry directory: %w", err)
	}
	if strings.TrimSpace(systemUser) != "" {
		if _, err := executeCommand("chown", "-R", siteOwner(systemUser), documentRoot); err != nil {
			return "", fmt.Errorf("Failed to set Web entry directory permissions: %w", err)
		}
	}
	return documentRoot, nil
}
