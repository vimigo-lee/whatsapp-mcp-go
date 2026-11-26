package main

// Editing and cancelling an event the line sent.
//
// WhatsApp sends an event edit as a SecretEncryptedMessage (secret type
// EVENT_EDIT) aimed at the original event. Its plaintext is a BuildEdit-shaped
// message (EditedMessage → ProtocolMessage MESSAGE_EDIT) whose EditedMessage is
// the complete new EventMessage; a cancellation is the same edit with
// is_canceled set. The key is derived from the event's message secret (use
// case "Event Edit"), so only events this bridge sent or received can be
// edited.
//
// An edit replaces the whole event, so the bridge keeps each event's last
// known EventMessage (event_messages) and an edit request only has to name
// the fields it changes.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// EditEventRequest is the body of POST /api/edit-event. Fields left out keep
// their current value; an empty end_time removes the end. /api/cancel-event
// needs only recipient and message_id. For an event the bridge holds no
// details of (sent or received before event_messages existed), the request
// must carry the whole event: at least name and start_time.
type EditEventRequest struct {
	Recipient          string  `json:"recipient"`
	MessageID          string  `json:"message_id"`
	Name               *string `json:"name,omitempty"`
	Description        *string `json:"description,omitempty"`
	StartTime          *string `json:"start_time,omitempty"`
	EndTime            *string `json:"end_time,omitempty"`
	Location           *string `json:"location,omitempty"`
	JoinLink           *string `json:"join_link,omitempty"`
	ExtraGuestsAllowed *bool   `json:"extra_guests_allowed,omitempty"`
	Canceled           bool    `json:"canceled,omitempty"`
}

var errEventNotFound = errors.New("event not found: only events this line sent or received after the bridge was linked can be edited")

// SaveEventMessage records an event's current details, replacing older ones.
func (store *MessageStore) SaveEventMessage(chatJID, msgID string, ev *waE2E.EventMessage) error {
	if store == nil || msgID == "" || ev == nil {
		return nil
	}
	raw, err := proto.Marshal(ev)
	if err != nil {
		return err
	}
	data := base64.StdEncoding.EncodeToString(raw)
	q := `INSERT INTO event_messages (msg_id, chat_jid, data) VALUES (?, ?, ?)
		ON CONFLICT(msg_id) DO UPDATE SET chat_jid = excluded.chat_jid, data = excluded.data`
	if isPostgres {
		q = `INSERT INTO event_messages (msg_id, chat_jid, data) VALUES ($1, $2, $3)
		ON CONFLICT (msg_id) DO UPDATE SET chat_jid = EXCLUDED.chat_jid, data = EXCLUDED.data`
	}
	_, err = store.db.Exec(q, msgID, chatJID, data)
	return err
}

// SaveEventInvite records a phone-sent event invite: its details as an
// EventMessage, and its cover thumbnail.
func (store *MessageStore) SaveEventInvite(chatJID, msgID string, inv *waE2E.EventInviteMessage) error {
	if store == nil || msgID == "" || inv == nil {
		return nil
	}
	raw, err := proto.Marshal(inviteToEventMessage(inv))
	if err != nil {
		return err
	}
	data := base64.StdEncoding.EncodeToString(raw)
	var cover any
	if t := inv.GetJPEGThumbnail(); len(t) > 0 {
		cover = base64.StdEncoding.EncodeToString(t)
	}
	q := `INSERT INTO event_messages (msg_id, chat_jid, data, invite, cover) VALUES (?, ?, ?, TRUE, ?)
		ON CONFLICT(msg_id) DO UPDATE SET chat_jid = excluded.chat_jid, data = excluded.data, invite = TRUE, cover = COALESCE(excluded.cover, event_messages.cover)`
	if isPostgres {
		q = `INSERT INTO event_messages (msg_id, chat_jid, data, invite, cover) VALUES ($1, $2, $3, TRUE, $4)
		ON CONFLICT (msg_id) DO UPDATE SET chat_jid = EXCLUDED.chat_jid, data = EXCLUDED.data, invite = TRUE, cover = COALESCE(EXCLUDED.cover, event_messages.cover)`
	}
	_, err = store.db.Exec(q, msgID, chatJID, data, cover)
	return err
}

// EventInviteInfo reports whether an event is a phone-sent invite, and its
// cover thumbnail (base64 JPEG, "" when none).
func (store *MessageStore) EventInviteInfo(msgID string) (invite bool, cover string) {
	q := "SELECT invite, COALESCE(cover, '') FROM event_messages WHERE msg_id = ?"
	if isPostgres {
		q = "SELECT invite, COALESCE(cover, '') FROM event_messages WHERE msg_id = $1"
	}
	if err := store.db.QueryRow(q, msgID).Scan(&invite, &cover); err != nil {
		return false, ""
	}
	return invite, cover
}

// LoadEventMessage returns an event's last known details.
func (store *MessageStore) LoadEventMessage(msgID string) (*waE2E.EventMessage, error) {
	q := "SELECT data FROM event_messages WHERE msg_id = ?"
	if isPostgres {
		q = "SELECT data FROM event_messages WHERE msg_id = $1"
	}
	var data string
	if err := store.db.QueryRow(q, msgID).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errEventNotFound
		}
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, err
	}
	var ev waE2E.EventMessage
	if err := proto.Unmarshal(raw, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// noteEventMessage keeps the details of an incoming event, or of an incoming
// edit of one, so a later edit from this line starts from them. msg must
// already be decrypted.
func noteEventMessage(messageStore *MessageStore, msg *events.Message, chatJID string) {
	if messageStore == nil || msg == nil || msg.Message == nil {
		return
	}
	m := unwrapMessage(msg.Message)
	if inv := m.GetEventInviteMessage(); inv != nil {
		if err := messageStore.SaveEventInvite(chatJID, msg.Info.ID, inv); err != nil {
			slog.Warn("failed to save event invite", "id", msg.Info.ID, "error", err)
		}
		return
	}
	if ev := m.GetEventMessage(); ev != nil {
		if err := messageStore.SaveEventMessage(chatJID, msg.Info.ID, ev); err != nil {
			slog.Warn("failed to save event details", "id", msg.Info.ID, "error", err)
		}
		return
	}
	pm := m.GetProtocolMessage()
	if pm == nil || pm.GetType() != waE2E.ProtocolMessage_MESSAGE_EDIT {
		return
	}
	if ev := unwrapMessage(pm.GetEditedMessage()).GetEventMessage(); ev != nil {
		if err := messageStore.SaveEventMessage(chatJID, pm.GetKey().GetID(), ev); err != nil {
			slog.Warn("failed to save edited event details", "id", pm.GetKey().GetID(), "error", err)
		}
	}
}

// applyEventEdit returns a copy of base with the request's changes.
func applyEventEdit(base *waE2E.EventMessage, req EditEventRequest) (*waE2E.EventMessage, error) {
	ev := proto.Clone(base).(*waE2E.EventMessage)
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, errors.New("name cannot be empty")
		}
		ev.Name = proto.String(name)
	}
	if req.Description != nil {
		if d := strings.TrimSpace(*req.Description); d != "" {
			ev.Description = proto.String(d)
		} else {
			ev.Description = nil
		}
	}
	if req.StartTime != nil {
		start, err := parseEventTime(*req.StartTime)
		if err != nil {
			return nil, fmt.Errorf("start_time: %w", err)
		}
		ev.StartTime = proto.Int64(start.Unix())
	}
	if req.EndTime != nil {
		if strings.TrimSpace(*req.EndTime) == "" {
			ev.EndTime = nil
		} else {
			end, err := parseEventTime(*req.EndTime)
			if err != nil {
				return nil, fmt.Errorf("end_time: %w", err)
			}
			ev.EndTime = proto.Int64(end.Unix())
		}
	}
	if ev.GetEndTime() > 0 && ev.GetEndTime() <= ev.GetStartTime() {
		return nil, errors.New("end_time must be after start_time")
	}
	if req.Location != nil {
		if l := strings.TrimSpace(*req.Location); l != "" {
			ev.Location = &waE2E.LocationMessage{Name: proto.String(l)}
		} else {
			ev.Location = nil
		}
	}
	if req.JoinLink != nil {
		if j := strings.TrimSpace(*req.JoinLink); j != "" {
			ev.JoinLink = proto.String(j)
		} else {
			ev.JoinLink = nil
		}
	}
	if req.ExtraGuestsAllowed != nil {
		ev.ExtraGuestsAllowed = proto.Bool(*req.ExtraGuestsAllowed)
	}
	if req.Canceled {
		ev.IsCanceled = proto.Bool(true)
	}
	return ev, nil
}

// eventEditMeta is the <meta event_type="edit"/> node official clients attach
// to an event edit's stanza (the creation carries event_type="creation").
func eventEditMeta() *[]waBinary.Node {
	return &[]waBinary.Node{{Tag: "meta", Attrs: waBinary.Attrs{"event_type": "edit"}}}
}

// buildEventEditMessage encrypts the new event against the original event's
// message secret.
func buildEventEditMessage(ctx context.Context, client *whatsmeow.Client, chat types.JID, msgID string, ev *waE2E.EventMessage) (*waE2E.Message, error) {
	if client.Store.ID == nil {
		return nil, errors.New("not logged in")
	}
	ownPN := client.Store.ID.ToNonAD()
	// The secret was stored under whichever identity sent the event (phone
	// number or LID); the edit must be keyed the same way.
	_, storedSender, err := client.Store.MsgSecrets.GetMessageSecret(ctx, chat, ownPN, msgID)
	if err != nil {
		return nil, fmt.Errorf("look up event secret: %w", err)
	}
	ownID := ownPN
	if storedSender.Server == types.HiddenUserServer && !client.Store.LID.IsEmpty() {
		ownID = client.Store.LID.ToNonAD()
	}
	inner := client.BuildEdit(chat, msgID, &waE2E.Message{EventMessage: ev})
	plaintext, err := proto.Marshal(inner)
	if err != nil {
		return nil, err
	}
	ciphertext, iv, err := client.DangerousInternals().EncryptMsgSecret(ctx, ownID, chat, ownID, msgID, whatsmeow.EncSecretEventEdit, plaintext)
	if err != nil {
		if errors.Is(err, whatsmeow.ErrOriginalMessageSecretNotFound) {
			return nil, errEventNotFound
		}
		return nil, fmt.Errorf("encrypt event edit: %w", err)
	}
	return &waE2E.Message{SecretEncryptedMessage: &waE2E.SecretEncryptedMessage{
		TargetMessageKey: &waCommon.MessageKey{
			RemoteJID: proto.String(chat.String()),
			FromMe:    proto.Bool(true),
			ID:        proto.String(msgID),
		},
		EncPayload:    ciphertext,
		EncIV:         iv,
		SecretEncType: waE2E.SecretEncryptedMessage_EVENT_EDIT.Enum(),
	}}, nil
}

// editEvent sends an edit (or cancellation) of an event this line sent and
// updates the stored card. It returns the edit message's ID.
func editEvent(client *whatsmeow.Client, messageStore *MessageStore, req EditEventRequest) (string, error) {
	if !client.IsConnected() {
		return "", errors.New("not connected to WhatsApp")
	}
	chat, err := parseRecipient(req.Recipient)
	if err != nil {
		return "", err
	}
	chat = normalizeUserJID(client, chat)
	if invite, _ := messageStore.EventInviteInfo(req.MessageID); invite {
		return "", errors.New("this event was created on the phone as an invite; edit or cancel it on the phone")
	}
	base, err := messageStore.LoadEventMessage(req.MessageID)
	if errors.Is(err, errEventNotFound) && req.Name != nil && req.StartTime != nil {
		// Unknown event, but the caller sent all of it.
		base, err = &waE2E.EventMessage{IsCanceled: proto.Bool(false)}, nil
	}
	if err != nil {
		return "", err
	}
	if base.GetIsCanceled() {
		return "", errors.New("the event is already cancelled")
	}
	ev, err := applyEventEdit(base, req)
	if err != nil {
		return "", err
	}
	ctx := context.Background()
	msg, err := buildEventEditMessage(ctx, client, chat, req.MessageID, ev)
	if err != nil {
		return "", err
	}
	resp, err := client.SendMessage(ctx, chat, msg, whatsmeow.SendRequestExtra{AdditionalNodes: eventEditMeta()})
	if err != nil {
		return "", fmt.Errorf("send event edit: %w", err)
	}
	presence.active(client)
	if err := messageStore.SaveEventMessage(chat.String(), req.MessageID, ev); err != nil {
		slog.Warn("failed to save edited event details", "error", err)
	}
	if err := messageStore.UpdateMessageContent(chat.String(), req.MessageID, eventContent(&waE2E.Message{EventMessage: ev})+" (edited)"); err != nil {
		slog.Warn("failed to update edited event in store", "error", err)
	}
	return resp.ID, nil
}

func parseRecipient(recipient string) (types.JID, error) {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return types.EmptyJID, errors.New("recipient is required")
	}
	if strings.Contains(recipient, "@") {
		jid, err := types.ParseJID(recipient)
		if err != nil {
			return types.EmptyJID, fmt.Errorf("invalid recipient: %w", err)
		}
		return jid, nil
	}
	return types.JID{User: recipient, Server: types.DefaultUserServer}, nil
}

// handleEditEvent serves POST /api/edit-event, and with cancel set,
// POST /api/cancel-event.
func handleEditEvent(client *whatsmeow.Client, messageStore *MessageStore, cancel bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req EditEventRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		req.MessageID = strings.TrimSpace(req.MessageID)
		if req.Recipient == "" || req.MessageID == "" {
			respondError(w, http.StatusBadRequest, "recipient and message_id are required")
			return
		}
		if cancel {
			req.Canceled = true
		}
		id, err := editEvent(client, messageStore, req)
		action := "event edited"
		if cancel {
			action = "event cancelled"
		}
		slog.Info(action, "event_id", req.MessageID, "success", err == nil, "error", err)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errEventNotFound) {
				status = http.StatusNotFound
			} else if !strings.HasPrefix(err.Error(), "send event edit") && !strings.HasPrefix(err.Error(), "not connected") {
				status = http.StatusBadRequest
			}
			respondError(w, status, err.Error())
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"success": true, "message_id": req.MessageID, "edit_id": id, "cancelled": cancel})
	}
}
