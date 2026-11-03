package main

import (
	"strings"
	"testing"
	"time"
)

// An action array whose elements refer back to itself used to cost N^9 isJS
// calls: a 480-byte file kept a CPU busy for minutes. Each object is now judged
// once, so even a wide array finishes at once and is not mistaken for JS.
func TestPDFSelfReferencingActionArrayIsCheap(t *testing.T) {
	data := buildPDF(
		"<< /Type /Catalog /Pages 2 0 R /OpenAction 4 0 R >>", pdfPages, pdfPage,
		"["+strings.Repeat("4 0 R ", 64)+"]",
	)
	start := time.Now()
	got := pdfReasons(data)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("pdfReasons took %v on a %d-byte file", d, len(data))
	}
	if len(got) != 0 {
		t.Errorf("reasons = %v, want none", got)
	}
}

// The memo must not hide JavaScript that is reachable through a shared object.
func TestPDFSharedJavaScriptActionStillFlagged(t *testing.T) {
	data := buildPDF(
		"<< /Type /Catalog /Pages 2 0 R /OpenAction 4 0 R >>", pdfPages, pdfPage,
		"[5 0 R 5 0 R 4 0 R]",
		"<< /S /JavaScript /JS (app.alert(1);) >>",
	)
	if got := pdfReasons(data); !sameReasons(got, []string{reasonPDFAutoJS}) {
		t.Errorf("reasons = %v, want %v", got, []string{reasonPDFAutoJS})
	}
}
