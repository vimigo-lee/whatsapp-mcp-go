package main

import (
	"database/sql"
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
