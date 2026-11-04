package helpers

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// send_media's media_path is chosen by the model, which also reads messages
// other people wrote, so a message can try to talk it into sending a secret
// from this machine to the sender. Ordinary documents and photos stay
// sendable; files that only a program would ever need are refused: hidden
// folders (.ssh, .aws, .config, .git, ...), Windows AppData and macOS
// Library/Application Support (browser profiles, saved logins), keychains,
// the process and device trees on Linux (/proc/self/environ holds this
// server's own bridge key), mounted
// container secrets, system configuration, and private key files whatever
// their name. MEDIA_PATH_ROOTS, a list of folders separated by the OS
// path-list separator, narrows sending to those folders.
//
// A refused file can still be sent by copying it somewhere ordinary first,
// which takes a deliberate step by the owner rather than a model's say-so.

var secretFileSuffixes = []string{
	".pem", ".p12", ".pfx", ".ppk", ".kdbx", ".keystore", ".jks",
	".ovpn", ".gpg", ".asc", ".db", ".sqlite", ".sqlite3", ".env", ".keychain", ".keychain-db",
	// SQLite journals sit beside the database and hold the same rows.
	".db-journal", ".db-wal", ".db-shm", ".sqlite-journal", ".sqlite-wal", ".sqlite-shm",
	".sqlite3-journal", ".sqlite3-wal", ".sqlite3-shm",
}

// macOS keeps each app's own data (browser profiles, chat stores, tokens)
// under ~/Library in these folders, the counterpart of Windows AppData.
var macAppDataFolders = map[string]bool{
	"application support": true, "containers": true, "group containers": true,
}

var secretFileNames = map[string]bool{
	"credentials": true, "credentials.json": true, "wallet.dat": true,
	"cookies": true, "login data": true, "web data": true, "known_hosts": true,
	"authorized_keys": true, "shadow": true, "passwd": true, "netrc": true,
	"logins.json": true, "key4.db": true, "environ": true,
}

var secretFilePrefixes = []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"}

// Top-level trees that hold no documents, only system state and secrets.
// "private" covers macOS, where /etc, /var and /tmp resolve to /private/...
var systemRoots = map[string]string{
	"proc": "it is part of the running system (/proc)", "sys": "it is part of the running system (/sys)",
	"dev": "it is a device", "etc": "it is a system configuration file", "boot": "it is a system file",
}

// sendableMediaPath returns the real path of a file that may be sent, or an
// error saying why not.
func sendableMediaPath(absPath string) (string, error) {
	// A UNC path (\\host\share, \\?\, \\.\) makes Windows contact that host,
	// handing it the user's login hash, before any rule below has run.
	if runtime.GOOS == "windows" && (strings.HasPrefix(absPath, `\\`) || strings.HasPrefix(absPath, "//")) {
		return "", fmt.Errorf("refusing to send %s: network and device paths are not supported; copy the file to this computer first", absPath)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %v", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a folder, not a file", absPath)
	}
	// A device, pipe or socket is not a document, and reading one can block.
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", absPath)
	}
	// Judge the file by where it really lives. EvalSymlinks fails on paths
	// through a Windows junction (a Documents folder moved to another drive);
	// those are judged by the path as given.
	real := absPath
	if r, err := filepath.EvalSymlinks(absPath); err == nil {
		real = r
	}
	if roots := os.Getenv("MEDIA_PATH_ROOTS"); roots != "" && !insideAnyRoot(real, filepath.SplitList(roots)) {
		return "", fmt.Errorf("refusing to send %s: it is outside the folders this server may send from (MEDIA_PATH_ROOTS)", absPath)
	}
	for _, p := range []string{absPath, real} {
		if why := secretPathReason(p); why != "" {
			return "", refusal(absPath, why)
		}
	}
	if looksLikePrivateKey(real) {
		return "", refusal(absPath, "it contains a private key")
	}
	return real, nil
}

func refusal(absPath, why string) error {
	return fmt.Errorf("refusing to send %s: %s. If the owner really means to share it, copy it to an ordinary folder first and send the copy", absPath, why)
}

func secretPathReason(p string) string {
	parts := strings.Split(filepath.ToSlash(strings.TrimPrefix(p, filepath.VolumeName(p))), "/")
	var clean []string
	for _, part := range parts {
		if part != "" {
			clean = append(clean, part)
		}
	}
	if len(clean) > 1 {
		top := strings.ToLower(clean[0])
		if top == "private" && len(clean) > 2 {
			top = strings.ToLower(clean[1])
		}
		if why, ok := systemRoots[top]; ok && runtime.GOOS != "windows" {
			return why
		}
		if (top == "run" || top == "var") && strings.Contains(strings.ToLower(strings.Join(clean, "/")), "secrets") {
			return "it is a mounted secret"
		}
	}
	for i, part := range clean {
		lower := strings.ToLower(part)
		last := i == len(clean)-1
		if strings.HasPrefix(part, ".") {
			if last {
				return "it is a hidden file"
			}
			return "it is inside the hidden folder " + part
		}
		if !last && lower == "appdata" && !(i+2 < len(clean) && strings.EqualFold(clean[i+1], "local") && strings.EqualFold(clean[i+2], "temp")) {
			// AppData\Local\Temp is where generated files (a report an
			// assistant just wrote) usually sit before they are sent.
			return "it is inside AppData, where programs keep their own data"
		}
		if lower == "library" && i+2 < len(clean) && macAppDataFolders[strings.ToLower(clean[i+1])] {
			return "it is inside Library/" + clean[i+1] + ", where programs keep their own data"
		}
		if !last && lower == "keychains" {
			return "it is a keychain"
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

// looksLikePrivateKey catches a PEM private key under any name (server.key,
// a renamed id_rsa) without refusing a Keynote .key deck, which is a zip.
func looksLikePrivateKey(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4096)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	return bytes.Contains(head, []byte("PRIVATE KEY-----")) || bytes.Contains(head, []byte("OPENSSH PRIVATE KEY"))
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
