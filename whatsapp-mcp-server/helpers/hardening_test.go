package helpers

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A client that sends part of its headers and then stalls must be cut off
// after the header timeout instead of holding the connection forever.
func TestMCPHTTPServerDropsSlowHeaders(t *testing.T) {
	prev := mcpReadHeaderTimeout
	mcpReadHeaderTimeout = 200 * time.Millisecond
	t.Cleanup(func() { mcpReadHeaderTimeout = prev })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newMCPHTTPServer(ln.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if srv.ReadHeaderTimeout != mcpReadHeaderTimeout || srv.IdleTimeout == 0 || srv.MaxHeaderBytes != 64<<10 || srv.WriteTimeout != 0 {
		t.Errorf("server limits = header %v idle %v maxHeader %d write %v", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.MaxHeaderBytes, srv.WriteTimeout)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("POST / HTTP/1.1\r\nHost: localhost\r\nX-Slow: ")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = io.ReadAll(conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("connection still open after %v", time.Since(start))
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("connection closed after %v, want about %v", elapsed, mcpReadHeaderTimeout)
	}
}

// Tool arguments that land in a bridge URL path are escaped, so a chat_jid
// such as "../auth/passkey-code" stays under /chats/.
func TestBridgePathArgumentsAreEscaped(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
		w.Write([]byte(`{"chat":{},"chats":[]}`))
	}))
	defer srv.Close()

	prevBase := apiBaseURL
	apiBaseURL = srv.URL + "/api"
	tokenMutex.Lock()
	prevTok, prevExp := jwtToken, tokenExpiresAt
	jwtToken, tokenExpiresAt = "test", time.Now().Add(time.Hour)
	tokenMutex.Unlock()
	defer func() {
		apiBaseURL = prevBase
		tokenMutex.Lock()
		jwtToken, tokenExpiresAt = prevTok, prevExp
		tokenMutex.Unlock()
	}()

	const evil = "../auth/passkey-code"
	ctx := context.Background()
	for name, c := range map[string]struct {
		call func()
		want string
	}{
		"get_chat":         {func() { getChatHandler(ctx, nil, getChatInput{ChatJid: evil}) }, "/api/chats/..%2Fauth%2Fpasskey-code"},
		"message_context":  {func() { getMessageContextHandler(ctx, nil, getMessageContextInput{MessageID: evil}) }, "/api/messages/context/..%2Fauth%2Fpasskey-code"},
		"direct_chat":      {func() { getDirectChatByContactHandler(ctx, nil, getDirectChatByContactInput{SenderPhoneNumber: evil}) }, "/api/direct-contacts/..%2Fauth%2Fpasskey-code/chat"},
		"contact_chats":    {func() { getContactChatsHandler(ctx, nil, getContactChatsInput{Jid: evil}) }, "/api/contacts/..%2Fauth%2Fpasskey-code/chats"},
		"jid stays intact": {func() { getChatHandler(ctx, nil, getChatInput{ChatJid: "60123456789@s.whatsapp.net"}) }, "/api/chats/60123456789@s.whatsapp.net"},
	} {
		gotPath = ""
		c.call()
		if gotPath != c.want {
			t.Errorf("%s: bridge path %q, want %q", name, gotPath, c.want)
		}
	}

	gotPath = ""
	if res, _, _ := getChatHandler(ctx, nil, getChatInput{ChatJid: ".."}); !res.IsError || gotPath != "" {
		t.Errorf(`chat_jid "..": reached bridge path %q`, gotPath)
	}

	gotPath = ""
	if res, _, _ := listChatsHandler(ctx, nil, listChatsInput{SortBy: "name&unread_only=true"}); !res.IsError || gotPath != "" {
		t.Errorf("invalid sort_by reached the bridge: %q?%s", gotPath, gotQuery)
	}
	if res, _, _ := listChatsHandler(ctx, nil, listChatsInput{SortBy: "name"}); res.IsError || gotQuery == "" {
		t.Errorf("sort_by=name refused (query %q)", gotQuery)
	}
}

// upload_media sends x-internal-secret to the manager; a redirect must not
// carry it to another host.
func TestUploadMediaDoesNotFollowRedirects(t *testing.T) {
	leaked := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = leaked || r.Header.Get("x-internal-secret") != ""
		w.Write([]byte(`{"put_url":"http://x/put","get_url":"http://x/get"}`))
	}))
	defer other.Close()
	manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer manager.Close()

	t.Setenv("WEBHOOK_BASE_URL", manager.URL)
	t.Setenv("INTERNAL_SECRET", "internal-secret-value")
	res, out, _ := uploadMediaHandler(context.Background(), nil, uploadMediaInput{Filename: "a.pdf"})
	if !res.IsError || leaked {
		t.Errorf("redirect followed: isError=%v leaked=%v out=%v", res.IsError, leaked, out)
	}
}
