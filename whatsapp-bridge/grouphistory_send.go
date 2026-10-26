//go:build grouphistory

package main

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
	"go.mau.fi/whatsmeow/types"
)

var errGroupHistoryNotAllowed = errors.New("only a group admin can share history")

func init() {
	shareGroupHistoryHandler = handleShareGroupHistory
}

// handleShareGroupHistory serves POST /api/group/share-history in
// -tags grouphistory builds:
//
//	{ "jid": "...@g.us", "participants": ["60123...", "...@lid"], "dry_run": true }
//
// participants are members who were just added (phone numbers or JIDs).
// dry_run builds the bundle and reports what would be shared without
// uploading or sending anything.
func handleShareGroupHistory(client *whatsmeow.Client, store *MessageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			JID          string   `json:"jid"`
			Participants []string `json:"participants"`
			DryRun       bool     `json:"dry_run"`
			// Try even when this account is not a group admin (for groups
			// that let every member share history).
			AllowNonAdmin bool `json:"allow_non_admin"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		group, err := types.ParseJID(req.JID)
		if err != nil || group.Server != types.GroupServer {
			respondError(w, http.StatusBadRequest, "jid must be a group JID")
			return
		}
		if len(req.Participants) == 0 {
			respondError(w, http.StatusBadRequest, "participants is required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		res, err := shareGroupHistoryNative(ctx, client, store, group, req.Participants, req.DryRun, req.AllowNonAdmin)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			slog.Warn("group history share failed", "group", group.String(), "error", err)
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "error": err.Error(), "result": res})
			return
		}
		slog.Info("group history share", "group", group.String(), "dry_run", req.DryRun, "messages", res.Messages, "receivers", res.Receivers)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": res})
	}
}

type groupHistoryShareResult struct {
	Receivers     []string `json:"receivers"`
	Messages      int      `json:"messages"`
	FromMe        int      `json:"from_me"`
	WithSecret    int      `json:"with_secret"`
	Skipped       int      `json:"skipped"`
	OldestUnix    int64    `json:"oldest_unix,omitempty"`
	BundleBytes   int      `json:"bundle_bytes"`
	BundleSent    bool     `json:"bundle_sent"`
	NoticeSent    bool     `json:"notice_sent"`
	AddressingLID bool     `json:"addressing_lid"`
	SenderAdmin   bool     `json:"sender_admin"`
}

// shareGroupHistoryNative shares recent group messages with members who were
// just added directly. It sends the bundle group-direct and, once WhatsApp has
// acknowledged it, the notice to the group.
func shareGroupHistoryNative(ctx context.Context, client *whatsmeow.Client, store *MessageStore, group types.JID, members []string, dryRun, allowNonAdmin bool) (groupHistoryShareResult, error) {
	var res groupHistoryShareResult
	if store == nil {
		return res, fmt.Errorf("no message store")
	}
	info, err := client.GetGroupInfo(ctx, group)
	if err != nil {
		return res, fmt.Errorf("group info: %w", err)
	}
	res.AddressingLID = info.AddressingMode == types.AddressingModeLID

	ownLID := client.Store.GetLID()
	ownPN := client.Store.GetJID()
	isAdmin := false
	for _, p := range info.Participants {
		if (p.JID.User == ownLID.User || p.JID.User == ownPN.User || p.PhoneNumber.User == ownPN.User) && (p.IsAdmin || p.IsSuperAdmin) {
			isAdmin = true
			break
		}
	}
	res.SenderAdmin = isAdmin
	// Groups can let every member share history (member share mode), which
	// whatsmeow's GroupInfo does not expose yet. Non-admins are refused
	// unless the request explicitly allows trying.
	if !isAdmin && !allowNonAdmin {
		return res, errGroupHistoryNotAllowed
	}

	// Receivers are the roster entries themselves, so they carry exactly the
	// identity the group addresses them by.
	receivers := make([]types.JID, 0, len(members))
	for _, raw := range members {
		user := strings.TrimPrefix(strings.TrimSpace(raw), "+")
		if i := strings.IndexAny(user, "@:"); i >= 0 {
			user = user[:i]
		}
		found := false
		for _, p := range info.Participants {
			if p.JID.User == user || p.PhoneNumber.User == user {
				receivers = append(receivers, p.JID.ToNonAD())
				found = true
				break
			}
		}
		if !found {
			return res, fmt.Errorf("%s is not a member of the group", raw)
		}
	}
	for _, r := range receivers {
		res.Receivers = append(res.Receivers, r.String())
	}

	now := time.Now()
	rows, err := store.loadGroupHistoryRows(group.String(), now.Add(-groupHistoryWindow), groupHistoryMaxMessages*3)
	if err != nil {
		return res, fmt.Errorf("read messages: %w", err)
	}
	loaded := len(rows)
	resolveGroupHistorySenders(ctx, client, group, info.AddressingMode, rows)
	rows = selectGroupHistoryRows(rows, now, groupHistoryWindow, groupHistoryMaxMessages)
	res.Messages = len(rows)
	res.Skipped = loaded - len(rows)
	for _, r := range rows {
		if r.IsFromMe {
			res.FromMe++
		}
		if len(r.Secret) > 0 {
			res.WithSecret++
		}
	}
	if len(rows) == 0 {
		return res, fmt.Errorf("no messages to share")
	}

	body, err := packGroupHistory(buildGroupHistory(group, ownLID, rows))
	if err != nil {
		return res, err
	}
	res.BundleBytes = len(body)
	meta := groupHistoryMetadata(receivers, rows)
	res.OldestUnix = meta.GetOldestMessageTimestampInWindow()
	if dryRun {
		return res, nil
	}

	up, err := client.Upload(ctx, body, whatsmeow.MediaGroupHistory)
	if err != nil {
		return res, fmt.Errorf("upload history: %w", err)
	}
	if _, err := client.SendMessage(ctx, group, buildGroupHistoryBundle(up, meta), whatsmeow.SendRequestExtra{
		GroupHistoryReceivers: receivers,
	}); err != nil {
		return res, fmt.Errorf("send history bundle: %w", err)
	}
	res.BundleSent = true
	if _, err := client.SendMessage(ctx, group, buildGroupHistoryNotice(meta)); err != nil {
		return res, fmt.Errorf("history shared, but the group notice failed: %w", err)
	}
	res.NoticeSent = true
	presence.active(client)
	return res, nil
}
