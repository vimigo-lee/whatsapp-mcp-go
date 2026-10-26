package main

// Human-like presence sessions.
//
// The owner's phone shows a linked device as "Active" only while that device
// is presence-available, and "Last active <t>" from the moment it goes
// unavailable or disconnects. Reconnects, sends, read receipts, app-state and
// stats traffic never move it (verified against a live phone, Oct 2026). A
// bridge that never goes available keeps the date from its pairing session and
// the phone starts warning "Logging out in N days".
//
// Being available has visible side effects: contacts see "online" and the
// owner's phone mutes notifications. So the bridge behaves like a person with
// WhatsApp Web open for a moment, never like a timer:
//   - every send (with or without the typing delay, edits, reactions and
//     revokes too) goes available and stays online for a random 1–3 minutes
//     past the last send, then goes unavailable;
//   - an MCP read (a request tagged with presenceActivityHeader) opens the
//     same kind of session, at most once per presenceReadEvery, so an agent
//     polling list_messages does not keep the line online all day;
//   - a line that has not been active for a random 1–3 days opens one 1–3
//     minute session at a random minute in local daytime.
// Nothing is tied to reconnects, which happen on every deploy.

import (
	"context"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const (
	presenceLingerMin = 1 * time.Minute
	presenceLingerMax = 3 * time.Minute
	presenceIdleMin   = 24 * time.Hour
	presenceIdleMax   = 72 * time.Hour
	// Background sessions only start between these local hours (displayLoc).
	presenceDayStartHour = 9
	presenceDayEndHour   = 21
	// The background check runs this often and starts a due session with
	// presenceStartChance per check, so the start minute is spread out.
	presenceCheckEvery  = 10 * time.Minute
	presenceStartChance = 1.0 / 3
	// An MCP read opens at most one session this often; reads while the line
	// is already online do not extend it (only sends do).
	presenceReadEvery = 1 * time.Hour
	// The MCP server tags its bridge calls with this header (value "mcp"), so
	// the manager's own reads (dashboard, crons, health) never count.
	presenceActivityHeader = "X-Wamcp-Activity"
	// Last-active time survives restarts here (the bridge's working dir).
	presenceStateFile = "store/presence_last_active"
)

type presenceSessions struct {
	mu         sync.Mutex
	client     *whatsmeow.Client
	started    bool
	online     bool
	offAt      time.Time
	timer      *time.Timer
	lastActive time.Time
	nextIdle   time.Duration
	lastRead   time.Time
}

var presence = &presenceSessions{}

func randDuration(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int63n(int64(max-min)))
}

// presenceSessionDue reports whether a quiet line should open a background
// session now: inside local daytime and idle longer than its drawn interval.
func presenceSessionDue(now, lastActive time.Time, nextIdle time.Duration, loc *time.Location) bool {
	h := now.In(loc).Hour()
	if h < presenceDayStartHour || h >= presenceDayEndHour {
		return false
	}
	return now.Sub(lastActive) >= nextIdle
}

// start runs once per process, on the first Connected event.
func (p *presenceSessions) start(client *whatsmeow.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client = client
	if p.started {
		return
	}
	p.started = true
	p.lastActive = readPresenceLastActive()
	p.nextIdle = randDuration(presenceIdleMin, presenceIdleMax)
	go p.loop()
}

func (p *presenceSessions) loop() {
	t := time.NewTicker(presenceCheckEvery)
	defer t.Stop()
	for range t.C {
		p.mu.Lock()
		client := p.client
		due := !p.online && presenceSessionDue(time.Now(), p.lastActive, p.nextIdle, displayLoc)
		p.mu.Unlock()
		if !due || client == nil || !client.IsConnected() || client.Store.ID == nil {
			continue
		}
		if rand.Float64() >= presenceStartChance {
			continue
		}
		if err := client.SendPresence(context.Background(), types.PresenceAvailable); err != nil {
			slog.Debug("presence session: available failed", "err", err)
			continue
		}
		slog.Info("presence session: online")
		p.extend(client)
	}
}

// extend marks the line online and keeps it so until a random 1–3 minutes from
// now (or later, if an earlier call already reaches further).
func (p *presenceSessions) extend(client *whatsmeow.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client = client
	p.online = true
	if off := time.Now().Add(randDuration(presenceLingerMin, presenceLingerMax)); off.After(p.offAt) {
		p.offAt = off
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(time.Until(p.offAt), p.goOffline)
}

// active goes available and keeps the line online 1–3 minutes from now. Called
// after every send; best-effort, never fails the caller.
func (p *presenceSessions) active(client *whatsmeow.Client) {
	if client == nil || !client.IsConnected() || client.Store.ID == nil {
		return
	}
	if err := client.SendPresence(context.Background(), types.PresenceAvailable); err != nil {
		slog.Debug("presence session: available failed", "err", err)
		return
	}
	p.extend(client)
}

// read opens a session for an MCP read, unless the line is already online or
// a read opened one within presenceReadEvery.
func (p *presenceSessions) read(client *whatsmeow.Client) {
	p.mu.Lock()
	if p.online || time.Since(p.lastRead) < presenceReadEvery {
		p.mu.Unlock()
		return
	}
	p.lastRead = time.Now()
	p.mu.Unlock()
	p.active(client)
}

// presenceActivityMiddleware counts MCP-tagged API requests as reads.
func presenceActivityMiddleware(client *whatsmeow.Client, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(presenceActivityHeader) == "mcp" {
			go presence.read(client)
		}
		next.ServeHTTP(w, r)
	})
}

func (p *presenceSessions) goOffline() {
	p.mu.Lock()
	if wait := time.Until(p.offAt); wait > 0 {
		p.timer = time.AfterFunc(wait, p.goOffline)
		p.mu.Unlock()
		return
	}
	client := p.client
	p.online = false
	p.lastActive = time.Now()
	p.nextIdle = randDuration(presenceIdleMin, presenceIdleMax)
	last := p.lastActive
	p.mu.Unlock()

	// After a logout there is no session to mark offline (and nothing is shown
	// on the phone any more), so only a connected, logged-in client sends it.
	if client != nil && client.IsConnected() && client.Store.ID != nil {
		if err := client.SendPresence(context.Background(), types.PresenceUnavailable); err != nil {
			slog.Debug("presence session: unavailable failed", "err", err)
		}
	}
	writePresenceLastActive(last)
}

func readPresenceLastActive() time.Time {
	b, err := os.ReadFile(presenceStateFile)
	if err != nil {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

func writePresenceLastActive(t time.Time) {
	_ = os.MkdirAll(filepath.Dir(presenceStateFile), 0o755)
	if err := os.WriteFile(presenceStateFile, []byte(strconv.FormatInt(t.Unix(), 10)), 0o644); err != nil {
		slog.Debug("presence session: save last active failed", "err", err)
	}
}
