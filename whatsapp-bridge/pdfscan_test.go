package main

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// buildPDF writes a classic PDF: objs[i] is the body of object i+1, and
// object 1 is the catalog.
func buildPDF(objs ...string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

// buildPDFObjStm writes a PDF 1.5 file whose objects 1..len(packed) sit inside
// one Flate-compressed object stream, indexed by an xref stream: the layout
// that hides keywords from a plain byte search.
func buildPDFObjStm(packed ...string) []byte {
	var hdr, body bytes.Buffer
	for i, o := range packed {
		fmt.Fprintf(&hdr, "%d %d ", i+1, body.Len())
		body.WriteString(o + "\n")
	}
	first := hdr.Len()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(append(hdr.Bytes(), body.Bytes()...))
	zw.Close()

	n := len(packed)
	stmNum, xrefNum := n+1, n+2
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	stmOff := b.Len()
	fmt.Fprintf(&b, "%d 0 obj\n<< /Type /ObjStm /N %d /First %d /Filter /FlateDecode /Length %d >>\nstream\n", stmNum, n, first, z.Len())
	b.Write(z.Bytes())
	b.WriteString("\nendstream\nendobj\n")

	// xref stream entries, W [1 4 2]: free 0, compressed 1..n, the object
	// stream itself, and the xref stream.
	xrefOff := b.Len()
	var rows bytes.Buffer
	row := func(t byte, f2 uint32, f3 uint16) {
		rows.Write([]byte{t, byte(f2 >> 24), byte(f2 >> 16), byte(f2 >> 8), byte(f2), byte(f3 >> 8), byte(f3)})
	}
	row(0, 0, 65535)
	for i := 0; i < n; i++ {
		row(2, uint32(stmNum), uint16(i))
	}
	row(1, uint32(stmOff), 0)
	row(1, uint32(xrefOff), 0)
	fmt.Fprintf(&b, "%d 0 obj\n<< /Type /XRef /Size %d /W [1 4 2] /Root 1 0 R /Length %d >>\nstream\n", xrefNum, xrefNum+1, rows.Len())
	b.Write(rows.Bytes())
	fmt.Fprintf(&b, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xrefOff)
	return b.Bytes()
}

const (
	pdfPages = "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"
	pdfPage  = "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>"
)

func TestPDFReasons(t *testing.T) {
	for _, tc := range []struct {
		name string
		pdf  []byte
		want []string
	}{
		{"plain document", buildPDF("<< /Type /Catalog /Pages 2 0 R >>", pdfPages, pdfPage), nil},
		{"open at page one is harmless", buildPDF("<< /Type /Catalog /Pages 2 0 R /OpenAction [3 0 R /Fit] >>", pdfPages, pdfPage), nil},
		{"form script on a click is normal", buildPDF(
			"<< /Type /Catalog /Pages 2 0 R /AcroForm << /Fields [4 0 R] >> >>", pdfPages,
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Annots [4 0 R] >>",
			"<< /Type /Annot /Subtype /Widget /FT /Btn /T (calc) /Rect [0 0 50 20] /A << /S /JavaScript /JS (this.calculateNow();) >> >>",
		), nil},
		{"javascript on open", buildPDF(
			"<< /Type /Catalog /Pages 2 0 R /OpenAction 4 0 R >>", pdfPages, pdfPage,
			"<< /S /JavaScript /JS (app.alert(1);) >>",
		), []string{reasonPDFAutoJS}},
		{"javascript chained behind a harmless open action", buildPDF(
			"<< /Type /Catalog /Pages 2 0 R /OpenAction << /S /GoTo /D [3 0 R /Fit] /Next 4 0 R >> >>", pdfPages, pdfPage,
			"<< /S /JavaScript /JS (app.alert(1);) >>",
		), []string{reasonPDFAutoJS}},
		{"document-level named javascript", buildPDF(
			"<< /Type /Catalog /Pages 2 0 R /Names << /JavaScript << /Names [(init) 4 0 R] >> >> >>", pdfPages, pdfPage,
			"<< /S /JavaScript /JS (app.alert(1);) >>",
		), []string{reasonPDFAutoJS}},
		{"page-open trigger", buildPDF(
			"<< /Type /Catalog /Pages 2 0 R >>", pdfPages,
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /AA << /O << /S /JavaScript /JS (x();) >> >> >>",
		), []string{reasonPDFAutoJS}},
		{"launches a program", buildPDF(
			"<< /Type /Catalog /Pages 2 0 R /OpenAction << /S /Launch /F (cmd.exe) >> >>", pdfPages, pdfPage,
		), []string{reasonPDFLaunch}},
		{"link that opens another pdf is not a launch risk", buildPDF(
			"<< /Type /Catalog /Pages 2 0 R >>", pdfPages,
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Annots [4 0 R] >>",
			"<< /Type /Annot /Subtype /Link /Rect [0 0 50 20] /A << /S /Launch /F << /Type /Filespec /F (../D&B Experience List Jan2026 1.pdf) /UF (../D&B Experience List Jan2026 1.pdf) >> >> >>",
		), nil},
		{"launch with windows parameters", buildPDF(
			"<< /Type /Catalog /Pages 2 0 R /OpenAction << /S /Launch /Win << /F (cmd.exe) /P (/c start calc) >> >> >>", pdfPages, pdfPage,
		), []string{reasonPDFLaunch}},
		{"launch passing parameters to a document name", buildPDF(
			"<< /Type /Catalog /Pages 2 0 R /OpenAction << /S /Launch /Win << /F (report.pdf) /P (/c evil) >> >> >>", pdfPages, pdfPage,
		), []string{reasonPDFLaunch}},
		{"launch of a file without extension", buildPDF(
			"<< /Type /Catalog /Pages 2 0 R /OpenAction << /S /Launch /F (cmd) >> >>", pdfPages, pdfPage,
		), []string{reasonPDFLaunch}},
		{"broken file launching a document link", []byte("%PDF-1.4\n1 0 obj << /A << /S /Launch /F << /F (../Brochure.pdf) >> >> >> endobj\n%%EOF"), nil},
		{"broken file launching a program", []byte("%PDF-1.4\n1 0 obj << /A << /S /Launch /F (calc.exe) >> >> endobj\n%%EOF"), []string{reasonPDFLaunch}},
		{"disguised keyword", buildPDF(
			"<< /Type /Catalog /Pages 2 0 R /OpenAction 4 0 R >>", pdfPages, pdfPage,
			"<< /S /J#61vaScript /JS (app.alert(1);) >>",
		), []string{reasonPDFDisguise, reasonPDFAutoJS}},
		{"javascript hidden in a compressed object stream", buildPDFObjStm(
			"<< /Type /Catalog /Pages 2 0 R /OpenAction 4 0 R >>", pdfPages, pdfPage,
			"<< /S /JavaScript /JS (app.alert(1);) >>",
		), []string{reasonPDFAutoJS}},
		{"disguise hidden in a compressed object stream", buildPDFObjStm(
			"<< /Type /Catalog /Pages 2 0 R /OpenAction 4 0 R >>", pdfPages, pdfPage,
			"<< /S /JavaScript /J#53 (app.alert(1);) >>",
		), []string{reasonPDFAutoJS, reasonPDFDisguise}},
		{"compressed plain document", buildPDFObjStm("<< /Type /Catalog /Pages 2 0 R >>", pdfPages, pdfPage), nil},
		{"launch hidden in a compressed object stream", buildPDFObjStm(
			"<< /Type /Catalog /Pages 2 0 R >>", pdfPages,
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Annots [4 0 R] >>",
			"<< /Type /Annot /Subtype /Link /Rect [0 0 50 20] /A << /S /Launch /F (cmd.exe) >> >>",
		), []string{reasonPDFLaunch}},
		{"page-open trigger in a compressed object stream", buildPDFObjStm(
			"<< /Type /Catalog /Pages 2 0 R >>", pdfPages,
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /AA << /O << /S /JavaScript /JS (x();) >> >> >>",
		), []string{reasonPDFAutoJS}},
		{"broken file falls back to bytes", []byte("%PDF-1.4\n1 0 obj << /OpenAction << /S /JavaScript /JS (x) >> >> endobj\n%%EOF"), []string{reasonPDFAutoJS}},
		{"broken file with only form script", []byte("%PDF-1.4\n1 0 obj << /A << /S /JavaScript /JS (x) >> >> endobj\n%%EOF"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pdfReasons(tc.pdf)
			if !sameReasons(got, tc.want) {
				t.Fatalf("pdfReasons = %q, want %q", got, tc.want)
			}
		})
	}
}

// The parsed path, not the byte fallback, must be what reads these files:
// otherwise the compressed and chained cases would only pass by accident.
func TestPDFFixturesParse(t *testing.T) {
	for _, pdf := range [][]byte{
		buildPDF("<< /Type /Catalog /Pages 2 0 R >>", pdfPages, pdfPage),
		buildPDFObjStm("<< /Type /Catalog /Pages 2 0 R >>", pdfPages, pdfPage),
	} {
		if _, err := readPDF(pdf); err != nil {
			t.Fatalf("fixture did not parse: %v\n%s", err, strings.ToValidUTF8(string(pdf[:min(len(pdf), 200)]), "?"))
		}
	}
}

func sameReasons(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
