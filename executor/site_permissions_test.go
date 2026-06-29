package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsPathWithinRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "wp-content")
	outside := t.TempDir()
	if err := os.MkdirAll(inside, 0755); err != nil {
		t.Fatal(err)
	}

	if !isPathWithinRoot(root, inside) {
		t.Fatal("inside path should be allowed")
	}
	if isPathWithinRoot(root, outside) {
		t.Fatal("outside path should be rejected")
	}
}

func TestChownSitePathRejectsUnsafeInputs(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "wp-content")
	outside := t.TempDir()
	if err := os.MkdirAll(inside, 0755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		path       string
		root       string
		systemUser string
	}{
		{name: "empty path", path: "", root: root, systemUser: "wp_site"},
		{name: "root path", path: string(filepath.Separator), root: root, systemUser: "wp_site"},
		{name: "empty allowed root", path: inside, root: "", systemUser: "wp_site"},
		{name: "unsafe allowed root", path: inside, root: string(filepath.Separator), systemUser: "wp_site"},
		{name: "outside allowed root", path: outside, root: root, systemUser: "wp_site"},
		{name: "empty system user", path: inside, root: root, systemUser: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ChownSitePath(tt.path, tt.root, tt.systemUser); err == nil {
				t.Fatal("ChownSitePath error = nil, want rejection")
			}
		})
	}
}

func TestApplyWPFileModsLockBlockAddsAndRemovesManagedBlock(t *testing.T) {
	content := "<?php\n" +
		"define('DB_NAME', 'wordpress');\n" +
		"/* That's all, stop editing! Happy publishing. */\n" +
		"require_once ABSPATH . 'wp-settings.php';\n"

	locked, err := applyWPFileModsLockBlock(content, true)
	if err != nil {
		t.Fatalf("apply lock: %v", err)
	}
	if !strings.Contains(locked, wpPanelFileLockBegin) || !strings.Contains(locked, "define('DISALLOW_FILE_MODS', true);") || !strings.Contains(locked, "define('FS_METHOD', 'direct');") {
		t.Fatalf("managed lock block missing:\n%s", locked)
	}
	if strings.Index(locked, wpPanelFileLockBegin) > strings.Index(locked, "/* That's all, stop editing!") {
		t.Fatal("managed lock block should be inserted before wp-config marker")
	}

	unlocked, err := applyWPFileModsLockBlock(locked, false)
	if err != nil {
		t.Fatalf("remove lock: %v", err)
	}
	if strings.Contains(unlocked, wpPanelFileLockBegin) || strings.Contains(unlocked, "DISALLOW_FILE_MODS") {
		t.Fatalf("managed lock block was not removed:\n%s", unlocked)
	}
}

func TestApplyWPFileModsLockBlockAddsFSMethodForUserDefinedDisallow(t *testing.T) {
	content := "<?php\n" +
		"define('DISALLOW_FILE_MODS', true);\n"
	locked, err := applyWPFileModsLockBlock(content, true)
	if err != nil {
		t.Fatalf("apply lock: %v", err)
	}
	if strings.Count(locked, "define('DISALLOW_FILE_MODS', true);") != 1 {
		t.Fatalf("managed block should not add duplicate DISALLOW_FILE_MODS: %s", locked)
	}
	if !strings.Contains(locked, "define('FS_METHOD', 'direct');") {
		t.Fatalf("managed block should ensure FS_METHOD direct: %s", locked)
	}
}

func TestApplyWPFileModsLockBlockClearsManagedFSMethodOnUnlock(t *testing.T) {
	content := "<?php\n" +
		"define('DISALLOW_FILE_MODS', true);\n" +
		"define('FS_METHOD', 'ftp');\n" +
		"/* That's all, stop editing! Happy publishing. */\n"
	locked, err := applyWPFileModsLockBlock(content, true)
	if err != nil {
		t.Fatalf("apply lock: %v", err)
	}
	if !strings.Contains(locked, wpPanelFileLockBegin) {
		t.Fatalf("managed block should be present while locked: %s", locked)
	}

	unlocked, err := applyWPFileModsLockBlock(locked, false)
	if err != nil {
		t.Fatalf("remove lock: %v", err)
	}
	if strings.Contains(unlocked, wpPanelFileLockBegin) {
		t.Fatalf("managed block should be removed after unlock: %s", unlocked)
	}
	if strings.Contains(unlocked, "define('FS_METHOD', 'direct');") {
		t.Fatal("managed FS_METHOD direct should be removed after unlock")
	}
}

func TestApplyWPFileModsLockBlockRejectsExistingFalseConstant(t *testing.T) {
	content := "<?php\n" +
		"define('DISALLOW_FILE_MODS', false);\n" +
		"/* That's all, stop editing! Happy publishing. */\n"

	if _, err := applyWPFileModsLockBlock(content, true); err == nil {
		t.Fatal("apply lock error = nil, want rejection for existing false constant")
	}
}

func TestWPConfigHasUserFileModsLockIgnoresManagedBlock(t *testing.T) {
	webRoot := t.TempDir()
	configPath := filepath.Join(webRoot, "wp-config.php")
	managedOnly := "<?php\n" +
		wpPanelFileLockBegin + "\n" +
		"define('DISALLOW_FILE_MODS', true);\n" +
		wpPanelFileLockEnd + "\n" +
		"/* That's all, stop editing! Happy publishing. */\n"
	if err := os.WriteFile(configPath, []byte(managedOnly), 0600); err != nil {
		t.Fatal(err)
	}
	if wpConfigHasUserFileModsLock(webRoot) {
		t.Fatal("managed lock block should not be treated as a user lock")
	}

	userDefined := "<?php\n" +
		"define(\"DISALLOW_FILE_MODS\", true);\n" +
		"/* That's all, stop editing! Happy publishing. */\n"
	if err := os.WriteFile(configPath, []byte(userDefined), 0600); err != nil {
		t.Fatal(err)
	}
	if !wpConfigHasUserFileModsLock(webRoot) {
		t.Fatal("user-defined DISALLOW_FILE_MODS=true should be reported")
	}
}

func TestWPFileLockRuntimeWritablePathPolicy(t *testing.T) {
	root := t.TempDir()

	allowed := []string{
		filepath.Join(root, "wp-content", "uploads", "2026", "photo.jpg"),
		filepath.Join(root, "wp-content", "cache", "page.html"),
		filepath.Join(root, "wp-content", "cache", "pages", ".htaccess"),
		filepath.Join(root, "wp-content", "languages", "zh_CN.mo"),
		filepath.Join(root, "wp-content", "wflogs", "rules.php.json"),
	}
	for _, path := range allowed {
		if !IsWPFileLockRuntimeWritablePath(root, path, false, false) {
			t.Fatalf("%s should be writable runtime data", path)
		}
	}

	blocked := []string{
		filepath.Join(root, "index.php"),
		filepath.Join(root, "wp-config.php"),
		filepath.Join(root, "wordfence-waf.php"),
		filepath.Join(root, "wp-content"),
		filepath.Join(root, "wp-content", "advanced-cache.php"),
		filepath.Join(root, "wp-content", ".user.ini"),
		filepath.Join(root, "wp-content", "cache", ".user.ini"),
		filepath.Join(root, "wp-content", "upgrade", "wordpress.zip"),
		filepath.Join(root, "wp-content", "upgrade", "update.php"),
		filepath.Join(root, "wp-content", "upgrade-temp-backup", "plugins", "plugin.zip"),
		filepath.Join(root, "wp-content", "uploads", "shell.php"),
		filepath.Join(root, "wp-content", "plugins", "plugin.php"),
		filepath.Join(root, "wp-content", "themes", "theme", "functions.php"),
		filepath.Join(root, "wp-content", "mu-plugins", "loader.php"),
	}
	for _, path := range blocked {
		if IsWPFileLockRuntimeWritablePath(root, path, false, false) {
			t.Fatalf("%s should be blocked by file lock", path)
		}
	}

	if !IsWPFileLockRuntimeWritablePath(root, filepath.Join(root, "wp-content", "uploads", "shell.php"), false, true) {
		t.Fatal("runtime PHP cleanup should be allowed when explicitly requested")
	}
	if IsWPFileLockRuntimeWritablePath(root, filepath.Join(root, "wp-content", "advanced-cache.php"), false, true) {
		t.Fatal("drop-in PHP should stay blocked even during cleanup")
	}
	if wpFileLockPermissionWritablePath(root, filepath.Join(root, "wp-content"), true) {
		t.Fatal("wp-content root should not be writable")
	}
	if !wpFileLockPermissionWritablePath(root, filepath.Join(root, "wp-content", "cache"), true) {
		t.Fatal("existing runtime directories should be writable")
	}
	if wpFileLockPermissionWritablePath(root, filepath.Join(root, "wp-content", "upgrade"), true) {
		t.Fatal("upgrade directory should stay locked")
	}
	if wpFileLockPermissionWritablePath(root, filepath.Join(root, "wp-content", "upgrade-temp-backup"), true) {
		t.Fatal("upgrade-temp-backup directory should stay locked")
	}
}

func TestApplyWPFileModsLockBlockFallbackForNonstandardWPConfig(t *testing.T) {
	content := "<?php\n" +
		"define('DB_NAME', 'wordpress');\n" +
		"define('DB_USER', 'admin');\n"
	locked, err := applyWPFileModsLockBlock(content, true)
	if err != nil {
		t.Fatalf("apply lock: %v", err)
	}
	if !strings.Contains(locked, wpPanelFileLockBegin) {
		t.Fatal("managed lock block should be injected for nonstandard wp-config")
	}
	if !strings.Contains(locked, "define('DISALLOW_FILE_MODS', true);") {
		t.Fatalf("managed lock constant should be present: %s", locked)
	}
	if !strings.Contains(locked, "<?php") {
		t.Fatal("original PHP open tag should remain")
	}

	unlocked, err := applyWPFileModsLockBlock(locked, false)
	if err != nil {
		t.Fatalf("remove lock: %v", err)
	}
	if strings.Contains(unlocked, wpPanelFileLockBegin) || strings.Contains(unlocked, "DISALLOW_FILE_MODS") {
		t.Fatalf("managed lock block should be removed: %s", unlocked)
	}

	configWithClose := "<?php\n" +
		"define('DB_NAME', 'wordpress');\n" +
		"?>\n"
	got := insertBeforeMarker(configWithClose, wpPanelFileLockBegin+"\n"+"define('DISALLOW_FILE_MODS', true);\n"+wpPanelFileLockEnd+"\n")
	if !strings.Contains(got, wpPanelFileLockBegin) {
		t.Fatal("should inject before closing PHP tag when marker is missing")
	}
	if idxTag := strings.Index(got, wpPanelFileLockBegin); idxTag >= strings.Index(got, "?>") {
		t.Fatal("managed block should be before ?> when inserted by fallback")
	}
}
