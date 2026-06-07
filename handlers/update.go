package handlers

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/executor"
	"github.com/naibabiji/wp-panel/models"

	"github.com/gin-gonic/gin"
)

// Ed25519 公钥，用于验证 Release 签名的 .sha256 文件。
// 对应私钥离线存储，不在 GitHub / CI 上。
const releasePubKeyHex = "ee8ec641204d785c6469b003c710666126a3156d902b78665bb73e859b6f9546"

type UpdateHandler struct {
	CurrentVersion string
}

const (
	binaryName  = "wp-panel"
	installPath = "/usr/local/bin/wp-panel"
)

var updateMu sync.Mutex

func getGithubProxy() string {
	var v string
	database.GetDB().QueryRow("SELECT svalue FROM security_settings WHERE skey = 'github_proxy'").Scan(&v)
	return v
}

func proxyURL(proxy, original string) string {
	if proxy != "" {
		return proxy + "/" + original
	}
	return original
}

func (h *UpdateHandler) Check(c *gin.Context) {
	latest, err := executor.FetchLatestPanelRelease(getGithubProxy())
	if err != nil {
		c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
			"current_version": h.CurrentVersion,
			"latest_version":  "",
			"has_update":      false,
			"error":           "获取版本信息失败",
		}))
		return
	}

	hasUpdate := executor.CompareVersions(latest.TagName, h.CurrentVersion) > 0

	notes := latest.Body
	if idx := strings.Index(notes, "**Full Changelog**"); idx >= 0 {
		notes = strings.TrimSpace(notes[:idx])
	}
	if notes == "" {
		notes = "（无更新说明）"
	}

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"current_version": h.CurrentVersion,
		"latest_version":  latest.TagName,
		"release_notes":   notes,
		"has_update":      hasUpdate,
	}))
}

func (h *UpdateHandler) Update(c *gin.Context) {
	if runtime.GOOS != "linux" {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("仅支持 Linux 服务器更新"))
		return
	}
	if !updateMu.TryLock() {
		c.JSON(http.StatusConflict, models.ErrorResponse("已有更新任务正在执行，请稍后再试"))
		return
	}
	defer updateMu.Unlock()

	proxy := getGithubProxy()

	latest, err := executor.FetchLatestPanelRelease(proxy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("获取版本信息失败"))
		return
	}

	if executor.CompareVersions(latest.TagName, h.CurrentVersion) <= 0 {
		c.JSON(http.StatusBadRequest, models.ErrorResponse("已经是最新版本"))
		return
	}

	var downloadURL string
	var sha256URL string
	var sigURL string
	for _, a := range latest.Assets {
		if a.Name == binaryName {
			downloadURL = a.BrowserDownloadURL
		}
		if a.Name == binaryName+".sha256" {
			sha256URL = a.BrowserDownloadURL
		}
		if a.Name == binaryName+".sha256.sig" {
			sigURL = a.BrowserDownloadURL
		}
	}
	if downloadURL == "" {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("未找到适用于当前系统的二进制文件"))
		return
	}
	if sha256URL == "" {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("未找到 SHA256 校验文件，无法验证更新完整性"))
		return
	}
	if sigURL == "" {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("未找到 Ed25519 签名文件，无法验证更新来源"))
		return
	}

	// Download new binary
	tmpDir, err := os.MkdirTemp("", "wp-panel-update-*")
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("创建临时目录失败"))
		return
	}
	defer os.RemoveAll(tmpDir)

	newBinary := filepath.Join(tmpDir, binaryName)
	if err := downloadFile(proxyURL(proxy, downloadURL), newBinary); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("下载失败"))
		return
	}
	if err := os.Chmod(newBinary, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("设置新版本权限失败"))
		return
	}

	// Verify SHA256
	shaFile := filepath.Join(tmpDir, binaryName+".sha256")
	if err := downloadFile(proxyURL(proxy, sha256URL), shaFile); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("SHA256 校验文件下载失败"))
		return
	}
	if err := verifySHA256(newBinary, shaFile); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("校验失败"))
		return
	}

	// Verify Ed25519 signature of checksum file
	sigFile := filepath.Join(tmpDir, binaryName+".sha256.sig")
	if err := downloadFile(proxyURL(proxy, sigURL), sigFile); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("签名文件下载失败"))
		return
	}
	if err := verifyEd25519(shaFile, sigFile); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("签名校验失败"))
		return
	}

	if err := preflightBinary(newBinary); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("新版本预检失败"))
		return
	}

	backupPath := versionedBackupPath(h.CurrentVersion)
	if err := copyFile(installPath, backupPath, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("备份旧版本失败"))
		return
	}

	stagedBinary := installPath + ".new"
	os.Remove(stagedBinary)
	if err := copyFile(newBinary, stagedBinary, 0755); err != nil {
		os.Remove(stagedBinary)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("暂存新版本失败"))
		return
	}
	if err := os.Rename(stagedBinary, installPath); err != nil {
		os.Remove(stagedBinary)
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("替换失败，旧版本仍保留"))
		return
	}
	if err := os.Chmod(installPath, 0755); err != nil {
		if rbErr := copyFile(backupPath, installPath, 0755); rbErr != nil {
			c.JSON(http.StatusInternalServerError, models.ErrorResponse("替换后权限设置失败，且自动回滚失败"))
			return
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse("替换后权限设置失败，已回滚"))
		return
	}

	// Restart service
	go func() {
		time.Sleep(500 * time.Millisecond)
		exec.Command("systemctl", "restart", "wp-panel").Run()
	}()

	c.JSON(http.StatusOK, models.SuccessResponse(gin.H{
		"message": fmt.Sprintf("正在更新到 %s，面板即将重启...", latest.TagName),
	}))
}

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dest)
		return err
	}
	return nil
}

func verifySHA256(filePath, shaFile string) error {
	data, err := os.ReadFile(shaFile)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return fmt.Errorf("SHA256 文件为空")
	}
	expected := fields[0]
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("SHA256 长度异常")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := fmt.Sprintf("%x", h.Sum(nil))

	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("SHA256 不匹配")
	}
	return nil
}

func preflightBinary(path string) error {
	// Depends on the --info flag registered in main.go; keep this lightweight
	// so updates fail before replacing the current binary when a build is broken.
	cmd := exec.Command(path, "--info")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func versionedBackupPath(currentVersion string) string {
	version := sanitizeBackupPart(currentVersion)
	if version == "" {
		version = "unknown"
	}
	ts := time.Now().UTC().Format("20060102-150405")
	return fmt.Sprintf("%s.bak.%s.%s", installPath, version, ts)
}

func sanitizeBackupPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func copyFile(srcPath, dstPath string, mode os.FileMode) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	copied := false
	dstClosed := false
	defer func() {
		if !dstClosed {
			dst.Close()
		}
		if !copied {
			os.Remove(dstPath)
		}
	}()

	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	dstClosed = true
	if err := os.Chmod(dstPath, mode); err != nil {
		return err
	}
	copied = true
	return nil
}

func verifyEd25519(shaFile, sigFile string) error {
	pubKey, err := hex.DecodeString(releasePubKeyHex)
	if err != nil {
		return fmt.Errorf("解析内置公钥失败")
	}
	sig, err := os.ReadFile(sigFile)
	if err != nil {
		return err
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("签名长度异常: %d", len(sig))
	}
	message, err := os.ReadFile(shaFile)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pubKey, message, sig) {
		return fmt.Errorf("Ed25519 签名不匹配")
	}
	return nil
}
