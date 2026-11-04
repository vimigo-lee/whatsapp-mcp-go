package helpers

import (
	"os"
	"path/filepath"
	"testing"
)

// NV-8: a model steered by a message must not be able to send this machine's
// keys and credentials, while ordinary documents stay sendable.
func TestSendableMediaPath(t *testing.T) {
	home := t.TempDir()
	write := func(rel string) string {
		t.Helper()
		p := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for rel, ok := range map[string]bool{
		"Documents/report.pdf":                   true,
		"Desktop/quote 2026.xlsx":                true,
		"Pictures/photo.jpg":                     true,
		".ssh/id_ed25519":                        false,
		"keys/id_rsa":                            false,
		".aws/credentials":                       false,
		"project/.env":                           false,
		"project/.env.production":                false,
		"project/prod.env":                       false,
		"certs/server.pem":                       false,
		"AppData/Local/Google/Chrome/Login Data": false,
		"AppData/Roaming/app/settings.json":      false,
		"AppData/Local/Temp/claude/report.pdf":   true,
		"AppData/Local/Temp/.secret":             false,
		"whatsapp-bridge/store/messages.db":      false,
		".config/gh/hosts.yml":                   false,
		"vault.kdbx":                             false,
		// macOS app data, the counterpart of AppData.
		"Library/Application Support/Google/Chrome/Default/Cookies.txt": false,
		"Library/Application Support/Signal/config.json":                false,
		"Library/Containers/com.apple.mail/Data/msg.eml":                false,
		"Library/Group Containers/group.net.whatsapp/ChatStorage.txt":   false,
		"Library/Mobile Documents/report.pdf":                           true,
		"Documents/Library/notes.pdf":                                   true,
		// Linux per-user app config, covered as a hidden folder.
		".config/google-chrome/Default/Preferences": false,
		// SQLite sidecars hold the same rows as the database.
		"whatsapp-bridge/store/messages.db-journal": false,
		"whatsapp-bridge/store/messages.db-wal":     false,
		"whatsapp-bridge/store/messages.db-shm":     false,
		"profile/places.sqlite-wal":                 false,
		"profile/places.sqlite-shm":                 false,
		"profile/places.sqlite-journal":             false,
	} {
		_, err := sendableMediaPath(write(rel))
		if (err == nil) != ok {
			t.Errorf("%s: sendable=%v, want %v (err=%v)", rel, err == nil, ok, err)
		}
	}

	// A symlink with an innocent name is judged by what it points at.
	key := write(".ssh/id_rsa")
	link := filepath.Join(home, "Documents", "invoice.pdf")
	if err := os.Symlink(key, link); err == nil {
		if _, err := sendableMediaPath(link); err == nil {
			t.Error("a symlink to an SSH key was sendable")
		}
	}

	// MEDIA_PATH_ROOTS narrows sending to the listed folders.
	t.Setenv("MEDIA_PATH_ROOTS", filepath.Join(home, "Documents"))
	if _, err := sendableMediaPath(filepath.Join(home, "Documents", "report.pdf")); err != nil {
		t.Errorf("file inside MEDIA_PATH_ROOTS refused: %v", err)
	}
	if _, err := sendableMediaPath(filepath.Join(home, "Pictures", "photo.jpg")); err == nil {
		t.Error("file outside MEDIA_PATH_ROOTS was sendable")
	}
}
