package main

import (
	"database/sql"
	"strings"
	"testing"
)

// addDeletedAtColumn adds the revoke tombstone column the bridge adds at
// startup, for tests that build the messages table by hand and insert rows
// positionally before it exists.
func addDeletedAtColumn(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN deleted_at TIMESTAMP`); err != nil {
		t.Fatal(err)
	}
}

// addLifecycleColumns adds the revoke and edit columns and the pending-revoke
// table the bridge creates at startup, for tests that build the schema by hand.
func addLifecycleColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`ALTER TABLE messages ADD COLUMN deleted_at TIMESTAMP`,
		`ALTER TABLE messages ADD COLUMN edited_at TIMESTAMP`,
		`CREATE TABLE IF NOT EXISTS pending_revokes (id TEXT, chat_jid TEXT, is_from_me BOOLEAN NOT NULL,
			senders TEXT NOT NULL, revoked_at TIMESTAMP NOT NULL, PRIMARY KEY (id, chat_jid))`,
	} {
		if _, err := db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			t.Fatal(err)
		}
	}
}
