package helpers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stubBridge points callAPI at a test server and returns the decoded body of
// each /send call.
func stubBridge(t *testing.T) *[]map[string]any {
	t.Helper()
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		got = append(got, m)
		w.Write([]byte(`{"success":true,"message":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	prevBase := apiBaseURL
	apiBaseURL = srv.URL + "/api"
	tokenMutex.Lock()
	prevTok, prevExp := jwtToken, tokenExpiresAt
	jwtToken, tokenExpiresAt = "test", time.Now().Add(time.Hour)
	tokenMutex.Unlock()
	t.Cleanup(func() {
		apiBaseURL = prevBase
		tokenMutex.Lock()
		jwtToken, tokenExpiresAt = prevTok, prevExp
		tokenMutex.Unlock()
	})
	return &got
}

func TestSendMediaForwardsViewOnce(t *testing.T) {
	calls := stubBridge(t)
	b64 := base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xff"))
	yes, fname := true, "p.jpg"
	res, _, _ := sendMediaHandler(context.Background(), nil, sendMediaInput{Recipient: "60123456789", Base64: &b64, Filename: &fname, ViewOnce: &yes})
	if res.IsError || len(*calls) != 1 {
		t.Fatalf("isError=%v calls=%d", res.IsError, len(*calls))
	}
	if (*calls)[0]["view_once"] != true {
		t.Errorf("payload view_once = %v, want true", (*calls)[0]["view_once"])
	}

	// Without the flag the payload carries no view_once key at all.
	*calls = nil
	sendMediaHandler(context.Background(), nil, sendMediaInput{Recipient: "60123456789", Base64: &b64, Filename: &fname})
	if _, has := (*calls)[0]["view_once"]; has {
		t.Error("view_once sent when not requested")
	}
}

func TestSendMediaViewOnceRejectsGifAndSticker(t *testing.T) {
	calls := stubBridge(t)
	b64 := base64.StdEncoding.EncodeToString([]byte("x"))
	yes := true
	for _, sa := range []string{"gif", "sticker"} {
		sa := sa
		res, out, _ := sendMediaHandler(context.Background(), nil, sendMediaInput{Recipient: "60123456789", Base64: &b64, SendAs: &sa, ViewOnce: &yes})
		if !res.IsError || out["success"] != false {
			t.Errorf("send_as=%s with view_once: accepted", sa)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("bridge called %d times for rejected sends", len(*calls))
	}
}
