package main

// Revealing scheduled messages this bridge did not send (EXPERIMENTAL).
//
// A scheduled message reaches every device at once as a sealed
// ConditionalRevealMessage (see scheduled.go). Its key turns up later, in
// one of two ways:
//
//   - someone else's message: WhatsApp's server pushes the key at the
//     scheduled time in a mex notification (NotificationScheduledMessageReveal:
//     chat_jid, rk, rkid, st, sender_user_jid);
//   - one the owner scheduled on the phone: the key and time ride in the
//     sealed message's own stanza (<meta type="scheduled_message" st><key>),
//     which only the sender's devices get. At the time the sender's devices
//     get NotificationScheduledMessagePost (rkid, status) instead of a key.
//
// Whichever half comes first (ciphertext or key) is kept in
// scheduled_incoming until the other arrives; then the message is decrypted
// and delivered through the normal path (handleMessage: history, webhooks,
// MCP), with its own ID, sender and the scheduled time, as WhatsApp shows it.
//
// The key, time and mex notifications come from patches/whatsmeow.patch
// (events.MexUpdate, MsgMetaInfo.Scheduled*). A stock whatsmeow build has no
// way to see them: scheduledMetaOf stays a no-op and
// registerScheduledReveal stays nil, and others' scheduled messages are
// kept but never revealed. scheduled_reveal_patched.go (built with
// -tags grouphistory, which every vimigo image uses) wires them up.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

const (
	incomingPending   = "pending"
	incomingRevealed  = "revealed"
	incomingCancelled = "cancelled"

	// A message the owner scheduled on the phone is revealed when WhatsApp
	// says it went (the post notification); this is the fallback when that
	// notification never comes.
	ownRevealGrace = 2 * time.Minute
	revealSweep    = 30 * time.Second
)

// scheduledMetaOf returns the reveal key and time a scheduled message's
// stanza carried (only on the sender's own devices). The patched build
// replaces it; stock whatsmeow exposes neither.
var scheduledMetaOf = func(*events.Message) (keyID string, key []byte, at time.Time) {
	return "", nil, time.Time{}
}

// registerScheduledReveal, when set (patched build), subscribes to the mex
// notifications and starts the reveal sweeper.
var registerScheduledReveal func(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger)

func ensureIncomingScheduledTable(db *sql.DB, blobType string) error {
	_, err := db.Exec(fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS scheduled_incoming (
			reveal_key_id TEXT PRIMARY KEY,
			msg_id TEXT NOT NULL DEFAULT '',
			chat_jid TEXT NOT NULL DEFAULT '',
			sender_jid TEXT NOT NULL DEFAULT '',
			push_name TEXT NOT NULL DEFAULT '',
			is_from_me BOOLEAN NOT NULL DEFAULT FALSE,
			is_group BOOLEAN NOT NULL DEFAULT FALSE,
			payload %[1]s,
			reveal_key %[1]s,
			scheduled_at TIMESTAMP,
			status TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL
		);
		CREATE INDEX IF NOT EXISTS scheduled_incoming_msg_idx ON scheduled_incoming (chat_jid, msg_id);
	`, blobType))
	return err
}

// incomingScheduled is one row of scheduled_incoming.
type incomingScheduled struct {
	KeyID       string
	MsgID       string
	ChatJID     string
	SenderJID   string
	PushName    string
	IsFromMe    bool
	IsGroup     bool
	Payload     []byte // the marshaled ConditionalRevealMessage
	RevealKey   []byte
	ScheduledAt time.Time
	Status      string
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.In(time.Local)
}

// saveIncomingCiphertext keeps a sealed message that came in, merging with a
// key that may already be here.
func (store *MessageStore) saveIncomingCiphertext(r incomingScheduled) error {
	now := time.Now().In(time.Local)
	q := `INSERT INTO scheduled_incoming (reveal_key_id, msg_id, chat_jid, sender_jid, push_name, is_from_me, is_group,
			payload, reveal_key, scheduled_at, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (reveal_key_id) DO UPDATE SET
			msg_id = excluded.msg_id, chat_jid = excluded.chat_jid, sender_jid = excluded.sender_jid,
			push_name = excluded.push_name, is_from_me = excluded.is_from_me, is_group = excluded.is_group,
			payload = excluded.payload,
			reveal_key = COALESCE(scheduled_incoming.reveal_key, excluded.reveal_key),
			scheduled_at = COALESCE(excluded.scheduled_at, scheduled_incoming.scheduled_at),
			updated_at = excluded.updated_at`
	_, err := store.db.Exec(pgPlaceholders(q), r.KeyID, r.MsgID, r.ChatJID, r.SenderJID, r.PushName, r.IsFromMe, r.IsGroup,
		r.Payload, nullBytes(r.RevealKey), nullTime(r.ScheduledAt), incomingPending, now, now)
	return err
}

// saveIncomingKey keeps a reveal key that came in, merging with a ciphertext
// that may already be here (or waiting for it).
func (store *MessageStore) saveIncomingKey(keyID string, key []byte, at time.Time) error {
	now := time.Now().In(time.Local)
	q := `INSERT INTO scheduled_incoming (reveal_key_id, reveal_key, scheduled_at, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (reveal_key_id) DO UPDATE SET
			reveal_key = excluded.reveal_key,
			scheduled_at = COALESCE(excluded.scheduled_at, scheduled_incoming.scheduled_at),
			updated_at = excluded.updated_at`
	_, err := store.db.Exec(pgPlaceholders(q), keyID, key, nullTime(at), incomingPending, now, now)
	return err
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func (store *MessageStore) getIncoming(keyID string) (incomingScheduled, error) {
	var r incomingScheduled
	var at sql.NullTime
	err := store.db.QueryRow(pgPlaceholders(`SELECT reveal_key_id, msg_id, chat_jid, sender_jid, push_name, is_from_me, is_group,
		payload, reveal_key, scheduled_at, status FROM scheduled_incoming WHERE reveal_key_id = ?`), keyID).
		Scan(&r.KeyID, &r.MsgID, &r.ChatJID, &r.SenderJID, &r.PushName, &r.IsFromMe, &r.IsGroup,
			&r.Payload, &r.RevealKey, &at, &r.Status)
	if at.Valid {
		r.ScheduledAt = storedLocal(at.Time)
	}
	return r, err
}

func (store *MessageStore) setIncomingStatus(keyID, from, to string) (bool, error) {
	return store.execChanged(pgPlaceholders(`UPDATE scheduled_incoming SET status = ?, updated_at = ?
		WHERE reveal_key_id = ? AND status = ?`), to, time.Now().In(time.Local), keyID, from)
}

// cancelIncomingByMsg marks a pending incoming scheduled message cancelled
// (its sender took it back), so it is never revealed.
func (store *MessageStore) cancelIncomingByMsg(chatJID, msgID string) {
	if _, err := store.db.Exec(pgPlaceholders(`UPDATE scheduled_incoming SET status = ?, updated_at = ?
		WHERE chat_jid = ? AND msg_id = ? AND status = ?`), incomingCancelled, time.Now().In(time.Local), chatJID, msgID, incomingPending); err != nil {
		slog.Warn("could not cancel incoming scheduled message", "id", msgID, "error", err)
	}
}

// dueOwnIncoming lists the owner's phone-scheduled messages whose time (plus
// grace) has passed and that are still waiting with their key here.
func (store *MessageStore) dueOwnIncoming(now time.Time) ([]string, error) {
	rows, err := store.db.Query(pgPlaceholders(`SELECT reveal_key_id FROM scheduled_incoming
		WHERE status = ? AND is_from_me = ? AND reveal_key IS NOT NULL AND payload IS NOT NULL
		AND scheduled_at IS NOT NULL AND scheduled_at <= ?`), incomingPending, true, now.Add(-ownRevealGrace).In(time.Local))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

var errRevealNotReady = errors.New("scheduled message not ready to reveal")

// revealedMessage decrypts a pending row whose ciphertext and key are both
// here into the message WhatsApp shows at the scheduled time.
func revealedMessage(r incomingScheduled, now time.Time) (*events.Message, error) {
	if r.Status != incomingPending || len(r.Payload) == 0 || len(r.RevealKey) == 0 || r.MsgID == "" {
		return nil, errRevealNotReady
	}
	var outer waE2E.Message
	if err := proto.Unmarshal(r.Payload, &outer); err != nil {
		return nil, fmt.Errorf("decode sealed message: %w", err)
	}
	inner, err := openScheduledMessage(outer.GetConditionalRevealMessage(), r.RevealKey)
	if err != nil {
		return nil, fmt.Errorf("open sealed message: %w", err)
	}
	// WA Web's reveal unwraps an associated child the same way.
	if child := inner.GetAssociatedChildMessage().GetMessage(); child != nil {
		mci := inner.GetMessageContextInfo()
		inner = child
		if inner.MessageContextInfo == nil {
			inner.MessageContextInfo = mci
		}
	}
	chat, err := types.ParseJID(r.ChatJID)
	if err != nil {
		return nil, fmt.Errorf("chat %q: %w", r.ChatJID, err)
	}
	sender, _ := types.ParseJID(r.SenderJID)
	ts := r.ScheduledAt
	if ts.IsZero() || ts.After(now) {
		ts = now
	}
	evt := &events.Message{Message: inner, RawMessage: inner}
	evt.Info.ID = r.MsgID
	evt.Info.Chat = chat
	evt.Info.Sender = sender
	evt.Info.IsFromMe = r.IsFromMe
	evt.Info.IsGroup = r.IsGroup
	evt.Info.PushName = r.PushName
	evt.Info.Timestamp = ts
	return evt, nil
}

// revealIncoming delivers the scheduled message with reveal key keyID if its
// ciphertext and key are both here; it reports whether it did.
func revealIncoming(client *whatsmeow.Client, store *MessageStore, keyID string, logger waLog.Logger) (delivered bool) {
	if client == nil { // nothing to deliver through (tests)
		return false
	}
	// Runs from the sweeper goroutine too: a panic here must not take the
	// bridge down with it.
	defer func() {
		if p := recover(); p != nil {
			slog.Error("scheduled message reveal panicked", "reveal_key_id", keyID, "panic", fmt.Sprint(p))
			delivered = false
		}
	}()
	r, err := store.getIncoming(keyID)
	if err != nil {
		return false
	}
	evt, err := revealedMessage(r, time.Now())
	if errors.Is(err, errRevealNotReady) {
		return false
	}
	// Claim it first, so a reveal and the sweeper never deliver it twice.
	if ok, _ := store.setIncomingStatus(keyID, incomingPending, incomingRevealed); !ok {
		return false
	}
	if err != nil {
		slog.Warn("scheduled message could not be revealed", "reveal_key_id", keyID, "id", r.MsgID, "error", err)
		return false
	}
	if evt.Info.IsGroup && !evt.Info.IsFromMe {
		groupSenders.remember(evt.Info.ID, evt.Info.Sender.ToNonAD())
	}
	handleMessage(client, store, evt, logger)
	if r.IsFromMe {
		// The phone-scheduled row in scheduled_messages: its message is in now.
		if _, err := store.SetScheduledStatus(r.ChatJID, r.MsgID, scheduledStatusPending, scheduledStatusPosted); err != nil {
			slog.Warn("could not mark scheduled message posted", "id", r.MsgID, "error", err)
		}
	}
	slog.Info("scheduled message revealed", "id", r.MsgID, "chat", r.ChatJID, "from_me", r.IsFromMe, "reveal_key_id", keyID)
	return true
}

// runIncomingRevealSweeper reveals the owner's phone-scheduled messages when
// their time has passed and the post notification did not do it.
func runIncomingRevealSweeper(ctx context.Context, client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	t := time.NewTicker(revealSweep)
	defer t.Stop()
	for {
		ids, err := store.dueOwnIncoming(time.Now())
		if err != nil {
			slog.Warn("scheduled reveal sweep failed", "error", err)
		}
		for _, id := range ids {
			revealIncoming(client, store, id, logger)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- mex notifications ----

// flexInt64 reads a JSON number or numeric string.
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*f = flexInt64(n)
	return nil
}

type scheduledReveal struct {
	ChatJID   string    `json:"chat_jid"`
	Key       string    `json:"rk"`
	KeyID     string    `json:"rkid"`
	Time      flexInt64 `json:"st"`
	SenderJID string    `json:"sender_user_jid"`
}

type scheduledPost struct {
	ChatJID   string          `json:"chat_jid"`
	KeyID     string          `json:"rkid"`
	Status    string          `json:"status"`
	ErrorCode json.RawMessage `json:"error_code"`
}

// parseScheduledMex reads a mex update's JSON; at most one result is set.
func parseScheduledMex(data []byte) (*scheduledReveal, *scheduledPost, error) {
	var m struct {
		Data struct {
			Reveal *scheduledReveal `json:"xwa2_notify_scheduled_message_reveal"`
			Post   *scheduledPost   `json:"xwa2_notify_scheduled_message_post"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil, err
	}
	return m.Data.Reveal, m.Data.Post, nil
}

// decodeRevealKey reads the url-safe base64 key of a reveal notification.
func decodeRevealKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == revealKeyLen {
			return b, nil
		}
	}
	return nil, fmt.Errorf("reveal key is not %d bytes of base64", revealKeyLen)
}

// handleScheduledMex handles one mex update about a scheduled message and
// reports whether it was one.
func handleScheduledMex(client *whatsmeow.Client, store *MessageStore, opName string, data []byte, logger waLog.Logger) bool {
	if !strings.Contains(opName, "ScheduledMessage") && !strings.Contains(string(data), "scheduled_message") {
		return false
	}
	reveal, post, err := parseScheduledMex(data)
	if err != nil {
		slog.Warn("scheduled message notification unreadable", "op", opName, "error", err)
		return true
	}
	switch {
	case reveal != nil && reveal.KeyID != "":
		key, err := decodeRevealKey(reveal.Key)
		if err != nil {
			slog.Warn("scheduled message reveal key unreadable", "reveal_key_id", reveal.KeyID, "error", err)
			return true
		}
		var at time.Time
		if reveal.Time > 0 {
			at = time.Unix(int64(reveal.Time), 0)
		}
		if err := store.saveIncomingKey(reveal.KeyID, key, at); err != nil {
			slog.Warn("could not keep scheduled message key", "reveal_key_id", reveal.KeyID, "error", err)
			return true
		}
		slog.Info("scheduled message key received", "reveal_key_id", reveal.KeyID, "chat", reveal.ChatJID, "sender", reveal.SenderJID)
		revealIncoming(client, store, reveal.KeyID, logger)
	case post != nil && post.KeyID != "":
		handleScheduledPost(client, store, post, logger)
	default:
		slog.Info("scheduled message notification", "op", opName, "data", string(data))
	}
	return true
}

// handleScheduledPost applies WhatsApp's word on one of our scheduled
// messages at its time: SUCCESS, or a failure.
func handleScheduledPost(client *whatsmeow.Client, store *MessageStore, post *scheduledPost, logger waLog.Logger) {
	chat, id, known := store.scheduledByKeyID(post.KeyID)
	slog.Info("scheduled message post", "reveal_key_id", post.KeyID, "status", post.Status, "error_code", string(post.ErrorCode), "id", id)
	if strings.EqualFold(post.Status, "SUCCESS") {
		// Phone-scheduled: its message comes in now. (One sent from here is
		// already in history from the sweeper.)
		revealIncoming(client, store, post.KeyID, logger)
		return
	}
	if !known {
		return
	}
	reason := "WhatsApp could not send the scheduled message"
	if len(post.ErrorCode) > 0 && string(post.ErrorCode) != "null" {
		reason += " (" + strings.Trim(string(post.ErrorCode), `"`) + ")"
	}
	store.markScheduledFailed(chat, id, reason)
	if _, err := store.setIncomingStatus(post.KeyID, incomingPending, incomingCancelled); err != nil {
		slog.Warn("could not drop failed scheduled message", "reveal_key_id", post.KeyID, "error", err)
	}
}

// scheduledByKeyID finds our scheduled message (sent from here or from the
// phone) by its reveal key ID.
func (store *MessageStore) scheduledByKeyID(keyID string) (chat, id string, ok bool) {
	err := store.db.QueryRow(pgPlaceholders(`SELECT chat_jid, id FROM scheduled_messages WHERE reveal_key_id = ?`), keyID).Scan(&chat, &id)
	return chat, id, err == nil
}

// markScheduledFailed records that WhatsApp did not deliver one of our
// scheduled messages: the row's status, and the message row's failure.
func (store *MessageStore) markScheduledFailed(chatJID, id, reason string) {
	now := time.Now().In(time.Local)
	if _, err := store.db.Exec(pgPlaceholders(`UPDATE scheduled_messages SET status = ?, updated_at = ? WHERE chat_jid = ? AND id = ?`),
		scheduledStatusFailed, now, chatJID, id); err != nil {
		slog.Warn("could not mark scheduled message failed", "id", id, "error", err)
	}
	if _, err := store.db.Exec(pgPlaceholders(`UPDATE messages SET failed_at = ?, failure_reason = ? WHERE chat_jid = ? AND id = ?`),
		now, reason, chatJID, id); err != nil {
		slog.Warn("could not mark scheduled message row failed", "id", id, "error", err)
	}
}
