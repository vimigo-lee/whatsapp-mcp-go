package main

import (
	"context"
	"strings"
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

// fakeSecrets is whatsmeow's message-secret store reduced to one event.
type fakeSecrets struct {
	id     types.MessageID
	sender types.JID
	secret []byte
}

func (f *fakeSecrets) PutMessageSecrets(context.Context, []wastore.MessageSecretInsert) error {
	return nil
}
func (f *fakeSecrets) PutMessageSecret(context.Context, types.JID, types.JID, types.MessageID, []byte) error {
	return nil
}
func (f *fakeSecrets) GetMessageSecret(_ context.Context, _, _ types.JID, id types.MessageID) ([]byte, types.JID, error) {
	if id != f.id {
		return nil, types.EmptyJID, nil
	}
	return f.secret, f.sender, nil
}

// rsvpFrom builds the RSVP `responder` sends to the event, encrypted the way
// their phone does it, and wraps it as the event our bridge receives.
func rsvpFrom(t *testing.T, client *whatsmeow.Client, chat, responder, creator types.JID, eventID, rsvpID string,
	answer waE2E.EventResponseMessage_EventResponseType, guests int32, at time.Time) *events.Message {
	t.Helper()
	plaintext, err := proto.Marshal(&waE2E.EventResponseMessage{
		Response:        answer.Enum(),
		TimestampMS:     proto.Int64(at.UnixMilli()),
		ExtraGuestCount: proto.Int32(guests),
	})
	if err != nil {
		t.Fatal(err)
	}
	ct, iv, err := client.DangerousInternals().EncryptMsgSecret(context.Background(), responder, chat, creator, eventID, whatsmeow.EncSecretEventResponse, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	// The key names the event as the responder sees it: in a DM the chat is
	// the creator; in a group the creator is the participant.
	key := &waCommon.MessageKey{RemoteJID: proto.String(creator.String()), FromMe: proto.Bool(false), ID: proto.String(eventID)}
	if chat.Server == types.GroupServer {
		key.RemoteJID = proto.String(chat.String())
		key.Participant = proto.String(creator.String())
	}
	e := &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: responder, IsGroup: chat.Server == types.GroupServer},
		ID:            rsvpID, Timestamp: at,
	}, RawMessage: &waE2E.Message{EncEventResponseMessage: &waE2E.EncEventResponseMessage{
		EventCreationMessageKey: key, EncPayload: ct, EncIV: iv,
	}}}
	return e.UnwrapRaw()
}

func TestEventRSVPsAreDecryptedStoredAndListed(t *testing.T) {
	t.Chdir(t.TempDir())
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ms.Close() })

	own := types.NewJID("60100000000", types.DefaultUserServer)
	alice := types.NewJID("60111111111", types.DefaultUserServer)
	bob := types.NewJID("60122222222", types.DefaultUserServer)
	carol := types.NewJID("60133333333", types.DefaultUserServer)
	group := types.NewJID("120363000000000009", types.GroupServer)
	secrets := &fakeSecrets{id: "EV1", sender: own, secret: []byte(strings.Repeat("k", 32))}
	client := &whatsmeow.Client{Store: &wastore.Device{MsgSecrets: secrets}}

	t0 := time.Date(2026, 11, 28, 7, 0, 0, 0, time.UTC)
	if err := ms.StoreChat(group.String(), "Team", t0); err != nil {
		t.Fatal(err)
	}
	ev := &waE2E.Message{EventMessage: &waE2E.EventMessage{Name: proto.String("Launch"), StartTime: proto.Int64(t0.Add(time.Hour).Unix())}}
	if err := ms.StoreMessage("EV1", group.String(), own.String(), eventContent(ev), t0, true,
		"", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
		t.Fatal(err)
	}

	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	handleMessage(client, ms, rsvpFrom(t, client, group, alice, own, "EV1", "R1", waE2E.EventResponseMessage_GOING, 2, at(1)), waLog.Noop)
	handleMessage(client, ms, rsvpFrom(t, client, group, bob, own, "EV1", "R2", waE2E.EventResponseMessage_GOING, 0, at(2)), waLog.Noop)
	handleMessage(client, ms, rsvpFrom(t, client, group, carol, own, "EV1", "R3", waE2E.EventResponseMessage_MAYBE, 0, at(3)), waLog.Noop)
	// Bob changes his mind; the later answer wins.
	handleMessage(client, ms, rsvpFrom(t, client, group, bob, own, "EV1", "R4", waE2E.EventResponseMessage_NOT_GOING, 0, at(4)), waLog.Noop)
	// A stale re-delivery of Bob's first answer must not undo the change.
	handleMessage(client, ms, rsvpFrom(t, client, group, bob, own, "EV1", "R2", waE2E.EventResponseMessage_GOING, 0, at(2)), waLog.Noop)

	got := map[string]EventResponse{}
	for _, r := range ms.GetEventResponses("EV1") {
		got[r.Sender] = r
	}
	want := map[string]struct {
		answer string
		guests int
	}{own.User: {"going", 0}, alice.User: {"going", 2}, bob.User: {"not_going", 0}, carol.User: {"maybe", 0}}
	if len(got) != len(want) {
		t.Fatalf("got %d RSVPs, want %d: %+v", len(got), len(want), got)
	}
	for who, w := range want {
		if r := got[who]; r.Response != w.answer || r.ExtraGuests != w.guests {
			t.Errorf("%s: %s +%d, want %s +%d", who, r.Response, r.ExtraGuests, w.answer, w.guests)
		}
	}

	// RSVPs never become messages of their own.
	var n int
	if err := ms.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("messages = %d (%v), want only the event", n, err)
	}

	// The creator never sent an RSVP, so they are listed as going (organizer).
	if !got[own.User].Organizer || got[alice.User].Organizer {
		t.Errorf("organizer flags wrong: %+v", got)
	}

	line := ms.FormatMessage(MessageInteraction{ID: "EV1", ChatJID: group.String(), Content: eventContent(ev), IsFromMe: true, Timestamp: t0}, false)
	for _, part := range []string{"⤷ RSVPs: going (2): ", "+2", "maybe (1): ", "not going (1): "} {
		if !strings.Contains(line, part) {
			t.Errorf("listing lacks %q:\n%s", part, line)
		}
	}

	// The creator answering otherwise replaces the implied "going".
	handleMessage(client, ms, rsvpFrom(t, client, group, own, own, "EV1", "R5", waE2E.EventResponseMessage_MAYBE, 0, at(5)), waLog.Noop)
	var orgAnswers []string
	for _, r := range ms.GetEventResponses("EV1") {
		if r.Sender == own.User {
			orgAnswers = append(orgAnswers, r.Response)
			if !r.Organizer {
				t.Errorf("creator's own RSVP lost the organizer flag")
			}
		}
	}
	if len(orgAnswers) != 1 || orgAnswers[0] != "maybe" {
		t.Errorf("creator answers = %v, want [maybe]", orgAnswers)
	}
}

// An RSVP to a 1:1 event names the creator as the chat in its key.
func TestEventRSVPInDirectChat(t *testing.T) {
	own := types.NewJID("60100000000", types.DefaultUserServer)
	alice := types.NewJID("60111111111", types.DefaultUserServer)
	client := &whatsmeow.Client{Store: &wastore.Device{MsgSecrets: &fakeSecrets{id: "EV2", sender: own, secret: []byte(strings.Repeat("s", 32))}}}
	at := time.Date(2026, 11, 28, 7, 0, 0, 0, time.UTC)
	resp, err := decryptEventResponse(context.Background(), client, rsvpFrom(t, client, alice, alice, own, "EV2", "R1", waE2E.EventResponseMessage_MAYBE, 1, at))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResponse() != waE2E.EventResponseMessage_MAYBE || resp.GetExtraGuestCount() != 1 || resp.GetTimestampMS() != at.UnixMilli() {
		t.Fatalf("decrypted %+v", resp)
	}
}

// Without the event's secret (an event from before the bridge was linked) the
// RSVP is skipped, not stored as junk.
func TestEventRSVPWithoutSecretIsSkipped(t *testing.T) {
	t.Chdir(t.TempDir())
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ms.Close() })
	own := types.NewJID("60100000000", types.DefaultUserServer)
	alice := types.NewJID("60111111111", types.DefaultUserServer)
	sender := &whatsmeow.Client{Store: &wastore.Device{MsgSecrets: &fakeSecrets{id: "EV3", sender: own, secret: []byte(strings.Repeat("x", 32))}}}
	receiver := &whatsmeow.Client{Store: &wastore.Device{MsgSecrets: &fakeSecrets{}}}
	if err := ms.StoreChat(alice.String(), "Alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	handleMessage(receiver, ms, rsvpFrom(t, sender, alice, alice, own, "EV3", "R1", waE2E.EventResponseMessage_GOING, 0, time.Now()), waLog.Noop)
	if rs := ms.GetEventResponses("EV3"); len(rs) != 0 {
		t.Fatalf("stored %+v without the secret", rs)
	}
}

func TestEventContent(t *testing.T) {
	prev := displayLoc
	displayLoc = time.FixedZone("MYT", 8*3600)
	defer func() { displayLoc = prev }()
	start := time.Date(2026, 11, 28, 15, 0, 0, 0, displayLoc)
	got := eventContent(&waE2E.Message{EventMessage: &waE2E.EventMessage{
		Name:               proto.String("Project launch"),
		Description:        proto.String("Bring the deck"),
		StartTime:          proto.Int64(start.Unix()),
		EndTime:            proto.Int64(start.Add(2 * time.Hour).Unix()),
		Location:           &waE2E.LocationMessage{Name: proto.String("KL office")},
		JoinLink:           proto.String("https://call.whatsapp.com/video/abc"),
		ExtraGuestsAllowed: proto.Bool(true),
	}})
	want := "[event] Project launch\nWhen: Sat 28 Nov 2026 15:00 – 17:00 MYT\nWhere: KL office\nCall link: https://call.whatsapp.com/video/abc\nGuests may bring others\nBring the deck"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	cancelled := eventContent(&waE2E.Message{EventMessage: &waE2E.EventMessage{Name: proto.String("x"), IsCanceled: proto.Bool(true)}})
	if cancelled != "[event cancelled] x" {
		t.Fatalf("cancelled = %q", cancelled)
	}
	if eventContent(&waE2E.Message{Conversation: proto.String("hi")}) != "" {
		t.Fatal("non-event rendered as event")
	}
}

func TestParseEventTime(t *testing.T) {
	prev := displayLoc
	displayLoc = time.FixedZone("MYT", 8*3600)
	defer func() { displayLoc = prev }()
	want := time.Date(2026, 11, 28, 7, 0, 0, 0, time.UTC)
	for _, in := range []string{"2026-11-28T15:00:00+08:00", "2026-11-28 15:00", "2026-11-28T15:00", "1795849200"} {
		got, err := parseEventTime(in)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseEventTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseEventTime("next friday"); err == nil {
		t.Error("free text accepted")
	}
}

func TestBuildEventMessageCarriesSecret(t *testing.T) {
	start := time.Unix(1795849200, 0)
	m, err := buildEventMessage(SendEventRequest{Name: "Launch", Location: "KL", ExtraGuestsAllowed: true}, start, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.GetMessageContextInfo().GetMessageSecret()) != 32 {
		t.Fatal("event has no 32-byte message secret; RSVPs could not be decrypted")
	}
	ev := m.GetEventMessage()
	if ev.GetName() != "Launch" || ev.GetStartTime() != start.Unix() || ev.EndTime != nil || ev.GetLocation().GetName() != "KL" || !ev.GetExtraGuestsAllowed() {
		t.Fatalf("event = %+v", ev)
	}
	if meta := (*eventCreationMeta())[0]; meta.Tag != "meta" || meta.Attrs["event_type"] != "creation" {
		t.Fatalf("meta node = %+v", meta)
	}
}

func TestEventInviteContent(t *testing.T) {
	inv := &waE2E.EventInviteMessage{EventTitle: proto.String("Test 2"), StartTime: proto.Int64(1795681800)}
	got := describeMessage(&waE2E.Message{EventInviteMessage: inv})
	if !strings.HasPrefix(got, "[event] Test 2\nWhen: ") {
		t.Errorf("content = %q", got)
	}
}

// RSVP times are read back as the local time they were stored as, whatever
// zone the driver labels them with (lib/pq: a zero-offset zone), so an answer
// given at 16:08 in UTC+8 does not show as 00:08 the next day.
func TestEventResponseTimesStayLocal(t *testing.T) {
	prevLocal, prevDisplay := time.Local, displayLoc
	time.Local = time.FixedZone("MYT", 8*3600)
	displayLoc = time.Local
	defer func() { time.Local, displayLoc = prevLocal, prevDisplay }()

	got := storedLocal(time.Date(2026, 11, 26, 16, 8, 0, 0, time.FixedZone("", 0))).In(displayLoc)
	if got.Hour() != 16 || got.Day() != 26 {
		t.Errorf("RSVP time read back as %v, want 16:08 on the 26th", got)
	}
}
