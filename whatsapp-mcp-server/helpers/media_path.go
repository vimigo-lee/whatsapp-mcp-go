package helpers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// send_media's media_path is chosen by the model, which also reads messages
// other people wrote, so a message can try to talk it into sending a secret
// from this machine to the sender. Ordinary documents and photos stay
// sendable; files that only a program would ever need are refused, as are
// hidden folders (.ssh, .aws, .config, .git, ...) and Windows AppData (browser
// profiles, saved logins). MEDIA_PATH_ROOTS, a list of folders separated by
// the OS path-list separator, narrows sending to those folders.
//
// A refused file can still be sent by copying it somewhere ordinary first,
// which takes a deliberate step by the owner rather than a model's say-so.

var secretFileSuffixes = []string{
	".pem", ".key", ".p12", ".pfx", ".ppk", ".kdbx", ".keystore", ".jks",
	".ovpn", ".gpg", ".asc", ".db", ".sqlite", ".sqlite3", ".env",
}

var secretFileNames = map[string]bool{
	"credentials": true, "credentials.json": true, "wallet.dat": true,
	"cookies": true, "login data": true, "web data": true, "known_hosts": true,
	"authorized_keys": true, "shadow": true, "passwd": true, "netrc": true,
}

var secretFilePrefixes = []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"}

// sendableMediaPath resolves symlinks in an absolute path and returns the real
// path, or an error saying why the file may not be sent.
func sendableMediaPath(absPath string) (string, error) {
	real, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %v", err)
	}
	if roots := os.Getenv("MEDIA_PATH_ROOTS"); roots != "" && !insideAnyRoot(real, filepath.SplitList(roots)) {
		return "", fmt.Errorf("refusing to send %s: it is outside the folders this server may send from (MEDIA_PATH_ROOTS)", absPath)
	}
	if why := secretPathReason(real); why != "" {
		return "", fmt.Errorf("refusing to send %s: %s. If the owner really means to share it, copy it to an ordinary folder first and send the copy", absPath, why)
	}
	return real, nil
}

func secretPathReason(p string) string {
	parts := strings.Split(filepath.ToSlash(strings.TrimPrefix(p, filepath.VolumeName(p))), "/")
	for i, part := range parts {
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		last := i == len(parts)-1
		if strings.HasPrefix(part, ".") {
			if last {
				return "it is a hidden file"
			}
			return "it is inside the hidden folder " + part
		}
		// AppData\Local\Temp is where generated files (a report an assistant
		// just wrote) usually sit before they are sent, so it stays sendable.
		if !last && lower == "appdata" && !(i+2 < len(parts) && strings.EqualFold(parts[i+1], "local") && strings.EqualFold(parts[i+2], "temp")) {
			return "it is inside AppData, where programs keep their own data"
		}
		if !last && (lower == "etc" && i == 1) {
			return "it is a system configuration file"
		}
		if last {
			if secretFileNames[lower] {
				return "it looks like a credential or system file"
			}
			for _, pre := range secretFilePrefixes {
				if strings.HasPrefix(lower, pre) {
					return "it looks like an SSH key"
				}
			}
			for _, suf := range secretFileSuffixes {
				if strings.HasSuffix(lower, suf) {
					return "it looks like a key, credential or database file"
				}
			}
		}
	}
	return ""
}

func insideAnyRoot(p string, roots []string) bool {
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		if rel, err := filepath.Rel(r, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}
