package main

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// PDF checks read the file the way a PDF reader does (compressed object
// streams unpacked, #xx name escapes decoded, references followed) without
// rendering it or running anything. Only behaviour that acts without a click
// is flagged: JavaScript that runs on open, actions that launch programs, and
// keywords disguised to slip past scanners. Script in a fillable form that
// runs when someone types or clicks is normal and not flagged.

const (
	pdfMaxParseBytes = 64 << 20         // larger PDFs get the byte-level check only
	pdfParseTimeout  = 10 * time.Second // a malformed PDF can't stall the download
)

const (
	reasonPDFAutoJS   = "PDF runs JavaScript when opened"
	reasonPDFLaunch   = "PDF can launch programs"
	reasonPDFDisguise = "PDF disguises its keywords, a trick to hide from scanners"
)

func pdfReasons(data []byte) []string {
	var reasons []string
	if pdfHasDisguisedNames(data) {
		reasons = append(reasons, reasonPDFDisguise)
	}
	if len(data) > pdfMaxParseBytes {
		return append(reasons, pdfRawReasons(data)...)
	}
	ctx, err := readPDF(data)
	if err != nil {
		if pdfPasswordProtected(err) {
			// A password-locked PDF (bank statements, payslips) can't be
			// looked into; that alone is too common to be worth a warning.
			return reasons
		}
		// Readers repair broken PDFs that a strict parser rejects, and
		// malicious files are often broken on purpose, so fall back to bytes.
		return append(reasons, pdfRawReasons(data)...)
	}
	return append(reasons, pdfObjectReasons(ctx)...)
}

// readPDF parses data into pdfcpu's object table. It never touches pdfcpu's
// config folder, gives up after pdfParseTimeout, and turns a parser panic on
// a hostile file into an error.
func readPDF(data []byte) (ctx *model.Context, err error) {
	defer func() {
		if r := recover(); r != nil {
			ctx, err = nil, errPDFUnreadable
		}
	}()
	c, cancel := context.WithTimeout(context.Background(), pdfParseTimeout)
	defer cancel()
	conf := model.NewStatelessConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	return api.ReadContext(c, bytes.NewReader(data), conf)
}

type pdfError string

func (e pdfError) Error() string { return string(e) }

const errPDFUnreadable = pdfError("pdf unreadable")

func pdfPasswordProtected(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "password") || strings.Contains(msg, "encrypt")
}

// pdfAutoTriggers are additional-action (/AA) entries that fire without a
// click: page open (O), annotation page open/visible (PO, PV) and the
// document-level close/save/print events (WC, WS, DS, WP, DP).
var pdfAutoTriggers = []string{"O", "PO", "PV", "WC", "WS", "DS", "WP", "DP"}

// pdfObjectReasons walks every object in the parsed file.
func pdfObjectReasons(ctx *model.Context) []string {
	xrt := ctx.XRefTable
	deref := func(o types.Object) types.Object {
		d, err := xrt.Dereference(o)
		if err != nil {
			return nil
		}
		return d
	}
	var isJS func(o types.Object, depth int) bool
	isJS = func(o types.Object, depth int) bool {
		if depth > 8 {
			return false
		}
		switch v := deref(o).(type) {
		case types.Dict:
			if pdfName(v["S"]) == "JavaScript" {
				return true
			}
			return v["Next"] != nil && isJS(v["Next"], depth+1)
		case types.Array:
			for _, e := range v {
				if isJS(e, depth+1) {
					return true
				}
			}
		}
		return false
	}

	autoJS, launch, disguised := false, false, false

	// The catalog: OpenAction, document-level /AA, and named document
	// JavaScript (the /Names /JavaScript tree runs as the file opens).
	if xrt.Root != nil {
		if cat, ok := deref(*xrt.Root).(types.Dict); ok {
			if isJS(cat["OpenAction"], 0) {
				autoJS = true
			}
			if aa, ok := deref(cat["AA"]).(types.Dict); ok {
				for _, a := range aa {
					if isJS(a, 0) {
						autoJS = true
					}
				}
			}
			if names, ok := deref(cat["Names"]).(types.Dict); ok && names["JavaScript"] != nil {
				autoJS = true
			}
		}
	}

	// Every page, annotation and inline action: launch actions anywhere, and
	// page or annotation /AA triggers that fire on open.
	var walk func(o types.Object, depth int)
	walk = func(o types.Object, depth int) {
		if depth > 32 {
			return
		}
		switch v := o.(type) {
		case types.Dict:
			if pdfName(v["S"]) == "Launch" && pdfLaunchCanRun(v, deref) {
				launch = true
			}
			if aa, ok := deref(v["AA"]).(types.Dict); ok {
				for _, k := range pdfAutoTriggers {
					if a, ok := aa[k]; ok && isJS(a, 0) {
						autoJS = true
					}
				}
			}
			for _, e := range v {
				walk(e, depth+1)
			}
		case types.StreamDict:
			walk(v.Dict, depth+1)
		case types.ObjectStreamDict:
			// The unpacked stream text keeps the names as written, so
			// disguised keywords inside compressed sections show up here.
			// pdfcpu unpacks lazily; decode a copy, capped like the parse.
			sd := v.StreamDict
			if sd.Content == nil {
				_ = sd.DecodeWithLimit(pdfMaxParseBytes)
			}
			if pdfHasDisguisedNames(sd.Content) {
				disguised = true
			}
		case types.Array:
			for _, e := range v {
				walk(e, depth+1)
			}
		}
	}
	// Objects inside compressed object streams load lazily; dereferencing by
	// number materializes them so they are walked like any other.
	for nr, entry := range xrt.Table {
		if entry == nil || entry.Free {
			continue
		}
		gen := 0
		if entry.Generation != nil {
			gen = *entry.Generation
		}
		if o := deref(*types.NewIndirectRef(nr, gen)); o != nil {
			walk(o, 0)
		}
	}

	var reasons []string
	if autoJS {
		reasons = append(reasons, reasonPDFAutoJS)
	}
	if launch {
		reasons = append(reasons, reasonPDFLaunch)
	}
	if disguised {
		reasons = append(reasons, reasonPDFDisguise)
	}
	return reasons
}

// pdfDocumentExtensions are launch targets that only open another document.
// Office and PDF exports write these for "link to file" (e.g. a company
// profile linking ../Brochure.pdf); they can't run anything.
var pdfDocumentExtensions = map[string]bool{
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
	".odt": true, ".ods": true, ".odp": true, ".rtf": true, ".txt": true, ".csv": true,
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".bmp": true, ".tif": true, ".tiff": true,
	".mp3": true, ".mp4": true, ".mov": true, ".wav": true,
}

// pdfLaunchCanRun reports whether a /Launch action could start a program:
// it passes command-line parameters (/Win /P, the classic cmd.exe trick), or
// its target is a program or script, has no extension, or can't be read.
// A launch that only opens another document is not flagged.
func pdfLaunchCanRun(action types.Dict, deref func(types.Object) types.Object) bool {
	target := ""
	if win, ok := deref(action["Win"]).(types.Dict); ok {
		if p, _ := types.StringOrHexLiteral(deref(win["P"])); p != nil && strings.TrimSpace(*p) != "" {
			return true
		}
		target = pdfFileSpecName(win["F"], deref)
	}
	if target == "" {
		target = pdfFileSpecName(action["F"], deref)
	}
	if target == "" {
		return true
	}
	return !pdfDocumentExtensions[strings.ToLower(filepath.Ext(target))]
}

// pdfFileSpecName reads a file name from a PDF file specification: a plain
// string, or a dictionary whose /UF or /F holds it.
func pdfFileSpecName(o types.Object, deref func(types.Object) types.Object) string {
	switch v := deref(o).(type) {
	case types.Dict:
		for _, k := range []string{"UF", "F", "DOS", "Unix", "Mac"} {
			if s, _ := types.StringOrHexLiteral(deref(v[k])); s != nil && *s != "" {
				return *s
			}
		}
	default:
		if s, _ := types.StringOrHexLiteral(v); s != nil {
			return *s
		}
	}
	return ""
}

func pdfName(o types.Object) string {
	switch v := o.(type) {
	case types.Name:
		return string(v)
	case *types.Name:
		if v != nil {
			return string(*v)
		}
	}
	return ""
}

// pdfEscapedName matches a PDF name written with at least one #xx escape.
var pdfEscapedName = regexp.MustCompile(`/[^\s/<>\[\]()%{}]*#[0-9A-Fa-f]{2}[^\s/<>\[\]()%{}]*`)

// pdfActionNames are the keywords worth hiding. A PDF writer has no reason to
// escape plain letters in them, so an escaped spelling is a deliberate dodge.
var pdfActionNames = map[string]bool{
	"JavaScript": true, "JS": true, "OpenAction": true, "AA": true, "Launch": true,
	"EmbeddedFile": true, "EmbeddedFiles": true, "URI": true, "SubmitForm": true,
	"ImportData": true, "RichMedia": true, "XFA": true,
}

func pdfHasDisguisedNames(b []byte) bool {
	for _, m := range pdfEscapedName.FindAll(b, -1) {
		if decoded, err := types.DecodeName(string(m[1:])); err == nil && pdfActionNames[decoded] {
			return true
		}
	}
	return false
}

// pdfRawPatterns back up the parser for files it can't read. A name token must
// end at a delimiter, so /JSON or /Launcher don't count.
var (
	pdfRawLaunch     = regexp.MustCompile(`/Launch[\s/<>\[\]()]`)
	pdfRawJavaScript = regexp.MustCompile(`/(JavaScript|JS)[\s/<>\[\]()]`)
	pdfRawAutoRun    = regexp.MustCompile(`/(OpenAction|AA)[\s/<>\[\]()]`)
)

// pdfRawReasons is the byte-level fallback. Without the object structure it
// can't tell where an action points, so JavaScript counts as running on open
// whenever the file also has an open or additional action.
func pdfRawReasons(data []byte) []string {
	var reasons []string
	if pdfRawJavaScript.Match(data) && pdfRawAutoRun.Match(data) {
		reasons = append(reasons, reasonPDFAutoJS)
	}
	if pdfRawLaunchCanRun(data) {
		reasons = append(reasons, reasonPDFLaunch)
	}
	return reasons
}

// pdfRawLiteral matches a short PDF string literal (no nested parentheses).
var pdfRawLiteral = regexp.MustCompile(`\(([^()\\]{1,200})\)`)

// pdfRawLaunchCanRun applies pdfLaunchCanRun's rule to raw bytes: it reads the
// action text after each /Launch, so a link that only opens another document
// is not flagged while parameters (/P) or a program target are.
func pdfRawLaunchCanRun(data []byte) bool {
	for _, loc := range pdfRawLaunch.FindAllIndex(data, -1) {
		window := data[loc[0]:min(len(data), loc[1]+400)]
		if end := bytes.Index(window, []byte("endobj")); end > 0 {
			window = window[:end]
		}
		if bytes.Contains(window, []byte("/P")) && bytes.Contains(window, []byte("/Win")) {
			return true
		}
		names := pdfRawLiteral.FindAllSubmatch(window, -1)
		if len(names) == 0 {
			return true
		}
		for _, n := range names {
			if !pdfDocumentExtensions[strings.ToLower(filepath.Ext(string(n[1])))] {
				return true
			}
		}
	}
	return false
}
