package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestSafeMediaFilename(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Invoice March.pdf", "Invoice March.pdf"},
		{"../../../../AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup/x.bat", "x.bat"},
		{`..\..\evil.bat`, "evil.bat"},
		{"..", "fallback"},
		{"", "fallback"},
		{"  . ", "fallback"},
		{"photo‮gpj.exe", "photo_gpj.exe"},
		{"a:b|c?.pdf", "a_b_c_.pdf"},
		{"CON.txt", "_CON.txt"},
		{".hidden", "hidden"},
	} {
		if got := safeMediaFilename(tc.in, "fallback"); got != tc.want {
			t.Errorf("safeMediaFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := strings.Repeat("a", 300) + ".pdf"
	if got := safeMediaFilename(long, "x"); len([]rune(got)) != 180 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("long name not capped with its extension kept: %d runes, %q", len([]rune(got)), got[len(got)-8:])
	}
}

func makeZip(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("content of " + n))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var jpegBytes = append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 64)...)

func TestScanFile(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mediaType string
		filename  string
		data      []byte
		status    string
		reason    string // substring expected in Reason
	}{
		{"clean photo", "image", "image_ab.jpg", jpegBytes, scanClean, ""},
		{"clean docx", "document", "Report.docx", makeZip(t, "[Content_Types].xml", "word/document.xml"), scanClean, ""},
		{"zip hiding a disk image", "document", "INVOIS.zip", makeZip(t, "INVOIS.vhd"), scanSuspicious, "zip contains disk image INVOIS.vhd"},
		{"zip with a program", "document", "files.zip", makeZip(t, "readme.txt", "setup.exe"), scanSuspicious, "zip contains setup.exe"},
		{"program named as pdf", "document", "invoice.pdf", append([]byte("MZ\x90\x00"), make([]byte, 64)...), scanSuspicious, "file is a Windows program"},
		{"disguised name", "document", "invoice.pdf.exe", append([]byte("MZ"), make([]byte, 64)...), scanSuspicious, "disguised name: looks like .pdf but is .exe"},
		{"padded extension", "document", "OPEN        .EXE", []byte("MZ"), scanSuspicious, "name pads spaces"},
		{"image that is a zip", "image", "image_ab.jpg", makeZip(t, "x.txt"), scanSuspicious, "sent as image but is really zip"},
		{"animated sticker zip is fine", "sticker", "sticker_ab.webp", makeZip(t, "animation/animation.json"), scanClean, ""},
		{"office macros", "document", "q.docm", makeZip(t, "word/document.xml", "word/vbaProject.bin"), scanSuspicious, "Office file with macros"},
		{"pdf with javascript", "document", "form.pdf", []byte("%PDF-1.7\n1 0 obj << /OpenAction << /S /JavaScript /JS (app.alert(1)) >> >>"), scanSuspicious, "PDF contains JavaScript"},
		{"plain pdf", "document", "plain.pdf", []byte("%PDF-1.7\n1 0 obj << /Type /Catalog /JSON 1 >>"), scanClean, ""},
		{"web page named txt", "document", "notes.txt", []byte("<!DOCTYPE html><script>x()</script>"), scanSuspicious, "really a web page"},
		{"voice note amr", "audio", "audio_ab.ogg", []byte("#!AMR\n\x00\x00"), scanClean, ""},
		{"rtf posing as doc", "document", "letter.doc", []byte(`{\rtf1\ansi {\object}}`), scanSuspicious, "named .doc but is really rtf"},
		{"rtf named rtf", "document", "letter.rtf", []byte(`{\rtf1\ansi hello}`), scanClean, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scanFile(tc.mediaType, tc.filename, tc.data)
			if s.Status != tc.status {
				t.Fatalf("status = %q (%s), want %q", s.Status, s.Reason, tc.status)
			}
			if tc.reason != "" && !strings.Contains(s.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to mention %q", s.Reason, tc.reason)
			}
			if tc.reason == "" && s.Reason != "" {
				t.Errorf("unexpected reason %q", s.Reason)
			}
			if len(s.SHA256) != 64 {
				t.Errorf("sha256 = %q", s.SHA256)
			}
		})
	}
}

func TestKnownMalwareFlaggedBeforeDownload(t *testing.T) {
	sha, _ := hex.DecodeString("6c896d12eb9c90d96cdb6d0b81b87da3ea4b6ddde0dc0ae53aadb5875ad68649")
	s := preScanFile("document", "INVOIS.zip", sha)
	if s.Status != scanMalware || !strings.Contains(s.Reason, "known malware") {
		t.Fatalf("pre-scan = %+v, want known malware", s)
	}
	// A suspicious-looking name alone stays suspicious, and an ordinary file
	// stays unchecked until its bytes are seen.
	if s := preScanFile("document", "setup.exe", []byte{1, 2, 3}); s.Status != scanSuspicious {
		t.Errorf("setup.exe pre-scan = %+v", s)
	}
	if s := preScanFile("document", "report.pdf", []byte{1, 2, 3}); s.Status != "" {
		t.Errorf("report.pdf pre-scan = %+v, want unchecked", s)
	}
}

func TestZipBombFlagged(t *testing.T) {
	// Only the directory's claimed size matters (nothing is decompressed), so
	// a raw entry that claims 3 GB stands in for a real bomb.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "big.bin", Method: zip.Deflate, CompressedSize64: 4, UncompressedSize64: 3 << 30})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte{0x03, 0x00, 0x00, 0x00})
	zw.Close()
	if r := zipReasons(buf.Bytes()); len(r) == 0 || !strings.Contains(strings.Join(r, ";"), "zip bomb") {
		t.Fatalf("zipReasons = %v, want zip bomb", r)
	}
}

// The verdict travels from the store to list_messages: a pre-scan flag shows
// under the message line, and a byte scan replaces it.
func TestScanVerdictInListMessages(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()

	for _, q := range []string{
		`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP)`,
		`CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP,
			is_from_me BOOLEAN, media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB,
			file_enc_sha256 BLOB, file_length INTEGER, quoted_message_id TEXT, quoted_sender TEXT, quoted_content TEXT,
			is_forwarded BOOLEAN, direct_path TEXT, scan_status TEXT, scan_reason TEXT, scanned_at TIMESTAMP,
			PRIMARY KEY (id, chat_jid))`,
		`INSERT INTO chats VALUES ('120363@g.us', 'Finance', NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s := &MessageStore{db: db}
	sha, _ := hex.DecodeString("6c896d12eb9c90d96cdb6d0b81b87da3ea4b6ddde0dc0ae53aadb5875ad68649")
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	if err := s.StoreMessage("BAD1", "120363@g.us", "60123456789", "", at, false, "document", "INVOIS.zip", "", "", []byte{1}, sha, []byte{1}, 10, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("OK1", "120363@g.us", "60123456789", "", at.Add(time.Minute), false, "document", "report.pdf", "", "", []byte{1}, []byte{9}, []byte{1}, 10, "", "", "", false); err != nil {
		t.Fatal(err)
	}

	chat := "120363@g.us"
	out, err := s.ListMessages(ListMessagesParams{ChatJid: &chat, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "⤷ ⚠ flagged file: known malware (INVOIS.vhd dropper)") {
		t.Errorf("malware flag missing:\n%s", out)
	}
	if strings.Count(out, "⤷") != 1 {
		t.Errorf("only the malware message should carry a flag line:\n%s", out)
	}

	// A byte scan wins over the arrival verdict, and a later re-store keeps it.
	if err := s.SaveFileScan("OK1", chat, FileScan{Status: scanSuspicious, Reason: "PDF contains JavaScript"}); err != nil {
		t.Fatal(err)
	}
	s.recordPreScan("OK1", chat, FileScan{Status: scanSuspicious, Reason: "should not overwrite"})
	got, fromBytes := s.GetFileScan("OK1", chat)
	if !fromBytes || got.Reason != "PDF contains JavaScript" || got.SHA256 != "09" {
		t.Errorf("GetFileScan = %+v fromBytes=%v", got, fromBytes)
	}
	out, _ = s.ListMessages(ListMessagesParams{ChatJid: &chat, Limit: 10})
	if !strings.Contains(out, "⤷ ⚠ flagged file: PDF contains JavaScript") {
		t.Errorf("byte-scan flag missing:\n%s", out)
	}
}
