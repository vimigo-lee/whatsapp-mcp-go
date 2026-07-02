package main

import (
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // embed the IANA zoneinfo DB so BRIDGE_TZ works on bare alpine
	"whatsapp-bridge/auth"
	"whatsapp-bridge/config"
	bridgelogger "whatsapp-bridge/logger"
	"whatsapp-bridge/wastate"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/socket"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	qrcode "github.com/skip2/go-qrcode"

	"bytes"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type MessageInteraction struct {
	Timestamp time.Time `json:"timestamp"`
	Sender    string    `json:"sender"`
	Content   string    `json:"content"`
	IsFromMe  bool      `json:"is_from_me"`
	ChatJID   string    `json:"chat_jid"`
	ID        string    `json:"id"`
	ChatName  string    `json:"chat_name,omitempty"`
	MediaType string    `json:"media_type,omitempty"`
}

type Chat struct {
	JID             string    `json:"jid"`
	Name            string    `json:"name,omitempty"`
	LastMessageTime time.Time `json:"last_message_time,omitempty"`
	LastMessage     string    `json:"last_message,omitempty"`
	LastSender      string    `json:"last_sender,omitempty"`
	LastIsFromMe    bool      `json:"last_is_from_me,omitempty"`
}

func (c *Chat) IsGroup() bool {
	return strings.HasSuffix(c.JID, "@g.us")
}

type Contact struct {
	PhoneNumber string `json:"phone_number"`
	Name        string `json:"name,omitempty"`
	JID         string `json:"jid"`
}

type MessageContext struct {
	Message MessageInteraction   `json:"message"`
	Before  []MessageInteraction `json:"before"`
	After   []MessageInteraction `json:"after"`
}

type ListMessagesParams struct {
	After, Before     string
	SenderPhoneNumber *string
	ChatJid           *string
	Query             *string
	Limit, Page       int
	IncludeContext    bool
	ContextBefore     int
	ContextAfter      int
}

type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

type MessageStore struct {
	db *sql.DB
}

var isPostgres = false

// passkeyState holds the in-flight WhatsApp "Shortcake" passkey linking handshake
// for this bridge's single client. The challenge arrives via events.PairPasskeyRequest,
// is relayed to a browser (which runs navigator.credentials.get on a whatsapp.com
// origin), and the resulting assertion is posted back to /auth/passkey-response.
var passkeyState = struct {
	mu      sync.Mutex
	request *types.WebAuthnPublicKey
	code    string
	skipUX  bool
	errMsg  string
}{}

// displayLoc is the timezone used when rendering timestamps in API responses
// (chat last-message times, message timestamps). DB rows are stored UTC-labeled,
// so MCP/HTTP clients would otherwise see UTC; we convert to this zone so the
// wall-clock and the RFC3339 offset reflect local time. Override with BRIDGE_TZ
// (IANA name, e.g. "Asia/Singapore"); defaults to Asia/Kuala_Lumpur. Falls back
// to UTC if the name can't be loaded. The zoneinfo DB is embedded via the
// time/tzdata import, so this works without OS tzdata on alpine.
var displayLoc = func() *time.Location {
	name := os.Getenv("BRIDGE_TZ")
	if name == "" {
		name = "Asia/Kuala_Lumpur"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		slog.Warn("invalid BRIDGE_TZ, falling back to UTC", "tz", name, "err", err)
		return time.UTC
	}
	return loc
}()

func openDatabase(dbName string) (*sql.DB, error) {
	if val, ok := os.LookupEnv("IS_POSTGRES"); ok && strings.ToLower(val) == "true" {
		cfg, err := config.LoadConfig()
		if err != nil {
			return nil, fmt.Errorf("missing environment variable")
		}
		isPostgres = cfg.DB.IsPostgres

		connStr := fmt.Sprintf("postgresql://%s:%s@%s:%s/%s?sslmode=disable",
			cfg.DB.User, cfg.DB.Pass, cfg.DB.Host, cfg.DB.Port, dbName)
		log.Println("Connecting to postgres")
		return sql.Open("postgres", connStr)
	}

	// Fallback to SQLite
	log.Println("Connecting to sqlite3")
	return sql.Open("sqlite3", "file:store/messages.db?_foreign_keys=on")
}

// validateMediaPath sanitize media path
func validateMediaPath(mediaPath string) (string, error) {
	if mediaPath == "" {
		return "", fmt.Errorf("empty media path")
	}

	// Allowed media directory
	baseDir := "./media"

	absBaseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}

	// Reject absolute paths
	if filepath.IsAbs(mediaPath) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}

	// Clean traversal sequences
	cleanPath := filepath.Clean(mediaPath)

	fullPath := filepath.Join(absBaseDir, cleanPath)

	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return "", err
	}

	// Ensure resolved path stays inside media directory
	if !strings.HasPrefix(absPath, absBaseDir+string(os.PathSeparator)) &&
		absPath != absBaseDir {
		return "", fmt.Errorf("path traversal detected")
	}

	return absPath, nil
}

// NewMessageStore Initialize message store
func NewMessageStore() (*MessageStore, error) {
	if err := os.MkdirAll("store", 0755); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	db, err := openDatabase("whatsapp")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	val, ok := os.LookupEnv("IS_POSTGRES")

	var blobType string
	if ok && strings.ToLower(val) == "true" {
		blobType = "BYTEA"
	} else {
		blobType = "BLOB"
	}

	_, err = db.Exec(fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP,
			unread_count INTEGER NOT NULL DEFAULT 0
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key %s,
			file_sha256 %s,
			file_enc_sha256 %s,
			file_length INTEGER,
			quoted_message_id TEXT,
			quoted_sender TEXT,
			quoted_content TEXT,
			is_forwarded BOOLEAN NOT NULL DEFAULT FALSE,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);

		-- Reactions attach to a target message (one per sender per message; an
		-- empty reaction text means the sender removed theirs). Kept in the
		-- bridge's own store so the MCP /messages output can show them — the
		-- manager's Postgres reactions table is a separate store the MCP can't read.
		CREATE TABLE IF NOT EXISTS reactions (
			chat_jid TEXT,
			msg_id TEXT,
			sender TEXT,
			emoji TEXT,
			timestamp TIMESTAMP,
			PRIMARY KEY (msg_id, sender)
		);
	`, blobType, blobType, blobType))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	// Migrate pre-existing stores whose chats table predates unread_count.
	// CREATE TABLE IF NOT EXISTS above is a no-op on an existing table, so add
	// the column explicitly; ignore the error when it already exists (SQLite
	// has no ADD COLUMN IF NOT EXISTS).
	if isPostgres {
		_, _ = db.Exec(`ALTER TABLE chats ADD COLUMN IF NOT EXISTS unread_count INTEGER NOT NULL DEFAULT 0`)
	} else {
		_, _ = db.Exec(`ALTER TABLE chats ADD COLUMN unread_count INTEGER NOT NULL DEFAULT 0`)
	}

	// Soft-delete (revoke) and edit-timestamp columns so revokes/edits leave a
	// tombstone instead of dropping the row — keeps history a complete feed.
	if isPostgres {
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMP`)
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS edited_at TIMESTAMP`)
	} else {
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN deleted_at TIMESTAMP`)
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN edited_at TIMESTAMP`)
	}

	// Reply/quote context columns so a quoted message can be shown above the
	// reply (StanzaID + participant + a text preview of the quoted message).
	if isPostgres {
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS quoted_message_id TEXT`)
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS quoted_sender TEXT`)
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS quoted_content TEXT`)
	} else {
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN quoted_message_id TEXT`)
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN quoted_sender TEXT`)
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN quoted_content TEXT`)
	}

	// Forwarded marker so a "Forwarded" label can be shown above a message that
	// was forwarded (incoming or sent by us) — mirrors WhatsApp's chain-forward
	// indicator. Defaults FALSE for pre-existing rows.
	if isPostgres {
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS is_forwarded BOOLEAN NOT NULL DEFAULT FALSE`)
	} else {
		_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN is_forwarded BOOLEAN NOT NULL DEFAULT 0`)
	}

	return &MessageStore{db: db}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// StoreReaction upserts a sender's reaction emoji on a target message.
func (store *MessageStore) StoreReaction(chatJID, msgID, sender, emoji string, ts time.Time) error {
	if isPostgres {
		_, err := store.db.Exec(
			`INSERT INTO reactions (chat_jid, msg_id, sender, emoji, timestamp)
             VALUES ($1, $2, $3, $4, $5)
             ON CONFLICT (msg_id, sender) DO UPDATE SET
                emoji = EXCLUDED.emoji,
                timestamp = EXCLUDED.timestamp`,
			chatJID, msgID, sender, emoji, ts,
		)
		return err
	}
	_, err := store.db.Exec(
		`INSERT INTO reactions (chat_jid, msg_id, sender, emoji, timestamp) VALUES (?, ?, ?, ?, ?)
         ON CONFLICT(msg_id, sender) DO UPDATE SET
            emoji = excluded.emoji,
            timestamp = excluded.timestamp`,
		chatJID, msgID, sender, emoji, ts,
	)
	return err
}

// DeleteReaction removes a sender's reaction from a message (reaction cleared).
func (store *MessageStore) DeleteReaction(msgID, sender string) error {
	q := "DELETE FROM reactions WHERE msg_id = ? AND sender = ?"
	if isPostgres {
		q = "DELETE FROM reactions WHERE msg_id = $1 AND sender = $2"
	}
	_, err := store.db.Exec(q, msgID, sender)
	return err
}

// UpdateMessageContent rewrites a stored message's text — used when an edit
// (protocol MESSAGE_EDIT) arrives for an earlier message.
func (store *MessageStore) UpdateMessageContent(chatJID, msgID, content string) error {
	q := "UPDATE messages SET content = ?, edited_at = CURRENT_TIMESTAMP WHERE id = ? AND chat_jid = ?"
	if isPostgres {
		q = "UPDATE messages SET content = $1, edited_at = now() WHERE id = $2 AND chat_jid = $3"
	}
	_, err := store.db.Exec(q, content, msgID, chatJID)
	return err
}

// DeleteMessage soft-deletes a stored message on revoke — the row is kept with
// deleted_at set so history stays a complete change feed (readers may hide it).
func (store *MessageStore) DeleteMessage(chatJID, msgID string) error {
	q := "UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ? AND chat_jid = ?"
	if isPostgres {
		q = "UPDATE messages SET deleted_at = now() WHERE id = $1 AND chat_jid = $2"
	}
	_, err := store.db.Exec(q, msgID, chatJID)
	return err
}

// GetReactions returns "emoji (sender)" entries for a message, for display.
func (store *MessageStore) GetReactions(msgID string) []string {
	q := "SELECT emoji, sender FROM reactions WHERE msg_id = ? ORDER BY timestamp"
	if isPostgres {
		q = "SELECT emoji, sender FROM reactions WHERE msg_id = $1 ORDER BY timestamp"
	}
	rows, err := store.db.Query(q, msgID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var emoji, sender string
		if err := rows.Scan(&emoji, &sender); err != nil {
			continue
		}
		name := store.GetSenderName(sender)
		out = append(out, fmt.Sprintf("%s (%s)", emoji, name))
	}
	return out
}

// normalizeUserJID converts a LID JID (xxxx@lid) into a phone-number JID (xxxx@s.whatsapp.net)
// using the whatsmeow LID mapping store. Non-LID JIDs are returned unchanged.
func normalizeUserJID(client *whatsmeow.Client, jid types.JID) types.JID {
	if client == nil || client.Store == nil || client.Store.LIDs == nil {
		return jid
	}

	// Only normalize hidden-user server (@lid)
	if jid.Server != types.HiddenUserServer {
		return jid
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pn, err := client.Store.LIDs.GetPNForLID(ctx, jid)
	if err != nil || pn.IsEmpty() {
		return jid
	}

	return pn
}

// migrateLIDChatsToPhoneJIDs merges chats stored under @lid
// into their corresponding @s.whatsapp.net chats.
//
// This is idempotent and safe to run on every startup.
//
// Migration order:
//
// 1. Create/upsert PN chat
// 2. Move messages to PN chat
// 3. Delete leftover duplicate messages
// 4. Delete old LID chat
func migrateLIDChatsToPhoneJIDs(
	client *whatsmeow.Client,
	store *MessageStore,
	logger waLog.Logger,
	isPostgresDB bool,
) {
	if client == nil || store == nil || store.db == nil {
		return
	}

	db := store.db

	var query string
	arg := "%@" + types.HiddenUserServer

	if isPostgresDB {
		query = `
			SELECT jid, name, last_message_time
			FROM chats
			WHERE jid LIKE $1
		`
	} else {
		query = `
			SELECT jid, name, last_message_time
			FROM chats
			WHERE jid LIKE ?
		`
	}

	rows, err := db.Query(query, arg)
	if err != nil {
		logger.Errorf("LID migration: failed listing chats: %v", err)
		return
	}
	defer rows.Close()

	type lidChat struct {
		JID             string
		Name            string
		LastMessageTime time.Time
	}

	var chats []lidChat

	for rows.Next() {
		var c lidChat

		if err := rows.Scan(&c.JID, &c.Name, &c.LastMessageTime); err != nil {
			logger.Warnf("LID migration: scan failed: %v", err)
			continue
		}

		chats = append(chats, c)
	}

	if len(chats) == 0 {
		return
	}

	logger.Infof("LID migration: found %d @lid chats", len(chats))

	merged := 0
	skipped := 0

	for _, c := range chats {

		tx, err := db.Begin()
		if err != nil {
			logger.Warnf("LID migration: tx begin failed for %s: %v", c.JID, err)
			skipped++
			continue
		}

		commit := false
		defer func() {
			if !commit {
				_ = tx.Rollback()
			}
		}()

		lidJID, parseErr := types.ParseJID(c.JID)
		if parseErr != nil {
			logger.Warnf("LID migration: invalid jid %s: %v", c.JID, parseErr)
			_ = tx.Rollback()
			skipped++
			continue
		}

		pnJID := normalizeUserJID(client, lidJID)

		if pnJID.Server != types.DefaultUserServer {
			_ = tx.Rollback()
			skipped++
			continue
		}

		pnStr := pnJID.String()

		var upsertQuery string

		if isPostgresDB {
			upsertQuery = `
				INSERT INTO chats (jid, name, last_message_time)
				VALUES ($1, $2, $3)
				ON CONFLICT (jid)
				DO UPDATE SET
					name = COALESCE(NULLIF(chats.name, ''), EXCLUDED.name),
					last_message_time = GREATEST(
						chats.last_message_time,
						EXCLUDED.last_message_time
					)
			`
		} else {
			upsertQuery = `
				INSERT INTO chats (jid, name, last_message_time)
				VALUES (?, ?, ?)
				ON CONFLICT(jid)
				DO UPDATE SET
					name = COALESCE(NULLIF(chats.name, ''), excluded.name),
					last_message_time = MAX(
						chats.last_message_time,
						excluded.last_message_time
					)
			`
		}

		if _, err = tx.Exec(upsertQuery, pnStr, c.Name, c.LastMessageTime); err != nil {
			logger.Warnf("LID migration: upsert failed %s -> %s: %v", c.JID, pnStr, err)
			_ = tx.Rollback()
			skipped++
			continue
		}

		var moveMessagesQuery string

		if isPostgresDB {
			moveMessagesQuery = `
				UPDATE messages
				SET chat_jid = $1
				WHERE chat_jid = $2
			`
		} else {
			moveMessagesQuery = `
				UPDATE messages
				SET chat_jid = ?
				WHERE chat_jid = ?
			`
		}

		if _, err = tx.Exec(moveMessagesQuery, pnStr, c.JID); err != nil {
			logger.Warnf("LID migration: move messages failed %s -> %s: %v", c.JID, pnStr, err)
			_ = tx.Rollback()
			skipped++
			continue
		}

		var deleteChatQuery string

		if isPostgresDB {
			deleteChatQuery = `DELETE FROM chats WHERE jid = $1`
		} else {
			deleteChatQuery = `DELETE FROM chats WHERE jid = ?`
		}

		if _, err = tx.Exec(deleteChatQuery, c.JID); err != nil {
			logger.Warnf("LID migration: delete old chat failed %s: %v", c.JID, err)
			_ = tx.Rollback()
			skipped++
			continue
		}

		if err = tx.Commit(); err != nil {
			logger.Warnf("LID migration: commit failed %s: %v", c.JID, err)
			_ = tx.Rollback()
			skipped++
			continue
		}

		commit = true

		logger.Infof("LID migration: merged %s -> %s", c.JID, pnStr)
		merged++
	}

	logger.Infof(
		"LID migration complete: %d merged, %d skipped",
		merged,
		skipped,
	)
}

// StoreChat Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	if isPostgres {
		_, err := store.db.Exec(
			`INSERT INTO chats (jid, name, last_message_time) 
         VALUES ($1, $2, $3)
         ON CONFLICT (jid) DO UPDATE SET 
            name = EXCLUDED.name,
            last_message_time = GREATEST(chats.last_message_time, EXCLUDED.last_message_time)`,
			jid, name, lastMessageTime,
		)
		return err
	}
	_, err := store.db.Exec(
		`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
         ON CONFLICT(jid) DO UPDATE SET
            name = excluded.name,
            last_message_time = MAX(chats.last_message_time, excluded.last_message_time)`,
		jid, name, lastMessageTime,
	)
	return err
}

// SetUnreadCount overwrites a chat's unread counter with an authoritative value
// (WhatsApp's own per-chat count from history sync, or 0 when a read receipt
// clears it).
func (store *MessageStore) SetUnreadCount(jid string, count int) error {
	q := "UPDATE chats SET unread_count = ? WHERE jid = ?"
	if isPostgres {
		q = "UPDATE chats SET unread_count = $1 WHERE jid = $2"
	}
	_, err := store.db.Exec(q, count, jid)
	return err
}

// MarkUnread flags a chat as unread (at least 1) without clobbering a larger
// live count — used when the phone marks a chat as unread via app state.
func (store *MessageStore) MarkUnread(jid string) error {
	q := "UPDATE chats SET unread_count = MAX(unread_count, 1) WHERE jid = ?"
	if isPostgres {
		q = "UPDATE chats SET unread_count = GREATEST(unread_count, 1) WHERE jid = $1"
	}
	_, err := store.db.Exec(q, jid)
	return err
}

// IncrementUnread bumps a chat's unread counter by one for a freshly arrived
// incoming message.
func (store *MessageStore) IncrementUnread(jid string) error {
	q := "UPDATE chats SET unread_count = unread_count + 1 WHERE jid = ?"
	if isPostgres {
		q = "UPDATE chats SET unread_count = unread_count + 1 WHERE jid = $1"
	}
	_, err := store.db.Exec(q, jid)
	return err
}

// LatestIncoming returns the id and sender (normalized user part) of the most
// recent incoming message in a chat — the message to send a read receipt for so
// WhatsApp clears the chat's unread state. ok is false when the chat has no
// incoming message (nothing to mark read).
func (store *MessageStore) LatestIncoming(chatJID string) (id string, sender string, ok bool) {
	q := "SELECT id, sender FROM messages WHERE chat_jid = ? AND is_from_me = 0 ORDER BY timestamp DESC LIMIT 1"
	if isPostgres {
		q = "SELECT id, sender FROM messages WHERE chat_jid = $1 AND is_from_me = false ORDER BY timestamp DESC LIMIT 1"
	}
	if err := store.db.QueryRow(q, chatJID).Scan(&id, &sender); err != nil {
		return "", "", false
	}
	return id, sender, id != ""
}

// StoreMessage Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64,
	quotedID, quotedSender, quotedContent string, isForwarded bool) error {
	if content == "" && mediaType == "" {
		return nil
	}

	if !isPostgres {
		_, err := store.db.Exec(
			`INSERT OR REPLACE INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, quoted_message_id, quoted_sender, quoted_content, is_forwarded)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, nullable(quotedID), nullable(quotedSender), nullable(quotedContent), isForwarded,
		)
		return err
	}
	_, err := store.db.Exec(
		`INSERT INTO messages
    (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, quoted_message_id, quoted_sender, quoted_content, is_forwarded)
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
    ON CONFLICT(id, chat_jid) DO UPDATE SET
    chat_jid = EXCLUDED.chat_jid,
    sender = EXCLUDED.sender,
    content = EXCLUDED.content,
    timestamp = EXCLUDED.timestamp,
    is_from_me = EXCLUDED.is_from_me,
    media_type = EXCLUDED.media_type,
    filename = EXCLUDED.filename,
    url = EXCLUDED.url,
    media_key = EXCLUDED.media_key,
    file_sha256 = EXCLUDED.file_sha256,
    file_enc_sha256 = EXCLUDED.file_enc_sha256,
    file_length = EXCLUDED.file_length,
    quoted_message_id = COALESCE(EXCLUDED.quoted_message_id, messages.quoted_message_id),
    quoted_sender = COALESCE(EXCLUDED.quoted_sender, messages.quoted_sender),
    quoted_content = COALESCE(EXCLUDED.quoted_content, messages.quoted_content),
    is_forwarded = messages.is_forwarded OR EXCLUDED.is_forwarded`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, nullable(quotedID), nullable(quotedSender), nullable(quotedContent), isForwarded,
	)

	return err
}

// nullable converts an empty string to a SQL NULL so optional columns (e.g. the
// quoted_* reply fields) stay NULL rather than empty string — keeps COALESCE
// upserts and readers that test for NULL working as intended.
func nullable(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// GetMessages Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	var rows *sql.Rows
	var err error

	if isPostgres {
		rows, err = store.db.Query(
			`SELECT sender, content, timestamp, is_from_me, media_type, filename 
					 FROM messages 
					 WHERE chat_jid = $1 
					 ORDER BY timestamp DESC 
					 LIMIT $2`,
			chatJID, limit,
		)
	} else {
		rows, err = store.db.Query(
			"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
			chatJID, limit,
		)
	}

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp.In(displayLoc)
		messages = append(messages, msg)
	}

	return messages, nil
}

// GetChats Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
func extractTextContent(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	return ""
}

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	MessageID string `json:"message_id,omitempty"`
	// Reason is a stable machine code for a known send failure (empty on success
	// or an unrecognized error), so callers can branch without parsing Message.
	Reason string `json:"reason,omitempty"`
}

// classifySendError maps a raw whatsmeow send error to a stable machine reason
// plus a human-readable explanation. WhatsApp only returns a numeric nack code
// (whatsmeow surfaces it as "server returned error <code>"), so we key off the
// code. Returns ("", "") when nothing matches, leaving the original message as-is.
func classifySendError(errMsg string) (reason, friendly string) {
	switch {
	case strings.Contains(errMsg, "error 463"):
		// NackCallerReachoutTimelocked: this number is time-locked from starting
		// chats with NEW contacts (a cold-outreach throttle). The tcToken
		// lifecycle already covers contactable recipients; this is the genuine
		// reachout lock and clears on its own after a cooldown.
		return "caller_reachout_timelocked",
			"WhatsApp reachout time-lock (463): this number can't start chats with new contacts right now. Stop cold sends and let it cool down (hours–days)."
	case strings.Contains(errMsg, "error 479"):
		// SmaxInvalid: a stale/invalid tcToken was attached. Usually transient —
		// whatsmeow re-issues the token, so a later retry typically succeeds.
		return "invalid_tctoken",
			"WhatsApp rejected the privacy token (479): stale tcToken, retry shortly."
	}
	return "", ""
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
	// MediaBase64 carries the media bytes inline (raw base64, no data: prefix).
	// Preferred over MediaPath: the manager web tier and the bridge run in
	// separate containers with separate filesystems, so a temp-file handoff
	// requires a shared volume. Inline bytes need none. When set, MediaPath is
	// ignored and the extension/type are derived from MediaFilename/MediaMimetype.
	MediaBase64 string `json:"media_base64,omitempty"`
	// Original upload filename — used as the document FileName/Title so the
	// recipient sees a real name (not the temp path) and can open it.
	MediaFilename string `json:"media_filename,omitempty"`
	// Optional MIME override from the manager. Without it the bridge guesses
	// "application/octet-stream" for anything outside its small extension switch,
	// which makes documents show as "untitled" and fail to open.
	MediaMimetype string `json:"media_mimetype,omitempty"`
	// MentionedJIDs tags participants. Each entry must be a full JID; the caller
	// also places the matching "@<number>" tokens in Message.
	MentionedJIDs []string `json:"mentioned_jids,omitempty"`
	// Reply: quote an earlier message. QuotedID is its message ID; QuotedText a
	// short preview shown in the quote bar; QuotedParticipant the original
	// sender's JID (required for the reply to bind correctly in groups).
	QuotedID          string `json:"quoted_id,omitempty"`
	QuotedText        string `json:"quoted_text,omitempty"`
	QuotedParticipant string `json:"quoted_participant,omitempty"`
	// Forward: mark the outgoing message as forwarded.
	IsForwarded bool `json:"is_forwarded,omitempty"`
	// NoDelay skips the humanized pre-send typing delay for this message. Set by
	// interactive callers (the admin composer, where a human already spent time
	// typing) so the operator isn't made to wait again. Automated/API sends leave
	// it false so they pick up the anti-restriction pacing.
	NoDelay bool `json:"no_delay,omitempty"`
}

var clientVersionRegex = regexp.MustCompile(`"client_revision":(\d+),`)

func CustomGetLatestVersion(ctx context.Context, httpClient *http.Client) (*store.WAVersionContainer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, socket.Origin, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	} else if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected response with status %d: %s", resp.StatusCode, data)
	} else if match := clientVersionRegex.FindSubmatch(data); len(match) == 0 {
		return nil, fmt.Errorf("version number not found")
	} else if parsedVer, err := strconv.ParseInt(string(match[1]), 10, 64); err != nil {
		return nil, fmt.Errorf("failed to parse version number: %w", err)
	} else {
		return &store.WAVersionContainer{2, 3000, uint32(parsedVer)}, nil
	}
}

// envIntDefault reads a non-negative integer env var, falling back to def when
// unset or unparseable. Zero is a valid value (e.g. SEND_DELAY_MIN_MS=0).
func envIntDefault(key string, def int) int {
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	if v, err := strconv.Atoi(s); err == nil && v >= 0 {
		return v
	}
	return def
}

// humanSendDelay returns a randomized pre-send delay so automated traffic does
// not fire on a robotic fixed cadence — the pattern WhatsApp flags and that gets
// numbers restricted. Bounds come from SEND_DELAY_MIN_MS / SEND_DELAY_MAX_MS
// (defaults 3s / 30s). Returns 0 (disabled) when the band is non-positive.
func humanSendDelay() time.Duration {
	minMs := envIntDefault("SEND_DELAY_MIN_MS", 3000)
	maxMs := envIntDefault("SEND_DELAY_MAX_MS", 30000)
	if maxMs <= 0 || maxMs < minMs {
		return 0
	}
	pick := minMs
	if maxMs > minMs {
		pick = minMs + rand.Intn(maxMs-minMs+1)
	}
	return time.Duration(pick) * time.Millisecond
}

// sendChatPresence streams a typing state to one recipient. WhatsApp only
// accepts chat presence after the client has broadcast availability, so we send
// Available first (idempotent). Best-effort and purely cosmetic — never fail a
// send because presence did not go through.
func sendChatPresence(client *whatsmeow.Client, jid types.JID, state types.ChatPresence) {
	if client == nil || !client.IsConnected() {
		return
	}
	if err := client.SendPresence(context.Background(), types.PresenceAvailable); err != nil {
		slog.Debug("send available presence failed", "err", err)
	}
	if err := client.SendChatPresence(context.Background(), jid, state, types.ChatPresenceMediaText); err != nil {
		slog.Debug("send chat presence failed", "jid", jid.String(), "state", string(state), "err", err)
	}
}

// holdWithTyping shows a typing indicator for the given duration before a send.
// Composing presence auto-expires on the recipient after ~10s, so it is
// refreshed every 8s across the wait so the "typing…" never flickers off. It
// deliberately does NOT send Paused at the end: the message that follows clears
// the indicator itself, so the typing stays visible continuously right up to the
// moment the message lands (a trailing Paused would blink it off first). This is
// the humanized pacing that spaces out otherwise-bursty automated sends.
func holdWithTyping(client *whatsmeow.Client, jid types.JID, d time.Duration) {
	if d <= 0 {
		return
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		sendChatPresence(client, jid, types.ChatPresenceComposing)
		remaining := time.Until(deadline)
		if remaining > 8*time.Second {
			remaining = 8 * time.Second
		}
		time.Sleep(remaining)
	}
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, messageStore *MessageStore, recipient string, message string, mediaPath string, mediaBase64 string, mediaFilename string, mediaMimetype string, mentionedJIDs []string, quotedID string, quotedText string, quotedParticipant string, isForwarded bool, noDelay bool) (bool, string, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp", ""
	}

	var recipientJID types.JID
	var err error

	isJID := strings.Contains(recipient, "@")

	if isJID {
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err), ""
		}
	} else {
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	// Resolve @lid JIDs to @s.whatsapp.net before sending — normalizeUserJID
	// is a no-op for already-resolved JIDs, so warm contacts are unaffected.
	recipientJID = normalizeUserJID(client, recipientJID)

	// Mentions / reply / forward all ride in ContextInfo. Built once and attached
	// to whichever message variant we send below. Nil when none requested.
	var ctxInfo *waE2E.ContextInfo
	if len(mentionedJIDs) > 0 || quotedID != "" || isForwarded {
		ctxInfo = &waE2E.ContextInfo{}
		if len(mentionedJIDs) > 0 {
			ctxInfo.MentionedJID = mentionedJIDs
		}
		if quotedID != "" {
			ctxInfo.StanzaID = proto.String(quotedID)
			if quotedParticipant != "" {
				ctxInfo.Participant = proto.String(quotedParticipant)
			}
			// A minimal quoted message (text preview) is enough for the reply bar
			// to render on the recipient and bind to the original by StanzaID.
			ctxInfo.QuotedMessage = &waE2E.Message{Conversation: proto.String(quotedText)}
		}
		if isForwarded {
			ctxInfo.IsForwarded = proto.Bool(true)
			ctxInfo.ForwardingScore = proto.Uint32(1)
		}
	}

	// Captured from the media upload below so the sent message can be persisted
	// to our own store (WhatsApp never echoes a message back to the device that
	// sent it, so without this the send vanishes from the chat view on reload).
	var stMediaType, stFilename, stURL string
	var stMediaKey, stFileSHA, stFileEncSHA []byte
	var stFileLen uint64

	msg := &waE2E.Message{}

	if mediaPath != "" || mediaBase64 != "" {
		// Inline bytes (MediaBase64) take precedence over a temp-file path, so a
		// split web/worker deployment needs no shared volume. Fall back to the
		// on-disk path for legacy callers.
		var mediaData []byte
		if mediaBase64 != "" {
			decoded, derr := base64.StdEncoding.DecodeString(mediaBase64)
			if derr != nil {
				return false, fmt.Sprintf("Error decoding media base64: %v", derr), ""
			}
			if len(decoded) == 0 {
				return false, "media_base64 decoded to empty", ""
			}
			mediaData = decoded
		} else {
			validatedPath, verr := validateMediaPath(mediaPath)
			if verr != nil {
				return false, fmt.Sprintf("Invalid media path: %v", verr), ""
			}
			data, rerr := os.ReadFile(validatedPath)
			if rerr != nil {
				return false, fmt.Sprintf("Error reading media file: %v", rerr), ""
			}
			mediaData = data
		}

		// Derive the extension from whichever name we have — the temp path for
		// file sends, the original filename for inline sends.
		nameForExt := mediaPath
		if nameForExt == "" {
			nameForExt = mediaFilename
		}
		fileExt := strings.ToLower(nameForExt[strings.LastIndex(nameForExt, ".")+1:])
		var mediaType whatsmeow.MediaType
		var mimeType string

		switch fileExt {
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/octet-stream"
		}

		// Manager-provided MIME beats the extension guess — critical for
		// documents, which otherwise default to octet-stream and won't open.
		if mediaMimetype != "" {
			mimeType = mediaMimetype
		}

		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err), ""
		}

		slog.Info("media uploaded", "response", resp)

		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waE2E.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			var seconds uint32 = 30
			var waveform []byte = nil

			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err), ""
				}
			} else {
				slog.Warn("not an Ogg Opus file", "mime_type", mimeType)
			}

			msg.AudioMessage = &waE2E.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waE2E.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			docName := mediaPath[strings.LastIndex(mediaPath, "/")+1:]
			if mediaFilename != "" {
				docName = mediaFilename
			}
			msg.DocumentMessage = &waE2E.DocumentMessage{
				Title:         proto.String(docName),
				FileName:      proto.String(docName),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}

		stFilename = mediaFilename
		stURL = resp.URL
		stMediaKey = resp.MediaKey
		stFileSHA = resp.FileSHA256
		stFileEncSHA = resp.FileEncSHA256
		stFileLen = resp.FileLength
		switch mediaType {
		case whatsmeow.MediaImage:
			stMediaType = "image"
		case whatsmeow.MediaVideo:
			stMediaType = "video"
		case whatsmeow.MediaAudio:
			stMediaType = "audio"
		case whatsmeow.MediaDocument:
			stMediaType = "document"
		}
	} else if ctxInfo != nil {
		msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{
			Text:        proto.String(message),
			ContextInfo: ctxInfo,
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Attach mentions to whichever captioned media message was built above.
	if ctxInfo != nil {
		switch {
		case msg.ImageMessage != nil:
			msg.ImageMessage.ContextInfo = ctxInfo
		case msg.VideoMessage != nil:
			msg.VideoMessage.ContextInfo = ctxInfo
		case msg.DocumentMessage != nil:
			msg.DocumentMessage.ContextInfo = ctxInfo
		case msg.AudioMessage != nil:
			msg.AudioMessage.ContextInfo = ctxInfo
		}
	}

	// Humanize delivery: show a typing indicator and wait a randomized delay
	// before sending, so automated traffic does not fire as an instant burst on a
	// fixed cadence (the fingerprint that gets numbers restricted). Skipped for
	// interactive sends (noDelay) and when the delay band is disabled.
	if !noDelay {
		holdWithTyping(client, recipientJID, humanSendDelay())
	}

	sendResp, err := client.SendMessage(context.Background(), recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err), ""
	}

	// Persist our own outgoing message — WhatsApp does not echo it back to the
	// sending device, so this is the only way it lands in the chat view/store.
	// Keyed by message ID, so a later sync from another device just upserts.
	if messageStore != nil && client.Store.ID != nil {
		sender := client.Store.ID.ToNonAD().String()
		if storeErr := messageStore.StoreMessage(
			sendResp.ID, recipientJID.String(), sender, message, time.Now(), true,
			stMediaType, stFilename, stURL, stMediaKey, stFileSHA, stFileEncSHA, stFileLen,
			quotedID, quotedParticipant, quotedText, isForwarded,
		); storeErr != nil {
			slog.Warn("failed to store sent message", "error", storeErr)
		}
	}

	return true, fmt.Sprintf("Message sent to %s", recipient), sendResp.ID
}

// histRow is one stored message with the fields needed to rebuild a
// WebMessageInfo for a history-sync bundle.
type histRow struct {
	ID        string
	Sender    string
	Content   string
	Time      time.Time
	IsFromMe  bool
	MediaType string
}

// getHistoryRows returns the last `limit` messages of a chat (newest first),
// including the message ID (GetMessages omits it).
func (store *MessageStore) getHistoryRows(chatJID string, limit int) ([]histRow, error) {
	q := "SELECT id, sender, content, timestamp, is_from_me, media_type FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?"
	if isPostgres {
		q = "SELECT id, sender, content, timestamp, is_from_me, media_type FROM messages WHERE chat_jid = $1 ORDER BY timestamp DESC LIMIT $2"
	}
	rows, err := store.db.Query(q, chatJID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []histRow
	for rows.Next() {
		var r histRow
		var ts time.Time
		var sender, content, mediaType sql.NullString
		if err := rows.Scan(&r.ID, &sender, &content, &ts, &r.IsFromMe, &mediaType); err != nil {
			return nil, err
		}
		r.Sender = sender.String
		r.Content = content.String
		r.MediaType = mediaType.String
		r.Time = ts
		out = append(out, r)
	}
	return out, rows.Err()
}

// shareGroupHistory builds a RECENT history-sync bundle of the last `count`
// messages in gjid and sends it as an (invisible) HistorySyncNotification to
// each member — WhatsApp's "share message history with new member" mechanism, so
// the history appears for the newcomer without posting anything into the group.
// Text only; media is rendered as a "[type]" placeholder.
func shareGroupHistory(client *whatsmeow.Client, store *MessageStore, gjid types.JID, members []types.JID, count int) error {
	if store == nil {
		return fmt.Errorf("no message store")
	}
	if count <= 0 || count > 100 {
		count = 50
	}
	rows, err := store.getHistoryRows(gjid.String(), count)
	if err != nil {
		return fmt.Errorf("read messages: %w", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("no messages to share")
	}
	// Newest-first → chronological.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}

	var ownJID string
	if client.Store.ID != nil {
		ownJID = client.Store.ID.ToNonAD().String()
	}
	isGroup := gjid.Server == types.GroupServer

	hsMsgs := make([]*waHistorySync.HistorySyncMsg, 0, len(rows))
	for idx, m := range rows {
		body := m.Content
		if body == "" && m.MediaType != "" {
			body = "[" + m.MediaType + "]"
		}
		if body == "" {
			continue
		}
		participant := m.Sender
		if m.IsFromMe || participant == "" {
			participant = ownJID
		} else if !strings.Contains(participant, "@") {
			participant = participant + "@" + types.DefaultUserServer
		}
		key := &waCommon.MessageKey{
			RemoteJID: proto.String(gjid.String()),
			FromMe:    proto.Bool(m.IsFromMe),
			ID:        proto.String(m.ID),
		}
		wmi := &waWeb.WebMessageInfo{
			Key:              key,
			Message:          &waE2E.Message{Conversation: proto.String(body)},
			MessageTimestamp: proto.Uint64(uint64(m.Time.Unix())),
			Status:           waWeb.WebMessageInfo_DELIVERY_ACK.Enum(),
		}
		if isGroup {
			key.Participant = proto.String(participant)
			wmi.Participant = proto.String(participant)
		}
		hsMsgs = append(hsMsgs, &waHistorySync.HistorySyncMsg{
			Message:    wmi,
			MsgOrderID: proto.Uint64(uint64(idx + 1)),
		})
	}
	if len(hsMsgs) == 0 {
		return fmt.Errorf("no shareable messages")
	}

	hs := &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{{
			ID:       proto.String(gjid.String()),
			Messages: hsMsgs,
		}},
		ChunkOrder: proto.Uint32(1),
		Progress:   proto.Uint32(100),
	}
	raw, err := proto.Marshal(hs)
	if err != nil {
		return fmt.Errorf("marshal history sync: %w", err)
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return fmt.Errorf("compress: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("compress close: %w", err)
	}

	up, err := client.Upload(context.Background(), buf.Bytes(), whatsmeow.MediaHistory)
	if err != nil {
		return fmt.Errorf("upload history: %w", err)
	}

	notif := &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
			HistorySyncNotification: &waE2E.HistorySyncNotification{
				FileSHA256:    up.FileSHA256,
				FileLength:    proto.Uint64(up.FileLength),
				MediaKey:      up.MediaKey,
				FileEncSHA256: up.FileEncSHA256,
				DirectPath:    proto.String(up.DirectPath),
				SyncType:      waE2E.HistorySyncType_RECENT.Enum(),
				ChunkOrder:    proto.Uint32(1),
			},
		},
	}
	var sendErr error
	for _, member := range members {
		if _, err := client.SendMessage(context.Background(), member, notif); err != nil {
			slog.Warn("failed to send history sync", "member", member.String(), "error", err)
			sendErr = err
		}
	}
	return sendErr
}

// Extract media info from a message
// captionOf returns the caption of a media message (image/video/document).
// extractTextContent only reads plain text, so without this captioned media
// would be stored with empty content.
func captionOf(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	if img := msg.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return vid.GetCaption()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetCaption()
	}
	return ""
}

// mediaStem builds a stable, collision-free filename stem for media that carries
// no name of its own (image / video / audio / sticker). Keying off the file's
// SHA-256 makes the name deterministic — the webhook handler and the later
// /download call derive the identical path — and unique per distinct file. This
// replaces the old time.Now() scheme, whose 1-second resolution overwrote any
// two media arriving in the same second onto a single file, so every colliding
// message rendered the same image. Falls back to a timestamp when no hash exists.
func mediaStem(prefix string, sha []byte) string {
	if len(sha) >= 8 {
		return prefix + "_" + fmt.Sprintf("%x", sha[:8])
	}
	return prefix + "_" + time.Now().Format("20060102_150405")
}

func extractMediaInfo(msg *waE2E.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	if img := msg.GetImageMessage(); img != nil {
		return "image", mediaStem("image", img.GetFileSHA256()) + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", mediaStem("video", vid.GetFileSHA256()) + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", mediaStem("audio", aud.GetFileSHA256()) + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = mediaStem("document", doc.GetFileSHA256())
		}
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	if stk := msg.GetStickerMessage(); stk != nil {
		return "sticker", mediaStem("sticker", stk.GetFileSHA256()) + ".webp",
			stk.GetURL(), stk.GetMediaKey(), stk.GetFileSHA256(), stk.GetFileEncSHA256(), stk.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

// unwrapMessage peels ephemeral / view-once / device-sent wrappers to reach the
// real message — without this, messages in disappearing chats look empty.
func unwrapMessage(m *waE2E.Message) *waE2E.Message {
	for m != nil {
		switch {
		case m.GetEphemeralMessage().GetMessage() != nil:
			m = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage().GetMessage() != nil:
			m = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2().GetMessage() != nil:
			m = m.GetViewOnceMessageV2().GetMessage()
		case m.GetViewOnceMessageV2Extension().GetMessage() != nil:
			m = m.GetViewOnceMessageV2Extension().GetMessage()
		case m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			m = m.GetDocumentWithCaptionMessage().GetMessage()
		case m.GetDeviceSentMessage().GetMessage() != nil:
			m = m.GetDeviceSentMessage().GetMessage()
		case m.GetEditedMessage().GetMessage() != nil:
			// Edits arrive wrapped in a top-level EditedMessage; peel it so the
			// inner ProtocolMessage(MESSAGE_EDIT) is reachable (revokes aren't
			// wrapped, which is why they worked but edits didn't).
			m = m.GetEditedMessage().GetMessage()
		default:
			return m
		}
	}
	return m
}

// describeMessage gives a short human label for non-text, non-basic-media
// messages (location, contact, poll) so they are stored and visible in the MCP
// instead of being silently dropped by the empty-content guard. Returns "" for
// types kept out of the timeline (reactions have their own table; protocol
// revoke/edit events aren't real messages).
func describeMessage(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	switch {
	case m.GetLocationMessage() != nil, m.GetLiveLocationMessage() != nil:
		return "[location]"
	case m.GetContactMessage() != nil, m.GetContactsArrayMessage() != nil:
		return "[contact]"
	case m.GetPollCreationMessage() != nil, m.GetPollCreationMessageV2() != nil, m.GetPollCreationMessageV3() != nil:
		return "[poll]"
	case m.GetPollUpdateMessage() != nil:
		return "[poll vote]"
	}
	return ""
}

// contextInfoOf returns the ContextInfo attached to whichever message variant
// carries it (text/media/etc). The ContextInfo holds the reply/quote metadata
// and mentioned JIDs. Returns nil when the message has no context.
func contextInfoOf(m *waE2E.Message) *waE2E.ContextInfo {
	if m == nil {
		return nil
	}
	switch {
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetContextInfo()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetContextInfo()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetContextInfo()
	case m.GetPtvMessage() != nil:
		return m.GetPtvMessage().GetContextInfo()
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage().GetContextInfo()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetContextInfo()
	case m.GetStickerMessage() != nil:
		return m.GetStickerMessage().GetContextInfo()
	case m.GetLocationMessage() != nil:
		return m.GetLocationMessage().GetContextInfo()
	case m.GetLiveLocationMessage() != nil:
		return m.GetLiveLocationMessage().GetContextInfo()
	case m.GetContactMessage() != nil:
		return m.GetContactMessage().GetContextInfo()
	case m.GetContactsArrayMessage() != nil:
		return m.GetContactsArrayMessage().GetContextInfo()
	case m.GetPollCreationMessage() != nil:
		return m.GetPollCreationMessage().GetContextInfo()
	}
	return nil
}

// quotedPreview produces a short text preview of a quoted message for the reply
// box — plain text, else a caption, else a media-type / type label.
func quotedPreview(qm *waE2E.Message) string {
	if qm == nil {
		return ""
	}
	if t := extractTextContent(qm); t != "" {
		return t
	}
	if c := captionOf(qm); c != "" {
		return c
	}
	if mt, _, _, _, _, _, _ := extractMediaInfo(qm); mt != "" {
		return mt
	}
	return describeMessage(qm)
}

// quotedFromMessage pulls the reply/quote fields (quoted message id, the quoted
// message's author JID, and a short content preview) out of a message's
// ContextInfo. All three are empty when the message is not a reply.
func quotedFromMessage(m *waE2E.Message) (quotedID, quotedSender, quotedContent string) {
	ctx := contextInfoOf(m)
	if ctx == nil {
		return "", "", ""
	}
	quotedID = ctx.GetStanzaID()
	if quotedID == "" {
		return "", "", ""
	}
	quotedSender = ctx.GetParticipant()
	quotedContent = quotedPreview(ctx.GetQuotedMessage())
	return quotedID, quotedSender, quotedContent
}

// buildWebhookPayload produces a full-fidelity payload for an incoming message.
// Friendly fields cover the common types; "raw" carries the complete decoded
// message so nothing is ever lost, including types not mapped explicitly.
func buildWebhookPayload(client *whatsmeow.Client, evt *events.Message) map[string]interface{} {
	p := map[string]interface{}{
		"chat_jid":   evt.Info.Chat.String(),
		"sender":     normalizeUserJID(client, evt.Info.Sender).User,
		"is_from_me": evt.Info.IsFromMe,
		"timestamp":  evt.Info.Timestamp.String(),
		"push_name":  evt.Info.PushName,
		"is_group":   evt.Info.IsGroup,
		"message_id": evt.Info.ID,
	}

	m := unwrapMessage(evt.Message)
	if m == nil {
		p["message_type"] = "unknown"
		return p
	}

	if raw, err := protojson.Marshal(m); err == nil {
		p["raw"] = json.RawMessage(raw)
	}

	msgType := "unknown"
	content := ""
	var ctx *waE2E.ContextInfo

	switch {
	case m.GetConversation() != "":
		msgType, content = "text", m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		msgType, content = "text", m.GetExtendedTextMessage().GetText()
		ctx = m.GetExtendedTextMessage().GetContextInfo()
	case m.GetImageMessage() != nil:
		msgType, content = "image", m.GetImageMessage().GetCaption()
		ctx = m.GetImageMessage().GetContextInfo()
	case m.GetVideoMessage() != nil:
		if m.GetVideoMessage().GetGifPlayback() {
			msgType = "gif"
		} else {
			msgType = "video"
		}
		content = m.GetVideoMessage().GetCaption()
		ctx = m.GetVideoMessage().GetContextInfo()
	case m.GetPtvMessage() != nil:
		msgType = "video_note"
		ctx = m.GetPtvMessage().GetContextInfo()
	case m.GetAudioMessage() != nil:
		if m.GetAudioMessage().GetPTT() {
			msgType = "voice"
		} else {
			msgType = "audio"
		}
		ctx = m.GetAudioMessage().GetContextInfo()
	case m.GetDocumentMessage() != nil:
		msgType, content = "document", m.GetDocumentMessage().GetCaption()
		ctx = m.GetDocumentMessage().GetContextInfo()
	case m.GetStickerMessage() != nil:
		msgType = "sticker"
		ctx = m.GetStickerMessage().GetContextInfo()
	case m.GetLocationMessage() != nil:
		msgType, content = "location", m.GetLocationMessage().GetName()
		ctx = m.GetLocationMessage().GetContextInfo()
	case m.GetLiveLocationMessage() != nil:
		msgType = "live_location"
		ctx = m.GetLiveLocationMessage().GetContextInfo()
	case m.GetContactMessage() != nil:
		msgType, content = "contact", m.GetContactMessage().GetDisplayName()
		ctx = m.GetContactMessage().GetContextInfo()
	case m.GetContactsArrayMessage() != nil:
		msgType, content = "contacts_array", m.GetContactsArrayMessage().GetDisplayName()
		ctx = m.GetContactsArrayMessage().GetContextInfo()
	case m.GetPollCreationMessage() != nil:
		msgType, content = "poll", m.GetPollCreationMessage().GetName()
		ctx = m.GetPollCreationMessage().GetContextInfo()
	case m.GetPollCreationMessageV2() != nil:
		msgType, content = "poll", m.GetPollCreationMessageV2().GetName()
	case m.GetPollCreationMessageV3() != nil:
		msgType, content = "poll", m.GetPollCreationMessageV3().GetName()
	case m.GetPollUpdateMessage() != nil:
		msgType = "poll_vote"
		if k := m.GetPollUpdateMessage().GetPollCreationMessageKey(); k != nil {
			p["target_message_id"] = k.GetID()
		}
	case m.GetReactionMessage() != nil:
		msgType, content = "reaction", m.GetReactionMessage().GetText()
		p["reaction"] = m.GetReactionMessage().GetText()
		if k := m.GetReactionMessage().GetKey(); k != nil {
			p["target_message_id"] = k.GetID()
		}
	case m.GetProtocolMessage() != nil:
		pm := m.GetProtocolMessage()
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			msgType = "revoked"
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			msgType = "edited"
			content = extractTextContent(pm.GetEditedMessage())
		default:
			msgType = "protocol"
		}
		if k := pm.GetKey(); k != nil {
			p["target_message_id"] = k.GetID()
		}
	}

	p["message_type"] = msgType
	p["content"] = content

	if mediaType, filename, mediaURL, mediaKey, _, _, fileLength := extractMediaInfo(m); mediaType != "" {
		p["media_type"] = mediaType
		p["filename"] = filename
		p["media_url"] = mediaURL
		p["media_key"] = base64.StdEncoding.EncodeToString(mediaKey)
		p["file_length"] = fileLength
	}

	if ctx != nil {
		if q := ctx.GetStanzaID(); q != "" {
			p["quoted_message_id"] = q
			p["quoted_sender"] = ctx.GetParticipant()
			if qm := ctx.GetQuotedMessage(); qm != nil {
				p["quoted_content"] = extractTextContent(qm)
			}
		}
		if len(ctx.GetMentionedJID()) > 0 {
			p["mentioned_jids"] = ctx.GetMentionedJID()
		}
		if ctx.GetIsForwarded() {
			p["is_forwarded"] = true
		}
	}

	return p
}

// handleGroupInfoEvent posts a group participant/role change to the number's
// configured webhook. The payload carries an "event" discriminator ("group_*")
// so consumers can branch on it — ordinary message webhooks have no "event"
// key. JIDs are normalized to bare phone numbers where possible, matching the
// rest of the webhook surface.
func handleGroupInfoEvent(client *whatsmeow.Client, evt *events.GroupInfo, logger waLog.Logger) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Println("Recovered in group-info webhook goroutine:", r)
			}
		}()
		cfg, err := config.LoadConfig()
		if err != nil || cfg.WebhookUrl == "" {
			return
		}

		nums := func(jids []types.JID) []string {
			out := make([]string, 0, len(jids))
			for _, j := range jids {
				out = append(out, normalizeUserJID(client, j).User)
			}
			return out
		}

		// Pick the most specific change for the "event" label; the arrays are
		// always included so a consumer can read every change in one payload.
		event := "group_update"
		switch {
		case len(evt.Join) > 0:
			event = "group_join"
		case len(evt.Leave) > 0:
			event = "group_leave"
		case len(evt.Promote) > 0:
			event = "group_promote"
		case len(evt.Demote) > 0:
			event = "group_demote"
		}

		payload := map[string]interface{}{
			"event":       event,
			"chat_jid":    evt.JID.String(),
			"is_group":    true,
			"timestamp":   evt.Timestamp.String(),
			"join":        nums(evt.Join),
			"leave":       nums(evt.Leave),
			"promote":     nums(evt.Promote),
			"demote":      nums(evt.Demote),
			"join_reason": evt.JoinReason,
		}
		if evt.Sender != nil {
			payload["sender"] = normalizeUserJID(client, *evt.Sender).User
		}

		jsonData, err := json.Marshal(payload)
		if err != nil {
			log.Println("Group webhook marshal error:", err)
			return
		}
		resp, err := http.Post(cfg.WebhookUrl, "application/json", bytes.NewBuffer(jsonData))
		if err != nil {
			log.Println("Group webhook POST error:", err)
			return
		}
		defer resp.Body.Close()
	}()
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	chatJID := normalizeUserJID(client, msg.Info.Chat).String()
	sender := normalizeUserJID(client, msg.Info.Sender).User

	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// MSG-DEBUG (temporary): dump every message's raw protobuf + info to diagnose
	// how edits are delivered. Remove after diagnosis.
	if b, mErr := protojson.Marshal(msg.Message); mErr == nil {
		logger.Infof("MSG-DEBUG id=%s chat=%s type=%s cat=%s raw=%s", msg.Info.ID, chatJID, msg.Info.Type, msg.Info.Category, string(b))
	}

	// Modern clients deliver OTHER parties' message edits (and poll/event edits)
	// as an encrypted SecretEncryptedMessage — the plaintext is a BuildEdit-shaped
	// message (EditedMessage → ProtocolMessage MESSAGE_EDIT). Decrypt in place so
	// the protocol-message handler below applies it like a plain edit. Without
	// this the event has no text/media and is silently dropped. Own-device edits
	// still arrive as plain ProtocolMessage and skip this path. Decryption needs
	// the target's MessageSecret, which whatsmeow stores on receipt — edits of
	// messages received before this bridge existed can't decrypt (logged, skipped).
	if sem := msg.Message.GetSecretEncryptedMessage(); sem != nil {
		decrypted, decErr := client.DecryptSecretEncryptedMessage(context.Background(), msg)
		if decErr != nil {
			logger.Warnf("Failed to decrypt secret-encrypted message %s (type %s, target %s): %v",
				msg.Info.ID, sem.GetSecretEncType().String(), sem.GetTargetMessageKey().GetID(), decErr)
			return
		}
		msg.Message = decrypted
	}

	// Reactions carry no text/media, so the empty-content guard below would drop
	// them. Persist them against their target message instead so the MCP's
	// /messages output can surface them (empty text = the sender cleared theirs).
	if reaction := unwrapMessage(msg.Message).GetReactionMessage(); reaction != nil {
		target := ""
		if k := reaction.GetKey(); k != nil {
			target = k.GetID()
		}
		if target != "" {
			if reaction.GetText() == "" {
				if err := messageStore.DeleteReaction(target, sender); err != nil {
					logger.Warnf("Failed to delete reaction: %v", err)
				}
			} else if err := messageStore.StoreReaction(chatJID, target, sender, reaction.GetText(), msg.Info.Timestamp); err != nil {
				logger.Warnf("Failed to store reaction: %v", err)
			}
		}
		return
	}

	// Edits and deletes arrive as protocol messages targeting an earlier
	// message. Apply them to the stored row so the MCP reflects the change
	// instead of showing stale (or deleted) content.
	if pm := unwrapMessage(msg.Message).GetProtocolMessage(); pm != nil {
		target := ""
		if k := pm.GetKey(); k != nil {
			target = k.GetID()
		}
		if target != "" {
			switch pm.GetType() {
			case waE2E.ProtocolMessage_REVOKE:
				if err := messageStore.DeleteMessage(chatJID, target); err != nil {
					logger.Warnf("Failed to delete revoked message: %v", err)
				}
			case waE2E.ProtocolMessage_MESSAGE_EDIT:
				edited := extractTextContent(pm.GetEditedMessage())
				if edited == "" {
					edited = captionOf(pm.GetEditedMessage())
				}
				if edited != "" {
					if err := messageStore.UpdateMessageContent(chatJID, target, edited+" (edited)"); err != nil {
						logger.Warnf("Failed to apply message edit: %v", err)
					}
				}
			}
		}
		return
	}

	content := extractTextContent(msg.Message)

	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	if content == "" {
		content = captionOf(msg.Message)
	}

	if content == "" && mediaType == "" {
		// Identify non-text / non-basic-media messages (location, contact, poll)
		// so they're stored and visible in the MCP instead of being dropped.
		content = describeMessage(unwrapMessage(msg.Message))
	}

	if content == "" && mediaType == "" {
		return
	}

	// Reply context: if this message quotes another, capture the quoted id,
	// author and a short text preview so the store can render a quote box above
	// the reply (mirrors the same fields the webhook payload carries).
	quotedID, quotedSender, quotedContent := quotedFromMessage(unwrapMessage(msg.Message))

	// Forwarded marker rides in the same ContextInfo; persist it so the manager
	// can show a "Forwarded" label above the message.
	isForwarded := contextInfoOf(unwrapMessage(msg.Message)).GetIsForwarded()

	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
		quotedID,
		quotedSender,
		quotedContent,
		isForwarded,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
		return
	}

	// Count genuinely-unread incoming messages so the manager's unread board
	// mirrors the phone. Skip status broadcasts (not a real chat). An outgoing
	// message (sent from any of our devices) means we've seen the chat, so
	// WhatsApp treats it as read — clear the badge to match, otherwise a chat
	// you replied to on your phone lingers as unread on the board.
	if chatJID != "status@broadcast" {
		if !msg.Info.IsFromMe {
			if err := messageStore.IncrementUnread(chatJID); err != nil {
				logger.Warnf("Failed to increment unread: %v", err)
			}
		} else if err := messageStore.SetUnreadCount(chatJID, 0); err != nil {
			logger.Warnf("Failed to clear unread on outgoing: %v", err)
		}
	}
	timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
	direction := "←"
	if msg.Info.IsFromMe {
		direction = "→"
	}

	if mediaType != "" {
		slog.Info("message", "ts", timestamp, "direction", direction, "sender", sender, "media_type", mediaType, "filename", filename, "content", content)
	} else if content != "" {
		slog.Info("message", "ts", timestamp, "direction", direction, "sender", sender, "content", content)
	}

	// Fire the webhook only after the message is durably stored, so a webhook
	// consumer never observes a message that the MCP's own message store lacks.
	go func() {
		cfg, err := config.LoadConfig()
		if err != nil {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				log.Println("Recovered in webhook goroutine:", r)
			}
		}()
		if cfg.WebhookUrl == "" {
			return
		}

		payload := buildWebhookPayload(client, msg)

		jsonData, err := json.Marshal(payload)
		if err != nil {
			log.Println("Webhook marshal error:", err)
			return
		}

		resp, err := http.Post(
			cfg.WebhookUrl,
			"application/json",
			bytes.NewBuffer(jsonData),
		)

		if err != nil {
			log.Println("Webhook POST error:", err)
			return
		}
		defer resp.Body.Close()

		slog.Info("Webhook sent",
			"message_id", msg.Info.ID,
			"chat_jid", chatJID,
		)
	}()
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// StoreMediaInfo Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	if isPostgres {
		_, err := store.db.Exec(
			`UPDATE messages 
					 SET url = $1, 
						 media_key = $2, 
						 file_sha256 = $3, 
						 file_enc_sha256 = $4, 
						 file_length = $5 
					 WHERE id = $6 AND chat_jid = $7`,
			url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
		)
		return err
	}

	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// GetMediaInfo Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	if isPostgres {
		err = store.db.QueryRow(
			`SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length 
					 FROM messages 
					 WHERE id = $1 AND chat_jid = $2`,
			id, chatJID,
		).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)
	} else {
		err = store.db.QueryRow(
			"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
			id, chatJID,
		).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)
	}

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetURL implements the DownloadableMessage interface
func (d *MediaDownloader) GetURL() string {
	return d.URL
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// Function to download media from a message
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	chatDir := fmt.Sprintf("store/%s", strings.ReplaceAll(chatJID, ":", "_"))
	localPath := ""

	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)

	if err != nil {
		if isPostgres {
			err = messageStore.db.QueryRow(
				`SELECT media_type, filename FROM messages WHERE id = $1 AND chat_jid = $2`,
				messageID, chatJID,
			).Scan(&mediaType, &filename)
		} else {
			err = messageStore.db.QueryRow(
				"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
				messageID, chatJID,
			).Scan(&mediaType, &filename)
		}

		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
	}

	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	if err := os.MkdirAll(chatDir, 0755); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	localPath = fmt.Sprintf("%s/%s", chatDir, filename)

	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	if _, err := os.Stat(localPath); err == nil {
		return true, mediaType, filename, absPath, nil
	}

	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}

	slog.Info("attempting to download media", "message_id", messageID, "chat_jid", chatJID)

	directPath := extractDirectPathFromURL(url)

	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "sticker":
		waMediaType = whatsmeow.MediaImage
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    directPath,
		MediaKey:      mediaKey,
		FileLength:    fileLength,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	mediaData, err := client.Download(context.Background(), downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}

	if err := os.WriteFile(localPath, mediaData, 0644); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	slog.Info("successfully downloaded media", "media_type", mediaType, "path", absPath, "bytes", len(mediaData))
	return true, mediaType, filename, absPath, nil
}

func extractDirectPathFromURL(url string) string {
	// The direct path is typically in the URL, we need to extract it
	// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/13812002_698058036224062_3424455886509161511_n.enc?ccb=11-4&oh=...

	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url // Return original URL if parsing fails
	}

	pathPart := parts[1]

	pathPart = strings.SplitN(pathPart, "?", 2)[0]

	return "/" + pathPart
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, cfg *config.Config, state *wastate.State) {
	apiMux := http.NewServeMux()

	// Send message
	// Group info — returns the full participant roster so the manager's mention
	// picker can offer every member, not just those who've spoken in the thread.
	apiMux.HandleFunc("/group-info", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JID string `json:"jid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		gjid, err := types.ParseJID(req.JID)
		if err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid jid: %v", err))
			return
		}
		info, err := client.GetGroupInfo(context.Background(), gjid)
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get group info: %v", err))
			return
		}
		participants := make([]map[string]interface{}, 0, len(info.Participants))
		for _, pp := range info.Participants {
			participants = append(participants, map[string]interface{}{
				"jid":          pp.JID.String(),
				"phone_number": pp.PhoneNumber.User,
				"lid":          pp.LID.String(),
				"is_admin":     pp.IsAdmin || pp.IsSuperAdmin,
				"display_name": pp.DisplayName,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"participants": participants})
	})

	// Edit — replace the text of one of our own sent messages (WhatsApp permits
	// edits within ~15 min). Updates our store so the change shows locally too.
	apiMux.HandleFunc("/edit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Recipient string `json:"recipient"`
			MessageID string `json:"message_id"`
			NewText   string `json:"new_text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.Recipient == "" || req.MessageID == "" || req.NewText == "" {
			respondError(w, http.StatusBadRequest, "recipient, message_id and new_text are required")
			return
		}
		chatJID, err := types.ParseJID(req.Recipient)
		if err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid recipient: %v", err))
			return
		}
		chatJID = normalizeUserJID(client, chatJID)
		editMsg := client.BuildEdit(chatJID, req.MessageID, &waE2E.Message{Conversation: proto.String(req.NewText)})
		if _, err := client.SendMessage(context.Background(), chatJID, editMsg); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to send edit: %v", err))
			return
		}
		if messageStore != nil {
			if err := messageStore.UpdateMessageContent(chatJID.String(), req.MessageID, req.NewText+" (edited)"); err != nil {
				slog.Warn("failed to update edited message in store", "error", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	})

	// Revoke — delete one of our own messages for everyone. Marks it deleted in
	// our store so the tombstone shows locally too.
	apiMux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Recipient string `json:"recipient"`
			MessageID string `json:"message_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.Recipient == "" || req.MessageID == "" {
			respondError(w, http.StatusBadRequest, "recipient and message_id are required")
			return
		}
		chatJID, err := types.ParseJID(req.Recipient)
		if err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid recipient: %v", err))
			return
		}
		chatJID = normalizeUserJID(client, chatJID)
		var sender types.JID
		if client.Store.ID != nil {
			sender = client.Store.ID.ToNonAD()
		}
		revokeMsg := client.BuildRevoke(chatJID, sender, req.MessageID)
		if _, err := client.SendMessage(context.Background(), chatJID, revokeMsg); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to send revoke: %v", err))
			return
		}
		if messageStore != nil {
			if err := messageStore.DeleteMessage(chatJID.String(), req.MessageID); err != nil {
				slog.Warn("failed to mark revoked message in store", "error", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	})

	// React — add or remove (empty emoji) a reaction on a target message.
	// Mirrors edit/revoke but builds a ReactionMessage. `sender` is the target
	// message's author (required for group messages; defaults to the chat for
	// 1:1). Set from_me=true to react to one of our own messages.
	apiMux.HandleFunc("/react", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Recipient string `json:"recipient"`
			MessageID string `json:"message_id"`
			Emoji     string `json:"emoji"`
			Sender    string `json:"sender"`
			FromMe    bool   `json:"from_me"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.Recipient == "" || req.MessageID == "" {
			respondError(w, http.StatusBadRequest, "recipient and message_id are required")
			return
		}
		chatJID, err := types.ParseJID(req.Recipient)
		if err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid recipient: %v", err))
			return
		}
		chatJID = normalizeUserJID(client, chatJID)
		// BuildReaction derives the key's FromMe from whether sender matches our
		// own JID, so own reactions need our JID; group reactions need the
		// participant; 1:1 defaults to the chat.
		var sender types.JID
		if req.FromMe {
			if client.Store.ID != nil {
				sender = client.Store.ID.ToNonAD()
			}
		} else if req.Sender != "" {
			sender, err = types.ParseJID(req.Sender)
			if err != nil {
				respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid sender: %v", err))
				return
			}
			sender = normalizeUserJID(client, sender)
		} else {
			sender = chatJID
		}
		reactMsg := client.BuildReaction(chatJID, sender, req.MessageID, req.Emoji)
		if _, err := client.SendMessage(context.Background(), chatJID, reactMsg); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to send reaction: %v", err))
			return
		}
		if messageStore != nil {
			var own string
			if client.Store.ID != nil {
				own = client.Store.ID.ToNonAD().String()
			}
			if req.Emoji == "" {
				if err := messageStore.DeleteReaction(req.MessageID, own); err != nil {
					slog.Warn("failed to delete reaction in store", "error", err)
				}
			} else if err := messageStore.StoreReaction(chatJID.String(), req.MessageID, own, req.Emoji, time.Now()); err != nil {
				slog.Warn("failed to store reaction in store", "error", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	})

	// POST /api/group/share-history
	//   { "jid": "...@g.us", "participants": ["60123...@s.whatsapp.net"], "count": 50 }
	// Sends the last `count` group messages to each listed member as a silent
	// HistorySyncNotification — WhatsApp's "share message history with new member"
	// mechanism. Nothing is posted into the group; the history surfaces only for
	// the recipients. Text only; media shows as a "[type]" placeholder.
	apiMux.HandleFunc("/group/share-history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JID          string   `json:"jid"`
			Participants []string `json:"participants"`
			Count        int      `json:"count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		gjid, err := types.ParseJID(req.JID)
		if err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid jid: %v", err))
			return
		}
		members, err := parseParticipantJIDs(req.Participants)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(members) == 0 {
			respondError(w, http.StatusBadRequest, "participants is required")
			return
		}
		if err := shareGroupHistory(client, messageStore, gjid, members, req.Count); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to share history: %v", err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	})

	apiMux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		if req.Message == "" && req.MediaPath == "" && req.MediaBase64 == "" {
			http.Error(w, "Message, media, or media_base64 is required", http.StatusBadRequest)
			return
		}

		slog.Info("received request to send message", "message", req.Message, "media_path", req.MediaPath)

		success, message, sentID := sendWhatsAppMessage(client, messageStore, req.Recipient, req.Message, req.MediaPath, req.MediaBase64, req.MediaFilename, req.MediaMimetype, req.MentionedJIDs, req.QuotedID, req.QuotedText, req.QuotedParticipant, req.IsForwarded, req.NoDelay)
		var reason string
		if !success {
			// Decode known WhatsApp nack codes (463 = reachout time-lock, 479 =
			// stale tcToken) into a readable message + a stable reason code, so
			// the logs and the manager UI explain the failure instead of just
			// echoing "server returned error 463".
			if r, friendly := classifySendError(message); r != "" {
				reason = r
				message = friendly
			}
		}
		slog.Info("message sent", "success", success, "message", message, "message_id", sentID, "reason", reason)
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
	})

	// Handler for downloading media
	apiMux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		success, mediaType, filename, path, err := downloadMedia(client, messageStore, req.MessageID, req.ChatJID)

		w.Header().Set("Content-Type", "application/json")

		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	})

	// Logout — unlinks this device from WhatsApp (removes it from the phone's
	// Linked Devices), not just a local session wipe.
	apiMux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := client.Logout(context.Background()); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	})

	// Manual chat resync — re-request history (re-seeds unread counts from
	// WhatsApp's own per-chat numbers) and re-fetch app state (replays read /
	// unread markers via the MarkChatAsRead handler). Both run async; the data
	// lands through the normal event handlers shortly after.
	apiMux.HandleFunc("/resync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if client.Store.ID == nil || !client.IsConnected() {
			respondError(w, http.StatusConflict, "not connected")
			return
		}
		go requestHistorySync(client)
		go func() {
			for _, name := range []appstate.WAPatchName{appstate.WAPatchRegular, appstate.WAPatchRegularHigh, appstate.WAPatchRegularLow} {
				if err := client.FetchAppState(context.Background(), name, true, false); err != nil {
					slog.Warn("resync: fetch app state failed", "patch", string(name), "err", err)
				}
			}
		}()
		respondJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	// Mark a chat as read — sends a real read receipt to WhatsApp (clearing the
	// unread state on the owner's phone, like opening the chat there would) and
	// zeroes the local unread badge so the manager's board reflects it at once.
	// Driven by the web UI when an operator opens a chat. No-op (still 200) when
	// the chat has no incoming message to receipt.
	apiMux.HandleFunc("/mark-read", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ChatJID string `json:"chat_jid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ChatJID == "" {
			respondError(w, http.StatusBadRequest, "chat_jid required")
			return
		}
		chat, err := types.ParseJID(body.ChatJID)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid chat_jid")
			return
		}
		// Clear the local badge unconditionally — the operator has seen the chat
		// in the manager regardless of whether we can send a receipt upstream.
		if err := messageStore.SetUnreadCount(chat.String(), 0); err != nil {
			slog.Warn("mark-read: clear unread failed", "chat", chat.String(), "err", err)
		}
		// Send the read receipt to WhatsApp so the phone (and the sender's ticks)
		// mirror it. Needs a concrete message ID + its sender; skip quietly if the
		// chat holds no incoming message or we're offline.
		if id, senderUser, ok := messageStore.LatestIncoming(chat.String()); ok && client.IsConnected() {
			var sender types.JID
			if chat.Server == types.GroupServer {
				sender = types.NewJID(senderUser, types.DefaultUserServer)
			}
			if err := client.MarkRead(context.Background(), []types.MessageID{id}, time.Now(), chat, sender); err != nil {
				slog.Warn("mark-read: send receipt failed", "chat", chat.String(), "err", err)
			}
		}
		respondJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	// Stream a typing indicator to a chat without sending a message. Driven by
	// the manager when an operator is typing in the composer, or by an upstream
	// caller (e.g. HR-AI) while it generates a reply — so the recipient sees
	// "typing…" during the genuine compose/think time. Composing presence expires
	// after ~10s, so the caller re-pings to keep it alive; "paused" clears it.
	// Cosmetic and best-effort: always 200 when a JID parses.
	apiMux.HandleFunc("/presence", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Recipient string `json:"recipient"`
			State     string `json:"state"` // "composing" | "paused"
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Recipient == "" {
			respondError(w, http.StatusBadRequest, "recipient required")
			return
		}
		rcpt := body.Recipient
		if !strings.Contains(rcpt, "@") {
			rcpt = rcpt + "@" + types.DefaultUserServer
		}
		jid, err := types.ParseJID(rcpt)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid recipient")
			return
		}
		jid = normalizeUserJID(client, jid)
		state := types.ChatPresencePaused
		if body.State == "composing" {
			state = types.ChatPresenceComposing
		}
		sendChatPresence(client, jid, state)
		respondJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	// List recent chats
	apiMux.HandleFunc("/chats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		q := r.URL.Query().Get("q")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		sortBy := r.URL.Query().Get("sort")

		if limit < 1 {
			limit = 30
		}
		if page < 0 {
			page = 0
		}

		var queryPtr *string
		if q != "" {
			queryPtr = &q
		}

		chats, err := messageStore.ListChats(queryPtr, limit, page, true, sortBy)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"chats": chats,
			"count": len(chats),
		})
	})

	// Get single chat
	apiMux.HandleFunc("/chats/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		jid := strings.TrimPrefix(r.URL.Path, "/chats/")
		if jid == "" {
			http.Error(w, "Missing chat JID", http.StatusBadRequest)
			return
		}

		chat, err := messageStore.GetChat(jid, true)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if chat == nil {
			http.Error(w, "Chat not found", http.StatusNotFound)
			return
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"chat": chat,
		})
	})

	// List messages (very flexible)
	apiMux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		params := ListMessagesParams{
			Limit:          50,
			Page:           0,
			IncludeContext: false,
			ContextBefore:  5,
			ContextAfter:   5,
		}

		q := r.URL.Query()
		if after := q.Get("after"); after != "" {
			params.After = after
		}
		if before := q.Get("before"); before != "" {
			params.Before = before
		}
		if chat := q.Get("chat"); chat != "" {
			params.ChatJid = &chat
		}
		if sender := q.Get("sender"); sender != "" {
			params.SenderPhoneNumber = &sender
		}
		if search := q.Get("search"); search != "" {
			params.Query = &search
		}
		if lim := q.Get("limit"); lim != "" {
			if v, err := strconv.Atoi(lim); err == nil && v > 0 {
				params.Limit = v
			}
		}
		if pg := q.Get("page"); pg != "" {
			if v, err := strconv.Atoi(pg); err == nil && v >= 0 {
				params.Page = v
			}
		}
		if ctx := q.Get("context"); ctx == "true" {
			params.IncludeContext = true
		}

		result, err := messageStore.ListMessages(params)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"result": result,
		})
	})

	// Get message + context
	apiMux.HandleFunc("/messages/context/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		messageID := strings.TrimPrefix(r.URL.Path, "/messages/context/")
		if messageID == "" {
			http.Error(w, "Missing message ID", http.StatusBadRequest)
			return
		}

		before, _ := strconv.Atoi(r.URL.Query().Get("before"))
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		if before == 0 {
			before = 6
		}
		if after == 0 {
			after = 6
		}

		ctx, err := messageStore.GetMessageContext(messageID, before, after)
		if err != nil {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}

		respondJSON(w, http.StatusOK, ctx)
	})

	// Search contacts
	apiMux.HandleFunc("/contacts/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		query := r.URL.Query().Get("q")
		if query == "" {
			http.Error(w, "Missing search query (?q=...)", http.StatusBadRequest)
			return
		}

		contacts, err := messageStore.SearchContacts(query)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"contacts": contacts,
		})
	})

	// GET /api/group/participants?jid=<groupJID>
	// Full member roster for a group, so the manager's @-mention picker can offer
	// silent members too (not just those who have spoken in synced history).
	apiMux.HandleFunc("/group/participants", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jidStr := r.URL.Query().Get("jid")
		if jidStr == "" {
			http.Error(w, "Missing group jid (?jid=...)", http.StatusBadRequest)
			return
		}
		gjid, err := types.ParseJID(jidStr)
		if err != nil {
			http.Error(w, "Invalid jid", http.StatusBadRequest)
			return
		}
		info, err := client.GetGroupInfo(context.Background(), gjid)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}

		type participant struct {
			Number string `json:"number"`
			Name   string `json:"name"`
			Admin  bool   `json:"admin"`
		}
		out := make([]participant, 0, len(info.Participants))
		for _, p := range info.Participants {
			// Prefer the real phone number; fall back to resolving the primary
			// (possibly @lid) JID to a phone number via the LID map. The number's
			// User part matches the manager's contacts-map keys and the bare-number
			// form its mention sender resolves against.
			pjid := p.PhoneNumber
			if pjid.User == "" {
				pjid = normalizeUserJID(client, p.JID)
			}
			number := pjid.User
			if number == "" {
				continue
			}
			name := ""
			if c, cErr := client.Store.Contacts.GetContact(context.Background(), pjid); cErr == nil {
				switch {
				case c.FullName != "":
					name = c.FullName
				case c.PushName != "":
					name = c.PushName
				case c.BusinessName != "":
					name = c.BusinessName
				case c.FirstName != "":
					name = c.FirstName
				}
			}
			// Members who hide their phone number expose only an obfuscated
			// DisplayName — use it so they don't surface as a bare LID number.
			if name == "" && p.DisplayName != "" {
				name = p.DisplayName
			}
			out = append(out, participant{Number: number, Name: name, Admin: p.IsAdmin || p.IsSuperAdmin})
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"participants": out,
		})
	})

	// ── Group management (write) ───────────────────────────────────────────
	// All POST + JSON. JIDs may be passed as bare numbers ("60123…"), full user
	// JIDs ("…@s.whatsapp.net") or LID JIDs ("…@lid"); parseParticipantJID
	// normalizes the first two and passes the rest through.

	// POST /api/group/create  { "name": "...", "participants": ["60123...", ...] }
	// Creates a new group with us as owner. Name is capped at 25 chars by
	// WhatsApp (longer → 406). Returns the new group's jid + info.
	apiMux.HandleFunc("/group/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Name         string   `json:"name"`
			Participants []string `json:"participants"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request format")
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			respondError(w, http.StatusBadRequest, "name is required")
			return
		}
		parts, err := parseParticipantJIDs(req.Participants)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		info, err := client.CreateGroup(context.Background(), whatsmeow.ReqCreateGroup{
			Name:         req.Name,
			Participants: parts,
		})
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create group: %v", err))
			return
		}
		respondJSON(w, http.StatusOK, groupInfoJSON(info))
	})

	// POST /api/group/update-participants
	//   { "jid": "...@g.us", "participants": ["60123...", ...], "action": "add" }
	// action ∈ add | remove | promote | demote. Returns a per-participant result
	// so callers can see whose add failed (e.g. privacy settings → needs invite).
	apiMux.HandleFunc("/group/update-participants", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JID          string   `json:"jid"`
			Participants []string `json:"participants"`
			Action       string   `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request format")
			return
		}
		gjid, err := types.ParseJID(req.JID)
		if err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid jid: %v", err))
			return
		}
		var action whatsmeow.ParticipantChange
		switch req.Action {
		case "add":
			action = whatsmeow.ParticipantChangeAdd
		case "remove":
			action = whatsmeow.ParticipantChangeRemove
		case "promote":
			action = whatsmeow.ParticipantChangePromote
		case "demote":
			action = whatsmeow.ParticipantChangeDemote
		default:
			respondError(w, http.StatusBadRequest, "action must be one of add|remove|promote|demote")
			return
		}
		parts, err := parseParticipantJIDs(req.Participants)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(parts) == 0 {
			respondError(w, http.StatusBadRequest, "participants is required")
			return
		}
		res, err := client.UpdateGroupParticipants(context.Background(), gjid, parts, action)
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update participants: %v", err))
			return
		}
		results := make([]map[string]interface{}, 0, len(res))
		for _, p := range res {
			entry := map[string]interface{}{
				"jid":          p.JID.String(),
				"phone_number": p.PhoneNumber.User,
				"lid":          p.LID.String(),
				"is_admin":     p.IsAdmin || p.IsSuperAdmin,
				// 0 = success; non-zero = WhatsApp error code (e.g. 403 = blocked
				// by the target's privacy settings, must invite instead).
				"error": p.Error,
			}
			if p.AddRequest != nil {
				// Direct add refused — caller should send the invite link instead.
				entry["needs_invite"] = true
			}
			results = append(results, entry)
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"results": results})
	})

	// POST /api/group/name  { "jid": "...@g.us", "name": "New name" }
	apiMux.HandleFunc("/group/name", func(w http.ResponseWriter, r *http.Request) {
		gjid, ok := decodeGroupJID(w, r)
		if !ok {
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.TrimSpace(req.Name) == "" {
			respondError(w, http.StatusBadRequest, "name is required")
			return
		}
		if err := client.SetGroupName(context.Background(), gjid, req.Name); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to set name: %v", err))
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	})

	// POST /api/group/topic  { "jid": "...@g.us", "topic": "Group description" }
	apiMux.HandleFunc("/group/topic", func(w http.ResponseWriter, r *http.Request) {
		gjid, ok := decodeGroupJID(w, r)
		if !ok {
			return
		}
		var req struct {
			Topic string `json:"topic"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Empty previousID/newID lets whatsmeow assign a fresh topic id.
		if err := client.SetGroupTopic(context.Background(), gjid, "", "", req.Topic); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to set topic: %v", err))
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	})

	// POST /api/group/announce  { "jid": "...@g.us", "announce": true }
	// announce=true → only admins can send messages.
	apiMux.HandleFunc("/group/announce", func(w http.ResponseWriter, r *http.Request) {
		gjid, ok := decodeGroupJID(w, r)
		if !ok {
			return
		}
		var req struct {
			Announce bool `json:"announce"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := client.SetGroupAnnounce(context.Background(), gjid, req.Announce); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to set announce: %v", err))
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	})

	// POST /api/group/locked  { "jid": "...@g.us", "locked": true }
	// locked=true → only admins can edit group info.
	apiMux.HandleFunc("/group/locked", func(w http.ResponseWriter, r *http.Request) {
		gjid, ok := decodeGroupJID(w, r)
		if !ok {
			return
		}
		var req struct {
			Locked bool `json:"locked"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := client.SetGroupLocked(context.Background(), gjid, req.Locked); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to set locked: %v", err))
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	})

	// POST /api/group/leave  { "jid": "...@g.us" }
	apiMux.HandleFunc("/group/leave", func(w http.ResponseWriter, r *http.Request) {
		gjid, ok := decodeGroupJID(w, r)
		if !ok {
			return
		}
		if err := client.LeaveGroup(context.Background(), gjid); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to leave group: %v", err))
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
	})

	// POST /api/group/invite-link  { "jid": "...@g.us", "reset": false }
	// Returns the group's invite URL. reset=true revokes the old link first.
	apiMux.HandleFunc("/group/invite-link", func(w http.ResponseWriter, r *http.Request) {
		gjid, ok := decodeGroupJID(w, r)
		if !ok {
			return
		}
		var req struct {
			Reset bool `json:"reset"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		link, err := client.GetGroupInviteLink(context.Background(), gjid, req.Reset)
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to get invite link: %v", err))
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"link": link})
	})

	// GET /api/groups — every group this number is a member of.
	apiMux.HandleFunc("/groups", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		groups, err := client.GetJoinedGroups(context.Background())
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list groups: %v", err))
			return
		}
		out := make([]map[string]interface{}, 0, len(groups))
		for _, g := range groups {
			out = append(out, groupInfoJSON(g))
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"groups": out})
	})

	// GET /api/direct-contacts/:phone/chat
	// Find the 1:1 (direct) chat for a given phone number
	apiMux.HandleFunc("/direct-contacts/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/direct-contacts/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 || parts[1] != "chat" {
			http.Error(w, "Invalid path. Use /api/direct-contacts/{phone}/chat", http.StatusBadRequest)
			return
		}

		phone := parts[0]
		if phone == "" {
			http.Error(w, "Phone number is required", http.StatusBadRequest)
			return
		}

		chat, err := messageStore.GetDirectChatByContact(phone)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if chat == nil {
			respondJSON(w, http.StatusNotFound, map[string]string{
				"error": "No direct chat found for this phone number",
			})
			return
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"chat": chat,
		})
	})

	// GET /api/contacts/:jid/chats
	// List all chats where this contact (by JID) appears as sender or in group
	apiMux.HandleFunc("/contacts/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/contacts/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 || parts[1] != "chats" {
			// Skip if not this endpoint (previous handler already took /chat)
			return
		}

		jid := parts[0]
		if jid == "" {
			http.Error(w, "JID is required", http.StatusBadRequest)
			return
		}

		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if limit <= 0 {
			limit = 20
		}
		if page < 0 {
			page = 0
		}

		chats, err := messageStore.GetContactChats(jid, limit, page)
		if err != nil {
			respondError(w, http.StatusInternalServerError, err.Error())
			return
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"chats": chats,
			"count": len(chats),
		})
	})

	apiMux.HandleFunc("/auth/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{
			"connected":        state.Connected(),
			"logged_in":        state.LoggedIn(),
			"pairing_required": state.PairingRequired(),
			"wa_version":       state.WAVersion(),
		})
	})

	apiMux.HandleFunc("/auth/pairing-qr", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		png := state.PairingQRPNG()
		if png == nil {
			http.Error(w, "no pairing QR available; client is logged in or has not started pairing yet", http.StatusGone)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(png)
	})

	// Pair via phone number — caller supplies phone in E.164 format; bridge
	// returns the 8-char code the user types on their WhatsApp phone under
	// Linked Devices → Link with phone number.
	apiMux.HandleFunc("/auth/pair-phone", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Phone string `json:"phone"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Phone == "" {
			http.Error(w, "phone required", http.StatusBadRequest)
			return
		}
		slog.Info("pair-phone requested", "phone", body.Phone,
			"connected", client.IsConnected(), "loggedIn", client.IsLoggedIn(),
			"hasStoreID", client.Store.ID != nil)
		if client.Store.ID != nil {
			slog.Warn("pair-phone refused: already paired", "jid", client.Store.ID.String())
			http.Error(w, "already paired", http.StatusConflict)
			return
		}
		if !client.IsConnected() {
			slog.Warn("pair-phone: client not connected yet")
		}
		code, err := client.PairPhone(context.Background(), body.Phone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
		if err != nil {
			slog.Error("pair-phone PairPhone failed", "err", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		slog.Info("pair-phone code generated", "code", code)
		respondJSON(w, http.StatusOK, map[string]string{"code": code})
	})

	// --- WhatsApp passkey (Shortcake) linking relay ---------------------------
	// GET /auth/passkey-request → pending WebAuthn challenge (204 if none, 409 on error).
	apiMux.HandleFunc("/auth/passkey-request", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		passkeyState.mu.Lock()
		req, errMsg := passkeyState.request, passkeyState.errMsg
		passkeyState.mu.Unlock()
		if errMsg != "" {
			respondJSON(w, http.StatusConflict, map[string]string{"error": errMsg})
			return
		}
		if req == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		respondJSON(w, http.StatusOK, req)
	})

	// POST /auth/passkey-response → body = WebAuthnResponse JSON; forwards to server.
	apiMux.HandleFunc("/auth/passkey-response", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var resp types.WebAuthnResponse
		if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
			http.Error(w, "invalid WebAuthnResponse: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := client.SendPasskeyResponse(context.Background(), &resp); err != nil {
			slog.Error("SendPasskeyResponse failed", "err", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		slog.Info("passkey response submitted to server")
		respondJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	})

	// GET /auth/passkey-code → cross-device confirmation code (204 until ready).
	apiMux.HandleFunc("/auth/passkey-code", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		passkeyState.mu.Lock()
		code, skip, errMsg := passkeyState.code, passkeyState.skipUX, passkeyState.errMsg
		passkeyState.mu.Unlock()
		if errMsg != "" {
			respondJSON(w, http.StatusConflict, map[string]string{"error": errMsg})
			return
		}
		if code == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"code": code, "skip_handoff_ux": skip})
	})

	// POST /auth/passkey-confirm → SendPasskeyConfirmation (finishes linking).
	apiMux.HandleFunc("/auth/passkey-confirm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := client.SendPasskeyConfirmation(context.Background()); err != nil {
			slog.Error("SendPasskeyConfirmation failed", "err", err.Error())
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		passkeyState.mu.Lock()
		passkeyState.request = nil
		passkeyState.code = ""
		passkeyState.mu.Unlock()
		slog.Info("passkey confirmation submitted; linking should complete")
		respondJSON(w, http.StatusOK, map[string]string{"status": "confirmed"})
	})

	// Authentication
	protected := auth.JwtAuthMiddleware(cfg, apiMux)
	http.Handle("/api/", http.StripPrefix("/api", protected))
	http.Handle("/auth/login", auth.LoginHandler(cfg))

	serverAddr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	slog.Info("starting REST API server", "addr", serverAddr)

	go func() {
		if err := http.ListenAndServe(serverAddr, nil); err != nil {
			slog.Error("rest api server error", "err", err)
		}
	}()
}

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func respondError(w http.ResponseWriter, status int, msg string) {
	respondJSON(w, status, map[string]string{
		"error": msg,
	})
}

// parseParticipantJID turns a caller-supplied identifier into a types.JID.
// Bare numbers ("60123…") become user JIDs; anything already qualified
// ("…@s.whatsapp.net", "…@lid") is parsed as-is so the caller can target a
// member by their LID when they hide their phone number.
func parseParticipantJID(raw string) (types.JID, error) {
	s := strings.TrimSpace(strings.TrimPrefix(raw, "@"))
	if s == "" {
		return types.JID{}, fmt.Errorf("empty participant")
	}
	if strings.Contains(s, "@") {
		return types.ParseJID(s)
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
	if digits == "" {
		return types.JID{}, fmt.Errorf("invalid participant: %q", raw)
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}

func parseParticipantJIDs(raws []string) ([]types.JID, error) {
	out := make([]types.JID, 0, len(raws))
	for _, raw := range raws {
		jid, err := parseParticipantJID(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, jid)
	}
	return out, nil
}

// decodeGroupJID is the shared preamble for the single-group write handlers:
// enforces POST and pulls a valid group jid out of the JSON body. It does not
// consume the rest of the body — handlers re-decode for their own fields, which
// is fine since each reads a distinct key.
func decodeGroupJID(w http.ResponseWriter, r *http.Request) (types.JID, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return types.JID{}, false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to read body")
		return types.JID{}, false
	}
	// Re-seat the body so the handler's own json.Decode can read it again.
	r.Body = io.NopCloser(bytes.NewReader(body))
	var head struct {
		JID string `json:"jid"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request format")
		return types.JID{}, false
	}
	gjid, err := types.ParseJID(head.JID)
	if err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid jid: %v", err))
		return types.JID{}, false
	}
	return gjid, true
}

// groupInfoJSON serializes a *types.GroupInfo to the JSON shape the manager
// consumes (jid + name + topic + participant roster).
func groupInfoJSON(info *types.GroupInfo) map[string]interface{} {
	participants := make([]map[string]interface{}, 0, len(info.Participants))
	for _, pp := range info.Participants {
		participants = append(participants, map[string]interface{}{
			"jid":          pp.JID.String(),
			"phone_number": pp.PhoneNumber.User,
			"lid":          pp.LID.String(),
			"is_admin":     pp.IsAdmin || pp.IsSuperAdmin,
			"display_name": pp.DisplayName,
		})
	}
	return map[string]interface{}{
		"jid":          info.JID.String(),
		"name":         info.Name,
		"topic":        info.Topic,
		"owner_jid":    info.OwnerJID.String(),
		"participants": participants,
	}
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// The chat with your own number is the "Message yourself" chat — label it
	// clearly instead of showing your own phone number.
	if client.Store.ID != nil && jid.Server != "g.us" && jid.User == client.Store.ID.User {
		return "Me"
	}

	nameQ := "SELECT name FROM chats WHERE jid = ?"
	if isPostgres {
		nameQ = "SELECT name FROM chats WHERE jid = $1"
	}
	var existingName string
	err := messageStore.db.QueryRow(nameQ, chatJID).Scan(&existingName)
	// Reuse a previously stored name only if it's a *real* name — not the bare
	// phone number / sender digits we fall back to when no contact name is known.
	// Without this, a chat first seen before its contact name was available stays
	// stuck showing the number forever (e.g. it never picks up a later-saved contact).
	if err == nil && existingName != "" && existingName != jid.User && existingName != sender {
		logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	var name string

	if jid.Server == "g.us" {
		logger.Infof("Getting name for group: %s", chatJID)

		if conversation != nil {
			var displayName, convName *string
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Infof("Using group name: %s", name)
	} else {
		logger.Infof("Getting name for contact: %s", chatJID)

		// Mirror WhatsApp's own display priority: the name you saved in your
		// address book (FullName) wins, then the contact's self-set WhatsApp name
		// (PushName), then a business name, then their first name. Only when none
		// of those exist do we fall back to the bare number.
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil {
			switch {
			case contact.FullName != "":
				name = contact.FullName
			case contact.PushName != "":
				name = contact.PushName
			case contact.BusinessName != "":
				name = contact.BusinessName
			case contact.FirstName != "":
				name = contact.FirstName
			}
		}
		if name == "" {
			if sender != "" {
				name = sender
			} else {
				name = jid.User
			}
		}

		logger.Infof("Using contact name: %s", name)
	}

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	slog.Info("received history sync event", "conversations", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}
		jid = normalizeUserJID(client, jid)
		chatJID = jid.String()

		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		messages := conversation.Messages
		if len(messages) > 0 {
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)
			// Seed the unread badge from WhatsApp's own per-chat count so the
			// manager shows true unread state right after a (re)sync. A chat the
			// user explicitly marked unread reports count 0 but MarkedAsUnread.
			unreadSeed := int(conversation.GetUnreadCount())
			if unreadSeed == 0 && conversation.GetMarkedAsUnread() {
				unreadSeed = 1
			}
			if err := messageStore.SetUnreadCount(chatJID, unreadSeed); err != nil {
				logger.Warnf("Failed to set unread count: %v", err)
			}

			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}
				if content == "" {
					content = captionOf(msg.Message.Message)
				}

				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				logger.Infof("Message content: %v, Media Type: %v", content, mediaType)

				if content == "" && mediaType == "" {
					continue
				}

				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						if pJid, err := types.ParseJID(*msg.Message.Key.Participant); err == nil {
							sender = normalizeUserJID(client, pJid).User
						} else {
							sender = *msg.Message.Key.Participant
						}
					} else if isFromMe {
						sender = client.Store.ID.User
					} else if p := msg.Message.GetParticipant(); p != "" {
						// Group history entries frequently carry the participant on
						// the WebMessageInfo itself rather than the message key.
						// Without this, every history-synced group message falls back
						// to the group JID as its sender, so per-participant name and
						// avatar can never resolve in the UI.
						if pJid, err := types.ParseJID(p); err == nil {
							sender = normalizeUserJID(client, pJid).User
						} else {
							sender = p
						}
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				quotedID, quotedSender, quotedContent := quotedFromMessage(unwrapMessage(msg.Message.Message))
				isForwarded := contextInfoOf(unwrapMessage(msg.Message.Message)).GetIsForwarded()

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
					quotedID,
					quotedSender,
					quotedContent,
					isForwarded,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					if mediaType != "" {
						logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
					} else {
						logger.Infof("Stored message: [%s] %s -> %s: %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
					}
				}
			}
		}
	}

	slog.Info("history sync complete", "stored_messages", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		slog.Error("client is not initialized, cannot request history sync")
		return
	}

	if !client.IsConnected() {
		slog.Warn("client is not connected to whatsapp")
		return
	}

	if client.Store.ID == nil {
		slog.Warn("client is not logged in, please scan the qr code")
		return
	}

	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		slog.Error("failed to build history sync request")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		slog.Error("failed to request history sync", "err", err)
	} else {
		slog.Info("history sync requested, waiting for server response")
	}
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position
	var lastGranule uint64
	var sampleRate uint32 = 48000 // Default Opus sample rate
	var preSkip uint16 = 0
	var foundOpusHead bool

	for i := 0; i < len(data); {
		if i+27 >= len(data) {
			break
		}

		if string(data[i:i+4]) != "OggS" {
			i++
			continue
		}

		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		if !foundOpusHead && pageSeqNum <= 1 {
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 && headPos+12 < len(pageData) {
				headPos += 8
				if headPos+12 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
					slog.Info("found OpusHead", "sample_rate", sampleRate, "pre_skip", preSkip)
				}
			}
		}

		if granulePos != 0 {
			lastGranule = granulePos
		}

		i += pageSize
	}

	if !foundOpusHead {
		slog.Warn("opushead not found, using default values")
	}

	if lastGranule > 0 {
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		slog.Info("calculated Opus duration from granule", "duration_seconds", durationSeconds, "last_granule", lastGranule)
	} else {
		slog.Warn("no valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0
		duration = uint32(durationEstimate)
	}

	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	waveform = placeholderWaveform(duration)

	slog.Info("ogg opus analysis complete", "size_bytes", len(data), "duration_sec", duration, "waveform_bytes", len(waveform))

	return duration, waveform, nil
}

func placeholderWaveform(duration uint32) []byte {
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	source := rand.NewSource(int64(duration))
	rng := rand.New(source)

	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		pos := float64(i) / float64(waveformLength)

		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		val += (rng.Float64() - 0.5) * 15

		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		val = val + 50

		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}

func (store *MessageStore) GetSenderName(senderJID string) string {
	var name string
	var err error

	if isPostgres {
		err = store.db.QueryRow(
			"SELECT name FROM chats WHERE jid = $1 LIMIT 1",
			senderJID,
		).Scan(&name)

		if err == nil && name != "" {
			return name
		}

		phonePart := senderJID
		if idx := strings.Index(senderJID, "@"); idx > 0 {
			phonePart = senderJID[:idx]
		}

		err = store.db.QueryRow(
			"SELECT name FROM chats WHERE jid LIKE $1 LIMIT 1",
			"%"+phonePart+"%",
		).Scan(&name)

		if err == nil && name != "" {
			return name
		}

		return senderJID
	}

	err = store.db.QueryRow(
		"SELECT name FROM chats WHERE jid = ? LIMIT 1",
		senderJID,
	).Scan(&name)

	if err == nil && name != "" {
		return name
	}

	phonePart := senderJID
	if idx := strings.Index(senderJID, "@"); idx > 0 {
		phonePart = senderJID[:idx]
	}

	err = store.db.QueryRow(
		"SELECT name FROM chats WHERE jid LIKE ? LIMIT 1",
		"%"+phonePart+"%",
	).Scan(&name)

	if err == nil && name != "" {
		return name
	}

	return senderJID
}

func (store *MessageStore) FormatMessage(msg MessageInteraction, showChatInfo bool) string {
	var sb strings.Builder

	ts := msg.Timestamp.Format("2006-01-02 15:04:05")

	if showChatInfo && msg.ChatName != "" {
		sb.WriteString(fmt.Sprintf("[%s] Chat: %s ", ts, msg.ChatName))
	} else {
		sb.WriteString(fmt.Sprintf("[%s] ", ts))
	}

	prefix := ""
	if msg.MediaType != "" {
		prefix = fmt.Sprintf("[%s - Message ID: %s - Chat JID: %s] ", msg.MediaType, msg.ID, msg.ChatJID)
	}

	senderName := "Me"
	if !msg.IsFromMe {
		senderName = store.GetSenderName(msg.Sender)
	}

	sb.WriteString(fmt.Sprintf("From: %s: %s%s\n", senderName, prefix, msg.Content))

	// Reactions on their own indented sub-line so they're clearly distinct from
	// the message text (which may itself be long or contain brackets/pipes).
	if rx := store.GetReactions(msg.ID); len(rx) > 0 {
		sb.WriteString("    ⤷ reactions: " + strings.Join(rx, ", ") + "\n")
	}

	return sb.String()
}

func (store *MessageStore) FormatMessagesList(messages []MessageInteraction, showChatInfo bool) string {
	if len(messages) == 0 {
		return "No messages to display.\n"
	}
	var sb strings.Builder
	for _, m := range messages {
		sb.WriteString(store.FormatMessage(m, showChatInfo))
	}
	return sb.String()
}

func (store *MessageStore) ListMessages(s ListMessagesParams) (string, error) {
	var args []any
	var where []string

	placeholder := func(n int) string {
		if isPostgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}

	q := `
        SELECT 
            m.timestamp, m.sender, c.name, m.content, m.is_from_me, 
            c.jid, m.id, m.media_type
        FROM messages m
        JOIN chats c ON m.chat_jid = c.jid
    `

	if s.After != "" {
		t, err := time.Parse(time.RFC3339, s.After)
		if err != nil {
			return "", fmt.Errorf("invalid after format: %w", err)
		}
		where = append(where, "m.timestamp > "+placeholder(len(args)+1))
		args = append(args, t)
	}

	if s.Before != "" {
		t, err := time.Parse(time.RFC3339, s.Before)
		if err != nil {
			return "", fmt.Errorf("invalid before format: %w", err)
		}
		where = append(where, "m.timestamp < "+placeholder(len(args)+1))
		args = append(args, t)
	}

	if s.SenderPhoneNumber != nil && *s.SenderPhoneNumber != "" {
		where = append(where, "m.sender = "+placeholder(len(args)+1))
		args = append(args, *s.SenderPhoneNumber)
	}

	if s.ChatJid != nil && *s.ChatJid != "" {
		where = append(where, "m.chat_jid = "+placeholder(len(args)+1))
		args = append(args, *s.ChatJid)
	}

	if s.Query != nil && *s.Query != "" {
		where = append(where, "LOWER(m.content) LIKE LOWER("+placeholder(len(args)+1)+")")
		args = append(args, "%"+*s.Query+"%")
	}

	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}

	q += " ORDER BY m.timestamp DESC"
	q += " LIMIT " + placeholder(len(args)+1)
	args = append(args, s.Limit)

	q += " OFFSET " + placeholder(len(args)+1)
	args = append(args, s.Page*s.Limit)

	rows, err := store.db.Query(q, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var msgs []MessageInteraction
	for rows.Next() {
		var (
			ts        time.Time
			sender    string
			chatName  sql.NullString
			content   string
			isFromMe  bool
			cJid      string
			id        string
			mediaType sql.NullString
		)

		if err := rows.Scan(&ts, &sender, &chatName, &content, &isFromMe, &cJid, &id, &mediaType); err != nil {
			log.Printf("scan error: %v", err)
			continue
		}

		m := MessageInteraction{
			Timestamp: ts.In(displayLoc),
			Sender:    sender,
			Content:   content,
			IsFromMe:  isFromMe,
			ChatJID:   cJid,
			ID:        id,
		}

		if chatName.Valid {
			m.ChatName = chatName.String
		}
		if mediaType.Valid {
			m.MediaType = mediaType.String
		}

		msgs = append(msgs, m)
	}

	if s.IncludeContext && len(msgs) > 0 {
		var all []MessageInteraction
		for _, m := range msgs {
			ctx, err := store.GetMessageContext(m.ID, s.ContextBefore, s.ContextAfter)
			if err != nil {
				log.Printf("context error for %s: %v", m.ID, err)
				continue
			}
			all = append(all, ctx.Before...)
			all = append(all, ctx.Message)
			all = append(all, ctx.After...)
		}
		return store.FormatMessagesList(all, true), nil
	}

	return store.FormatMessagesList(msgs, true), nil
}

func (store *MessageStore) GetMessageContext(messageID string, before, after int) (MessageContext, error) {
	placeholder := func(n int) string {
		if isPostgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}

	// --- Fetch the target message ---
	q := `
        SELECT m.timestamp, m.sender, c.name, m.content, m.is_from_me,
               c.jid, m.id, m.media_type, m.chat_jid
        FROM messages m
        JOIN chats c ON m.chat_jid = c.jid
        WHERE m.id = ` + placeholder(1)

	var (
		ts       time.Time
		sender   string
		chatName sql.NullString
		content  string
		isFromMe bool
		chatJid  string
		id       string
		media    sql.NullString
		chatJid2 string
	)

	row := store.db.QueryRow(q, messageID)
	if err := row.Scan(&ts, &sender, &chatName, &content, &isFromMe, &chatJid, &id, &media, &chatJid2); err != nil {
		if err == sql.ErrNoRows {
			return MessageContext{}, fmt.Errorf("message not found: %s", messageID)
		}
		return MessageContext{}, err
	}

	target := MessageInteraction{
		Timestamp: ts.In(displayLoc),
		Sender:    sender,
		Content:   content,
		IsFromMe:  isFromMe,
		ChatJID:   chatJid,
		ID:        id,
	}
	if chatName.Valid {
		target.ChatName = chatName.String
	}
	if media.Valid {
		target.MediaType = media.String
	}

	qBefore := `
        SELECT m.timestamp, m.sender, c.name, m.content, m.is_from_me,
               c.jid, m.id, m.media_type
        FROM messages m
        JOIN chats c ON m.chat_jid = c.jid
        WHERE m.chat_jid = ` + placeholder(1) + `
          AND m.timestamp < ` + placeholder(2) + `
        ORDER BY m.timestamp DESC
        LIMIT ` + placeholder(3)

	bRows, err := store.db.Query(qBefore, chatJid2, ts, before)
	if err != nil {
		return MessageContext{}, err
	}
	defer bRows.Close()

	var beforeMsgs []MessageInteraction
	for bRows.Next() {
		var (
			bts      time.Time
			bsender  sql.NullString
			bname    sql.NullString
			bcontent string
			bif      bool
			bjid     string
			bid      string
			bmedia   sql.NullString
		)

		if err := bRows.Scan(&bts, &bsender, &bname, &bcontent, &bif, &bjid, &bid, &bmedia); err != nil {
			continue
		}

		m := MessageInteraction{
			Timestamp: bts.In(displayLoc),
			Sender:    bsender.String,
			Content:   bcontent,
			IsFromMe:  bif,
			ChatJID:   bjid,
			ID:        bid,
		}
		if bname.Valid {
			m.ChatName = bname.String
		}
		if bmedia.Valid {
			m.MediaType = bmedia.String
		}
		beforeMsgs = append(beforeMsgs, m)
	}

	qAfter := `
        SELECT m.timestamp, m.sender, c.name, m.content, m.is_from_me,
               c.jid, m.id, m.media_type
        FROM messages m
        JOIN chats c ON m.chat_jid = c.jid
        WHERE m.chat_jid = ` + placeholder(1) + `
          AND m.timestamp > ` + placeholder(2) + `
        ORDER BY m.timestamp ASC
        LIMIT ` + placeholder(3)

	aRows, err := store.db.Query(qAfter, chatJid2, ts, after)
	if err != nil {
		return MessageContext{}, err
	}
	defer aRows.Close()

	var afterMsgs []MessageInteraction
	for aRows.Next() {
		var (
			ats      time.Time
			asender  sql.NullString
			aname    sql.NullString
			acontent string
			aif      bool
			ajid     string
			aid      string
			amedia   sql.NullString
		)

		if err := aRows.Scan(&ats, &asender, &aname, &acontent, &aif, &ajid, &aid, &amedia); err != nil {
			continue
		}

		m := MessageInteraction{
			Timestamp: ats.In(displayLoc),
			Sender:    asender.String,
			Content:   acontent,
			IsFromMe:  aif,
			ChatJID:   ajid,
			ID:        aid,
		}
		if aname.Valid {
			m.ChatName = aname.String
		}
		if amedia.Valid {
			m.MediaType = amedia.String
		}
		afterMsgs = append(afterMsgs, m)
	}

	return MessageContext{
		Message: target,
		Before:  beforeMsgs,
		After:   afterMsgs,
	}, nil
}

func (store *MessageStore) ListChats(
	query *string,
	limit, page int,
	includeLastMessage bool,
	sortBy string,
) ([]Chat, error) {

	placeholder := func(n int) string {
		if isPostgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}

	q := `
        SELECT 
            c.jid, c.name, c.last_message_time,
            m.content AS last_message,
            m.sender AS last_sender,
            m.is_from_me AS last_is_from_me
        FROM chats c
    `

	if includeLastMessage {
		q += `
            LEFT JOIN messages m 
            ON c.jid = m.chat_jid 
            AND c.last_message_time = m.timestamp
        `
	}

	var args []any
	var where []string

	if query != nil && *query != "" {
		if isPostgres {
			where = append(where, "(LOWER(c.name) LIKE LOWER("+placeholder(len(args)+1)+") OR c.jid LIKE "+placeholder(len(args)+2)+")")
		} else {
			where = append(where, "(LOWER(c.name) LIKE LOWER(?) OR c.jid LIKE ?)")
		}
		args = append(args, "%"+*query+"%", "%"+*query+"%")
	}

	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}

	order := "c.last_message_time DESC"
	if sortBy == "name" {
		order = "c.name ASC"
	}
	q += " ORDER BY " + order

	cast := ""
	if isPostgres {
		cast = "::int"
	}
	q += " LIMIT " + placeholder(len(args)+1) + cast
	args = append(args, limit)

	q += " OFFSET " + placeholder(len(args)+1) + cast
	args = append(args, page*limit)

	rows, err := store.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var (
			jid         string
			name        sql.NullString
			lastMsgTime sql.NullTime
			lastMsg     sql.NullString
			lastSender  sql.NullString
			lastFromMe  sql.NullBool
		)

		if err := rows.Scan(&jid, &name, &lastMsgTime, &lastMsg, &lastSender, &lastFromMe); err != nil {
			log.Printf("list_chats scan error: %v", err)
			continue
		}

		c := Chat{JID: jid}

		if name.Valid {
			c.Name = name.String
		}
		if lastMsgTime.Valid {
			c.LastMessageTime = lastMsgTime.Time.In(displayLoc)
		}
		if lastMsg.Valid {
			c.LastMessage = lastMsg.String
		}
		if lastSender.Valid {
			c.LastSender = lastSender.String
		}
		if lastFromMe.Valid {
			c.LastIsFromMe = lastFromMe.Bool
		}

		chats = append(chats, c)
	}

	return chats, nil
}

func (store *MessageStore) SearchContacts(query string) ([]Contact, error) {
	placeholder := func(n int) string {
		if isPostgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}

	q := `
        SELECT DISTINCT their_jid, first_name
        FROM whatsmeow_contacts
        WHERE (LOWER(first_name) LIKE LOWER(` + placeholder(1) + `)
           OR LOWER(their_jid) LIKE LOWER(` + placeholder(2) + `))
          AND their_jid NOT LIKE '%@g.us'
        ORDER BY first_name, their_jid
        LIMIT 50
    `

	args := []any{"%" + query + "%", "%" + query + "%"}

	rows, err := store.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contacts []Contact

	for rows.Next() {
		var jid, name sql.NullString

		if err := rows.Scan(&jid, &name); err != nil {
			continue
		}
		if !jid.Valid {
			continue
		}

		phone := strings.Split(jid.String, "@")[0]

		c := Contact{
			PhoneNumber: phone,
			JID:         jid.String,
		}
		if name.Valid {
			c.Name = name.String
		}

		contacts = append(contacts, c)
	}

	return contacts, nil
}

func (store *MessageStore) GetContactChats(jid string, limit, page int) ([]Chat, error) {
	placeholder := func(n int) string {
		if isPostgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}

	q := `
        SELECT DISTINCT
            c.jid, c.name, c.last_message_time,
            m.content AS last_message,
            m.sender AS last_sender,
            m.is_from_me AS last_is_from_me
        FROM chats c
        JOIN messages m ON c.jid = m.chat_jid
        WHERE m.sender = ` + placeholder(1) + ` 
           OR c.jid = ` + placeholder(2) + `
        ORDER BY c.last_message_time DESC
        LIMIT ` + placeholder(3) + `
        OFFSET ` + placeholder(4)

	args := []any{jid, jid, limit, page * limit}

	rows, err := store.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat

	for rows.Next() {
		var (
			cjid    string
			name    sql.NullString
			lmt     sql.NullTime
			lmsg    sql.NullString
			lsender sql.NullString
			lfromme sql.NullBool
		)

		if err := rows.Scan(&cjid, &name, &lmt, &lmsg, &lsender, &lfromme); err != nil {
			continue
		}

		c := Chat{JID: cjid}

		if name.Valid {
			c.Name = name.String
		}
		if lmt.Valid {
			c.LastMessageTime = lmt.Time
		}
		if lmsg.Valid {
			c.LastMessage = lmsg.String
		}
		if lsender.Valid {
			c.LastSender = lsender.String
		}
		if lfromme.Valid {
			c.LastIsFromMe = lfromme.Bool
		}

		chats = append(chats, c)
	}

	return chats, nil
}

func (store *MessageStore) GetLastInteraction(jid string) (string, error) {
	placeholder := func(n int) string {
		if isPostgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}

	q := `
        SELECT 
            m.timestamp, m.sender, c.name, m.content, m.is_from_me,
            c.jid, m.id, m.media_type
        FROM messages m
        JOIN chats c ON m.chat_jid = c.jid
        WHERE m.sender = ` + placeholder(1) + `
           OR c.jid = ` + placeholder(2) + `
        ORDER BY m.timestamp DESC
        LIMIT 1
    `

	row := store.db.QueryRow(q, jid, jid)

	var (
		ts        time.Time
		sender    string
		chatName  sql.NullString
		content   string
		isFromMe  bool
		chatJid   string
		id        string
		mediaType sql.NullString
	)

	if err := row.Scan(&ts, &sender, &chatName, &content, &isFromMe, &chatJid, &id, &mediaType); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}

	msg := MessageInteraction{
		Timestamp: ts.In(displayLoc),
		Sender:    sender,
		Content:   content,
		IsFromMe:  isFromMe,
		ChatJID:   chatJid,
		ID:        id,
	}

	if chatName.Valid {
		msg.ChatName = chatName.String
	}
	if mediaType.Valid {
		msg.MediaType = mediaType.String
	}

	return store.FormatMessage(msg, true), nil
}

func (store *MessageStore) GetChat(chatJID string, includeLastMessage bool) (*Chat, error) {
	placeholder := func(n int) string {
		if isPostgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}

	q := `
        SELECT 
            c.jid, c.name, c.last_message_time,
            m.content AS last_message,
            m.sender AS last_sender,
            m.is_from_me AS last_is_from_me
        FROM chats c
    `

	if includeLastMessage {
		q += `
            LEFT JOIN messages m 
            ON c.jid = m.chat_jid 
            AND c.last_message_time = m.timestamp
        `
	}

	q += " WHERE c.jid = " + placeholder(1)

	row := store.db.QueryRow(q, chatJID)

	var (
		jid     string
		name    sql.NullString
		lmt     sql.NullTime
		lmsg    sql.NullString
		lsender sql.NullString
		lfromme sql.NullBool
	)

	if err := row.Scan(&jid, &name, &lmt, &lmsg, &lsender, &lfromme); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	c := Chat{JID: jid}

	if name.Valid {
		c.Name = name.String
	}
	if lmt.Valid {
		c.LastMessageTime = lmt.Time
	}
	if lmsg.Valid {
		c.LastMessage = lmsg.String
	}
	if lsender.Valid {
		c.LastSender = lsender.String
	}
	if lfromme.Valid {
		c.LastIsFromMe = lfromme.Bool
	}

	return &c, nil
}

func (store *MessageStore) GetDirectChatByContact(phone string) (*Chat, error) {
	placeholder := func(n int) string {
		if isPostgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}

	q := `
       SELECT 
           c.jid, c.name, c.last_message_time,
           m.content AS last_message,
           m.sender AS last_sender,
           m.is_from_me AS last_is_from_me
       FROM chats c
       LEFT JOIN messages m 
           ON c.jid = m.chat_jid 
          AND c.last_message_time = m.timestamp
       WHERE c.jid LIKE ` + placeholder(1) + `
         AND c.jid NOT LIKE '%@g.us'
       LIMIT 1
    `

	arg := "%" + phone + "%"

	row := store.db.QueryRow(q, arg)

	var (
		jid     string
		name    sql.NullString
		lmt     sql.NullTime
		lmsg    sql.NullString
		lsender sql.NullString
		lfromme sql.NullBool
	)

	if err := row.Scan(&jid, &name, &lmt, &lmsg, &lsender, &lfromme); err != nil {
		return nil, err
	}

	return &Chat{
		JID:             jid,
		Name:            name.String,
		LastMessageTime: lmt.Time,
		LastMessage:     lmsg.String,
		LastSender:      lsender.String,
		LastIsFromMe:    lfromme.Bool,
	}, nil
}

func main() {
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	dbLog := waLog.Stdout("Database", "INFO", true)

	if err := os.MkdirAll("store", 0755); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	slog.SetDefault(bridgelogger.New(os.Getenv("LOG_LEVEL")))

	cfg, err := config.LoadConfig()
	if err != nil {
		logger.Errorf("Failed to load config: %v", err)
		return
	}

	dialect := "sqlite3"
	connStr := "file:store/whatsapp.db?_foreign_keys=on"

	if cfg.DB.IsPostgres {
		dialect = "postgres"
		connStr = fmt.Sprintf("postgresql://%s:%s@%s:%s/%s?sslmode=disable", cfg.DB.User,
			cfg.DB.Pass, cfg.DB.Host, cfg.DB.Port, "whatsapp")
	}

	container, err := sqlstore.New(context.Background(), dialect, connStr, dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	state := wastate.New()

	version, err := CustomGetLatestVersion(context.Background(), nil)
	if err != nil {
		logger.Errorf("Failed to retrieve current WhatsApp Web client Version")
	} else {
		store.SetWAVersion(*version)
		state.SetWAVersion(fmt.Sprintf("%d.%d.%d", version[0], version[1], version[2]))
		logger.Infof("WhatsApp Web Client Version: %d.%d.%d\n", version[0], version[1], version[2])
	}
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Per-number outbound proxy for WhatsApp traffic. Set by the supervisor via
	// PROXY_URL (socks5://, http://, or https://). Must be configured before
	// Connect(); changing it requires a bridge restart.
	if addr := os.Getenv("PROXY_URL"); addr != "" {
		if err := client.SetProxyAddress(addr); err != nil {
			logger.Errorf("invalid PROXY_URL: %v", err)
		} else {
			logger.Infof("using proxy for WhatsApp traffic")
		}
	}

	store.SetOSInfo("Linux", store.GetWAVersion())
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()

	// Device name shown in the phone's Linked Devices list, set per-number by the
	// supervisor via DEVICE_NAME so each linked device is identifiable. Baked into
	// the link-time registration, so it only applies on (re-)pair.
	if dn := os.Getenv("DEVICE_NAME"); dn != "" {
		store.DeviceProps.Os = proto.String(dn)
	}

	// Per-number history-sync window. WhatsApp's default companion sync only
	// pushes a recent window (~6 months, server-decided), so dormant chats and
	// older history never reach the store. When the manager sets FULL_SYNC=true
	// for this number, request a full sync covering FULLSYNC_DAYS days (default
	// 3650 ≈ 10y) with generous size caps so the day limit — not the size cap —
	// is the boundary. RequireFullSync is baked into the link-time registration
	// payload, so this only takes effect when the number (re-)pairs.
	if strings.EqualFold(os.Getenv("FULL_SYNC"), "true") {
		days := 3650
		if d, err := strconv.Atoi(os.Getenv("FULLSYNC_DAYS")); err == nil && d > 0 {
			days = d
		}
		store.DeviceProps.RequireFullSync = proto.Bool(true)
		store.DeviceProps.HistorySyncConfig = &waCompanionReg.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:   proto.Uint32(uint32(days)),
			FullSyncSizeMbLimit: proto.Uint32(102400),
			StorageQuotaMb:      proto.Uint32(102400),
		}
		logger.Infof("Full history sync enabled: up to %d days", days)
	}

	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	state.SetLoggedIn(client.Store.ID != nil) // existing session means already logged in

	const maxOutdatedRetries = 3
	var outdatedRetries int
	var outdatedRetriesMu sync.Mutex

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			handleHistorySync(client, messageStore, v, logger)

		case *events.Receipt:
			// Clear the unread badge when *we* read the chat elsewhere (phone or
			// another linked device). IsFromMe distinguishes our own read from a
			// contact reading our outgoing message, which must not clear unread.
			if v.IsFromMe && (v.Type == types.ReceiptTypeRead || v.Type == types.ReceiptTypeReadSelf) {
				readChat := normalizeUserJID(client, v.Chat).String()
				if err := messageStore.SetUnreadCount(readChat, 0); err != nil {
					logger.Warnf("Failed to clear unread on read receipt: %v", err)
				}
			}

		case *events.MarkChatAsRead:
			// App-state read/unread toggle from another device (the owner's
			// phone). Replays on connect via FromFullSync, so the board mirrors
			// the phone's read state, including the explicit "mark as unread".
			markChat := normalizeUserJID(client, v.JID).String()
			if v.Action != nil && v.Action.GetRead() {
				if err := messageStore.SetUnreadCount(markChat, 0); err != nil {
					logger.Warnf("Failed to clear unread on mark-read: %v", err)
				}
			} else if v.Action != nil {
				if err := messageStore.MarkUnread(markChat); err != nil {
					logger.Warnf("Failed to mark unread: %v", err)
				}
			}

		case *events.GroupInfo:
			// Membership / role change in a group we're in (someone added,
			// removed, promoted or demoted — by us or anyone else). whatsmeow
			// emits this live; offline changes arrive via history sync instead.
			// Surface it to the number's webhook so automations can react (e.g.
			// auto-welcome a new member). Best-effort, fire-and-forget.
			handleGroupInfoEvent(client, v, logger)

		case *events.PairPasskeyRequest:
			passkeyState.mu.Lock()
			passkeyState.request = v.PublicKey
			passkeyState.code = ""
			passkeyState.errMsg = ""
			passkeyState.mu.Unlock()
			slog.Info("passkey request captured; awaiting browser assertion",
				"rpId", v.PublicKey.RelyingPartID, "uv", v.PublicKey.UserVerification,
				"allowCreds", len(v.PublicKey.AllowCredentials))

		case *events.PairPasskeyConfirmation:
			passkeyState.mu.Lock()
			passkeyState.code = v.Code
			passkeyState.skipUX = v.SkipHandoffUX
			passkeyState.mu.Unlock()
			slog.Info("passkey confirmation code ready", "code", v.Code, "skipHandoffUX", v.SkipHandoffUX)

		case *events.PairPasskeyError:
			passkeyState.mu.Lock()
			passkeyState.errMsg = v.Error.Error()
			passkeyState.mu.Unlock()
			slog.Error("passkey linking error", "err", v.Error.Error(), "continuation", v.Continuation)

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")
			state.SetConnected(true)
			state.SetLoggedIn(true)
			state.ClearPairingQR()
			outdatedRetriesMu.Lock()
			outdatedRetries = 0
			outdatedRetriesMu.Unlock()

		case *events.Disconnected:
			logger.Warnf("Disconnected from WhatsApp")
			state.SetConnected(false)

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
			state.SetLoggedIn(false)
			state.SetConnected(false)

		case *events.ClientOutdated:
			outdatedRetriesMu.Lock()
			outdatedRetries++
			n := outdatedRetries
			outdatedRetriesMu.Unlock()
			state.SetConnected(false)
			if n > maxOutdatedRetries {
				slog.Error("client outdated: exceeded retry budget; whatsmeow library likely needs a real upgrade",
					"retries", n, "max", maxOutdatedRetries)
				return
			}
			slog.Warn("client outdated (405); refreshing wa version and reconnecting",
				"attempt", n, "max", maxOutdatedRetries)
			go func() {
				time.Sleep(5 * time.Second)
				newVersion, err := CustomGetLatestVersion(context.Background(), nil)
				if err != nil {
					slog.Error("failed to refresh wa version", "err", err)
					return
				}
				store.SetWAVersion(*newVersion)
				state.SetWAVersion(fmt.Sprintf("%d.%d.%d", newVersion[0], newVersion[1], newVersion[2]))
				slog.Info("applied refreshed wa version, attempting reconnect", "version", state.WAVersion())
				if err := client.Connect(); err != nil {
					slog.Error("reconnect after wa version refresh failed", "err", err)
				}
			}()
		}
	})

	migrateLIDChatsToPhoneJIDs(client, messageStore, logger, cfg.DB.IsPostgres)

	// REST server comes up first so /api/auth/status and /api/auth/pairing-qr
	// are reachable during pairing. WhatsApp connect runs concurrently below.
	startRESTServer(client, messageStore, cfg, state)

	// Periodically refresh the WhatsApp Web client version so reconnects
	// after transient drops use a current version string. Only the next
	// connection picks up the refreshed value; the active session is unaffected.
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for range t.C {
			v, err := CustomGetLatestVersion(context.Background(), nil)
			if err != nil {
				slog.Warn("periodic wa version refresh failed", "err", err)
				continue
			}
			store.SetWAVersion(*v)
			next := fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
			if next != state.WAVersion() {
				slog.Info("wa version updated by periodic refresh", "from", state.WAVersion(), "to", next)
				state.SetWAVersion(next)
			}
		}
	}()

	// Pair / connect to WhatsApp in a goroutine so main can block on signals.
	go func() {
		if client.Store.ID == nil {
			if os.Getenv("PAIR_MODE") == "phone" {
				// Phone-code mode: just connect; /auth/pair-phone drives pairing.
				if err := client.Connect(); err != nil {
					logger.Errorf("Failed to connect: %v", err)
					return
				}
			} else {
				qrChan, _ := client.GetQRChannel(context.Background())
				if err := client.Connect(); err != nil {
					logger.Errorf("Failed to connect: %v", err)
					return
				}
				for evt := range qrChan {
					switch evt.Event {
					case "code":
						fmt.Println("\nScan this QR code with your WhatsApp app:")
						qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
						if png, err := qrcode.Encode(evt.Code, qrcode.Medium, 256); err == nil {
							state.SetPairingQRPNG(png)
						} else {
							slog.Warn("failed to encode pairing qr as png", "err", err)
						}
					case "success":
						fmt.Println("\nSuccessfully connected and authenticated!")
						return
					case "timeout":
						logger.Errorf("Pairing QR timeout")
						return
					}
				}
			}
		} else {
			if err := client.Connect(); err != nil {
				logger.Errorf("Failed to connect: %v", err)
				return
			}
		}
	}()

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	<-exitChan

	fmt.Println("Disconnecting...")
	client.Disconnect()
}
