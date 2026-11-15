package main

import (
	"fmt"
	"testing"

	"go.mau.fi/whatsmeow"
)

// A CDN blob that does not match the message's media key (invalid HMAC) must
// take the same media-retry path as an expired blob, so the sender's device is
// asked to re-upload it.
func TestIsExpiredMediaErrCoversInvalidHMAC(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"403 expired", whatsmeow.ErrMediaDownloadFailedWith403, true},
		{"404 expired", whatsmeow.ErrMediaDownloadFailedWith404, true},
		{"410 expired", whatsmeow.ErrMediaDownloadFailedWith410, true},
		{"wrapped invalid hmac", fmt.Errorf("download: %w", whatsmeow.ErrInvalidMediaHMAC), true},
		{"file length mismatch", whatsmeow.ErrFileLengthMismatch, false},
		{"no url", whatsmeow.ErrNoURLPresent, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExpiredMediaErr(tc.err); got != tc.want {
				t.Fatalf("isExpiredMediaErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
