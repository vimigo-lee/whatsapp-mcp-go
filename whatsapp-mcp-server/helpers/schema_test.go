package helpers

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestListMessagesSchemaAfterOptional(t *testing.T) {
	s, err := jsonschema.For[listMessagesInput](nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	t.Log(string(b))
	for _, r := range s.Required {
		if r == "after" || r == "After" {
			t.Fatalf("after must be optional, required=%v", s.Required)
		}
	}
	if _, ok := s.Properties["After"]; !ok {
		t.Fatalf("After property renamed; existing callers would be rejected: %v", s.Properties)
	}
	var in listMessagesInput
	if err := json.Unmarshal([]byte(`{"After":"2026-10-29T09:00:00+08:00","limit":1}`), &in); err != nil || in.After == nil {
		t.Fatalf("legacy After key not accepted: %v", err)
	}
	if _, err := jsonschema.For[sendMessageInput](nil); err != nil {
		t.Fatal(err)
	}
}
