package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// list_messages with context must honour the sizes it is given and list each
// message once, even when two matches share their surroundings.
func TestListMessagesContextSizesAndDedupe(t *testing.T) {
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
			is_from_me BOOLEAN, media_type TEXT, quoted_message_id TEXT, quoted_sender TEXT, quoted_content TEXT, scan_status TEXT, scan_reason TEXT,
			PRIMARY KEY (id, chat_jid))`,
		`INSERT INTO chats VALUES ('120363@g.us', 'Finance', NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// M0..M9 a minute apart; M3 and M4 are the matches, next to each other.
	for i := 0; i < 10; i++ {
		content := "filler"
		if i == 3 || i == 4 {
			content = "invoice paid"
		}
		id := "M" + string(rune('0'+i))
		ts := time.Date(2026, 10, 29, 10, i, 0, 0, time.UTC)
		if _, err := db.Exec(`INSERT INTO messages VALUES (?, '120363@g.us', '60123456789', ?, ?, 0, NULL, NULL, NULL, NULL, NULL, NULL)`, id, content, ts); err != nil {
			t.Fatal(err)
		}
	}
	addDeletedAtColumn(t, db)
	s := &MessageStore{db: db}

	chat, query := "120363@g.us", "invoice"
	ids := func(before, after int) []string {
		t.Helper()
		out, err := s.ListMessages(ListMessagesParams{ChatJid: &chat, Query: &query, Limit: 10,
			IncludeContext: true, ContextBefore: before, ContextAfter: after})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, line := range strings.Split(out, "\n") {
			if i := strings.Index(line, "[Message ID: "); i >= 0 {
				got = append(got, line[i+len("[Message ID: "):i+len("[Message ID: ")+2])
			}
		}
		return got
	}
	set := func(got []string) map[string]int {
		m := map[string]int{}
		for _, id := range got {
			m[id]++
		}
		return m
	}

	// 1 either side: M2 M3 M4 M5, each once (without dedupe M3 and M4 repeat).
	got := ids(1, 1)
	if m := set(got); len(got) != 4 || m["M2"] != 1 || m["M3"] != 1 || m["M4"] != 1 || m["M5"] != 1 {
		t.Errorf("context 1/1: got %v, want M2..M5 once each", got)
	}
	// 0 either side: only the matches.
	if got := ids(0, 0); len(got) != 2 || set(got)["M3"] != 1 || set(got)["M4"] != 1 {
		t.Errorf("context 0/0: got %v, want only M3 and M4", got)
	}
	// 5 either side covers M0..M9, still each once.
	if got := ids(5, 5); len(got) != 10 || len(set(got)) != 10 {
		t.Errorf("context 5/5: got %v, want M0..M9 once each", got)
	}
}

func TestContextParam(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want int
	}{{"", 5}, {"0", 0}, {"2", 2}, {"-1", 5}, {"x", 5}} {
		if got := contextParam(c.raw, 5); got != c.want {
			t.Errorf("contextParam(%q, 5) = %d, want %d", c.raw, got, c.want)
		}
	}
}
