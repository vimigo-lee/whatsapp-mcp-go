package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// An RSVP our bridge sends must decrypt and be stored on the event creator's
// side, exactly as an RSVP from a phone is.
func TestSentRSVPReachesTheCreator(t *testing.T) {
	t.Chdir(t.TempDir())
	prevPG := isPostgres
	isPostgres = false
	defer func() { isPostgres = prevPG }()
	ms, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ms.Close() })

	creator := types.NewJID("60100000000", types.DefaultUserServer)
	us := types.NewJID("60111111111", types.DefaultUserServer)
	secrets := &fakeSecrets{id: "EV1", sender: creator, secret: []byte(strings.Repeat("k", 32))}
	ours := &whatsmeow.Client{Store: &wastore.Device{ID: &us, MsgSecrets: secrets}}
	theirs := &whatsmeow.Client{Store: &wastore.Device{ID: &creator, MsgSecrets: secrets}}

	resp := &waE2E.EventResponseMessage{
		Response:        waE2E.EventResponseMessage_GOING.Enum(),
		TimestampMS:     proto.Int64(time.Now().UnixMilli()),
		ExtraGuestCount: proto.Int32(2),
	}
	// From our side the DM is the creator's chat.
	msg, err := buildEventResponseMessage(context.Background(), ours, creator, creator.User, "EV1", resp)
	if err != nil {
		t.Fatal(err)
	}
	if k := msg.GetEncEventResponseMessage().GetEventCreationMessageKey(); k.GetFromMe() || k.GetID() != "EV1" {
		t.Fatalf("unexpected key: %+v", k)
	}

	// The creator's bridge holds the event and receives the RSVP from us.
	t0 := time.Now().Add(-time.Hour)
	if err := ms.StoreChat(us.String(), "Friend", t0); err != nil {
		t.Fatal(err)
	}
	ev := &waE2E.Message{EventMessage: &waE2E.EventMessage{Name: proto.String("Launch"), StartTime: proto.Int64(t0.Add(48 * time.Hour).Unix())}}
	if err := ms.StoreMessage("EV1", us.String(), creator.String(), eventContent(ev), t0, true,
		"", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	in := &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: us, Sender: us},
		ID:            "R1", Timestamp: time.Now(),
	}, Message: msg}
	handleMessage(theirs, ms, in, waLog.Noop)

	var got *EventResponse
	for _, r := range ms.GetEventResponses("EV1") {
		if r.Sender == us.User {
			r := r
			got = &r
		}
	}
	if got == nil || got.Response != "going" || got.ExtraGuests != 2 {
		t.Fatalf("creator did not record our RSVP: %+v", ms.GetEventResponses("EV1"))
	}
}

func TestEventResponseType(t *testing.T) {
	for _, a := range []string{"going", "maybe", "not_going"} {
		if _, ok := eventResponseType(a); !ok {
			t.Errorf("%s not accepted", a)
		}
	}
	if _, ok := eventResponseType("yes"); ok {
		t.Error(`"yes" accepted`)
	}
}
