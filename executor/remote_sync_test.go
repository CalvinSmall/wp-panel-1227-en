package executor

import "testing"

func TestValidateRemoteBackupSettingsRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		port       int
		username   string
		authType   string
		remotePath string
	}{
		{name: "host command chars", host: "example.com;reboot", port: 22, username: "root", authType: "password", remotePath: "/backup"},
		{name: "bad port", host: "example.com", port: 70000, username: "root", authType: "password", remotePath: "/backup"},
		{name: "bad username", host: "example.com", port: 22, username: "root;id", authType: "password", remotePath: "/backup"},
		{name: "bad auth", host: "example.com", port: 22, username: "root", authType: "agent", remotePath: "/backup"},
		{name: "bad path chars", host: "example.com", port: 22, username: "root", authType: "password", remotePath: "/backup;rm"},
		{name: "path traversal", host: "example.com", port: 22, username: "root", authType: "password", remotePath: "/backup/../other"},
		{name: "ipv6 unsupported", host: "2001:db8::1", port: 22, username: "root", authType: "password", remotePath: "/backup"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateRemoteBackupSettings(tt.host, tt.port, tt.username, tt.authType, tt.remotePath); err == nil {
				t.Fatal("ValidateRemoteBackupSettings error = nil, want error")
			}
		})
	}
}

func TestValidateRemoteBackupSettingsAcceptsSafeValues(t *testing.T) {
	if err := ValidateRemoteBackupSettings("backup.example.com", 2222, "wp_backup", "key", "/srv/wp-panel/backups"); err != nil {
		t.Fatalf("ValidateRemoteBackupSettings safe domain: %v", err)
	}
	if err := ValidateRemoteBackupSettings("192.0.2.10", 22, "root", "password", "~/backups"); err != nil {
		t.Fatalf("ValidateRemoteBackupSettings safe IP: %v", err)
	}
}

func TestLocalBackupRelPathRequiresBackupsRoot(t *testing.T) {
	got, err := localBackupRelPath(backupsRoot + "/example.com/db/site.sql.gz")
	if err != nil {
		t.Fatalf("localBackupRelPath inside root: %v", err)
	}
	if got != "example.com/db/site.sql.gz" {
		t.Fatalf("localBackupRelPath = %q", got)
	}
	if _, err := localBackupRelPath("/tmp/site.sql.gz"); err == nil {
		t.Fatal("localBackupRelPath outside root error = nil, want error")
	}
}
