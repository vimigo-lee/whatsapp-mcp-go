package main

import (
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mediaReuseStore(t *testing.T) *MessageStore {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP,
		is_from_me BOOLEAN, media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB,
		file_enc_sha256 BLOB, file_length INTEGER, quoted_message_id TEXT, quoted_sender TEXT, quoted_content TEXT,
		is_forwarded BOOLEAN, direct_path TEXT, scan_status TEXT, scan_reason TEXT, scanned_at TIMESTAMP,
		PRIMARY KEY (id, chat_jid))`); err != nil {
		t.Fatal(err)
	}
	return &MessageStore{db: db}
}

// A file already at a message's path may belong to another message, because
// document names come from the sender. It is served only when its hash is the
// message's own; a different file is never returned under someone else's
// message or verdict.
func TestDownloadMediaNeverServesAnotherMessagesFile(t *testing.T) {
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	t.Chdir(t.TempDir())
	s := mediaReuseStore(t)
	at := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	group := "120363000000000002@g.us"
	bad := append([]byte("MZ"), make([]byte, 64)...)
	good := []byte("%PDF-1.4\n%genuine\n%%EOF\n")
	hb, hg := sha256.Sum256(bad), sha256.Sum256(good)
	onDisk := func(id, name string, data []byte) {
		dir := filepath.Join("store", group)
		os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		s.SaveFileScan(id, group, scanFile("document", name, data))
	}
	store := func(id string, sha []byte, size int) {
		if err := s.StoreMessage(id, group, "x", "", at, false, "document", "report.pdf", "", "", []byte{1}, sha, []byte{1}, uint64(size), "", "", "", false); err != nil {
			t.Fatal(err)
		}
	}

	// The attacker's report.pdf was downloaded first; the victim's genuine
	// report.pdf must not be answered with it.
	store("ATTACKER", hb[:], len(bad))
	onDisk("ATTACKER", "report.pdf", bad)
	store("VICTIM", hg[:], len(good))
	ok, _, _, path, _, err := downloadMedia(nil, s, "VICTIM", group)
	if ok {
		served, _ := os.ReadFile(path)
		if sum := sha256.Sum256(served); sum != hg {
			t.Fatalf("victim's download served another file (%s)", path)
		}
	} else if err == nil {
		t.Fatal("victim's download failed without an error")
	}

	// Once the victim's own file is on disk under its own name, it is reused.
	stemName := mediaStem("document", hg[:]) + "_report.pdf"
	onDisk("VICTIM", stemName, good)
	ok, _, name, path, _, err := downloadMedia(nil, s, "VICTIM", group)
	if !ok || err != nil || name != stemName {
		t.Fatalf("own file not reused: ok=%v name=%q err=%v", ok, name, err)
	}
	if served, _ := os.ReadFile(path); sha256.Sum256(served) != hg {
		t.Fatal("reused file is not the victim's")
	}

	// The attacker's own message still gets its own file.
	ok, _, name, _, _, err = downloadMedia(nil, s, "ATTACKER", group)
	if !ok || err != nil || name != "report.pdf" {
		t.Errorf("attacker's own file: ok=%v name=%q err=%v", ok, name, err)
	}
}

// Rows stored before hashes were recorded have none; their files are reused as
// before rather than re-downloaded.
func TestDownloadMediaReusesFilesOfRowsWithoutAHash(t *testing.T) {
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	t.Chdir(t.TempDir())
	s := mediaReuseStore(t)
	group := "120363000000000004@g.us"
	if err := s.StoreMessage("OLD", group, "x", "", time.Now(), false, "document", "old.pdf", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("store", group)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "old.pdf"), []byte("%PDF-1.4\n%%EOF\n"), 0o644)
	if ok, _, name, _, _, err := downloadMedia(nil, s, "OLD", group); !ok || err != nil || name != "old.pdf" {
		t.Errorf("old row: ok=%v name=%q err=%v", ok, name, err)
	}
}
