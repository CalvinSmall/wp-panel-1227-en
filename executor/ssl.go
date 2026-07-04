package executor

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/naibabiji/wp-panel/config"
	"github.com/naibabiji/wp-panel/database"
	"github.com/naibabiji/wp-panel/models"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"
)

type acmeUser struct {
	Email        string
	Registration *registration.Resource
	key          crypto.PrivateKey
}

func (u *acmeUser) GetEmail() string                        { return u.Email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.Registration }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

func executeEnableSSL(task *Task) TaskResult {
	payload, ok := task.Payload.(*EnableSSLPayload)
	if !ok {
		return TaskResult{Success: false, Message: "Invalid task parameter type"}
	}

	site := payload.Site
	cfg := config.AppConfig
	certDir := filepath.Join(cfg.Paths.Certificates, site.Domain)
	certPath := filepath.Join(certDir, "fullchain.pem")
	keyPath := filepath.Join(certDir, "privkey.pem")

	os.RemoveAll(certDir)
	if err := os.MkdirAll(certDir, 0700); err != nil {
		log.Printf("Failed to create certificate directory: %v", err)
		return TaskResult{Success: false, Message: "Failed to create certificate directory"}
	}

	var expiry time.Time
	var applyErr error

	if payload.Mode == "manual" {
		if payload.Certificate == "" || payload.PrivateKey == "" {
			return TaskResult{Success: false, Message: "Certificate content and private key cannot be empty"}
		}
		if err := os.WriteFile(certPath, []byte(payload.Certificate), 0644); err != nil {
			log.Printf("Failed to write certificate file: %v", err)
			return TaskResult{Success: false, Message: "Failed to write certificate file"}
		}
		if err := os.WriteFile(keyPath, []byte(payload.PrivateKey), 0600); err != nil {
			os.Remove(certPath)
			log.Printf("Failed to write private key file: %v", err)
			return TaskResult{Success: false, Message: "Failed to write private key file"}
		}
		expiry, applyErr = validateCertificate(certPath, site.Domain)
		if applyErr != nil {
			os.Remove(certPath)
			os.Remove(keyPath)
			log.Printf("Certificate verification failed: %v", applyErr)
			return TaskResult{Success: false, Message: "Certificate verification failed"}
		}
	} else {
		documentRoot, err := EnsureEffectiveDocumentRoot(site.WebRoot, site.SiteType, site.DocumentRootSubdir, site.SystemUser)
		if err != nil {
			return taskFailure("Failed to prepare SSL verification directory", err)
		}
		expiry, applyErr = obtainLegoCert(site.Domain, site.Aliases,
			documentRoot, certDir)
		if applyErr != nil {
			log.Printf("Let's Encrypt certificate request failed: %v", applyErr)
			os.RemoveAll(certDir)
			msg := FriendlySSLError(applyErr)
			database.GetDB().Exec("UPDATE websites SET ssl_last_error = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?", msg, site.ID)
			return TaskResult{Success: false, Message: msg}
		}
	}

	if applyErr = applySSLToSite(site, certPath, keyPath, expiry); applyErr != nil {
		os.RemoveAll(certDir)
		log.Printf("Failed to apply SSL config: %v", applyErr)
		return taskFailure("Failed to apply SSL config", applyErr)
	}

	return TaskResult{
		Success: true,
		Message: fmt.Sprintf("Site %s SSL enabled (expires: %s)", site.Domain, expiry.Format("2006-01-02")),
	}
}

func FriendlySSLError(err error) string {
	if err == nil {
		return ""
	}
	raw := strings.TrimSpace(err.Error())
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "invalid response from http://") && strings.Contains(lower, ": 404"):
		return "Let's Encrypt HTTP-01 verification file returned 404. Check if domain A/AAAA points to this server; if using CDN, confirm CDN is correctly sourcing and has not cached, rewritten, or blocked /.well-known/acme-challenge/."
	case strings.Contains(lower, "no valid a records found") || strings.Contains(lower, "no valid aaaa records found") || strings.Contains(lower, "nxdomain"):
		return "Domain DNS records invalid or not yet propagated. Check main and alias domain A/AAAA records and retry."
	case strings.Contains(lower, "connection refused"):
		return "Let's Encrypt cannot connect to site port 80. Confirm domain points to this server, and firewall, CDN, Nginx are not blocking HTTP access."
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "i/o timeout") || strings.Contains(lower, "context deadline exceeded"):
		return "Let's Encrypt verification file access timed out. Check DNS resolution, port 80 connectivity, and CDN origin configuration."
	case strings.Contains(lower, "unauthorized"):
		return "Let's Encrypt verification failed. Check if domain resolves to this server; if using CDN, confirm CDN allows /.well-known/acme-challenge/ and correctly sources."
	default:
		return "Let's Encrypt certificate request failed: " + raw
	}
}

func executeRemoveSSL(task *Task) TaskResult {
	payload, ok := task.Payload.(*RemoveSSLPayload)
	if !ok {
		return TaskResult{Success: false, Message: "Invalid task parameter type"}
	}

	site := payload.Site
	cfg := config.AppConfig

	certDir := filepath.Join(cfg.Paths.Certificates, site.Domain)
	os.RemoveAll(certDir)

	engine := NewTemplateEngine(cfg.Panel.BackupDir)
	nginxData, err := nginxDataFromSiteChecked(site)
	if err != nil {
		return taskFailure("CDN Real IP config invalid", err)
	}
	nginxData.UseSSL = false
	nginxData.SSLCertPath = ""
	nginxData.SSLKeyPath = ""

	nginxConfig, err := engine.RenderNginxConfig(nginxData)
	if err != nil {
		log.Printf("Failed to render HTTP config: %v", err)
		return taskFailure("Failed to render HTTP config", err)
	}

	if err := engine.ApplyNginxConfig(nginxConfig, site.NginxConfPath,
		nginxEnabledPath(cfg, site.NginxConfPath, site.Domain)); err != nil {
		log.Printf("Failed to apply HTTP config: %v", err)
		return taskFailure("Failed to apply HTTP config", err)
	}

	db := database.GetDB()
	db.Exec(`UPDATE websites SET ssl_enabled = 0, ssl_cert_path = '', ssl_key_path = '', ssl_expires_at = NULL, ssl_last_error = '', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, site.ID)

	return TaskResult{Success: true, Message: "Site " + site.Domain + " SSL certificate removed, reverted to HTTP"}
}

const acmeAccountDir = "/www/server/panel/acme"

type acmeAccountMetadata struct {
	Registration *registration.Resource `json:"registration,omitempty"`
}

func newACMEClient(user *acmeUser, caDirURL string) (*lego.Client, error) {
	legoCfg := lego.NewConfig(user)
	legoCfg.CADirURL = caDirURL

	client, err := lego.NewClient(legoCfg)
	if err != nil {
		return nil, fmt.Errorf("Failed to create lego client: %w", err)
	}
	return client, nil
}

func loadACMERegistration(path string) (*registration.Resource, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}

	var meta acmeAccountMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	if meta.Registration == nil || strings.TrimSpace(meta.Registration.URI) == "" {
		return nil, nil
	}
	return meta.Registration, nil
}

func saveACMERegistration(path string, reg *registration.Resource) error {
	if reg == nil || strings.TrimSpace(reg.URI) == "" {
		return fmt.Errorf("ACME account registration info is empty")
	}
	data, err := json.MarshalIndent(acmeAccountMetadata{Registration: reg}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func getOrCreateACMEClient(email string, caDirURL string) (*lego.Client, error) {
	if err := os.MkdirAll(acmeAccountDir, 0700); err != nil {
		return nil, fmt.Errorf("Failed to create ACME directory: %w", err)
	}

	accountKeyPath := filepath.Join(acmeAccountDir, "account.key")
	accountMetaPath := filepath.Join(acmeAccountDir, "account.json")

	var privateKey crypto.PrivateKey
	var err error

	if keyData, readErr := os.ReadFile(accountKeyPath); readErr == nil {
		block, _ := pem.Decode(keyData)
		if block != nil {
			privateKey, err = x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("Failed to parse ACME account private key: %w", err)
			}
		}
	}

	if privateKey == nil {
		privateKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("Failed to generate ACME account private key: %w", err)
		}
		keyBytes, _ := x509.MarshalECPrivateKey(privateKey.(*ecdsa.PrivateKey))
		pemData := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
		if err := os.WriteFile(accountKeyPath, pemData, 0600); err != nil {
			return nil, fmt.Errorf("Failed to save ACME account private key: %w", err)
		}
	}

	user := &acmeUser{Email: email, key: privateKey}
	if reg, loadErr := loadACMERegistration(accountMetaPath); loadErr != nil {
		return nil, fmt.Errorf("Failed to read ACME account info: %w", loadErr)
	} else if reg != nil {
		user.Registration = reg
	}

	client, err := newACMEClient(user, caDirURL)
	if err != nil {
		return nil, err
	}

	if user.Registration == nil {
		reg, resolveErr := client.Registration.ResolveAccountByKey()
		if resolveErr != nil {
			reg, err = client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
			if err != nil {
				return nil, fmt.Errorf("Failed to register ACME account: %w", err)
			}
		}
		user.Registration = reg
		if err := saveACMERegistration(accountMetaPath, reg); err != nil {
			return nil, fmt.Errorf("Failed to save ACME account info: %w", err)
		}
		client, err = newACMEClient(user, caDirURL)
		if err != nil {
			return nil, err
		}
	}

	return client, nil
}

func isTransientACMEOrderError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "certificate not found") ||
		strings.Contains(lower, "authorizations for these identifiers not found")
}

func obtainLegoCert(domain string, aliases string, webRoot string, certDir string) (time.Time, error) {
	client, err := getOrCreateACMEClient("admin@"+domain, lego.LEDirectoryProduction)
	if err != nil {
		return time.Time{}, err
	}

	provider := &webrootProvider{webroot: webRoot}
	if err := client.Challenge.SetHTTP01Provider(provider); err != nil {
		return time.Time{}, fmt.Errorf("Failed to set HTTP-01 validation provider: %w", err)
	}

	domains := []string{domain}
	if aliases != "" {
		for _, a := range strings.Split(aliases, "\n") {
			a = strings.TrimSpace(a)
			if a != "" && a != domain {
				domains = append(domains, a)
			}
		}
	}

	req := certificate.ObtainRequest{
		Domains: domains,
		Bundle:  true,
	}

	certRes, err := client.Certificate.Obtain(req)
	if err != nil && isTransientACMEOrderError(err) {
		log.Printf("ACME order temporary error, preparing retry domain=%s: %v", domain, err)
		time.Sleep(3 * time.Second)
		certRes, err = client.Certificate.Obtain(req)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("Failed to obtain certificate: %w", err)
	}

	certPath := filepath.Join(certDir, "fullchain.pem")
	keyPath := filepath.Join(certDir, "privkey.pem")

	if err := os.WriteFile(certPath, certRes.Certificate, 0644); err != nil {
		return time.Time{}, fmt.Errorf("Failed to save certificate: %w", err)
	}
	if err := os.WriteFile(keyPath, certRes.PrivateKey, 0600); err != nil {
		return time.Time{}, fmt.Errorf("Failed to save private key: %w", err)
	}

	expiry, err := validateCertificate(certPath, domain)
	if err != nil {
		return time.Time{}, fmt.Errorf("Failed to verify issued certificate: %w", err)
	}

	return expiry, nil
}

type webrootProvider struct {
	webroot string
}

func (w *webrootProvider) Present(domain, token, keyAuth string) error {
	challengePath := filepath.Join(w.webroot, ".well-known", "acme-challenge")
	if err := os.MkdirAll(challengePath, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(challengePath, token), []byte(keyAuth), 0644)
}

func (w *webrootProvider) CleanUp(domain, token, keyAuth string) error {
	challengeFile := filepath.Join(w.webroot, ".well-known", "acme-challenge", token)
	os.Remove(challengeFile)
	return nil
}

func applySSLToSite(site *models.Website, certPath, keyPath string, expiry time.Time) error {
	cfg := config.AppConfig

	engine := NewTemplateEngine(cfg.Panel.BackupDir)
	nginxData, err := nginxDataFromSiteChecked(site)
	if err != nil {
		return fmt.Errorf("CDN Real IP config invalid: %w", err)
	}
	nginxData.UseSSL = true
	nginxData.SSLCertPath = certPath
	nginxData.SSLKeyPath = keyPath

	nginxConfig, err := engine.RenderNginxConfig(nginxData)
	if err != nil {
		return fmt.Errorf("Failed to render Nginx config: %w", err)
	}

	if err := engine.ApplyNginxConfig(nginxConfig, site.NginxConfPath,
		nginxEnabledPath(cfg, site.NginxConfPath, site.Domain)); err != nil {
		return fmt.Errorf("Failed to apply Nginx config: %w", err)
	}

	db := database.GetDB()
	_, err = db.Exec(
		`UPDATE websites SET ssl_enabled = 1, ssl_cert_path = ?, ssl_key_path = ?, ssl_expires_at = ?, ssl_last_error = '', updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		certPath, keyPath, expiry, site.ID,
	)
	return err
}

func validateCertificate(certPath string, domain string) (time.Time, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("Failed to read certificate file: %w", err)
	}

	var expiry time.Time
	found := false

	for rest := data; len(rest) > 0; {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remaining

		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			if !cert.IsCA {
				now := time.Now()
				if now.After(cert.NotAfter) {
					return time.Time{}, fmt.Errorf("Certificate expired (expired: %s)", cert.NotAfter.Format("2006-01-02"))
				}
				if now.Before(cert.NotBefore) {
					return time.Time{}, fmt.Errorf("Certificate not yet valid (valid from: %s)", cert.NotBefore.Format("2006-01-02"))
				}
				if err := cert.VerifyHostname(domain); err != nil {
					matched := false
					certDomains := cert.DNSNames
					if len(certDomains) == 0 && cert.Subject.CommonName != "" {
						certDomains = []string{cert.Subject.CommonName}
					}
					for _, san := range certDomains {
						if san == domain {
							matched = true
							break
						}
					}
					if !matched {
						hint := strings.Join(certDomains, ", ")
						if hint == "" {
							hint = "(certificate does not contain any domains)"
						}
						return time.Time{}, fmt.Errorf("Certificate does not match domain %s, certificate contains domains: %s", domain, hint)
					}
				}
				expiry = cert.NotAfter
				found = true
			}
		}
	}

	if !found {
		return time.Time{}, fmt.Errorf("No valid certificate content found")
	}

	return expiry, nil
}

func executeRenewSSL(task *Task) TaskResult {
	cfg := config.AppConfig
	db := database.GetDB()

	rows, err := db.Query(
		`SELECT id, name, domain, aliases, status, system_user, web_root, document_root_subdir, log_dir,
		        db_name, db_user, php_pool_path, nginx_conf_path, site_type, ssl_enabled,
		        ssl_cert_path, ssl_key_path, template_version, ssl_expires_at
		 FROM websites WHERE ssl_enabled = 1 AND ssl_cert_path != ''`,
	)
	if err != nil {
		log.Printf("Failed to query SSL sites: %v", err)
		return TaskResult{Success: false, Message: "Failed to query SSL sites"}
	}
	defer rows.Close()

	var renewed []string
	var failed []string
	now := time.Now()
	renewThreshold := now.AddDate(0, 0, 30)

	for rows.Next() {
		var w models.Website
		var aliases string
		var status string
		var sslEnabled int
		var sslExpiresAt *time.Time
		if scanErr := rows.Scan(
			&w.ID, &w.Name, &w.Domain, &aliases, &status, &w.SystemUser,
			&w.WebRoot, &w.DocumentRootSubdir, &w.LogDir, &w.DBName, &w.DBUser, &w.PHPPoolPath,
			&w.NginxConfPath, &w.SiteType, &sslEnabled, &w.SSLCertPath, &w.SSLKeyPath,
			&w.TemplateVersion, &sslExpiresAt,
		); scanErr != nil {
			failed = append(failed, w.Domain+"(read failed)")
			continue
		}
		w.Aliases = aliases
		w.Status = models.WebsiteStatus(status)
		w.SSLEnabled = sslEnabled == 1
		w.SSLExpiresAt = sslExpiresAt

		expiry, certErr := validateCertificate(w.SSLCertPath, w.Domain)
		if certErr != nil {
			log.Printf("SSL certificate abnormal domain=%s: %v", w.Domain, certErr)
			failed = append(failed, w.Domain+"(certificate abnormal)")
			continue
		}

		if expiry.After(renewThreshold) {
			continue
		}

		if expiry.Before(now) {
			failed = append(failed, w.Domain+"(certificate expired)")
			continue
		}

		documentRoot, docRootErr := EnsureEffectiveDocumentRoot(w.WebRoot, w.SiteType, w.DocumentRootSubdir, w.SystemUser)
		if docRootErr != nil {
			log.Printf("SSL renewal failed to prepare verification directory domain=%s: %v", w.Domain, docRootErr)
			failed = append(failed, w.Domain+"(verification directory failed)")
			continue
		}

		newExpiry, renewErr := obtainLegoCert(w.Domain, w.Aliases, documentRoot,
			filepath.Join(cfg.Paths.Certificates, w.Domain))
		if renewErr != nil {
			log.Printf("SSL renewal failed domain=%s: %v", w.Domain, renewErr)
			failed = append(failed, w.Domain+"(renewal failed)")
			continue
		}

		db.Exec("UPDATE websites SET ssl_expires_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
			newExpiry, w.ID)

		renewed = append(renewed, w.Domain)
	}

	if len(renewed) > 0 {
		exec.Command("nginx", "-s", "reload").Run()
	}

	msg := fmt.Sprintf("Renewal completed. Succeeded: %d", len(renewed))
	if len(failed) > 0 {
		msg += "; Failed: " + strings.Join(failed, ", ")
	}

	if len(renewed) > 0 {
		log.Printf("SSL auto-renewal: %s", msg)
	}

	return TaskResult{Success: true, Message: msg, Data: map[string]interface{}{"renewed": renewed, "failed": failed}}
}

func StartSSLRenewalScheduler() {
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 3, 0, 0, 0, now.Location())
			time.Sleep(next.Sub(now))
			GlobalQueue.Enqueue(TaskRenewSSL, nil)
		}
	}()
}
