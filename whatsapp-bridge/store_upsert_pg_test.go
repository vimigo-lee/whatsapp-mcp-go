package main

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// The Postgres upsert in StoreMessage, which the SQLite tests cannot reach.
// Needs a throwaway database in WA_TEST_PG_DSN.
//
// NV-4: a re-store with different file bytes clears the verdict taken from the
// old bytes, so the new file is scanned instead of inheriting "clean".
// NV-5: another sender reusing the ID leaves the row alone.
func TestStoreMessagePostgresUpsert(t *testing.T) {
	dsn := os.Getenv("WA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("WA_TEST_PG_DSN not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prevPG := isPostgres
	isPostgres = true
	defer func() { isPostgres = prevPG }()
	for _, q := range []string{
		`DROP TABLE IF EXISTS messages`,
		`CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP,
			is_from_me BOOLEAN, media_type TEXT, filename TEXT, url TEXT, media_key BYTEA, file_sha256 BYTEA,
			file_enc_sha256 BYTEA, file_length BIGINT, quoted_message_id TEXT, quoted_sender TEXT, quoted_content TEXT,
			is_forwarded BOOLEAN, direct_path TEXT, scan_status TEXT, scan_reason TEXT, scanned_at TIMESTAMP,
			PRIMARY KEY (id, chat_jid))`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s := &MessageStore{db: db}
	chat := "60100000001@s.whatsapp.net"
	at := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	doc := func(id, sender, name string, sha []byte) {
		t.Helper()
		if err := s.StoreMessage(id, chat, sender, "", at, false, "document", name, "https://mmg.whatsapp.net/x", "", []byte{1}, sha, []byte{2}, 10, "", "", "", false); err != nil {
			t.Fatal(err)
		}
	}

	doc("X", "60100000001", "a.pdf", []byte("sha-one"))
	if err := s.SaveFileScan("X", chat, FileScan{Status: scanClean}); err != nil {
		t.Fatal(err)
	}
	if _, fromBytes := s.GetFileScan("X", chat); !fromBytes {
		t.Fatal("setup: verdict not recorded as from the bytes")
	}

	// Same sender, different file under the same ID: the old verdict must go.
	doc("X", "60100000001", "b.exe", []byte("sha-two"))
	if _, fromBytes := s.GetFileScan("X", chat); fromBytes {
		t.Error("new bytes kept the old byte verdict")
	}

	// Same file again: a byte verdict survives a plain re-store.
	if err := s.SaveFileScan("X", chat, FileScan{Status: scanClean}); err != nil {
		t.Fatal(err)
	}
	doc("X", "60100000001", "b.exe", []byte("sha-two"))
	if _, fromBytes := s.GetFileScan("X", chat); !fromBytes {
		t.Error("re-storing the same bytes dropped their verdict")
	}

	// Another sender reusing the ID changes nothing.
	doc("X", "60100000002", "evil.exe", []byte("sha-three"))
	var sender, name string
	if err := db.QueryRow(`SELECT sender, filename FROM messages WHERE id = 'X' AND chat_jid = $1`, chat).Scan(&sender, &name); err != nil {
		t.Fatal(err)
	}
	if sender != "60100000001" || name != "b.exe" {
		t.Errorf("another sender replaced the row: %s %s", sender, name)
	}
}
