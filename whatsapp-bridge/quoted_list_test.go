package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// A reply read back through list_messages / get_message_context must name the
// message it quotes, so "received 42868.80" can be tied to its card.
func TestListMessagesShowsQuotedMessage(t *testing.T) {
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
			is_from_me BOOLEAN, media_type TEXT, quoted_message_id TEXT, quoted_sender TEXT, quoted_content TEXT,
			PRIMARY KEY (id, chat_jid))`,
		`INSERT INTO chats VALUES ('120363@g.us', 'Finance', NULL)`,
		`INSERT INTO chats VALUES ('60123456789@s.whatsapp.net', 'Ali', NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	at := func(min int) time.Time { return time.Date(2026, 10, 29, 10, min, 0, 0, time.UTC) }
	card := "Order #A123\nRM 42,868.80"
	for _, row := range [][]any{
		{"CARD1", "60123456789", card, at(2), nil, nil, nil},
		{"REPLY1", "60199999999", "received 42868.80", at(20), "CARD1", "60123456789@s.whatsapp.net", card},
	} {
		if _, err := db.Exec(`INSERT INTO messages VALUES (?, '120363@g.us', ?, ?, ?, 0, NULL, ?, ?, ?)`, row...); err != nil {
			t.Fatal(err)
		}
	}
	s := &MessageStore{db: db}

	chat := "120363@g.us"
	out, err := s.ListMessages(ListMessagesParams{ChatJid: &chat, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := "    ⤷ replying to Ali (60123456789) [Message ID: CARD1]: Order #A123 RM 42,868.80\n"
	if !strings.Contains(out, "received 42868.80\n"+want) {
		t.Errorf("reply line missing its quote\n got %q\nwant it to contain %q", out, want)
	}
	if strings.Count(out, "replying to") != 1 {
		t.Errorf("only the reply should carry a quote line: %q", out)
	}

	ctx, err := s.GetMessageContext("REPLY1", 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if m := ctx.Message; m.QuotedMessageID != "CARD1" || m.QuotedSender != "60123456789@s.whatsapp.net" {
		t.Errorf("context target lost its quote: %+v", m)
	}
	if len(ctx.Before) != 1 || ctx.Before[0].QuotedMessageID != "" {
		t.Errorf("context before: %+v", ctx.Before)
	}

	long := s.formatQuoteLine(MessageInteraction{QuotedMessageID: "X", QuotedContent: strings.Repeat("a", 400)})
	if !strings.Contains(long, "replying to unknown [Message ID: X]: "+strings.Repeat("a", quotePreviewMaxRunes)+"…\n") {
		t.Errorf("long preview not capped: %q", long)
	}
}
