package main

import (
	"testing"
	"time"
)

// /mark-read sends the phone a MarkChatAsRead patch only when ClearUnread says
// the chat was unread. The manager calls /mark-read on every chat open, so a
// chat that is already read must report false, or every click becomes an
// app-state write.
func TestClearUnreadReportsOnlyARealRead(t *testing.T) {
	t.Chdir(t.TempDir())
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ms.Close() }) // before TempDir cleanup: Windows cannot remove an open WAL database
	chat := "60100000002@s.whatsapp.net"
	at := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(chat, "Siti", at); err != nil {
		t.Fatal(err)
	}

	if was, err := ms.ClearUnread(chat); err != nil || was {
		t.Fatalf("already-read chat: got (%v, %v), want (false, nil)", was, err)
	}
	if err := ms.IncrementUnread(chat); err != nil {
		t.Fatal(err)
	}
	if was, err := ms.ClearUnread(chat); err != nil || !was {
		t.Fatalf("unread chat: got (%v, %v), want (true, nil)", was, err)
	}
	if was, err := ms.ClearUnread(chat); err != nil || was {
		t.Fatalf("second clear: got (%v, %v), want (false, nil)", was, err)
	}
	if was, err := ms.ClearUnread("60100000099@s.whatsapp.net"); err != nil || was {
		t.Fatalf("unknown chat: got (%v, %v), want (false, nil)", was, err)
	}
}

// The patch's message range is anchored on the latest incoming message, so
// LatestIncoming must return that message's own timestamp, not our later reply.
func TestLatestIncomingReturnsItsTimestamp(t *testing.T) {
	t.Chdir(t.TempDir())
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ms.Close() })
	chat := "60100000003@s.whatsapp.net"
	at := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(chat, "Ah Kow", at); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("IN1", chat, "60100000003", "hi", at, false,
		"", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("OUT1", chat, "me", "ok", at.Add(time.Minute), true,
		"", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
		t.Fatal(err)
	}

	id, _, ts, ok := ms.LatestIncoming(chat)
	if !ok || id != "IN1" {
		t.Fatalf("got id %q ok %v, want IN1 true", id, ok)
	}
	if !ts.Equal(at) {
		t.Fatalf("got ts %v, want %v", ts, at)
	}
}
