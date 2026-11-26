package main

// Answering an event (going / maybe / not going) from this line.
//
// An RSVP is an EncEventResponseMessage: an EventResponseMessage encrypted with
// the event's message secret (use case "Event Response"), keyed to the event's
// creation message. It travels like the event itself (stanza type "event",
// <meta event_type="response"/>, the way a poll vote carries polltype="vote").
// The answer is also stored locally, so the line's own RSVP shows at once.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// RespondEventRequest is the body of POST /api/respond-event.
type RespondEventRequest struct {
	Recipient   string `json:"recipient"`
	MessageID   string `json:"message_id"`
	Response    string `json:"response"` // going, maybe, not_going
	ExtraGuests int    `json:"extra_guests,omitempty"`
}

func eventResponseType(answer string) (waE2E.EventResponseMessage_EventResponseType, bool) {
	switch answer {
	case "going":
		return waE2E.EventResponseMessage_GOING, true
	case "maybe":
		return waE2E.EventResponseMessage_MAYBE, true
	case "not_going":
		return waE2E.EventResponseMessage_NOT_GOING, true
	}
	return 0, false
}

func eventResponseMeta() *[]waBinary.Node {
	return &[]waBinary.Node{{Tag: "meta", Attrs: waBinary.Attrs{"event_type": "response"}}}
}

// buildEventResponseMessage encrypts an RSVP to the event msgID in chat, sent
// by creator (a bare number or a JID).
func buildEventResponseMessage(ctx context.Context, client *whatsmeow.Client, chat types.JID, creator, msgID string, resp *waE2E.EventResponseMessage) (*waE2E.Message, error) {
	if client.Store.ID == nil {
		return nil, errors.New("not logged in")
	}
	creatorJID := types.NewJID(bareUser(creator), types.DefaultUserServer)
	if strings.Contains(creator, "@") {
		if j, err := types.ParseJID(creator); err == nil {
			creatorJID = j.ToNonAD()
		}
	}
	// The secret is stored under whichever identity sent the event; the
	// lookup returns it, and the RSVP is keyed the same way.
	secret, stored, err := client.Store.MsgSecrets.GetMessageSecret(ctx, chat, creatorJID, msgID)
	if err != nil {
		return nil, fmt.Errorf("look up event secret: %w", err)
	}
	if secret == nil {
		return nil, errEventNotFound
	}
	if !stored.IsEmpty() {
		creatorJID = stored.ToNonAD()
	}
	ownID := client.Store.ID.ToNonAD()
	if creatorJID.Server != types.DefaultUserServer && !client.Store.LID.IsEmpty() {
		ownID = client.Store.LID.ToNonAD()
	}
	fromMe := creatorJID.User == client.Store.ID.User || (!client.Store.LID.IsEmpty() && creatorJID.User == client.Store.LID.User)

	plaintext, err := proto.Marshal(resp)
	if err != nil {
		return nil, err
	}
	ciphertext, iv, err := client.DangerousInternals().EncryptMsgSecret(ctx, ownID, chat, creatorJID, msgID, whatsmeow.EncSecretEventResponse, plaintext)
	if err != nil {
		if errors.Is(err, whatsmeow.ErrOriginalMessageSecretNotFound) {
			return nil, errEventNotFound
		}
		return nil, fmt.Errorf("encrypt RSVP: %w", err)
	}
	key := &waCommon.MessageKey{RemoteJID: proto.String(chat.String()), FromMe: proto.Bool(fromMe), ID: proto.String(msgID)}
	if chat.Server == types.GroupServer && !fromMe {
		key.Participant = proto.String(creatorJID.String())
	}
	return &waE2E.Message{EncEventResponseMessage: &waE2E.EncEventResponseMessage{
		EventCreationMessageKey: key,
		EncPayload:              ciphertext,
		EncIV:                   iv,
	}}, nil
}

// respondEvent sends this line's RSVP to an event and records it.
func respondEvent(client *whatsmeow.Client, messageStore *MessageStore, req RespondEventRequest) (string, error) {
	answer, ok := eventResponseType(req.Response)
	if !ok {
		return "", errors.New(`response must be "going", "maybe" or "not_going"`)
	}
	if req.ExtraGuests < 0 || req.ExtraGuests > 50 {
		return "", errors.New("extra_guests must be between 0 and 50")
	}
	if !client.IsConnected() {
		return "", errors.New("not connected to WhatsApp")
	}
	chat, err := parseRecipient(req.Recipient)
	if err != nil {
		return "", err
	}
	chat = normalizeUserJID(client, chat)
	creator, _, known := messageStore.eventCreator(req.MessageID)
	if !known {
		return "", errEventNotFound
	}
	if ev, err := messageStore.LoadEventMessage(req.MessageID); err == nil && ev.GetIsCanceled() {
		return "", errors.New("the event was cancelled")
	}
	guests := req.ExtraGuests
	if answer != waE2E.EventResponseMessage_GOING {
		guests = 0
	}
	now := time.Now()
	resp := &waE2E.EventResponseMessage{
		Response:        answer.Enum(),
		TimestampMS:     proto.Int64(now.UnixMilli()),
		ExtraGuestCount: proto.Int32(int32(guests)),
	}
	ctx := context.Background()
	msg, err := buildEventResponseMessage(ctx, client, chat, creator, req.MessageID, resp)
	if err != nil {
		return "", err
	}
	sent, err := client.SendMessage(ctx, chat, msg, whatsmeow.SendRequestExtra{AdditionalNodes: eventResponseMeta()})
	if err != nil {
		return "", fmt.Errorf("send RSVP: %w", err)
	}
	presence.active(client)
	if err := messageStore.StoreEventResponse(chat.String(), req.MessageID, client.Store.ID.User, req.Response, guests, now); err != nil {
		slog.Warn("failed to store own RSVP", "error", err)
	}
	return sent.ID, nil
}

// handleRespondEvent serves POST /api/respond-event.
func handleRespondEvent(client *whatsmeow.Client, messageStore *MessageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req RespondEventRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		req.MessageID = strings.TrimSpace(req.MessageID)
		if req.Recipient == "" || req.MessageID == "" {
			respondError(w, http.StatusBadRequest, "recipient and message_id are required")
			return
		}
		id, err := respondEvent(client, messageStore, req)
		slog.Info("event rsvp sent", "event_id", req.MessageID, "response", req.Response, "success", err == nil, "error", err)
		if err != nil {
			status := http.StatusBadRequest
			switch {
			case errors.Is(err, errEventNotFound):
				status = http.StatusNotFound
			case strings.HasPrefix(err.Error(), "send RSVP"), strings.HasPrefix(err.Error(), "not connected"):
				status = http.StatusInternalServerError
			}
			respondError(w, status, err.Error())
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"success": true, "message_id": req.MessageID, "rsvp_id": id, "response": req.Response})
	}
}
