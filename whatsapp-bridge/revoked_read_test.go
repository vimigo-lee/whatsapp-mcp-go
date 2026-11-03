package main

import (
	"strings"
	"testing"
	"time"
)

// A message its sender deleted for everyone keeps its row as a tombstone, but
// readers must no longer be shown the retracted text as if it were live.
func TestRevokedMessagesReadAsDeleted(t *testing.T) {
	t.Chdir(t.TempDir())
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	chat := "60100000001@s.whatsapp.net"
	at := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(chat, "Ali", at); err != nil {
		t.Fatal(err)
	}
	for i, row := range [][2]string{{"KEEP1", "still here"}, {"GONE1", "SENT-BY-MISTAKE"}} {
		if err := ms.StoreMessage(row[0], chat, "60100000001", row[1], at.Add(time.Duration(i)*time.Minute), false,
			"", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := ms.StoreChat(chat, "Ali", at.Add(time.Minute)); err != nil { // GONE1 is the latest
		t.Fatal(err)
	}
	if err := ms.DeleteMessage(chat, "GONE1"); err != nil {
		t.Fatal(err)
	}

	list, err := ms.ListMessages(ListMessagesParams{ChatJid: &chat, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list, "SENT-BY-MISTAKE") || !strings.Contains(list, "[message deleted]") || !strings.Contains(list, "still here") {
		t.Errorf("list_messages:\n%s", list)
	}
	ctx, err := ms.GetMessageContext("GONE1", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Message.Content != "[message deleted]" {
		t.Errorf("get_message_context content = %q", ctx.Message.Content)
	}
	c, err := ms.GetChat(chat, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.LastMessage != "[message deleted]" {
		t.Errorf("chat last_message = %q", c.LastMessage)
	}
}
