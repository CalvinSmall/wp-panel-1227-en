package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/executor"
	"github.com/naibabiji/wp-panel/models"

	"github.com/gin-gonic/gin"
)

type BackupHandler struct{}

var mysqlIdentifierRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func (h *BackupHandler) List(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	db := database.GetDB()
	rows, err := db.Query(`SELECT id, site_id, filename, file_size, db_name, auto, transport_status, transport_message, created_at
		FROM db_backups WHERE site_id = ? ORDER BY created_at DESC`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("query failed"))
		return
	}
	defer rows.Close()

	var backups []models.DBBackup
	for rows.Next() {
		var b models.DBBackup
		var auto int
		if rows.Scan(&b.ID, &b.SiteID, &b.Filename, &b.FileSize, &b.DBName, &auto,
			&b.TransportStatus, &b.TransportMessage, &b.CreatedAt) != nil {
			continue
		}
		b.Auto = auto == 1
		backups = append(backups, b)
	}
	if backups == nil {
		backups = []models.DBBackup{}
	}
	c.JSON(http.StatusOK, models.SuccessResponse(backups))
}

func (h *BackupHandler) Create(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("website not found"))
		return
	}
	payload := &executor.CreateBackupPayload{Site: site, Auto: false}
	task := executor.GlobalQueue.Enqueue(executor.TaskCreateBackup, payload)
	result := <-task.ResultCh
	if result.Success {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": result.Message}))
	} else {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(result.Message))
	}
}

func (h *BackupHandler) Delete(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	bid, _ := strconv.Atoi(c.Param("bid"))

	db := database.GetDB()
	var filename string
	err := db.QueryRow("SELECT filename FROM db_backups WHERE id = ? AND site_id = ?", bid, id).Scan(&filename)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("backup record not found"))
		return
	}
	executor.ExecuteDeleteBackup(id, filename)
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "deleted"}))
}

func (h *BackupHandler) Download(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	bid, _ := strconv.Atoi(c.Param("bid"))

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("website not found"))
		return
	}

	db := database.GetDB()
	var filename string
	err := db.QueryRow("SELECT filename FROM db_backups WHERE id = ? AND site_id = ?", bid, id).Scan(&filename)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("backup record not found"))
		return
	}

	backupDir := filepath.Join("/www/server/panel/backups", site.Domain, "db")
	filePath := filepath.Join(backupDir, filename)
	if _, err := os.Stat(filePath); err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("backup file not found"))
		return
	}
	c.FileAttachment(filePath, filename)
}

func (h *BackupHandler) Restore(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	bid, _ := strconv.Atoi(c.Param("bid"))

	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("website not found"))
		return
	}

	db := database.GetDB()
	var filename string
	err := db.QueryRow("SELECT filename FROM db_backups WHERE id = ? AND site_id = ?", bid, id).Scan(&filename)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("backup record not found"))
		return
	}

	// Check if the local file exists
	backupDir := filepath.Join("/www/server/panel/backups", site.Domain, "db")
	filePath := filepath.Join(backupDir, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		var remoteEnabled int
		database.GetDB().QueryRow("SELECT enabled FROM remote_backup_settings WHERE id = 1").Scan(&remoteEnabled)
		if remoteEnabled == 1 {
			c.JSON(http.StatusNotFound, models.ErrorResponse("This backup has been synced to remote server. The local file was deleted per settings. Please download from remote server and upload to restore."))
		} else {
			c.JSON(http.StatusNotFound, models.ErrorResponse("backup file not found, may have been cleaned up"))
		}
		return
	}

	payload := &executor.RestoreBackupPayload{Site: site, Filename: filename}
	task := executor.GlobalQueue.Enqueue(executor.TaskRestoreBackup, payload)
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"async":   true,
		"message": "Database restore task has started",
		"task_id": task.ID,
		"status":  task.Status,
	}))
}

func (h *BackupHandler) UploadRestore(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("website not found"))
		return
	}

	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("please select a backup file"))
		return
	}
	ext := strings.ToLower(filepath.Ext(file.Filename))
	if ext != ".gz" && ext != ".sql" && ext != ".zip" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("only .sql / .sql.gz / .zip formats are supported"))
		return
	}

	tmpFile, err := os.CreateTemp("", "wppanel_upload_*"+ext)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("failed to create temporary file"))
		return
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	if err := c.SaveUploadedFile(file, tmpPath); err != nil {
		os.Remove(tmpPath)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("upload failed"))
		return
	}

	payload := &executor.RestoreBackupPayload{Site: site, FilePath: tmpPath, RemoveFileAfter: true}
	task := executor.GlobalQueue.Enqueue(executor.TaskRestoreBackup, payload)
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"async":   true,
		"message": "Database restore task has started",
		"task_id": task.ID,
		"status":  task.Status,
	}))
}

func (h *BackupHandler) RestoreStatus(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("website not found"))
		return
	}

	taskID := strings.TrimSpace(c.Param("task_id"))
	task, ok := executor.GlobalQueue.GetTask(taskID)
	if !ok {
		c.JSON(http.StatusNotFound, models.ErrorResponse("restore task not found"))
		return
	}
	payload, ok := task.Payload.(*executor.RestoreBackupPayload)
	if !ok || task.Type != executor.TaskRestoreBackup || payload.Site == nil || payload.Site.ID != site.ID {
		c.JSON(http.StatusNotFound, models.ErrorResponse("restore task not found"))
		return
	}

	message := "Database restore waiting"
	if task.Status == executor.TaskStatusRunning {
		message = "Database restoring"
	}
	success := false
	if task.Result != nil {
		message = task.Result.Message
		success = task.Result.Success
	}
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"task_id": task.ID,
		"status":  task.Status,
		"success": success,
		"message": message,
	}))
}

func (h *BackupHandler) GetSettings(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	db := database.GetDB()
	var enabled, keepCount int
	err := db.QueryRow("SELECT enabled, keep_count FROM backup_settings WHERE site_id = ?", id).Scan(&enabled, &keepCount)
	if err != nil {
		c.JSON(http.StatusOK, models.SuccessResponse(models.BackupSettings{Enabled: false, KeepCount: 7}))
		return
	}
	c.JSON(http.StatusOK, models.SuccessResponse(models.BackupSettings{Enabled: enabled == 1, KeepCount: keepCount}))
}

func (h *BackupHandler) UpdateSettings(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var req models.BackupSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("invalid parameters"))
		return
	}
	if req.KeepCount < 1 {
		req.KeepCount = 1
	}
	if req.KeepCount > 30 {
		req.KeepCount = 30
	}
	enabledVal := 0
	if req.Enabled {
		enabledVal = 1
	}
	db := database.GetDB()
	db.Exec(`INSERT INTO backup_settings (site_id, enabled, keep_count) VALUES (?, ?, ?)
		ON CONFLICT(site_id) DO UPDATE SET enabled = ?, keep_count = ?`,
		id, enabledVal, req.KeepCount, enabledVal, req.KeepCount)
	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Settings saved"}))
}

func (h *BackupHandler) ClearDatabase(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	site := getWebsiteByID(id)
	if site == nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse("website not found"))
		return
	}

	if site.DBName == "" || len(site.DBName) > 64 || !mysqlIdentifierRe.MatchString(site.DBName) {
		log.Printf("Rejected clearing abnormal database name site=%d db=%q", id, site.DBName)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("Abnormal database name, request rejected"))
		return
	}

	dbPass := readMariaDBPassword()
	if dbPass == "" {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("unable to read database password"))
		return
	}

	if err := executor.ClearDatabaseTables(site.DBName, dbPass); err != nil {
		log.Printf("Failed to clear database site=%s: %v", site.DBName, err)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse(err.Error()))
		return
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{"message": "Database cleared"}))
}

func readMariaDBPassword() string {
	data, err := os.ReadFile("/www/server/panel/config.json")
	if err != nil {
		return ""
	}
	var cfg struct {
		MariaDB struct {
			RootPassword string `json:"root_password"`
		} `json:"mariadb"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || cfg.MariaDB.RootPassword == "" {
		return ""
	}
	return cfg.MariaDB.RootPassword
}
