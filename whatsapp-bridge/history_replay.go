package main

// History replay (POST /api/history-replay).
//
// The phone uploads its pairing history as encrypted files on WhatsApp's
// media servers and sends a HISTORY_SYNC_NOTIFICATION pointing at each one.
// Those files stay downloadable for about a month. Every notification is
// saved here as it arrives, so a pairing upload that reached us but was lost
// on our side (a crash or deploy mid-upload, a store bug) can be downloaded
// and stored again later — with no request to the phone and no notification
// on it. It cannot help when the phone never uploaded; that needs a re-link.
//
// The saved rows also say whether an upload finished: the last chunk of each
// batch (RECENT, FULL) carries progress 100. A phone in the background pauses
// its upload and resumes when WhatsApp is opened again (verified Oct 2026),
// so a pairing with INITIAL_BOOTSTRAP but no finished RECENT/FULL usually
// just needs the customer to open WhatsApp.
//
// Asking the phone to upload again without a re-link was tried and does not
// work for us (Oct 2026): FULL_HISTORY_SYNC_ON_DEMAND is refused by a
// personal phone with ERROR_REQUEST_ON_NON_SMB_PRIMARY and got no answer at
// all from another, and HISTORY_SYNC_CHUNK_RETRY got no answer either.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/encoding/protojson"
)

// ensureHistoryNotificationTable creates the table in the number's own
// store (search_path on Postgres, the SQLite file otherwise).
func ensureHistoryNotificationTable(store *MessageStore) error {
	_, err := store.db.Exec(`CREATE TABLE IF NOT EXISTS history_sync_notifications (
		id TEXT PRIMARY KEY,
		sync_type TEXT NOT NULL,
		chunk_order INTEGER,
		progress INTEGER,
		notification TEXT NOT NULL,
		received_at TIMESTAMP NOT NULL
	)`)
	return err
}

// saveHistoryNotification records a history-sync notification from our own
// phone. Called for every message event; anything else is ignored.
func saveHistoryNotification(store *MessageStore, evt *events.Message) {
	if !evt.Info.IsFromMe {
		return
	}
	notif := evt.Message.GetProtocolMessage().GetHistorySyncNotification()
	if notif == nil {
		return
	}
	if err := storeHistoryNotification(store, string(evt.Info.ID), notif); err != nil {
		slog.Warn("failed to save history sync notification", "id", evt.Info.ID, "err", err)
	}
}

func storeHistoryNotification(store *MessageStore, id string, notif *waE2E.HistorySyncNotification) error {
	raw, err := protojson.Marshal(notif)
	if err != nil {
		return err
	}
	q := `INSERT INTO history_sync_notifications (id, sync_type, chunk_order, progress, notification, received_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`
	if isPostgres {
		q = `INSERT INTO history_sync_notifications (id, sync_type, chunk_order, progress, notification, received_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING`
	}
	_, err = store.db.Exec(q, id, notif.GetSyncType().String(), int(notif.GetChunkOrder()), int(notif.GetProgress()), string(raw), time.Now().UTC())
	return err
}

type savedNotification struct {
	ID    string
	Notif *waE2E.HistorySyncNotification
}

// loadHistoryNotifications returns the saved notifications of the given sync
// types, oldest first.
func loadHistoryNotifications(store *MessageStore, types map[string]bool) ([]savedNotification, error) {
	rows, err := store.db.Query(`SELECT id, sync_type, notification FROM history_sync_notifications ORDER BY received_at, chunk_order`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []savedNotification
	for rows.Next() {
		var id, syncType, raw string
		if err := rows.Scan(&id, &syncType, &raw); err != nil {
			return nil, err
		}
		if !types[syncType] {
			continue
		}
		var n waE2E.HistorySyncNotification
		if err := protojson.Unmarshal([]byte(raw), &n); err != nil {
			slog.Warn("skipping unreadable saved history notification", "id", id, "err", err)
			continue
		}
		out = append(out, savedNotification{ID: id, Notif: &n})
	}
	return out, rows.Err()
}

// ReplayStatus reports the last replay.
type ReplayStatus struct {
	Running    bool              `json:"running"`
	StartedAt  string            `json:"started_at,omitempty"`
	FinishedAt string            `json:"finished_at,omitempty"`
	Total      int               `json:"total"`
	Done       int               `json:"done"`
	Failed     map[string]string `json:"failed,omitempty"` // notification id -> error
	Chunks     []string          `json:"chunks"`           // "FULL#2 (49 conversations)"
}

var (
	replayMu  sync.Mutex
	replayCur = &ReplayStatus{}
)

func getReplayStatus() ReplayStatus {
	replayMu.Lock()
	defer replayMu.Unlock()
	cp := *replayCur
	cp.Failed = make(map[string]string, len(replayCur.Failed))
	for k, v := range replayCur.Failed {
		cp.Failed[k] = v
	}
	cp.Chunks = append([]string(nil), replayCur.Chunks...)
	return cp
}

// startHistoryReplay downloads and stores the given notifications in the
// background, one at a time. Returns false when a replay is already running.
func startHistoryReplay(client *whatsmeow.Client, store *MessageStore, notifs []savedNotification) bool {
	replayMu.Lock()
	if replayCur.Running {
		replayMu.Unlock()
		return false
	}
	replayCur = &ReplayStatus{Running: true, StartedAt: time.Now().UTC().Format(time.RFC3339), Total: len(notifs), Failed: map[string]string{}, Chunks: []string{}}
	replayMu.Unlock()

	go func() {
		for _, sn := range notifs {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			hs, err := client.DownloadHistorySync(ctx, sn.Notif, true)
			cancel()
			replayMu.Lock()
			if err != nil {
				replayCur.Failed[sn.ID] = err.Error()
			} else {
				replayCur.Chunks = append(replayCur.Chunks, sn.Notif.GetSyncType().String()+"#"+strconv.Itoa(int(sn.Notif.GetChunkOrder()))+" ("+strconv.Itoa(len(hs.GetConversations()))+" conversations)")
			}
			replayMu.Unlock()
			if err != nil {
				slog.Warn("history replay: download failed", "id", sn.ID, "type", sn.Notif.GetSyncType().String(), "err", err)
				continue
			}
			slog.Info("history replay: storing", "id", sn.ID, "type", hs.GetSyncType().String(), "chunk", hs.GetChunkOrder(), "conversations", len(hs.GetConversations()))
			handleHistorySync(client, store, &events.HistorySync{Data: hs}, client.Log)
			replayMu.Lock()
			replayCur.Done++
			replayMu.Unlock()
		}
		replayMu.Lock()
		replayCur.Running = false
		replayCur.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		replayMu.Unlock()
		slog.Info("history replay finished")
	}()
	return true
}

// handleHistoryReplay serves /api/history-replay.
//
//	GET  → the last replay's status.
//	POST → replay the saved notifications. Body (all optional):
//	       {"types": ["RECENT","FULL"]}       which sync types (default RECENT, FULL)
//	       {"notifications": [{"id": "...", "notification": {...}}]}
//	       — notifications to save first, as protojson; for ones that arrived
//	       before this store existed (e.g. copied from the bridge log).
func handleHistoryReplay(client *whatsmeow.Client, store *MessageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			respondJSON(w, http.StatusOK, getReplayStatus())
		case http.MethodPost:
			var body struct {
				Types         []string `json:"types"`
				Notifications []struct {
					ID           string          `json:"id"`
					Notification json.RawMessage `json:"notification"`
				} `json:"notifications"`
			}
			if r.ContentLength != 0 {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					respondError(w, http.StatusBadRequest, "invalid JSON body")
					return
				}
			}
			for _, n := range body.Notifications {
				var notif waE2E.HistorySyncNotification
				if n.ID == "" || protojson.Unmarshal(n.Notification, &notif) != nil {
					respondError(w, http.StatusBadRequest, "each notification needs an id and a protojson HistorySyncNotification")
					return
				}
				if err := storeHistoryNotification(store, n.ID, &notif); err != nil {
					respondError(w, http.StatusInternalServerError, "save failed: "+err.Error())
					return
				}
			}
			types := map[string]bool{"RECENT": true, "FULL": true}
			if len(body.Types) > 0 {
				types = map[string]bool{}
				for _, t := range body.Types {
					types[t] = true
				}
			}
			if client.Store.ID == nil || !client.IsConnected() {
				respondError(w, http.StatusConflict, "not connected")
				return
			}
			notifs, err := loadHistoryNotifications(store, types)
			if err != nil {
				respondError(w, http.StatusInternalServerError, "load failed: "+err.Error())
				return
			}
			if len(notifs) == 0 {
				respondError(w, http.StatusNotFound, "no saved history notifications of those types")
				return
			}
			if !startHistoryReplay(client, store, notifs) {
				respondError(w, http.StatusConflict, "a replay is already running")
				return
			}
			respondJSON(w, http.StatusAccepted, getReplayStatus())
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
