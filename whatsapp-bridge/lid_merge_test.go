package main

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// The same message stored in a contact's @lid chat and their phone-number chat
// used to make the merge hit the primary key, roll back, and fail again on
// every start (32 contacts across the prod managers, Nov 2026). The merge must
// now complete, keep one copy of each message with the @lid copy's revoke and
// newer edit, and bring the chat's labels and reactions along.
func TestMoveLIDChatRowsMergesDuplicates(t *testing.T) {
	type backend struct {
		name, driver, dsn string
		pg                bool
	}
	backends := []backend{{"sqlite", "sqlite3", ":memory:", false}}
	if dsn := os.Getenv("WA_TEST_PG_DSN"); dsn != "" {
		backends = append(backends, backend{"postgres", "postgres", dsn, true})
	} else {
		t.Log("WA_TEST_PG_DSN not set; Postgres case skipped")
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) { checkLIDMerge(t, b.driver, b.dsn, b.pg) })
	}
}

func checkLIDMerge(t *testing.T, driver, dsn string, pg bool) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ph := func(q string) string {
		if !pg {
			return q
		}
		out, n := "", 1
		for _, r := range q {
			if r == '?' {
				out += "$" + string(rune('0'+n))
				n++
				continue
			}
			out += string(r)
		}
		return out
	}
	for _, q := range []string{
		`DROP TABLE IF EXISTS messages`, `DROP TABLE IF EXISTS reactions`, `DROP TABLE IF EXISTS wa_chat_labels`,
		`CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN NOT NULL DEFAULT FALSE,
			deleted_at TIMESTAMP, edited_at TIMESTAMP, PRIMARY KEY (id, chat_jid))`,
		`CREATE TABLE reactions (msg_id TEXT, sender TEXT, chat_jid TEXT, emoji TEXT, PRIMARY KEY (msg_id, sender))`,
		`CREATE TABLE wa_chat_labels (chat_jid TEXT NOT NULL, label_id TEXT NOT NULL, PRIMARY KEY (chat_jid, label_id))`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	lid, pn := "249954396307592@lid", "60173779883@s.whatsapp.net"
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	later := at.Add(time.Hour)
	ins := func(id, chat, content string, deleted, edited *time.Time) {
		t.Helper()
		if _, err := db.Exec(ph(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, deleted_at, edited_at) VALUES (?, ?, '60173779883', ?, ?, ?, ?)`),
			id, chat, content, at, deleted, edited); err != nil {
			t.Fatal(err)
		}
	}
	ins("BOTH", pn, "hello", nil, nil)
	ins("BOTH", lid, "hello", nil, nil)
	ins("REVOKED", pn, "oops", nil, nil)
	ins("REVOKED", lid, "oops", &later, nil)
	ins("EDITED", pn, "typo", nil, nil)
	ins("EDITED", lid, "fixed (edited)", nil, &later)
	ins("LIDONLY", lid, "only here", nil, nil)
	ins("PNONLY", pn, "phone only", nil, nil)
	for _, q := range []string{
		`INSERT INTO reactions VALUES ('BOTH', '601', '` + lid + `', '👍')`,
		`INSERT INTO wa_chat_labels VALUES ('` + lid + `', 'L1')`,
		`INSERT INTO wa_chat_labels VALUES ('` + lid + `', 'L2')`,
		`INSERT INTO wa_chat_labels VALUES ('` + pn + `', 'L2')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := moveLIDChatRows(tx, pg, lid, pn); err != nil {
		tx.Rollback()
		t.Fatalf("merge failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(ph(q), args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, lid); n != 0 {
		t.Errorf("%d messages left under the @lid chat", n)
	}
	if n := count(`SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, pn); n != 5 {
		t.Errorf("phone chat has %d messages, want 5 (one copy each)", n)
	}
	if n := count(`SELECT COUNT(*) FROM messages WHERE chat_jid = ? AND id = 'REVOKED' AND deleted_at IS NOT NULL`, pn); n != 1 {
		t.Error("the @lid copy's revoke was lost")
	}
	var content string
	if err := db.QueryRow(ph(`SELECT content FROM messages WHERE chat_jid = ? AND id = 'EDITED'`), pn).Scan(&content); err != nil || content != "fixed (edited)" {
		t.Errorf("edited content = %q (%v), want the newer edit", content, err)
	}
	if n := count(`SELECT COUNT(*) FROM reactions WHERE chat_jid = ?`, pn); n != 1 {
		t.Error("reaction did not follow the chat")
	}
	if n := count(`SELECT COUNT(*) FROM wa_chat_labels WHERE chat_jid = ?`, pn); n != 2 {
		t.Errorf("phone chat has %d labels, want L1 and L2", n)
	}
	if n := count(`SELECT COUNT(*) FROM wa_chat_labels WHERE chat_jid = ?`, lid); n != 0 {
		t.Errorf("%d labels left on the @lid chat", n)
	}

	// Running again (every start) is a no-op.
	tx, _ = db.Begin()
	if err := moveLIDChatRows(tx, pg, lid, pn); err != nil {
		tx.Rollback()
		t.Fatalf("second run failed: %v", err)
	}
	tx.Commit()
	if n := count(`SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, pn); n != 5 {
		t.Errorf("second run changed the phone chat: %d messages", n)
	}
}
