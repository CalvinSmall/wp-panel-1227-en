package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSanitizeBackupPart(t *testing.T) {
	got := sanitizeBackupPart("v1.2.3; rm -rf /_ok")
	want := "v1.2.3rm-rf_ok"
	if got != want {
		t.Fatalf("sanitizeBackupPart() = %q, want %q", got, want)
	}
}

func TestVersionedBackupPath(t *testing.T) {
	got := versionedBackupPath("v1.2.3; bad")
	if !strings.HasPrefix(got, installPath+".bak.v1.2.3bad.") {
		t.Fatalf("versionedBackupPath() = %q", got)
	}
	if strings.ContainsAny(strings.TrimPrefix(got, installPath+".bak."), " ;/\\") {
		t.Fatalf("versionedBackupPath contains unsafe characters: %q", got)
	}
}

func TestCopyFileCopiesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello"), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	if err := copyFile(src, dst, 0750); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("dst content = %q, want hello", string(data))
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0750 {
		t.Fatalf("dst mode = %o, want 0750", info.Mode().Perm())
	}
}

func TestCopyFileRemovesPartialDestinationOnFailure(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "missing")
	dst := filepath.Join(dir, "dst")

	if err := copyFile(src, dst, 0755); err == nil {
		t.Fatal("copyFile missing source error = nil, want error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst exists after failed copy, err=%v", err)
	}
}

func TestClampPercent(t *testing.T) {
	if got := clampPercent(-1); got != 0 {
		t.Fatalf("clampPercent(-1) = %d, want 0", got)
	}
	if got := clampPercent(42); got != 42 {
		t.Fatalf("clampPercent(42) = %d, want 42", got)
	}
	if got := clampPercent(101); got != 100 {
		t.Fatalf("clampPercent(101) = %d, want 100", got)
	}
}

func TestSetUpdateStepClearsTerminalAndDownloadState(t *testing.T) {
	restore := preserveUpdateStatus(t)

	updateStatusMu.Lock()
	currentUpdateStatus = panelUpdateStatus{
		Completed:       true,
		Stage:           "completed",
		Message:         "done",
		Percent:         100,
		DownloadPercent: 100,
		DownloadedBytes: 20,
		TotalBytes:      20,
		HasTotal:        true,
		Error:           "old error",
		UpdatedAt:       time.Now(),
	}
	updateStatusMu.Unlock()

	setUpdateStep("fetch_release", "正在获取版本信息...", 5)
	got := snapshotUpdateStatus()
	if !got.Running || got.Completed {
		t.Fatalf("running/completed = %v/%v, want true/false", got.Running, got.Completed)
	}
	if got.Error != "" {
		t.Fatalf("error = %q, want empty", got.Error)
	}
	if got.DownloadPercent != 0 || got.DownloadedBytes != 0 || got.TotalBytes != 0 || got.HasTotal {
		t.Fatalf("download state not cleared: %+v", got)
	}
	if got.Stage != "fetch_release" || got.Percent != 5 {
		t.Fatalf("stage/percent = %q/%d, want fetch_release/5", got.Stage, got.Percent)
	}

	restore()
}

func TestSetBinaryDownloadProgress(t *testing.T) {
	restore := preserveUpdateStatus(t)

	setBinaryDownloadProgress(50, 100)
	got := snapshotUpdateStatus()
	if !got.Running {
		t.Fatal("download progress should mark update as running")
	}
	if got.Stage != "download_binary" {
		t.Fatalf("stage = %q, want download_binary", got.Stage)
	}
	if got.DownloadPercent != 50 {
		t.Fatalf("download_percent = %d, want 50", got.DownloadPercent)
	}
	if got.Percent != 37 {
		t.Fatalf("overall percent = %d, want 37", got.Percent)
	}
	if !got.HasTotal {
		t.Fatal("expected HasTotal for positive total")
	}

	restore()
}

func TestSetUpdateFailedAndCompleted(t *testing.T) {
	restore := preserveUpdateStatus(t)

	setUpdateFailed("下载失败")
	failed := snapshotUpdateStatus()
	if failed.Running || failed.Completed {
		t.Fatalf("failed running/completed = %v/%v, want false/false", failed.Running, failed.Completed)
	}
	if failed.Error != "下载失败" || failed.Message != "下载失败" {
		t.Fatalf("failed message/error = %q/%q", failed.Message, failed.Error)
	}

	setUpdateCompleted("更新完成")
	done := snapshotUpdateStatus()
	if done.Running || !done.Completed {
		t.Fatalf("completed running/completed = %v/%v, want false/true", done.Running, done.Completed)
	}
	if done.Percent != 100 || done.Error != "" {
		t.Fatalf("completed percent/error = %d/%q", done.Percent, done.Error)
	}

	restore()
}

func TestSnapshotUpdateStatusReturnsValueCopy(t *testing.T) {
	restore := preserveUpdateStatus(t)

	setUpdateStep("backup", "正在备份当前版本...", 88)
	snapshot := snapshotUpdateStatus()
	snapshot.Stage = "mutated"

	got := snapshotUpdateStatus()
	if got.Stage != "backup" {
		t.Fatalf("global status was mutated through snapshot: %q", got.Stage)
	}

	restore()
}

func TestSnapshotUpdateStatusExpiresTerminalState(t *testing.T) {
	restore := preserveUpdateStatus(t)

	updateStatusMu.Lock()
	currentUpdateStatus = panelUpdateStatus{
		Completed: true,
		Stage:     "completed",
		Message:   "更新完成",
		Percent:   100,
		UpdatedAt: time.Now().Add(-updateTerminalStatusTTL - time.Second),
	}
	updateStatusMu.Unlock()

	got := snapshotUpdateStatus()
	if got.Stage != "idle" || got.Completed || got.Running || got.Percent != 0 {
		t.Fatalf("expired status = %+v, want idle", got)
	}

	restore()
}

func TestDownloadFileWithProgressReportsContentLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("12345"))
		_, _ = w.Write([]byte("67890"))
	}))
	defer server.Close()

	var lastDownloaded int64
	var lastTotal int64
	dst := filepath.Join(t.TempDir(), "wp-panel")
	err := downloadFileWithProgress(server.URL, dst, time.Second, func(downloaded, total int64) {
		lastDownloaded = downloaded
		lastTotal = total
	})
	if err != nil {
		t.Fatalf("downloadFileWithProgress: %v", err)
	}

	if lastDownloaded != 10 {
		t.Fatalf("downloaded = %d, want 10", lastDownloaded)
	}
	if lastTotal != 10 {
		t.Fatalf("total = %d, want 10", lastTotal)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(data) != "1234567890" {
		t.Fatalf("downloaded data = %q", string(data))
	}
}

func TestDownloadFileWithProgressWithoutContentLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("chunked"))
	}))
	defer server.Close()

	var lastDownloaded int64
	var lastTotal int64
	dst := filepath.Join(t.TempDir(), "wp-panel")
	err := downloadFileWithProgress(server.URL, dst, time.Second, func(downloaded, total int64) {
		lastDownloaded = downloaded
		lastTotal = total
	})
	if err != nil {
		t.Fatalf("downloadFileWithProgress: %v", err)
	}
	if lastDownloaded != 7 {
		t.Fatalf("downloaded = %d, want 7", lastDownloaded)
	}
	if lastTotal > 0 {
		t.Fatalf("total = %d, want non-positive when content length is unknown", lastTotal)
	}
}

func TestDownloadFileWithProgressRejectsNonOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer server.Close()

	dst := filepath.Join(t.TempDir(), "wp-panel")
	err := downloadFileWithProgress(server.URL, dst, time.Second, nil)
	if err == nil {
		t.Fatal("downloadFileWithProgress non-200 error = nil, want error")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("destination exists after non-200 response, stat err=%v", statErr)
	}
}

func preserveUpdateStatus(t *testing.T) func() {
	t.Helper()
	prev := snapshotUpdateStatus()
	restored := false
	restore := func() {
		if restored {
			return
		}
		updateStatusMu.Lock()
		currentUpdateStatus = prev
		updateStatusMu.Unlock()
		restored = true
	}
	t.Cleanup(restore)
	return restore
}
