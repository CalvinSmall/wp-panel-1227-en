package executor

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/naibabiji/wp-panel/config"
	"github.com/naibabiji/wp-panel/database"
)

var mysqlIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func isValidMySQLIdentifier(name string) bool {
	return name != "" && len(name) <= 64 && mysqlIdentifierPattern.MatchString(name)
}

func wpOptionsTableName(tablePrefix string) (string, error) {
	tablePrefix = strings.TrimSpace(tablePrefix)
	if !IsValidWPTablePrefix(tablePrefix) {
		return "", fmt.Errorf("invalid WordPress table prefix")
	}
	tableName := tablePrefix + "options"
	if !isValidMySQLIdentifier(tableName) {
		return "", fmt.Errorf("invalid WordPress options table name")
	}
	return tableName, nil
}

func runMySQL(rootPassword string, args ...string) error {
	cmd := exec.Command("mysql", args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+rootPassword)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mysql: %s", stderr.String())
	}
	return nil
}

func createMariaDBDatabase(dbName, dbUser, dbPassword string, cfg *config.Config) error {
	dbUser = strings.ReplaceAll(dbUser, "'", "''")
	dbPassword = strings.ReplaceAll(dbPassword, "'", "''")

	if err := runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e",
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", dbName)); err != nil {
		return err
	}

	// DROP + CREATE ensures password is always consistent, avoiding old user password not updating under IF NOT EXISTS
	runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e",
		fmt.Sprintf("DROP USER IF EXISTS '%s'@'localhost'", dbUser))
	if err := runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e",
		fmt.Sprintf("CREATE USER '%s'@'localhost' IDENTIFIED BY '%s'", dbUser, dbPassword)); err != nil {
		return err
	}

	if err := runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e",
		fmt.Sprintf("GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'localhost'", dbName, dbUser)); err != nil {
		return err
	}

	return runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e", "FLUSH PRIVILEGES")
}

func dropMariaDBDatabase(dbName, dbUser string, cfg *config.Config) error {
	if err := runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e", fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", dbName)); err != nil {
		return err
	}
	if err := runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e", fmt.Sprintf("DROP USER IF EXISTS '%s'@'localhost'", dbUser)); err != nil {
		return err
	}
	return runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e", "FLUSH PRIVILEGES")
}

func changeMariaDBPassword(dbUser, newPassword string, cfg *config.Config) error {
	newPassword = strings.ReplaceAll(newPassword, "'", "''")

	if err := runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e",
		fmt.Sprintf("ALTER USER '%s'@'localhost' IDENTIFIED BY '%s'", dbUser, newPassword)); err != nil {
		return fmt.Errorf("Failed to change database password: %w", err)
	}

	return runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e", "FLUSH PRIVILEGES")
}

func executeChangeDBPassword(task *Task) TaskResult {
	payload, ok := task.Payload.(*ChangeDBPasswordPayload)
	if !ok {
		return TaskResult{Success: false, Message: "Invalid task parameter type"}
	}

	site := payload.Site
	cfg := config.AppConfig

	newPassword := payload.NewPassword
	if newPassword == "" {
		newPassword = generatePassword(24)
	}

	if site.SiteType == "php" {
		if err := changeMariaDBPassword(site.DBUser, newPassword, cfg); err != nil {
			log.Printf("MariaDB operation failed: %v", err)
			return TaskResult{Success: false, Message: "MariaDB operation failed"}
		}
		db := database.GetDB()
		db.Exec("UPDATE websites SET updated_at = CURRENT_TIMESTAMP WHERE id = ?", site.ID)
		masked := maskPassword(newPassword)
		return TaskResult{
			Success: true,
			Message: "Database password updated",
			Data:    map[string]interface{}{"new_password": masked},
		}
	}

	configPath := filepath.Join(site.WebRoot, "wp-config.php")
	content, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("Failed to read wp-config.php: %v", err)
		return TaskResult{Success: false, Message: "Failed to read wp-config.php"}
	}

	re := regexp.MustCompile(`define\(\s*'DB_PASSWORD'\s*,\s*'[^']*'\s*\)`)
	newContent := re.ReplaceAllString(string(content),
		fmt.Sprintf("define('DB_PASSWORD', '%s')", phpSingleQuoteEscape(newPassword)))

	if newContent == string(content) {
		return TaskResult{Success: false, Message: "DB_PASSWORD definition not found, wp-config.php may be malformed"}
	}

	if err := os.WriteFile(configPath, []byte(newContent), 0600); err != nil {
		log.Printf("Failed to update wp-config.php: %v", err)
		return TaskResult{Success: false, Message: "Failed to update wp-config.php"}
	}

	if err := changeMariaDBPassword(site.DBUser, newPassword, cfg); err != nil {
		os.WriteFile(configPath, content, 0600)
		log.Printf("MariaDB operation failed, wp-config.php rolled back: %v", err)
		return TaskResult{Success: false, Message: "MariaDB operation failed"}
	}

	masked := maskPassword(newPassword)

	db := database.GetDB()
	db.Exec("UPDATE websites SET updated_at = CURRENT_TIMESTAMP WHERE id = ?", site.ID)

	return TaskResult{
		Success: true,
		Message: "Database password updated",
		Data:    map[string]interface{}{"new_password": masked},
	}
}

// DetectDBTablePrefix queries the actual WordPress table prefix in the database
// Returns recommended prefix, all candidate prefixes, error
func DetectDBTablePrefix(dbName string, cfg *config.Config) (string, []string, error) {
	if !isValidMySQLIdentifier(dbName) {
		return "", nil, fmt.Errorf("invalid database name")
	}
	cmd := exec.Command("mysql", "-u", cfg.MariaDB.RootUser, "-N", "-e",
		fmt.Sprintf("SHOW TABLES FROM `%s` LIKE '%%options'", dbName))
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+cfg.MariaDB.RootPassword)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("Query failed: %s", strings.TrimSpace(stderr.String()))
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return "", nil, fmt.Errorf("options table not found, database may be empty or not a WordPress database")
	}

	prefixSet := make(map[string]struct{})
	for _, line := range strings.Split(output, "\n") {
		tableName := strings.TrimSpace(line)
		if prefix, ok := tablePrefixFromOptionsTable(tableName); ok {
			prefixSet[prefix] = struct{}{}
		}
	}

	if len(prefixSet) == 0 {
		return "", nil, fmt.Errorf("Cannot parse table prefix")
	}

	var candidates []string
	for p := range prefixSet {
		candidates = append(candidates, p)
	}
	sort.Strings(candidates)

	return candidates[0], candidates, nil
}

func tablePrefixFromOptionsTable(tableName string) (string, bool) {
	if !strings.HasSuffix(tableName, "options") {
		return "", false
	}
	prefix := strings.TrimSuffix(tableName, "options")
	if !IsValidWPTablePrefix(prefix) {
		return "", false
	}
	return prefix, true
}

// ReadWPSiteURLs reads siteurl and home from wp_options
func ReadWPSiteURLs(dbName, tablePrefix string, cfg *config.Config) (siteURL, homeURL string, err error) {
	if !isValidMySQLIdentifier(dbName) {
		return "", "", fmt.Errorf("invalid database name")
	}
	tableName, err := wpOptionsTableName(tablePrefix)
	if err != nil {
		return "", "", err
	}
	query := fmt.Sprintf(
		"SELECT option_name, option_value FROM `%s`.`%s` WHERE option_name IN ('siteurl','home')",
		dbName, tableName)
	cmd := exec.Command("mysql", "-u", cfg.MariaDB.RootUser, "-N", "-e", query)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+cfg.MariaDB.RootPassword)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("Query failed: %s", strings.TrimSpace(stderr.String()))
	}

	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "siteurl":
			siteURL = parts[1]
		case "home":
			homeURL = parts[1]
		}
	}
	return siteURL, homeURL, nil
}

func ReadWPDiagnosticOptions(dbName, tablePrefix string, cfg *config.Config) (map[string]string, error) {
	if cfg == nil {
		return nil, fmt.Errorf("Panel configuration not initialized")
	}
	if !isValidMySQLIdentifier(dbName) {
		return nil, fmt.Errorf("invalid database name")
	}
	tableName, err := wpOptionsTableName(tablePrefix)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(
		"SELECT option_name, option_value FROM `%s`.`%s` WHERE option_name IN ('template','stylesheet','active_plugins')",
		dbName, tableName)
	cmd := exec.Command("mysql", "-u", cfg.MariaDB.RootUser, "-N", "-e", query)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+cfg.MariaDB.RootPassword)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("Query failed: %s", strings.TrimSpace(stderr.String()))
	}

	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "template", "stylesheet", "active_plugins":
			result[parts[0]] = parts[1]
		}
	}
	return result, nil
}

// UpdateWPSiteURLs updates siteurl and home in wp_options (only non-empty fields)
func UpdateWPSiteURLs(dbName, tablePrefix, newSiteURL, newHomeURL string, cfg *config.Config) error {
	if newSiteURL == "" && newHomeURL == "" {
		return fmt.Errorf("At least one URL must be provided")
	}
	if !isValidMySQLIdentifier(dbName) {
		return fmt.Errorf("invalid database name")
	}
	tableName, err := wpOptionsTableName(tablePrefix)
	if err != nil {
		return err
	}

	if newSiteURL != "" {
		escURL := strings.ReplaceAll(newSiteURL, "'", "''")
		query := fmt.Sprintf(
			"UPDATE `%s`.`%s` SET option_value = '%s' WHERE option_name = 'siteurl'",
			dbName, tableName, escURL)
		if err := runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e", query); err != nil {
			return fmt.Errorf("Failed to update siteurl: %w", err)
		}
	}

	if newHomeURL != "" {
		escURL := strings.ReplaceAll(newHomeURL, "'", "''")
		query := fmt.Sprintf(
			"UPDATE `%s`.`%s` SET option_value = '%s' WHERE option_name = 'home'",
			dbName, tableName, escURL)
		if err := runMySQL(cfg.MariaDB.RootPassword, "-u", cfg.MariaDB.RootUser, "-e", query); err != nil {
			return fmt.Errorf("Failed to update home: %w", err)
		}
	}

	return nil
}

func maskPassword(pw string) string {
	if len(pw) < 8 {
		return "****"
	}
	return pw[:4] + "****" + pw[len(pw)-4:]
}
