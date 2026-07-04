package executor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/naibabiji/wp-panel/database"
)

const backupsRoot = "/www/server/panel/backups"

const (
	s3SinglePutMaxSize = 5 * 1024 * 1024 * 1024 * int64(1)
	s3ObjectMaxSize    = 5 * 1024 * 1024 * 1024 * 1024 * int64(1)
	s3MaxPartCount     = 10000
	s3UploadTimeout    = 6 * time.Hour
)

var (
	s3MultipartThreshold = 100 * 1024 * 1024 * int64(1)
	s3DefaultPartSize    = 64 * 1024 * 1024 * int64(1)
)

var (
	remoteUsernamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,31}$`)
	remoteHostPattern     = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
	remotePathPattern     = regexp.MustCompile(`^[/~][A-Za-z0-9._~/-]*$`)
	s3BucketPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	s3RegionPattern       = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	s3AccessKeyPattern    = regexp.MustCompile(`^[A-Za-z0-9._/+=:@-]{3,256}$`)
	s3PathPrefixPattern   = regexp.MustCompile(`^[A-Za-z0-9._~/-]*$`)
)

func ValidateRemoteBackupSettings(host string, port int, username string, authType string, remotePath string) error {
	host = strings.TrimSpace(host)
	username = strings.TrimSpace(username)
	authType = strings.TrimSpace(authType)
	remotePath = strings.TrimSpace(remotePath)

	if host == "" {
		return fmt.Errorf("Remote server address cannot be empty")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("Remote port invalid")
	}
	if !remoteUsernamePattern.MatchString(username) {
		return fmt.Errorf("Remote username format invalid")
	}
	if authType != "password" && authType != "key" {
		return fmt.Errorf("Remote authentication method invalid")
	}
	if ip := net.ParseIP(host); ip != nil {
		if strings.Contains(host, ":") {
			return fmt.Errorf("Remote server address does not support IPv6")
		}
	} else {
		if !remoteHostPattern.MatchString(host) || strings.Contains(host, "..") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
			return fmt.Errorf("Remote server address format invalid")
		}
		for _, label := range strings.Split(host, ".") {
			if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return fmt.Errorf("Remote server address format invalid")
			}
		}
	}
	if remotePath != "" {
		if !remotePathPattern.MatchString(remotePath) || strings.Contains(remotePath, "//") {
			return fmt.Errorf("Remote backup directory format invalid")
		}
		for _, part := range strings.Split(remotePath, "/") {
			if part == ".." {
				return fmt.Errorf("Remote backup directory cannot contain ..")
			}
		}
	}
	return nil
}

func ValidateRemoteBackupType(backupType string) error {
	switch strings.TrimSpace(backupType) {
	case "", "rsync", "s3":
		return nil
	default:
		return fmt.Errorf("Remote backup type invalid")
	}
}

func ValidateS3BackupSettings(endpoint, bucket, region, accessKeyID, secretKey, pathPrefix string) error {
	endpoint = strings.TrimSpace(endpoint)
	bucket = strings.TrimSpace(bucket)
	region = strings.TrimSpace(region)
	accessKeyID = strings.TrimSpace(accessKeyID)
	secretKey = strings.TrimSpace(secretKey)
	pathPrefix = strings.Trim(strings.TrimSpace(pathPrefix), "/")

	if endpoint == "" {
		return fmt.Errorf("S3 Endpoint cannot be empty")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("S3 Endpoint must be an HTTPS address")
	}
	if strings.ContainsAny(u.Host, "\r\n\t ") {
		return fmt.Errorf("S3 Endpoint format invalid")
	}
	if !s3BucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") || strings.Contains(bucket, ".-") || strings.Contains(bucket, "-.") {
		return fmt.Errorf("S3 Bucket name format invalid")
	}
	if region == "" {
		return fmt.Errorf("S3 Region cannot be empty")
	}
	if !s3RegionPattern.MatchString(region) {
		return fmt.Errorf("S3 Region format invalid")
	}
	if !s3AccessKeyPattern.MatchString(accessKeyID) {
		return fmt.Errorf("S3 Access Key ID format invalid")
	}
	if secretKey == "" || strings.ContainsAny(secretKey, "\x00\r\n") {
		return fmt.Errorf("S3 Secret Access Key format invalid")
	}
	if pathPrefix != "" {
		if !s3PathPrefixPattern.MatchString(pathPrefix) || strings.Contains(pathPrefix, "//") {
			return fmt.Errorf("S3 backup path prefix format invalid")
		}
		for _, part := range strings.Split(pathPrefix, "/") {
			if part == "." || part == ".." {
				return fmt.Errorf("S3 backup path prefix cannot contain . or ..")
			}
		}
	}
	return nil
}

func remoteBackupPath(username, remotePath string) string {
	remotePath = strings.TrimSpace(remotePath)
	if remotePath == "" {
		return "/home/" + username + "/backup"
	}
	return strings.TrimRight(remotePath, "/")
}

func localBackupRelPath(localFile string) (string, error) {
	base := filepath.Clean(backupsRoot)
	clean := filepath.Clean(localFile)
	rel, err := filepath.Rel(base, clean)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", fmt.Errorf("Local backup file path invalid")
	}
	if strings.ContainsAny(rel, "\x00\r\n") {
		return "", fmt.Errorf("Local backup file path invalid")
	}
	return filepath.ToSlash(rel), nil
}

// SyncBackupToRemote syncs a single backup file to the remote server, preserving domain/db/ or domain/files/ directory structure.
// If keep_local=0, deletes the local file after successful sync.
func SyncBackupToRemote(localFile string) {
	db := database.GetDB()
	var enabled, keepLocal, port int
	var backupType, host, username, authType, password, remotePath string
	var s3Endpoint, s3Bucket, s3Region, s3AccessKeyID, s3SecretKey, s3PathPrefix string
	err := db.QueryRow(`SELECT enabled, backup_type, host, port, username, auth_type, password, remote_path, keep_local,
			s3_endpoint, s3_bucket, s3_region, s3_access_key_id, s3_secret_key, s3_path_prefix
		FROM remote_backup_settings WHERE id = 1`).Scan(
		&enabled, &backupType, &host, &port, &username, &authType, &password, &remotePath, &keepLocal,
		&s3Endpoint, &s3Bucket, &s3Region, &s3AccessKeyID, &s3SecretKey, &s3PathPrefix)
	if err != nil {
		syncLog("", fmt.Sprintf("Failed to read remote backup settings: %v", err), "failed")
		return
	}
	if enabled == 0 {
		return
	}
	if backupType == "" {
		backupType = "rsync"
	}
	if err := ValidateRemoteBackupType(backupType); err != nil {
		syncLog("", err.Error(), "failed")
		return
	}
	if backupType == "s3" {
		syncBackupToS3(localFile, s3Endpoint, s3Bucket, s3Region, s3AccessKeyID, s3SecretKey, s3PathPrefix, keepLocal)
		return
	}
	syncBackupToRsync(localFile, host, port, username, authType, password, remotePath, keepLocal)
}

func syncBackupToRsync(localFile string, host string, port int, username string, authType string, password string, remotePath string, keepLocal int) {
	if host == "" {
		syncLog("", "Remote backup enabled but server address not provided", "failed")
		return
	}
	if port == 0 {
		port = 22
	}
	if username == "" {
		username = "root"
	}
	if authType == "" {
		authType = "password"
	}
	remotePath = remoteBackupPath(username, remotePath)
	if err := ValidateRemoteBackupSettings(host, port, username, authType, remotePath); err != nil {
		syncLog("", "Remote backup settings invalid: "+err.Error(), "failed")
		return
	}
	relPath, err := localBackupRelPath(localFile)
	if err != nil {
		syncLog("", err.Error(), "failed")
		return
	}

	// Use /. to separate backup root from relative path; rsync -R preserves structure after ./
	src := backupsRoot + "/./" + relPath
	dest := fmt.Sprintf("%s@%s:%s/", username, host, remotePath)

	sshOpts := fmt.Sprintf("-o UserKnownHostsFile=/www/server/panel/remote_backup_known_hosts -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -p %d", port)
	var cmd *exec.Cmd
	if authType == "key" {
		keyPath := "/www/server/panel/remote_backup_key"
		if _, err := os.Stat(keyPath); err != nil {
			syncLog("", "SSH key does not exist: "+keyPath, "failed")
			return
		}
		if err := os.Chmod(keyPath, 0600); err != nil {
			syncLog("", fmt.Sprintf("Failed to set SSH key permissions: %v", err), "failed")
			return
		}
		cmd = exec.Command("rsync", "-avzR",
			"-e", fmt.Sprintf("ssh -i %s %s", keyPath, sshOpts),
			src, dest)
	} else {
		if _, err := exec.LookPath("sshpass"); err != nil {
			syncLog("", "sshpass not installed", "failed")
			return
		}
		cmd = exec.Command("sshpass", "-e", "rsync", "-avzR",
			"-e", fmt.Sprintf("ssh %s", sshOpts),
			src, dest)
		cmd.Env = append(os.Environ(), "SSHPASS="+password)
	}
	domain, _, _ := strings.Cut(relPath, "/")

	out, err := cmd.CombinedOutput()
	if err != nil {
		syncLog(domain, fmt.Sprintf("Remote sync failed: %s — %s", relPath, strings.TrimSpace(string(out))), "failed")
		return
	}
	syncLog(domain, fmt.Sprintf("Remote sync succeeded: %s", relPath), "success")

	if keepLocal == 0 {
		os.Remove(localFile)
	}
}

func syncBackupToS3(localFile string, endpoint, bucket, region, accessKeyID, secretKey, pathPrefix string, keepLocal int) {
	if region == "" {
		region = "auto"
	}
	if err := ValidateS3BackupSettings(endpoint, bucket, region, accessKeyID, secretKey, pathPrefix); err != nil {
		syncLog("", "S3 Remote backup settings invalid: "+err.Error(), "failed")
		return
	}
	relPath, err := localBackupRelPath(localFile)
	if err != nil {
		syncLog("", err.Error(), "failed")
		return
	}
	objectKey := s3ObjectKey(pathPrefix, relPath)
	if objectKey == "" {
		syncLog("", "S3 object path invalid", "failed")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s3UploadTimeout)
	defer cancel()
	if err := putFileToS3(ctx, endpoint, bucket, region, accessKeyID, secretKey, objectKey, localFile); err != nil {
		domain, _, _ := strings.Cut(relPath, "/")
		syncLog(domain, fmt.Sprintf("S3 remote sync failed: %s — %v", relPath, err), "failed")
		return
	}
	domain, _, _ := strings.Cut(relPath, "/")
	syncLog(domain, fmt.Sprintf("S3 Remote sync succeeded: %s", objectKey), "success")
	if keepLocal == 0 {
		os.Remove(localFile)
	}
}

func ProbeS3BackupConnection(endpoint, bucket, region, accessKeyID, secretKey, pathPrefix string) error {
	if region == "" {
		region = "auto"
	}
	if err := ValidateS3BackupSettings(endpoint, bucket, region, accessKeyID, secretKey, pathPrefix); err != nil {
		return err
	}
	key := s3ObjectKey(pathPrefix, ".wp-panel-s3-test.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body := []byte("WP Panel S3 test")
	if err := putBytesToS3(ctx, endpoint, bucket, region, accessKeyID, secretKey, key, body); err != nil {
		return err
	}
	_ = deleteObjectFromS3(ctx, endpoint, bucket, region, accessKeyID, secretKey, key)
	return nil
}

func s3ObjectKey(prefix, relPath string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	relPath = strings.TrimLeft(filepath.ToSlash(relPath), "/")
	if prefix == "" {
		return relPath
	}
	return prefix + "/" + relPath
}

func putFileToS3(ctx context.Context, endpoint, bucket, region, accessKeyID, secretKey, objectKey, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("Failed to open backup file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("Failed to read backup file info: %w", err)
	}
	size := info.Size()
	if size > s3ObjectMaxSize {
		return fmt.Errorf("Backup file exceeds S3 maximum object limit of 5 TiB")
	}
	if size > s3SinglePutMaxSize || size >= s3MultipartThreshold {
		return putFileToS3Multipart(ctx, endpoint, bucket, region, accessKeyID, secretKey, objectKey, file, size)
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	if err != nil {
		return fmt.Errorf("Failed to compute backup file checksum: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("Failed to read backup file: %w", err)
	}
	return doS3Request(ctx, http.MethodPut, endpoint, bucket, region, accessKeyID, secretKey, objectKey, file, size, hex.EncodeToString(hash.Sum(nil)))
}

func putBytesToS3(ctx context.Context, endpoint, bucket, region, accessKeyID, secretKey, objectKey string, body []byte) error {
	sum := sha256.Sum256(body)
	return doS3Request(ctx, http.MethodPut, endpoint, bucket, region, accessKeyID, secretKey, objectKey, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]))
}

func deleteObjectFromS3(ctx context.Context, endpoint, bucket, region, accessKeyID, secretKey, objectKey string) error {
	emptyHash := sha256.Sum256(nil)
	return doS3Request(ctx, http.MethodDelete, endpoint, bucket, region, accessKeyID, secretKey, objectKey, http.NoBody, 0, hex.EncodeToString(emptyHash[:]))
}

func doS3Request(ctx context.Context, method, endpoint, bucket, region, accessKeyID, secretKey, objectKey string, body io.Reader, size int64, payloadHash string, rawQuery ...string) error {
	query := ""
	if len(rawQuery) > 0 {
		query = rawQuery[0]
	}
	resp, err := doS3RequestRaw(ctx, method, endpoint, bucket, region, accessKeyID, secretKey, objectKey, query, body, size, payloadHash)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func doS3RequestRaw(ctx context.Context, method, endpoint, bucket, region, accessKeyID, secretKey, objectKey, rawQuery string, body io.Reader, size int64, payloadHash string) (*http.Response, error) {
	return doS3RequestRawWithHeaders(ctx, method, endpoint, bucket, region, accessKeyID, secretKey, objectKey, rawQuery, body, size, payloadHash, nil)
}

func doS3RequestRawWithHeaders(ctx context.Context, method, endpoint, bucket, region, accessKeyID, secretKey, objectKey, rawQuery string, body io.Reader, size int64, payloadHash string, headers map[string]string) (*http.Response, error) {
	u, err := s3ObjectURL(endpoint, bucket, objectKey, rawQuery)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("Failed to create S3 request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", time.Now().UTC().Format("20060102T150405Z"))
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	signS3Request(req, region, accessKeyID, secretKey, payloadHash)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("S3 request failed: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	resp.Body.Close()
	if len(msg) > 0 {
		return nil, fmt.Errorf("S3 returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil, fmt.Errorf("S3 returned %s", resp.Status)
}

func putFileToS3Multipart(ctx context.Context, endpoint, bucket, region, accessKeyID, secretKey, objectKey string, file *os.File, size int64) error {
	partSize := s3MultipartPartSize(size)
	uploadID, err := createS3MultipartUpload(ctx, endpoint, bucket, region, accessKeyID, secretKey, objectKey)
	if err != nil {
		return err
	}
	abort := true
	defer func() {
		if abort {
			_ = abortS3MultipartUpload(ctx, endpoint, bucket, region, accessKeyID, secretKey, objectKey, uploadID)
		}
	}()

	parts := make([]s3CompletedPart, 0, (size+partSize-1)/partSize)
	for offset, partNumber := int64(0), 1; offset < size; offset, partNumber = offset+partSize, partNumber+1 {
		currentSize := partSize
		if remaining := size - offset; remaining < currentSize {
			currentSize = remaining
		}
		etag, err := uploadS3Part(ctx, endpoint, bucket, region, accessKeyID, secretKey, objectKey, uploadID, partNumber, file, offset, currentSize)
		if err != nil {
			return fmt.Errorf("Failed to upload part %d: %w", partNumber, err)
		}
		parts = append(parts, s3CompletedPart{PartNumber: partNumber, ETag: etag})
	}
	if err := completeS3MultipartUpload(ctx, endpoint, bucket, region, accessKeyID, secretKey, objectKey, uploadID, parts); err != nil {
		return err
	}
	abort = false
	return nil
}

func s3MultipartPartSize(size int64) int64 {
	partSize := s3DefaultPartSize
	if partSize < 5*1024*1024 {
		partSize = 5 * 1024 * 1024
	}
	minPartSize := (size + s3MaxPartCount - 1) / s3MaxPartCount
	if minPartSize > partSize {
		partSize = minPartSize
	}
	return partSize
}

func createS3MultipartUpload(ctx context.Context, endpoint, bucket, region, accessKeyID, secretKey, objectKey string) (string, error) {
	emptyHash := sha256.Sum256(nil)
	resp, err := doS3RequestRaw(ctx, http.MethodPost, endpoint, bucket, region, accessKeyID, secretKey, objectKey, "uploads=", http.NoBody, 0, hex.EncodeToString(emptyHash[:]))
	if err != nil {
		return "", fmt.Errorf("Failed to create S3 multipart upload: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil {
		return "", fmt.Errorf("Failed to parse S3 multipart upload response: %w", err)
	}
	if out.UploadID == "" {
		return "", fmt.Errorf("S3 did not return uploadId")
	}
	return out.UploadID, nil
}

func uploadS3Part(ctx context.Context, endpoint, bucket, region, accessKeyID, secretKey, objectKey, uploadID string, partNumber int, file *os.File, offset, size int64) (string, error) {
	section := io.NewSectionReader(file, offset, size)
	hash := sha256.New()
	if _, err := io.Copy(hash, section); err != nil {
		return "", fmt.Errorf("Failed to compute part checksum: %w", err)
	}
	if _, err := section.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("Failed to read part: %w", err)
	}
	query := fmt.Sprintf("partNumber=%d&uploadId=%s", partNumber, awsQueryEscape(uploadID))
	resp, err := doS3RequestRaw(ctx, http.MethodPut, endpoint, bucket, region, accessKeyID, secretKey, objectKey, query, section, size, hex.EncodeToString(hash.Sum(nil)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	if etag == "" {
		return "", fmt.Errorf("S3 did not return part ETag")
	}
	return etag, nil
}

type s3CompletedPart struct {
	PartNumber int
	ETag       string
}

func completeS3MultipartUpload(ctx context.Context, endpoint, bucket, region, accessKeyID, secretKey, objectKey, uploadID string, parts []s3CompletedPart) error {
	var body strings.Builder
	body.WriteString("<CompleteMultipartUpload>")
	for _, part := range parts {
		body.WriteString("<Part><PartNumber>")
		body.WriteString(fmt.Sprintf("%d", part.PartNumber))
		body.WriteString("</PartNumber><ETag>")
		body.WriteString(escapeS3XMLText(part.ETag))
		body.WriteString("</ETag></Part>")
	}
	body.WriteString("</CompleteMultipartUpload>")
	payload := []byte(body.String())
	sum := sha256.Sum256(payload)
	query := "uploadId=" + awsQueryEscape(uploadID)
	resp, err := doS3RequestRawWithHeaders(ctx, http.MethodPost, endpoint, bucket, region, accessKeyID, secretKey, objectKey, query, bytes.NewReader(payload), int64(len(payload)), hex.EncodeToString(sum[:]), map[string]string{"Content-Type": "application/xml"})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func abortS3MultipartUpload(ctx context.Context, endpoint, bucket, region, accessKeyID, secretKey, objectKey, uploadID string) error {
	emptyHash := sha256.Sum256(nil)
	query := "uploadId=" + awsQueryEscape(uploadID)
	return doS3Request(ctx, http.MethodDelete, endpoint, bucket, region, accessKeyID, secretKey, objectKey, http.NoBody, 0, hex.EncodeToString(emptyHash[:]), query)
}

func s3ObjectURL(endpoint, bucket, objectKey, rawQuery string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("S3 Endpoint format invalid")
	}
	escapedPath := strings.TrimRight(base.EscapedPath(), "/")
	escapedPath += "/" + awsPathEscape(bucket) + "/" + awsPathEscape(objectKey)
	base.Path = strings.TrimRight(base.Path, "/") + "/" + bucket + "/" + objectKey
	base.RawPath = escapedPath
	base.RawQuery = rawQuery
	return base, nil
}

func signS3Request(req *http.Request, region, accessKeyID, secretKey, payloadHash string) {
	amzDate := req.Header.Get("x-amz-date")
	shortDate := amzDate[:8]
	canonicalURI := req.URL.EscapedPath()
	canonicalHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalS3Query(req.URL.RawQuery),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := shortDate + "/" + region + "/s3/aws4_request"
	canonicalHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(canonicalHash[:]),
	}, "\n")
	signingKey := s3SigningKey(secretKey, shortDate, region)
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKeyID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func canonicalS3Query(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return rawQuery
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0)
	for _, key := range keys {
		vals := values[key]
		sort.Strings(vals)
		for _, val := range vals {
			parts = append(parts, awsQueryEscape(key)+"="+awsQueryEscape(val))
		}
	}
	return strings.Join(parts, "&")
}

func s3SigningKey(secret, date, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func awsPathEscape(s string) string {
	parts := strings.Split(s, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func awsQueryEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func escapeS3XMLText(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(s)
}

func syncLog(domain string, msg string, status string) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("[WP-Panel] %s %s\n", timestamp, msg)
	if domain == "" {
		domain = "—"
	}
	recordOperationLog("Remote backup", domain, status, msg)
}
