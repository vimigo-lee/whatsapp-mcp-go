package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// protocolEvent builds a received edit or revoke the way whatsmeow delivers it.
func protocolEvent(chat, sender types.JID, fromMe bool, id string, pm *waE2E.ProtocolMessage, at time.Time) *events.Message {
	e := &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: chat.Server == types.GroupServer, IsFromMe: fromMe},
		ID:            id, Timestamp: at,
	}, RawMessage: &waE2E.Message{ProtocolMessage: pm}}
	return e.UnwrapRaw()
}

func editOf(target, text string) *waE2E.ProtocolMessage {
	return &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: proto.String(target)},
		EditedMessage: &waE2E.Message{Conversation: proto.String(text)},
	}
}

func revokeOf(target string) *waE2E.ProtocolMessage {
	return &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String(target)}}
}

// The ID in a received edit or revoke is chosen by its sender and the server
// cannot see it, so only the author may change a row. Forged edits and revokes
// from other participants are ignored; the author's own still apply.
func TestReceivedEditsAndRevokesOnlyTouchTheAuthorsRow(t *testing.T) {
	t.Chdir(t.TempDir())
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ms.Close() }) // before TempDir cleanup: Windows cannot remove an open WAL database
	client := &whatsmeow.Client{Store: &wastore.Device{}}
	admin := types.NewJID("60144444444", types.DefaultUserServer)
	prevAdmin := isGroupAdmin
	isGroupAdmin = func(_ *whatsmeow.Client, _ types.JID, user types.JID) bool { return user == admin }
	defer func() { isGroupAdmin = prevAdmin }()
	own := types.NewJID("60100000000", types.DefaultUserServer)
	alice := types.NewJID("60111111111", types.DefaultUserServer)
	bob := types.NewJID("60122222222", types.DefaultUserServer)
	mallory := types.NewJID("60133333333", types.DefaultUserServer)
	group := types.NewJID("120363000000000009", types.GroupServer)
	t0 := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	store := func(id string, chat types.JID, sender, content string, fromMe bool) {
		t.Helper()
		if err := ms.StoreMessage(id, chat.String(), sender, content, t0, fromMe,
			"", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
			t.Fatal(err)
		}
	}
	for c, name := range map[types.JID]string{alice: "Alice", group: "Team"} {
		if err := ms.StoreChat(c.String(), name, t0); err != nil {
			t.Fatal(err)
		}
	}
	store("OWNERDM", alice, "", "owner says A", true)
	store("BOB", group, "60122222222", "bob says B", false)
	store("OWNERG", group, "", "owner says C", true)
	store("BOB2", group, "60122222222", "bob says D", false)
	store("ALICE1", alice, "60111111111", "alice says E", false)
	store("BOB3", group, "60122222222", "bob says F", false)

	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	// Forged: a DM peer edits the owner's message; a group member edits and
	// revokes other people's messages.
	handleMessage(client, ms, protocolEvent(alice, alice, false, "F1", editOf("OWNERDM", "FORGED"), at(1)), waLog.Noop)
	handleMessage(client, ms, protocolEvent(group, mallory, false, "F2", editOf("BOB", "FORGED"), at(2)), waLog.Noop)
	handleMessage(client, ms, protocolEvent(group, mallory, false, "F3", revokeOf("OWNERG"), at(3)), waLog.Noop)
	// Legitimate: the owner's other device edits its own message, Bob revokes
	// his own, Alice edits hers.
	handleMessage(client, ms, protocolEvent(alice, own, true, "L1", editOf("OWNERDM", "owner fixed A"), at(4)), waLog.Noop)
	handleMessage(client, ms, protocolEvent(group, bob, false, "L2", revokeOf("BOB2"), at(5)), waLog.Noop)
	handleMessage(client, ms, protocolEvent(alice, alice, false, "L3", editOf("ALICE1", "alice fixed E"), at(6)), waLog.Noop)
	// A group admin may delete someone else's message.
	handleMessage(client, ms, protocolEvent(group, admin, false, "L4", revokeOf("BOB3"), at(7)), waLog.Noop)

	want := map[string]struct {
		content string
		deleted bool
	}{
		"OWNERDM": {"owner fixed A (edited)", false},
		"BOB":     {"bob says B", false},
		"OWNERG":  {"owner says C", false},
		"BOB2":    {"bob says D", true},
		"ALICE1":  {"alice fixed E (edited)", false},
		"BOB3":    {"bob says F", true},
	}
	for id, w := range want {
		var content string
		var deleted bool
		if err := ms.db.QueryRow(`SELECT content, deleted_at IS NOT NULL FROM messages WHERE id = ?`, id).Scan(&content, &deleted); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if content != w.content || deleted != w.deleted {
			t.Errorf("%s: content=%q deleted=%v, want %q deleted=%v", id, content, deleted, w.content, w.deleted)
		}
	}
}

// Postgres numbers placeholders; the WHERE clause follows the SET value.
func TestAuthorScopedUpdatePostgresPlaceholders(t *testing.T) {
	prevPG := isPostgres
	isPostgres = true
	defer func() { isPostgres = prevPG }()
	q, args := authorScopedUpdate("content = ?", "content = $1, edited_at = now()", "c@g.us", "M1", false, []string{"601", "LID1"})
	wantQ := "UPDATE messages SET content = $1, edited_at = now() WHERE id = $2 AND chat_jid = $3 AND is_from_me = $4 AND sender IN ($5,$6)"
	if q != wantQ || len(args) != 5 {
		t.Errorf("edit:\n got %q (%d args)\nwant %q (5 args)", q, len(args), wantQ)
	}
	q, args = authorScopedUpdate("deleted_at = CURRENT_TIMESTAMP", "deleted_at = now()", "c@g.us", "M1", true, []string{"601"})
	wantQ = "UPDATE messages SET deleted_at = now() WHERE id = $1 AND chat_jid = $2 AND is_from_me = $3"
	if q != wantQ || len(args) != 3 {
		t.Errorf("own revoke:\n got %q (%d args)\nwant %q (3 args)", q, len(args), wantQ)
	}
}
