package main

// Native WhatsApp scheduled messages (EXPERIMENTAL).
//
// See docs/scheduled-messages-re.md. In short: the message goes to everyone
// now, sealed with AES-256-GCM under a random 32-byte reveal key, as a
// ConditionalRevealMessage. The key goes only to WhatsApp's server, in the
// stanza's <meta type="scheduled_message" st="<unix s>"><key rkid="…">key</key></meta>,
// and the server hands it to the recipients at that time. The bridge can be
// offline by then. The owner's phone gets the key too and lists the message
// as scheduled. A cancel is a ProtocolMessage MESSAGE_UNSCHEDULE naming it.
//
// Limits (WhatsApp's own AB-prop defaults): 10 minutes (from the start of the
// current minute) to 14 days ahead, 30 pending per chat. Text only here: a
// scheduled photo or video needs the stanza's type="media" and mediatype of
// the inner message, which stock whatsmeow cannot set.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
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
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

const (
	scheduledMetaType     = "scheduled_message"
	revealKeyLen          = 32
	revealIVLen           = 12
	scheduleMinAhead      = 10 * time.Minute
	scheduleMaxAhead      = 14 * 24 * time.Hour
	scheduleMaxPerChat    = 30
	scheduleSweepInterval = 30 * time.Second

	scheduledStatusPending   = "scheduled"
	scheduledStatusCancelled = "cancelled"
	scheduledStatusPosted    = "posted" // its time passed; WhatsApp's server delivered it (not confirmed)
)

// errScheduleWindow is returned for a time WhatsApp will not take.
var errScheduleWindow = errors.New("schedule_at must be from 10 minutes to 14 days from now")

// normalizeScheduleTime truncates at to the minute (WhatsApp schedules to the
// minute) and checks WhatsApp's window, as WA Web's isScheduledTimeValid
// does: at least the minimum past the start of the current minute, at most
// the maximum past now.
func normalizeScheduleTime(at, now time.Time) (time.Time, error) {
	at = at.Truncate(time.Minute)
	if at.Sub(now.Truncate(time.Minute)) < scheduleMinAhead || at.Sub(now) > scheduleMaxAhead {
		return at, errScheduleWindow
	}
	return at, nil
}

// sealedSchedule is a message sealed for scheduling, with what the server
// needs to reveal it.
type sealedSchedule struct {
	Outer     *waE2E.Message
	RevealKey []byte
	KeyID     string
}

// sealScheduledMessage seals inner under a fresh reveal key. inner gets a
// message secret when it has none (WA Web does the same); the outer message
// carries the same secret so whatsmeow stores it under the stanza's ID.
func sealScheduledMessage(inner *waE2E.Message) (*sealedSchedule, error) {
	key := make([]byte, revealKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate reveal key: %w", err)
	}
	keyID, err := newRevealKeyID()
	if err != nil {
		return nil, err
	}
	inner = proto.Clone(inner).(*waE2E.Message)
	if inner.MessageContextInfo == nil {
		inner.MessageContextInfo = &waE2E.MessageContextInfo{}
	}
	if len(inner.MessageContextInfo.MessageSecret) == 0 {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generate message secret: %w", err)
		}
		inner.MessageContextInfo.MessageSecret = secret
	}
	plain, err := proto.Marshal(inner)
	if err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}
	iv := make([]byte, revealIVLen)
	if _, err := rand.Read(iv); err != nil {
		return nil, fmt.Errorf("generate iv: %w", err)
	}
	enc, err := revealSeal(key, iv, plain)
	if err != nil {
		return nil, err
	}
	outer := &waE2E.Message{
		ConditionalRevealMessage: &waE2E.ConditionalRevealMessage{
			EncPayload:                   enc,
			EncIV:                        iv,
			ConditionalRevealMessageType: waE2E.ConditionalRevealMessage_SCHEDULED_MESSAGE.Enum(),
			RevealKeyID:                  proto.String(keyID),
		},
		MessageContextInfo: &waE2E.MessageContextInfo{
			MessageSecret: append([]byte(nil), inner.MessageContextInfo.MessageSecret...),
		},
	}
	return &sealedSchedule{Outer: outer, RevealKey: key, KeyID: keyID}, nil
}

// openScheduledMessage decrypts a ConditionalRevealMessage with its key.
func openScheduledMessage(cr *waE2E.ConditionalRevealMessage, key []byte) (*waE2E.Message, error) {
	if cr == nil {
		return nil, errors.New("no conditional reveal message")
	}
	plain, err := revealOpen(key, cr.GetEncIV(), cr.GetEncPayload())
	if err != nil {
		return nil, err
	}
	var inner waE2E.Message
	if err := proto.Unmarshal(plain, &inner); err != nil {
		return nil, fmt.Errorf("decode revealed message: %w", err)
	}
	return &inner, nil
}

// newRevealKeyID makes a reveal key ID the way WA Web does
// (crypto.randomUUID: a lower-case version 4 UUID).
func newRevealKeyID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate reveal key id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func revealGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != revealKeyLen {
		return nil, fmt.Errorf("reveal key must be %d bytes, got %d", revealKeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func revealSeal(key, iv, plain []byte) ([]byte, error) {
	g, err := revealGCM(key)
	if err != nil {
		return nil, err
	}
	if len(iv) != revealIVLen {
		return nil, fmt.Errorf("reveal iv must be %d bytes", revealIVLen)
	}
	return g.Seal(nil, iv, plain, nil), nil
}

func revealOpen(key, iv, enc []byte) ([]byte, error) {
	g, err := revealGCM(key)
	if err != nil {
		return nil, err
	}
	if len(iv) != revealIVLen {
		return nil, fmt.Errorf("reveal iv must be %d bytes", revealIVLen)
	}
	return g.Open(nil, iv, enc, nil)
}

// scheduleMetaNode hands the server a scheduled message's key and time.
func scheduleMetaNode(keyID string, key []byte, at time.Time) waBinary.Node {
	return waBinary.Node{
		Tag:   "meta",
		Attrs: waBinary.Attrs{"type": scheduledMetaType, "st": strconv.FormatInt(at.Unix(), 10)},
		Content: []waBinary.Node{{
			Tag:     "key",
			Attrs:   waBinary.Attrs{"rkid": keyID},
			Content: key,
		}},
	}
}

// unscheduleMetaNode names the key of a scheduled message being cancelled
// (no key bytes, no time), as WA Web's unschedule mixin does.
func unscheduleMetaNode(keyID string) waBinary.Node {
	return waBinary.Node{
		Tag:     "meta",
		Attrs:   waBinary.Attrs{"type": scheduledMetaType},
		Content: []waBinary.Node{{Tag: "key", Attrs: waBinary.Attrs{"rkid": keyID}}},
	}
}

// buildUnscheduleMessage takes back our scheduled message id in chat.
// participant is our own JID in groups and empty in DMs.
//
// WA Web sends it with edit="7" (sender revoke); stock whatsmeow sends no
// edit attribute for MESSAGE_UNSCHEDULE. Whether the server needs it is
// unverified (see the RE doc).
func buildUnscheduleMessage(chat types.JID, id string, participant types.JID) *waE2E.Message {
	key := &waCommon.MessageKey{
		RemoteJID: proto.String(chat.String()),
		FromMe:    proto.Bool(true),
		ID:        proto.String(id),
	}
	if chat.Server == types.GroupServer && !participant.IsEmpty() {
		key.Participant = proto.String(participant.ToNonAD().String())
	}
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Key:  key,
		Type: waE2E.ProtocolMessage_MESSAGE_UNSCHEDULE.Enum(),
	}}
}

// scheduleableChat reports whether WhatsApp takes scheduled messages in jid:
// people and groups, not channels, broadcasts, status or bots.
func scheduleableChat(jid types.JID) bool {
	switch jid.Server {
	case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
		return jid.User != "" && jid != types.StatusBroadcastJID
	}
	return false
}

// ---- store ----

// ScheduledMessage is one row of scheduled_messages.
type ScheduledMessage struct {
	ID          string    `json:"id"`
	ChatJID     string    `json:"chat_jid"`
	Content     string    `json:"content"`
	ScheduledAt time.Time `json:"scheduled_at"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	KeyID       string    `json:"-"`
	RevealKey   []byte    `json:"-"`
}

// ensureScheduledTable creates the scheduled_messages table. The reveal key
// is kept so a later version can show the message's content from the phone's
// copy or re-send it; it is useless without the ciphertext the recipients
// already hold.
func ensureScheduledTable(db *sql.DB, blobType string) error {
	_, err := db.Exec(fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS scheduled_messages (
			id TEXT NOT NULL,
			chat_jid TEXT NOT NULL,
			content TEXT NOT NULL,
			sender TEXT NOT NULL DEFAULT '',
			reveal_key_id TEXT NOT NULL,
			reveal_key %s,
			scheduled_at TIMESTAMP NOT NULL,
			status TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (id, chat_jid)
		);
		CREATE INDEX IF NOT EXISTS scheduled_messages_due_idx ON scheduled_messages (status, scheduled_at);
	`, blobType))
	return err
}

// The bridge binds times as time.Local wall clock (Postgres TIMESTAMP drops
// the offset; see ListMessages), and storedLocal reads them back the same way.

// storedLocal reinterprets a TIMESTAMP read back from the database as the
// local wall-clock time it was written as (lib/pq labels it UTC).
func storedLocal(t time.Time) time.Time {
	// lib/pq returns a zone-less TIMESTAMP in a zero-offset zone that is not
	// time.UTC itself, so test the offset, not the *Location.
	if _, off := t.Zone(); t.IsZero() || off != 0 {
		return t
	}
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
}

func (store *MessageStore) SaveScheduledMessage(m ScheduledMessage, sender string) error {
	now := time.Now().In(time.Local)
	_, err := store.db.Exec(pgPlaceholders(`INSERT INTO scheduled_messages
		(id, chat_jid, content, sender, reveal_key_id, reveal_key, scheduled_at, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		m.ID, m.ChatJID, m.Content, sender, m.KeyID, m.RevealKey, m.ScheduledAt.In(time.Local), scheduledStatusPending, now, now)
	return err
}

// CountPendingScheduled counts the chat's messages still waiting.
func (store *MessageStore) CountPendingScheduled(chatJID string) (int, error) {
	var n int
	err := store.db.QueryRow(pgPlaceholders(`SELECT COUNT(*) FROM scheduled_messages WHERE chat_jid = ? AND status = ?`),
		chatJID, scheduledStatusPending).Scan(&n)
	return n, err
}

// GetScheduledMessage returns one row, or sql.ErrNoRows.
func (store *MessageStore) GetScheduledMessage(chatJID, id string) (ScheduledMessage, error) {
	var m ScheduledMessage
	err := store.db.QueryRow(pgPlaceholders(`SELECT id, chat_jid, content, reveal_key_id, reveal_key, scheduled_at, status, created_at
		FROM scheduled_messages WHERE chat_jid = ? AND id = ?`), chatJID, id).
		Scan(&m.ID, &m.ChatJID, &m.Content, &m.KeyID, &m.RevealKey, &m.ScheduledAt, &m.Status, &m.CreatedAt)
	m.ScheduledAt, m.CreatedAt = storedLocal(m.ScheduledAt), storedLocal(m.CreatedAt)
	return m, err
}

// ListScheduledMessages lists rows, soonest first; chatJID and status filter
// when set.
func (store *MessageStore) ListScheduledMessages(chatJID, status string) ([]ScheduledMessage, error) {
	q := `SELECT id, chat_jid, content, scheduled_at, status, created_at FROM scheduled_messages WHERE 1 = 1`
	var args []any
	if chatJID != "" {
		q += ` AND chat_jid = ?`
		args = append(args, chatJID)
	}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	rows, err := store.db.Query(pgPlaceholders(q+` ORDER BY scheduled_at, created_at`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScheduledMessage{}
	for rows.Next() {
		var m ScheduledMessage
		if err := rows.Scan(&m.ID, &m.ChatJID, &m.Content, &m.ScheduledAt, &m.Status, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.ScheduledAt, m.CreatedAt = storedLocal(m.ScheduledAt), storedLocal(m.CreatedAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetScheduledStatus moves a row from one status to another; it reports
// whether the row was in from.
func (store *MessageStore) SetScheduledStatus(chatJID, id, from, to string) (bool, error) {
	return store.execChanged(pgPlaceholders(`UPDATE scheduled_messages SET status = ?, updated_at = ?
		WHERE chat_jid = ? AND id = ? AND status = ?`), to, time.Now().In(time.Local), chatJID, id, from)
}

// PostDueScheduled moves messages whose time came into the message history,
// timestamped at their scheduled time, and marks them posted. WhatsApp
// never echoes them back to the sending device, so this is how they reach
// list_messages. It returns how many it moved.
func (store *MessageStore) PostDueScheduled(now time.Time) (int, error) {
	rows, err := store.db.Query(pgPlaceholders(`SELECT id, chat_jid, content, sender, scheduled_at FROM scheduled_messages
		WHERE status = ? AND scheduled_at <= ?`), scheduledStatusPending, now.In(time.Local))
	if err != nil {
		return 0, err
	}
	type due struct {
		id, chat, content, sender string
		at                        time.Time
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.chat, &d.content, &d.sender, &d.at); err != nil {
			rows.Close()
			return 0, err
		}
		d.at = storedLocal(d.at)
		list = append(list, d)
	}
	rows.Close()
	moved := 0
	for _, d := range list {
		changed, err := store.SetScheduledStatus(d.chat, d.id, scheduledStatusPending, scheduledStatusPosted)
		if err != nil || !changed {
			continue
		}
		if err := store.StoreMessage(d.id, d.chat, d.sender, d.content, d.at, true,
			"", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
			slog.Warn("scheduled message: could not add to history", "id", d.id, "chat", d.chat, "error", err)
			continue
		}
		moved++
	}
	return moved, nil
}

// runScheduledSweeper posts due scheduled messages until ctx ends.
func runScheduledSweeper(ctx context.Context, store *MessageStore) {
	t := time.NewTicker(scheduleSweepInterval)
	defer t.Stop()
	for {
		if n, err := store.PostDueScheduled(time.Now()); err != nil {
			slog.Warn("scheduled message sweep failed", "error", err)
		} else if n > 0 {
			slog.Info("scheduled messages posted to history", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- send / cancel ----

// sendScheduled seals msg and sends it to be revealed at at. It returns the
// stanza ID.
func sendScheduled(client *whatsmeow.Client, to types.JID, msg *waE2E.Message, at time.Time) (string, *sealedSchedule, error) {
	sealed, err := sealScheduledMessage(msg)
	if err != nil {
		return "", nil, err
	}
	nodes := []waBinary.Node{scheduleMetaNode(sealed.KeyID, sealed.RevealKey, at)}
	resp, err := client.SendMessage(context.Background(), to, sealed.Outer, whatsmeow.SendRequestExtra{AdditionalNodes: &nodes})
	if err != nil {
		if strings.Contains(err.Error(), "419") {
			return "", nil, fmt.Errorf("WhatsApp holds at most %d scheduled messages per chat (error 419): %w", scheduleMaxPerChat, err)
		}
		return "", nil, err
	}
	return resp.ID, sealed, nil
}

// checkScheduledSend validates a scheduled send before anything is built.
func checkScheduledSend(store *MessageStore, to types.JID, at time.Time, hasMedia, viewOnce bool, sendAs string) (time.Time, error) {
	if hasMedia || viewOnce || (sendAs != "" && sendAs != "media") {
		return at, errors.New("schedule_at works for text messages only")
	}
	if !scheduleableChat(to) {
		return at, errors.New("only people and groups can get scheduled messages")
	}
	at, err := normalizeScheduleTime(at, time.Now())
	if err != nil {
		return at, err
	}
	if store != nil {
		n, err := store.CountPendingScheduled(to.String())
		if err != nil {
			return at, fmt.Errorf("count scheduled messages: %w", err)
		}
		if n >= scheduleMaxPerChat {
			return at, fmt.Errorf("this chat already has %d scheduled messages, WhatsApp's limit", n)
		}
	}
	return at, nil
}

type unscheduleRequest struct {
	Recipient string `json:"recipient"`
	MessageID string `json:"message_id"`
}

// registerScheduledRoutes adds /scheduled (list) and /unschedule (cancel),
// and starts the sweeper.
func registerScheduledRoutes(ctx context.Context, apiMux *http.ServeMux, client *whatsmeow.Client, store *MessageStore) {
	go runScheduledSweeper(ctx, store)

	apiMux.HandleFunc("/scheduled", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		chat := strings.TrimSpace(r.URL.Query().Get("chat_jid"))
		if chat != "" {
			jid, err := parseChatJID(chat)
			if err != nil {
				respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid chat_jid: %v", err))
				return
			}
			chat = normalizeUserJID(client, jid).String()
		}
		status := r.URL.Query().Get("status")
		if status == "" {
			status = scheduledStatusPending
		} else if status == "all" {
			status = ""
		}
		list, err := store.ListScheduledMessages(chat, status)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"success": true, "scheduled": list})
	})

	apiMux.HandleFunc("/unschedule", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req unscheduleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.Recipient == "" || req.MessageID == "" {
			respondError(w, http.StatusBadRequest, "recipient and message_id are required")
			return
		}
		chat, err := parseChatJID(req.Recipient)
		if err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid recipient: %v", err))
			return
		}
		chat = normalizeUserJID(client, chat)
		row, err := store.GetScheduledMessage(chat.String(), req.MessageID)
		if errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "no scheduled message with that ID in this chat")
			return
		} else if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if row.Status != scheduledStatusPending || !row.ScheduledAt.After(time.Now()) {
			respondError(w, http.StatusConflict, fmt.Sprintf("message is %s, not waiting to be sent", effectiveScheduledStatus(row, time.Now())))
			return
		}
		if !client.IsConnected() {
			respondError(w, http.StatusServiceUnavailable, "Not connected to WhatsApp")
			return
		}
		var me types.JID
		if client.Store.ID != nil {
			me = client.Store.ID.ToNonAD()
		}
		msg := buildUnscheduleMessage(chat, row.ID, me)
		nodes := []waBinary.Node{unscheduleMetaNode(row.KeyID)}
		if _, err := client.SendMessage(context.Background(), chat, msg, whatsmeow.SendRequestExtra{AdditionalNodes: &nodes}); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to cancel: %v", err))
			return
		}
		presence.active(client)
		if _, err := store.SetScheduledStatus(chat.String(), row.ID, scheduledStatusPending, scheduledStatusCancelled); err != nil {
			slog.Warn("cancelled scheduled message but could not mark it", "id", row.ID, "error", err)
		}
		store.markScheduledCancelled(chat.String(), row.ID, row.ScheduledAt)
		slog.Info("scheduled message cancelled", "id", row.ID, "chat", chat.String())
		respondJSON(w, http.StatusOK, map[string]any{"success": true})
	})
}

// parseChatJID takes a JID or, like /send, a bare phone number.
func parseChatJID(s string) (types.JID, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "@") {
		s = strings.TrimPrefix(s, "+")
		if s == "" || strings.Trim(s, "0123456789") != "" {
			return types.JID{}, fmt.Errorf("%q is neither a JID nor a phone number", s)
		}
		return types.NewJID(s, types.DefaultUserServer), nil
	}
	return types.ParseJID(s)
}

// effectiveScheduledStatus is a row's status, saying "posted" for a pending
// one whose time passed before the sweeper got to it.
func effectiveScheduledStatus(m ScheduledMessage, now time.Time) string {
	if m.Status == scheduledStatusPending && !m.ScheduledAt.After(now) {
		return scheduledStatusPosted
	}
	return m.Status
}

// sendScheduledText sends the built text message msg as a scheduled message
// and records it. It mirrors sendWhatsAppMessage's return values.
func sendScheduledText(client *whatsmeow.Client, store *MessageStore, recipient string, to types.JID, msg *waE2E.Message, content string, at time.Time) (bool, string, string) {
	id, sealed, err := sendScheduled(client, to, msg, at)
	if err != nil {
		return false, fmt.Sprintf("Error scheduling message: %v", err), ""
	}
	presence.active(client)
	if store != nil && client.Store.ID != nil {
		sender := client.Store.ID.ToNonAD().String()
		chatJID := to.String()
		// The chat row must exist before the message reaches history.
		if err := store.StoreChat(chatJID, GetChatName(client, store, to, chatJID, nil, sender, waLog.Noop), time.Now()); err != nil {
			slog.Warn("failed to store chat for scheduled message", "error", err)
		}
		if err := store.SaveScheduledMessage(ScheduledMessage{
			ID: id, ChatJID: chatJID, Content: content, ScheduledAt: at, KeyID: sealed.KeyID, RevealKey: sealed.RevealKey,
		}, sender); err != nil {
			slog.Warn("failed to store scheduled message", "id", id, "error", err)
		}
		// The chat's "You scheduled a message" notice; it becomes the message at its time.
		if err := store.StoreMessage(id, chatJID, sender, scheduledNotice(scheduledMarker, at, content), time.Now(), true,
			"", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
			slog.Warn("failed to store scheduled notice", "id", id, "error", err)
		}
	}
	slog.Info("message scheduled", "id", id, "chat", to.String(), "at", at.Format(time.RFC3339), "reveal_key_id", sealed.KeyID)
	return true, fmt.Sprintf("Message to %s scheduled for %s", recipient, at.In(displayLoc).Format("2006-01-02 15:04 MST")), id
}

// ---- chat notices ----
//
// A scheduled message has a row in messages from the moment it is scheduled,
// so a chat view can show WhatsApp's "You scheduled a message" notice:
//
//	[scheduled message]
//	Scheduled for: 2026-11-28T15:00:00+08:00
//
//	See you at 3
//
// stamped when it was scheduled. The text after the blank line is the
// message, for a chat list's "Scheduled: See you at 3" preview. At its time the row becomes the message
// itself (its text, stamped at the scheduled time); a cancel turns it into
//
//	[scheduled message cancelled]
//	Scheduled for: 2026-11-28T15:00:00+08:00
//
// The "Scheduled for" line is left out when the time is unknown, and the text
// when it is (both, for a message scheduled on the owner's phone: whatsmeow
// does not expose the stanza's st, and the bridge has no key to read it).
// Only the sender's devices get a notice; WhatsApp shows recipients nothing
// until the reveal.

const (
	scheduledMarker          = "[scheduled message]"
	scheduledCancelledMarker = "[scheduled message cancelled]"
)

// scheduledNotice is a notice row's content; at and text may be empty.
func scheduledNotice(marker string, at time.Time, text string) string {
	out := marker
	if !at.IsZero() {
		out += "\nScheduled for: " + at.In(time.Local).Format(time.RFC3339)
	}
	if text != "" {
		out += "\n\n" + text
	}
	return out
}

// parseScheduledNotice splits a notice row's content into its time and text
// (either may be empty).
func parseScheduledNotice(content string) (at time.Time, text string) {
	head, body, _ := strings.Cut(content, "\n\n")
	for _, line := range strings.Split(head, "\n") {
		if v, ok := strings.CutPrefix(line, "Scheduled for: "); ok {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				at = t
			}
		}
	}
	return at, body
}

// isScheduledNotice reports whether content is a scheduled-message notice
// (pending, not cancelled).
func isScheduledNotice(content string) bool {
	return content == scheduledMarker || strings.HasPrefix(content, scheduledMarker+"\n")
}

// setNoticeContent rewrites a notice row's text without marking it edited.
func (store *MessageStore) setNoticeContent(chatJID, id, content string) error {
	_, err := store.db.Exec(pgPlaceholders(`UPDATE messages SET content = ? WHERE chat_jid = ? AND id = ?`), content, chatJID, id)
	return err
}

// markScheduledCancelled turns a pending notice row into a cancelled one.
func (store *MessageStore) markScheduledCancelled(chatJID, id string, at time.Time) {
	var content string
	if err := store.db.QueryRow(pgPlaceholders(`SELECT content FROM messages WHERE chat_jid = ? AND id = ?`), chatJID, id).Scan(&content); err != nil {
		return
	}
	if !isScheduledNotice(content) {
		return
	}
	storedAt, text := parseScheduledNotice(content)
	if at.IsZero() {
		at = storedAt
	}
	if err := store.setNoticeContent(chatJID, id, scheduledNotice(scheduledCancelledMarker, at, text)); err != nil {
		slog.Warn("could not mark scheduled notice cancelled", "id", id, "error", err)
	}
}

// handleScheduledIncoming handles a ConditionalRevealMessage or a
// MESSAGE_UNSCHEDULE that came in, and reports whether it did:
//   - our own (scheduled on the owner's phone or another linked device):
//     a pending notice row, without a time;
//   - someone else's scheduled message: nothing (WhatsApp shows recipients
//     nothing until the reveal, which this bridge cannot see yet: the server
//     sends the key in a mex notification whatsmeow drops);
//   - a cancel of our own: the notice row becomes a cancelled one.
func handleScheduledIncoming(store *MessageStore, msg *events.Message, chatJID, sender string) bool {
	m := unwrapMessage(msg.Message)
	if cr := m.GetConditionalRevealMessage(); cr != nil {
		if msg.Info.IsFromMe && store != nil {
			if err := store.StoreMessage(msg.Info.ID, chatJID, sender, scheduledMarker, msg.Info.Timestamp, true,
				"", "", "", "", nil, nil, nil, 0, "", "", "", false); err != nil {
				slog.Warn("could not store scheduled notice", "id", msg.Info.ID, "error", err)
			}
		}
		slog.Info("scheduled message received", "id", msg.Info.ID, "chat", chatJID, "from_me", msg.Info.IsFromMe,
			"reveal_key_id", cr.GetRevealKeyID())
		return true
	}
	if pm := m.GetProtocolMessage(); pm != nil && pm.GetType() == waE2E.ProtocolMessage_MESSAGE_UNSCHEDULE {
		target := pm.GetKey().GetID()
		if msg.Info.IsFromMe && target != "" && store != nil {
			store.markScheduledCancelled(chatJID, target, time.Time{})
			if _, err := store.SetScheduledStatus(chatJID, target, scheduledStatusPending, scheduledStatusCancelled); err != nil {
				slog.Warn("could not mark scheduled message cancelled", "id", target, "error", err)
			}
		}
		slog.Info("scheduled message cancelled by sender", "id", target, "chat", chatJID, "from_me", msg.Info.IsFromMe)
		return true
	}
	return false
}
