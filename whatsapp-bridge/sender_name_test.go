package main

import (
	"database/sql"
	"testing"
)

// A sender's name must come from their own chat. The old substring fallback
// showed sender 4791112345 as "Boss" because Boss's JID, 447911123456,
// happens to contain those digits.
func TestGetSenderNameUsesOnlyTheSendersOwnChat(t *testing.T) {
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
		`INSERT INTO chats VALUES ('447911123456@s.whatsapp.net', 'Boss', NULL)`,
		`INSERT INTO chats VALUES ('60123456789@s.whatsapp.net', 'Ali', NULL)`,
		`INSERT INTO chats VALUES ('123456789012345@lid', 'Siti', NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s := &MessageStore{db: db}
	for sender, want := range map[string]string{
		"4791112345":                    "4791112345", // substring of Boss's number: not Boss
		"60123456789":                   "Ali",        // bare digits, as senders are stored
		"60123456789@s.whatsapp.net":    "Ali",
		"60123456789:12@s.whatsapp.net": "Ali",  // device suffix
		"123456789012345":               "Siti", // LID sender
		"60199999999":                   "60199999999",
	} {
		if got := s.GetSenderName(sender); got != want {
			t.Errorf("GetSenderName(%q) = %q, want %q", sender, got, want)
		}
	}
}
