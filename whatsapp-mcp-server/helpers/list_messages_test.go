package helpers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// list_messages must send the bridge the context sizes it was asked for; on
// its own, context=true makes the bridge use 5 either side.
func TestListMessagesSendsContextSizes(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/messages" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		got = r.URL.Query()
		w.Write([]byte(`{"result":""}`))
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

	three, big := 3, 50
	for _, c := range []struct {
		name string
		in   listMessagesInput
		want map[string]string // "" = must be absent
	}{
		{"defaults", listMessagesInput{},
			map[string]string{"limit": "20", "page": "0", "context": "", "context_before": "", "context_after": ""}},
		{"context unset sizes", listMessagesInput{IncludeContext: true},
			map[string]string{"context": "true", "context_before": "1", "context_after": "1"}},
		{"context sizes capped", listMessagesInput{Limit: 5, IncludeContext: true, ContextBefore: &three, ContextAfter: &big},
			map[string]string{"limit": "5", "context_before": "3", "context_after": "10"}},
	} {
		got = nil
		res, _, err := listMessagesHandler(context.Background(), nil, c.in)
		if err != nil || res.IsError {
			t.Fatalf("%s: err=%v result=%+v", c.name, err, res)
		}
		for k, want := range c.want {
			if v := got.Get(k); v != want {
				t.Errorf("%s: %s=%q, want %q (query %v)", c.name, k, v, want, got)
			}
		}
	}
}
