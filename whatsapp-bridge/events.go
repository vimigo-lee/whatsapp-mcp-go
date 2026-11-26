package main

// WhatsApp events: the "Event" card a chat can hold (name, time, place, call
// link) and the RSVPs people answer it with.
//
// An event carries a random message secret. Each RSVP comes back as an
// EncEventResponseMessage encrypted with a key derived from that secret, the
// event's ID, its creator and the responder — the same scheme as poll votes
// (whatsmeow's "Event Response" use case). whatsmeow stores the secret of every
// event it sends or receives, and exposes the derivation through
// DangerousInternals().DecryptMsgSecret, so no whatsmeow patch is needed.
// Without the secret (an event from before this bridge was linked) an RSVP
// cannot be read; that is logged and skipped.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// SendEventRequest is the body of POST /api/send-event.
type SendEventRequest struct {
	Recipient   string `json:"recipient"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// StartTime / EndTime: RFC3339 ("2026-11-28T15:00:00+08:00"), a local
	// "2026-11-28 15:00" (read in BRIDGE_TZ), or unix seconds.
	StartTime          string `json:"start_time"`
	EndTime            string `json:"end_time,omitempty"`
	Location           string `json:"location,omitempty"`
	JoinLink           string `json:"join_link,omitempty"`
	ExtraGuestsAllowed bool   `json:"extra_guests_allowed,omitempty"`
	NoDelay            bool   `json:"no_delay,omitempty"`
}

// EventResponse is one person's RSVP to an event.
type EventResponse struct {
	Sender      string    `json:"sender"`
	Name        string    `json:"name"`
	Response    string    `json:"response"` // going, not_going, maybe
	ExtraGuests int       `json:"extra_guests,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	// Organizer marks the event's creator. WhatsApp counts the creator as
	// going without them sending an RSVP, so until they answer otherwise the
	// bridge lists them as going too, to match what the phones show.
	Organizer bool `json:"organizer,omitempty"`
}

// parseEventTime reads an event time: RFC3339, a wall-clock time in BRIDGE_TZ,
// or unix seconds.
func parseEventTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty time")
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(secs, 0), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, displayLoc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("time %q is not RFC3339, \"YYYY-MM-DD HH:MM\" or unix seconds", s)
}

// buildEventMessage builds the event card. The message secret is what RSVPs
// are encrypted against; SendMessage stores it for us on send.
func buildEventMessage(req SendEventRequest, start, end time.Time) (*waE2E.Message, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate event secret: %w", err)
	}
	ev := &waE2E.EventMessage{
		Name:               proto.String(req.Name),
		StartTime:          proto.Int64(start.Unix()),
		IsCanceled:         proto.Bool(false),
		ExtraGuestsAllowed: proto.Bool(req.ExtraGuestsAllowed),
	}
	if d := strings.TrimSpace(req.Description); d != "" {
		ev.Description = proto.String(d)
	}
	if !end.IsZero() {
		ev.EndTime = proto.Int64(end.Unix())
	}
	if l := strings.TrimSpace(req.Location); l != "" {
		ev.Location = &waE2E.LocationMessage{Name: proto.String(l)}
	}
	if j := strings.TrimSpace(req.JoinLink); j != "" {
		ev.JoinLink = proto.String(j)
	}
	return &waE2E.Message{
		EventMessage:       ev,
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: secret},
	}, nil
}

// eventCreationMeta is the <meta event_type="creation"/> node official clients
// attach to an event's stanza.
func eventCreationMeta() *[]waBinary.Node {
	return &[]waBinary.Node{{Tag: "meta", Attrs: waBinary.Attrs{"event_type": "creation"}}}
}

// eventContent renders an event as the text stored for it, so the MCP and the
// manager show what it is instead of dropping it. "" when m holds no event.
func eventContent(m *waE2E.Message) string {
	ev := m.GetEventMessage()
	if ev == nil {
		return ""
	}
	var sb strings.Builder
	if ev.GetIsCanceled() {
		sb.WriteString("[event cancelled] ")
	} else {
		sb.WriteString("[event] ")
	}
	name := strings.TrimSpace(ev.GetName())
	if name == "" {
		name = "(untitled)"
	}
	sb.WriteString(name)
	if st := ev.GetStartTime(); st > 0 {
		start := time.Unix(st, 0).In(displayLoc)
		when := start.Format("Mon 2 Jan 2006 15:04")
		if et := ev.GetEndTime(); et > st {
			end := time.Unix(et, 0).In(displayLoc)
			if end.YearDay() == start.YearDay() && end.Year() == start.Year() {
				when += " – " + end.Format("15:04")
			} else {
				when += " – " + end.Format("Mon 2 Jan 2006 15:04")
			}
		}
		sb.WriteString("\nWhen: " + when + " " + start.Format("MST"))
	}
	if loc := ev.GetLocation(); loc != nil {
		where := strings.TrimSpace(loc.GetName())
		if addr := strings.TrimSpace(loc.GetAddress()); addr != "" && addr != where {
			where = strings.TrimSpace(where + ", " + addr)
		}
		if where != "" {
			sb.WriteString("\nWhere: " + where)
		}
	}
	if link := strings.TrimSpace(ev.GetJoinLink()); link != "" {
		sb.WriteString("\nCall link: " + link)
	}
	if ev.GetExtraGuestsAllowed() {
		sb.WriteString("\nGuests may bring others")
	}
	if d := strings.TrimSpace(ev.GetDescription()); d != "" {
		sb.WriteString("\n" + d)
	}
	return sb.String()
}

func eventResponseName(t waE2E.EventResponseMessage_EventResponseType) string {
	switch t {
	case waE2E.EventResponseMessage_GOING:
		return "going"
	case waE2E.EventResponseMessage_NOT_GOING:
		return "not_going"
	case waE2E.EventResponseMessage_MAYBE:
		return "maybe"
	}
	return ""
}

// decryptEventResponse reads an RSVP. It needs the event's message secret,
// which whatsmeow keeps for events it sent or received.
func decryptEventResponse(ctx context.Context, client *whatsmeow.Client, evt *events.Message) (*waE2E.EventResponseMessage, error) {
	enc := unwrapMessage(evt.Message).GetEncEventResponseMessage()
	if enc == nil {
		return nil, errors.New("not an event response")
	}
	plaintext, err := client.DangerousInternals().DecryptMsgSecret(ctx, evt, whatsmeow.EncSecretEventResponse, enc, enc.GetEventCreationMessageKey())
	if err != nil {
		return nil, err
	}
	var resp waE2E.EventResponseMessage
	if err := proto.Unmarshal(plaintext, &resp); err != nil {
		return nil, fmt.Errorf("decode event response: %w", err)
	}
	return &resp, nil
}

// handleEventResponse stores an incoming RSVP against its event. It reports
// whether msg was an RSVP (handled, nothing more to store).
func handleEventResponse(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, chatJID, sender string, logger waLog.Logger) bool {
	enc := unwrapMessage(msg.Message).GetEncEventResponseMessage()
	if enc == nil {
		return false
	}
	eventID := enc.GetEventCreationMessageKey().GetID()
	resp, err := decryptEventResponse(context.Background(), client, msg)
	if err != nil {
		logger.Warnf("Failed to decrypt RSVP %s to event %s in %s: %v", msg.Info.ID, eventID, chatJID, err)
		return true
	}
	answer := eventResponseName(resp.GetResponse())
	if eventID == "" || answer == "" {
		return true
	}
	ts := msg.Info.Timestamp
	if ms := resp.GetTimestampMS(); ms > 0 {
		ts = time.UnixMilli(ms)
	}
	if err := messageStore.StoreEventResponse(chatJID, eventID, sender, answer, int(resp.GetExtraGuestCount()), ts); err != nil {
		logger.Warnf("Failed to store RSVP: %v", err)
		return true
	}
	slog.Info("event rsvp", "event_id", eventID, "chat_jid", chatJID, "sender", sender, "response", answer)
	return true
}

// StoreEventResponse upserts a person's RSVP. A later answer replaces an
// earlier one, the way the phone shows only someone's latest choice.
func (store *MessageStore) StoreEventResponse(chatJID, eventID, sender, response string, extraGuests int, ts time.Time) error {
	if isPostgres {
		_, err := store.db.Exec(
			`INSERT INTO event_responses (chat_jid, msg_id, sender, response, extra_guests, timestamp)
             VALUES ($1, $2, $3, $4, $5, $6)
             ON CONFLICT (msg_id, sender) DO UPDATE SET
                response = EXCLUDED.response,
                extra_guests = EXCLUDED.extra_guests,
                timestamp = EXCLUDED.timestamp
             WHERE event_responses.timestamp <= EXCLUDED.timestamp`,
			chatJID, eventID, sender, response, extraGuests, ts,
		)
		return err
	}
	_, err := store.db.Exec(
		`INSERT INTO event_responses (chat_jid, msg_id, sender, response, extra_guests, timestamp) VALUES (?, ?, ?, ?, ?, ?)
         ON CONFLICT(msg_id, sender) DO UPDATE SET
            response = excluded.response,
            extra_guests = excluded.extra_guests,
            timestamp = excluded.timestamp
         WHERE event_responses.timestamp <= excluded.timestamp`,
		chatJID, eventID, sender, response, extraGuests, ts,
	)
	return err
}

// GetEventResponses lists the RSVPs to an event, oldest first, with the
// event's creator first as going unless they answered otherwise (see
// EventResponse.Organizer).
func (store *MessageStore) GetEventResponses(eventID string) []EventResponse {
	rs := store.getStoredEventResponses(eventID)
	creator, createdAt, ok := store.eventCreator(eventID)
	if !ok {
		return rs
	}
	for i := range rs {
		if bareUser(rs[i].Sender) == creator {
			rs[i].Organizer = true
			return rs
		}
	}
	org := EventResponse{
		Sender:    creator,
		Name:      store.GetSenderName(creator),
		Response:  "going",
		Timestamp: createdAt.In(displayLoc),
		Organizer: true,
	}
	return append([]EventResponse{org}, rs...)
}

// eventCreator finds who sent an event (a bare number) and when, from the
// stored event message. ok is false when the event is not in the store.
func (store *MessageStore) eventCreator(eventID string) (creator string, at time.Time, ok bool) {
	q := "SELECT sender, timestamp FROM messages WHERE id = ? AND content LIKE '[event%' LIMIT 1"
	if isPostgres {
		q = "SELECT sender, timestamp FROM messages WHERE id = $1 AND content LIKE '[event%' LIMIT 1"
	}
	var sender string
	if err := store.db.QueryRow(q, eventID).Scan(&sender, &at); err != nil {
		return "", time.Time{}, false
	}
	if creator = bareUser(sender); creator == "" {
		return "", time.Time{}, false
	}
	return creator, at, true
}

// bareUser reduces "60123:5@s.whatsapp.net" or "60123" to "60123", the form
// RSVP senders are stored in.
func bareUser(jid string) string {
	if i := strings.IndexByte(jid, '@'); i >= 0 {
		jid = jid[:i]
	}
	if i := strings.IndexByte(jid, ':'); i >= 0 {
		jid = jid[:i]
	}
	return jid
}

func (store *MessageStore) getStoredEventResponses(eventID string) []EventResponse {
	q := "SELECT sender, response, extra_guests, timestamp FROM event_responses WHERE msg_id = ? ORDER BY timestamp"
	if isPostgres {
		q = "SELECT sender, response, extra_guests, timestamp FROM event_responses WHERE msg_id = $1 ORDER BY timestamp"
	}
	rows, err := store.db.Query(q, eventID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []EventResponse
	for rows.Next() {
		var r EventResponse
		if err := rows.Scan(&r.Sender, &r.Response, &r.ExtraGuests, &r.Timestamp); err != nil {
			continue
		}
		r.Name = store.GetSenderName(r.Sender)
		r.Timestamp = r.Timestamp.In(displayLoc)
		out = append(out, r)
	}
	return out
}

// formatEventResponses is the "⤷ RSVPs:" sub-line list_messages shows under an
// event, grouped by answer. "" when nobody has answered.
func formatEventResponses(rs []EventResponse) string {
	groups := map[string][]string{}
	for _, r := range rs {
		who := oneLine(r.Name)
		if r.ExtraGuests > 0 {
			who += fmt.Sprintf(" +%d", r.ExtraGuests)
		}
		groups[r.Response] = append(groups[r.Response], who)
	}
	var parts []string
	for _, k := range []struct{ key, label string }{{"going", "going"}, {"maybe", "maybe"}, {"not_going", "not going"}} {
		if g := groups[k.key]; len(g) > 0 {
			parts = append(parts, fmt.Sprintf("%s (%d): %s", k.label, len(g), strings.Join(g, ", ")))
		}
	}
	return strings.Join(parts, " · ")
}

// sendEventMessage sends an event card and stores it like a received one.
func sendEventMessage(client *whatsmeow.Client, messageStore *MessageStore, req SendEventRequest, start, end time.Time) (bool, string, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp", ""
	}
	var recipientJID types.JID
	if strings.Contains(req.Recipient, "@") {
		jid, err := types.ParseJID(req.Recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err), ""
		}
		recipientJID = jid
	} else {
		recipientJID = types.JID{User: req.Recipient, Server: types.DefaultUserServer}
	}
	recipientJID = normalizeUserJID(client, recipientJID)

	msg, err := buildEventMessage(req, start, end)
	if err != nil {
		return false, err.Error(), ""
	}
	if !req.NoDelay {
		holdWithTyping(client, recipientJID, humanSendDelay())
	}
	sendResp, err := client.SendMessage(context.Background(), recipientJID, msg, whatsmeow.SendRequestExtra{AdditionalNodes: eventCreationMeta()})
	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err), ""
	}
	presence.active(client)

	// WhatsApp never echoes our own send, so store it here.
	if messageStore != nil && client.Store.ID != nil {
		sender := client.Store.ID.ToNonAD().String()
		chatJID := recipientJID.String()
		sentAt := time.Now()
		if storeErr := messageStore.StoreChat(
			chatJID, GetChatName(client, messageStore, recipientJID, chatJID, nil, sender, waLog.Noop), sentAt,
		); storeErr != nil {
			slog.Warn("failed to store chat for sent event", "error", storeErr)
		}
		if storeErr := messageStore.StoreMessage(
			sendResp.ID, chatJID, sender, eventContent(msg), sentAt, true,
			"", "", "", "", nil, nil, nil, 0,
			"", "", "", false,
		); storeErr != nil {
			slog.Warn("failed to store sent event", "error", storeErr)
		}
	}
	return true, fmt.Sprintf("Event sent to %s", req.Recipient), sendResp.ID
}

// handleSendEvent serves POST /api/send-event.
func handleSendEvent(client *whatsmeow.Client, messageStore *MessageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req SendEventRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Recipient == "" || req.Name == "" || strings.TrimSpace(req.StartTime) == "" {
			http.Error(w, "recipient, name and start_time are required", http.StatusBadRequest)
			return
		}
		start, err := parseEventTime(req.StartTime)
		if err != nil {
			http.Error(w, "start_time: "+err.Error(), http.StatusBadRequest)
			return
		}
		var end time.Time
		if strings.TrimSpace(req.EndTime) != "" {
			if end, err = parseEventTime(req.EndTime); err != nil {
				http.Error(w, "end_time: "+err.Error(), http.StatusBadRequest)
				return
			}
			if !end.After(start) {
				http.Error(w, "end_time must be after start_time", http.StatusBadRequest)
				return
			}
		}

		success, message, sentID := sendEventMessage(client, messageStore, req, start, end)
		var reason string
		if !success {
			if r, friendly := classifySendError(message); r != "" {
				reason = r
				message = friendly
			}
		}
		slog.Info("event sent", "success", success, "message", message, "message_id", sentID, "reason", reason)
		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(SendMessageResponse{
			Success:   success,
			Message:   message,
			MessageID: sentID,
			Reason:    reason,
		})
	}
}

// handleEventResponses serves GET /api/event-responses?message_id=<event id>.
func handleEventResponses(messageStore *MessageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimSpace(r.URL.Query().Get("message_id"))
		if id == "" {
			http.Error(w, "message_id is required", http.StatusBadRequest)
			return
		}
		rs := messageStore.GetEventResponses(id)
		if rs == nil {
			rs = []EventResponse{}
		}
		counts := map[string]int{"going": 0, "maybe": 0, "not_going": 0}
		guests := 0
		for _, x := range rs {
			counts[x.Response]++
			if x.Response == "going" {
				guests += x.ExtraGuests
			}
		}
		respondJSON(w, http.StatusOK, map[string]any{
			"message_id":         id,
			"responses":          rs,
			"counts":             counts,
			"extra_guests_going": guests,
		})
	}
}
