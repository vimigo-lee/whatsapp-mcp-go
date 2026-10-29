package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

func quoteTestStore(t *testing.T) *MessageStore {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP)`,
		`CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP,
			is_from_me BOOLEAN, media_type TEXT, PRIMARY KEY (id, chat_jid))`,
		`INSERT INTO chats VALUES ('60123456789@s.whatsapp.net', 'Ahmad', CURRENT_TIMESTAMP)`,
		`INSERT INTO messages VALUES ('IN1', '60123456789@s.whatsapp.net', '60123456789', 'hello', CURRENT_TIMESTAMP, 0, NULL)`,
		`INSERT INTO messages VALUES ('IMG1', '60123456789@s.whatsapp.net', '60123456789', '', CURRENT_TIMESTAMP, 0, 'image')`,
		`INSERT INTO messages VALUES ('OUT1', '60123456789@s.whatsapp.net', '60111111111@s.whatsapp.net', 'my reply', CURRENT_TIMESTAMP, 1, NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return &MessageStore{db: db}
}

func TestFormatMessageShowsIDAndSender(t *testing.T) {
	s := quoteTestStore(t)
	ts := time.Date(2026, 10, 29, 10, 15, 2, 0, displayLoc)

	text := s.FormatMessage(MessageInteraction{
		Timestamp: ts, Sender: "60123456789", Content: "hello",
		ChatJID: "60123456789@s.whatsapp.net", ID: "IN1", ChatName: "Ahmad",
	}, true)
	want := "[2026-10-29 10:15:02] Chat: Ahmad From: Ahmad (60123456789): [Message ID: IN1 - Chat JID: 60123456789@s.whatsapp.net] hello\n"
	if text != want {
		t.Errorf("text line\n got %q\nwant %q", text, want)
	}

	media := s.FormatMessage(MessageInteraction{
		Timestamp: ts, Sender: "60123456789", ChatJID: "60123456789@s.whatsapp.net",
		ID: "IMG1", MediaType: "image",
	}, false)
	if !strings.Contains(media, "[image - Message ID: IMG1 - Chat JID: 60123456789@s.whatsapp.net] ") {
		t.Errorf("media line lost its original prefix: %q", media)
	}

	// Unknown contact: GetSenderName falls back to the raw sender, so the
	// number must not be repeated in brackets.
	unknown := s.FormatMessage(MessageInteraction{
		Timestamp: ts, Sender: "60199999999", Content: "hi", ChatJID: "120363@g.us", ID: "G1",
	}, false)
	if !strings.Contains(unknown, "From: 60199999999: ") {
		t.Errorf("unknown sender line: %q", unknown)
	}

	mine := s.FormatMessage(MessageInteraction{
		Timestamp: ts, Sender: "60111111111@s.whatsapp.net", Content: "x", IsFromMe: true,
		ChatJID: "60123456789@s.whatsapp.net", ID: "OUT1",
	}, false)
	if !strings.Contains(mine, "From: Me: [Message ID: OUT1") {
		t.Errorf("own line: %q", mine)
	}
}

func TestFillQuoteFromID(t *testing.T) {
	s := quoteTestStore(t)
	own := types.NewJID("60111111111", types.DefaultUserServer)
	client := &whatsmeow.Client{Store: &store.Device{ID: &own}}

	cases := []struct {
		name, recipient, id, text, participant string
	}{
		{"incoming 1:1, bare recipient", "60123456789", "IN1", "hello", "60123456789@s.whatsapp.net"},
		{"recipient with plus sign", "+60 12-345 6789", "IN1", "hello", "60123456789@s.whatsapp.net"},
		{"media without caption", "60123456789@s.whatsapp.net", "IMG1", "[image]", "60123456789@s.whatsapp.net"},
		{"own message", "60123456789", "OUT1", "my reply", "60111111111@s.whatsapp.net"},
	}
	for _, c := range cases {
		req := SendMessageRequest{Recipient: c.recipient, Message: "ok", QuotedID: c.id}
		if err := fillQuote(client, s, &req); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if req.QuotedText != c.text || req.QuotedParticipant != c.participant {
			t.Errorf("%s: got text=%q participant=%q, want %q / %q",
				c.name, req.QuotedText, req.QuotedParticipant, c.text, c.participant)
		}
	}

	// A caller-supplied preview and participant are kept as-is.
	req := SendMessageRequest{Recipient: "60123456789", QuotedID: "IN1", QuotedText: "custom", QuotedParticipant: "x@lid"}
	if err := fillQuote(client, s, &req); err != nil || req.QuotedText != "custom" || req.QuotedParticipant != "x@lid" {
		t.Errorf("caller fields overwritten: %+v err=%v", req, err)
	}

	// The quoted message must belong to the recipient's chat.
	req = SendMessageRequest{Recipient: "60177777777", QuotedID: "IN1"}
	if err := fillQuote(client, s, &req); err == nil {
		t.Error("expected an error quoting a message from another chat")
	}
	req = SendMessageRequest{Recipient: "60123456789", QuotedID: "NOPE"}
	if err := fillQuote(client, s, &req); err == nil {
		t.Error("expected an error for an unknown message ID")
	}
}
