package database

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// migratePluginConfigs moves wp-panel-config.json from inside the web directory to outside,
// and rotates the exposed plugin_api_key.
func migratePluginConfigs() error {
	rows, err := DB.Query("SELECT domain, web_root, system_user FROM websites WHERE plugin_api_key IS NOT NULL AND plugin_api_key != ''")
	if err != nil {
		return fmt.Errorf("failed to query website list: %w", err)
	}
	defer rows.Close()

	baseSecretsDir := "/var/wp-panel/site-secrets"
	os.MkdirAll(baseSecretsDir, 0711)
	os.Chmod(baseSecretsDir, 0711)

	for rows.Next() {
		var domain, webRoot, systemUser string
		if err := rows.Scan(&domain, &webRoot, &systemUser); err != nil {
			log.Printf("[Migration] Failed to read website data: %v", err)
			continue
		}

		oldPath := filepath.Join(webRoot, "wp-content", "plugins", "wp-panel-optimizer", "wp-panel-config.json")
		oldData, err := os.ReadFile(oldPath)
		if err != nil {
			continue
		}

		var oldCfg map[string]string
		if err := json.Unmarshal(oldData, &oldCfg); err != nil || oldCfg["panel_url"] == "" {
			log.Printf("[Migration] %s: Old config file invalid, deleting", domain)
			os.Remove(oldPath)
			continue
		}

		// Rotate API Key
		b := make([]byte, 16)
		rand.Read(b)
		newKey := hex.EncodeToString(b)

		// Write to new path
		secretsDir := filepath.Join(baseSecretsDir, domain)
		os.MkdirAll(secretsDir, 0700)
		newCfg, _ := json.Marshal(map[string]string{
			"panel_url": oldCfg["panel_url"],
			"api_key":   newKey,
		})
		cfgPath := filepath.Join(secretsDir, "wp-panel-config.json")
		if err := os.WriteFile(cfgPath, newCfg, 0600); err != nil {
			log.Printf("[Migration] %s: Failed to write new config file: %v", domain, err)
			continue
		}

		// Update database
		if _, err := DB.Exec("UPDATE websites SET plugin_api_key = ? WHERE domain = ?", newKey, domain); err != nil {
			log.Printf("[Migration] %s: Failed to update API Key: %v, cleaning up written config", domain, err)
			os.Remove(cfgPath)
			continue
		}

		// Sync latest plugin PHP to site to ensure the plugin reads config from the new path
		srcPlugin := "/www/server/panel/packages/wp-panel-optimizer.php"
		dstPlugin := filepath.Join(webRoot, "wp-content", "plugins", "wp-panel-optimizer", "wp-panel-optimizer.php")
		srcData, err := os.ReadFile(srcPlugin)
		if err != nil {
			log.Printf("[Migration] %s: Failed to read plugin package: %v", domain, err)
			continue
		}
		if err := os.WriteFile(dstPlugin, srcData, 0644); err != nil {
			log.Printf("[Migration] %s: Failed to update site plugin: %v", domain, err)
			continue
		}

		// Only delete the old config file after all of the above succeed
		os.Remove(oldPath)
		exec.Command("chown", "-R", systemUser+":"+systemUser, secretsDir).Run()

		log.Printf("[Migration] %s: Config file migrated to %s", domain, secretsDir)
	}

	return nil
}
