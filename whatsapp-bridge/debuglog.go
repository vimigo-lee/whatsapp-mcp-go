package main

import (
	"encoding/json"
	"os"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/encoding/protojson"
)

// BRIDGE_DEBUG_MESSAGES=1 logs every incoming message's raw protobuf, for
// working out how WhatsApp delivers something (edits, polls, new message
// types). It is off by default: a message carries the keys that decrypt its
// media and its edits, and logs are kept and shipped far more widely than the
// store. Those fields are redacted even when it is on; the structure and
// everything else stay readable.
var debugRawMessages = os.Getenv("BRIDGE_DEBUG_MESSAGES") == "1"

// redactedMessageJSON is the message as protojson with every key, secret and
// hash field replaced by "[redacted]" and thumbnails dropped.
func redactedMessageJSON(m *waE2E.Message) string {
	b, err := protojson.Marshal(m)
	if err != nil {
		return "unmarshalable: " + err.Error()
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, _ := json.Marshal(redactSecrets(v))
	return string(out)
}

func redactSecrets(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if isSecretField(k) {
				t[k] = "[redacted]"
			} else {
				t[k] = redactSecrets(val)
			}
		}
	case []any:
		for i := range t {
			t[i] = redactSecrets(t[i])
		}
	}
	return v
}

func isSecretField(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "key") || strings.Contains(n, "secret") ||
		strings.Contains(n, "sha256") || strings.Contains(n, "thumbnail") ||
		strings.Contains(n, "payload") || n == "directpath" || n == "url"
}
