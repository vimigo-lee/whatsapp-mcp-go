package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Received files are flagged, never blocked: the scan's verdict rides along
// with the message (store, webhook, list_messages, /download) and the user
// decides what to do with the file. The checks only read bytes. Nothing is
// unpacked to disk, rendered or executed, so a hostile file can't touch the
// host that scans it.

const (
	scanSuspicious = "suspicious" // something about the file is worth a warning
	scanMalware    = "malware"    // its SHA-256 is on the known-malware list
	scanClean      = "clean"      // the bytes were checked and nothing stood out
)

// FileScan is a file's verdict. An empty Status means the bytes have not been
// checked yet (a message whose media was never downloaded).
type FileScan struct {
	Status   string `json:"scan_status,omitempty"`
	Reason   string `json:"scan_reason,omitempty"`
	RealType string `json:"real_type,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

// knownMalwareSHA256 holds plaintext SHA-256s (the same hash WhatsApp puts in
// the message as file_sha256) of malware seen on our numbers, so a copy is
// flagged before it is even downloaded. KNOWN_MALWARE_SHA256 (comma-separated)
// adds more without a rebuild.
var knownMalwareSHA256 = map[string]string{
	// Zipped VHD/IMG droppers posing as invoices and SSM notices, 2026-10.
	"6c896d12eb9c90d96cdb6d0b81b87da3ea4b6ddde0dc0ae53aadb5875ad68649": "INVOIS.vhd dropper",
	"4d3c8b32d2d7eccc81b473bd108214c429d165a26482645316e239c826cd40c8": "INVOIS.vhd dropper",
	"6d0b992fa257c348953e3bbcd55db975d2da7efe63a1a36e7f047564f63820a5": "PDF_*.vhd dropper",
	"8d0eb3dd82e96a64bbefab73d2e4e4389869adaf5c53024b8775e34d9ff3fd56": "PDF_*.vhd dropper",
	"715cbfe7d89b71dcfe2c2e0bac68c66224bf071155115f4aab234e4407fc61ed": "PDF_*.vhd dropper",
	"685d1d87db146c7fd51e5141d53842ea9be937a64882abdae8fbbb26a656b4a4": "SSM-1.IMG dropper",
	"c7d31dcd70f5b04cc5d3a6ff28fd9a48948803176170dd5b1051bb3716e28686": "disk image hiding OPEN .EXE",
}

func init() {
	for _, h := range strings.Split(os.Getenv("KNOWN_MALWARE_SHA256"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); len(h) == 64 {
			if _, ok := knownMalwareSHA256[h]; !ok {
				knownMalwareSHA256[h] = "listed in KNOWN_MALWARE_SHA256"
			}
		}
	}
}

// riskyExtensions maps an extension to what it is, for the warning text.
var riskyExtensions = map[string]string{}

func init() {
	for kind, exts := range map[string][]string{
		"program or script":         {"exe", "scr", "com", "pif", "bat", "cmd", "ps1", "vbs", "vbe", "js", "jse", "wsf", "wsh", "hta", "msi", "msix", "appx", "dll", "cpl", "jar", "sh", "reg", "scf", "lnk", "url", "chm"},
		"Android app":               {"apk", "xapk"},
		"disk image":                {"iso", "img", "vhd", "vhdx"},
		"macro-enabled Office file": {"docm", "xlsm", "pptm", "dotm", "xltm", "xlam", "ppam"},
		"OneNote file":              {"one"},
		"web page":                  {"html", "htm", "svg"},
	} {
		for _, e := range exts {
			riskyExtensions["."+e] = kind
		}
	}
}

// disguisedExt catches "invoice.pdf.exe": a harmless-looking extension right
// before the real one.
var disguisedExt = regexp.MustCompile(`(?i)\.(pdf|docx?|xlsx?|pptx?|jpe?g|png|gif|txt|csv|mp4|mp3|zip)\.([a-z0-9]{2,5})$`)

// paddedExt catches "OPEN        .EXE": spaces that push the extension out of
// sight in a narrow file list.
var paddedExt = regexp.MustCompile(`\s{3,}\.[A-Za-z0-9]{2,5}$`)

// preScanFile checks what is known before the bytes arrive: the file name and
// the SHA-256 WhatsApp sends with the message. Status stays empty when nothing
// stands out, since the content itself is unchecked.
func preScanFile(mediaType, filename string, fileSHA256 []byte) FileScan {
	s := FileScan{SHA256: hex.EncodeToString(fileSHA256)}
	var reasons []string
	if label, ok := knownMalwareSHA256[s.SHA256]; ok && s.SHA256 != "" {
		s.Status = scanMalware
		reasons = append(reasons, "known malware ("+label+")")
	}
	reasons = append(reasons, nameReasons(filename)...)
	return s.withReasons(reasons)
}

// scanFile checks a downloaded file: its name, its hash and what its bytes
// really are.
func scanFile(mediaType, filename string, data []byte) FileScan {
	sum := sha256.Sum256(data)
	s := preScanFile(mediaType, filename, sum[:])
	kind := sniffFileKind(data)
	s.RealType = kind

	var reasons []string
	if s.Reason != "" {
		reasons = strings.Split(s.Reason, "; ")
	}
	ext := strings.ToLower(filepath.Ext(filename))

	switch kind {
	case "windows-program":
		reasons = append(reasons, "file is a Windows program")
	case "linux-program", "mac-program":
		reasons = append(reasons, "file is a "+strings.TrimSuffix(kind, "-program")+" program")
	case "shell-script":
		reasons = append(reasons, "file is a script")
	case "html":
		if riskyExtensions[ext] == "" {
			reasons = append(reasons, "file is really a web page, which can run scripts when opened")
		}
	}

	if mediaType == "image" || mediaType == "video" || mediaType == "audio" || mediaType == "sticker" {
		// Animated stickers are zipped Lottie files, so a zip is expected there.
		if !mediaKinds[kind] && !(mediaType == "sticker" && kind == "zip") {
			reasons = append(reasons, fmt.Sprintf("sent as %s but is really %s", mediaType, kind))
		}
	} else if want, ok := extensionKinds[ext]; ok && kind != want && !executableKinds[kind] {
		reasons = append(reasons, fmt.Sprintf("named %s but is really %s", ext, kind))
	}

	switch kind {
	case "zip":
		reasons = append(reasons, zipReasons(data)...)
	case "ole":
		if bytes.Contains(data, utf16le("_VBA_PROJECT")) {
			reasons = append(reasons, "Office file with macros")
		}
	case "pdf":
		reasons = append(reasons, pdfReasons(data)...)
	}

	s = s.withReasons(reasons)
	if s.Status == "" {
		s.Status = scanClean
	}
	return s
}

// withReasons sets Reason and, unless the file is already known malware,
// marks it suspicious when there is anything to say.
func (s FileScan) withReasons(reasons []string) FileScan {
	seen := map[string]bool{}
	var out []string
	for _, r := range reasons {
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	s.Reason = strings.Join(out, "; ")
	if len(out) > 0 && s.Status != scanMalware {
		s.Status = scanSuspicious
	}
	return s
}

func nameReasons(filename string) []string {
	var reasons []string
	ext := strings.ToLower(filepath.Ext(filename))
	if kind, ok := riskyExtensions[ext]; ok {
		reasons = append(reasons, fmt.Sprintf("%s (%s)", kind, ext))
	}
	if m := disguisedExt.FindStringSubmatch(filename); m != nil && riskyExtensions["."+strings.ToLower(m[2])] != "" {
		reasons = append(reasons, fmt.Sprintf("disguised name: looks like .%s but is .%s", strings.ToLower(m[1]), strings.ToLower(m[2])))
	}
	if paddedExt.MatchString(filename) {
		reasons = append(reasons, "name pads spaces to hide its extension")
	}
	return reasons
}

var mediaKinds = map[string]bool{"jpeg": true, "png": true, "gif": true, "webp": true, "mp4": true, "ogg": true, "mp3": true, "webm": true, "wav": true, "amr": true}

var executableKinds = map[string]bool{"windows-program": true, "linux-program": true, "mac-program": true, "shell-script": true}

// extensionKinds is what the bytes of a common document should look like.
var extensionKinds = map[string]string{
	".pdf": "pdf", ".jpg": "jpeg", ".jpeg": "jpeg", ".png": "png", ".gif": "gif", ".webp": "webp",
	".docx": "zip", ".xlsx": "zip", ".pptx": "zip", ".zip": "zip",
	".doc": "ole", ".xls": "ole", ".ppt": "ole",
	".mp4": "mp4", ".mp3": "mp3", ".ogg": "ogg",
}

// sniffFileKind names a file's real format from its first bytes.
func sniffFileKind(b []byte) string {
	has := func(off int, sig string) bool {
		return len(b) >= off+len(sig) && string(b[off:off+len(sig)]) == sig
	}
	switch {
	case has(0, "MZ"):
		return "windows-program"
	case has(0, "\x7fELF"):
		return "linux-program"
	case has(0, "\xcf\xfa\xed\xfe"), has(0, "\xfe\xed\xfa\xcf"), has(0, "\xca\xfe\xba\xbe"):
		return "mac-program"
	case has(0, "PK\x03\x04"), has(0, "PK\x05\x06"):
		return "zip"
	case has(0, "%PDF"):
		return "pdf"
	case has(0, "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"):
		return "ole" // legacy Office (.doc/.xls/.ppt) and .msi
	case has(0, `{\rtf`):
		return "rtf" // a .doc that is really RTF is a classic exploit carrier
	case has(0, "Rar!"):
		return "rar"
	case has(0, "7z\xbc\xaf\x27\x1c"):
		return "7z"
	case has(0, "#!AMR"):
		return "amr"
	case has(0, "#!"):
		return "shell-script"
	case has(0, "\xff\xd8\xff"):
		return "jpeg"
	case has(0, "\x89PNG"):
		return "png"
	case has(0, "GIF8"):
		return "gif"
	case has(0, "RIFF") && has(8, "WEBP"):
		return "webp"
	case has(0, "RIFF") && has(8, "WAVE"):
		return "wav"
	case has(4, "ftyp"):
		return "mp4"
	case has(0, "OggS"):
		return "ogg"
	case has(0, "ID3"), len(b) > 1 && b[0] == 0xff && b[1]&0xe0 == 0xe0:
		return "mp3"
	case has(0, "\x1a\x45\xdf\xa3"):
		return "webm"
	}
	head := b
	if len(head) > 1024 {
		head = head[:1024]
	}
	t := strings.ToLower(strings.TrimSpace(string(head)))
	if strings.HasPrefix(t, "<!doctype html") || strings.HasPrefix(t, "<html") || strings.Contains(t, "<script") {
		return "html"
	}
	return "other"
}

// zipBombBytes is how far a zip may claim to expand before it is called out.
const zipBombBytes = 2 << 30

// zipReasons reads a zip's directory (names and sizes only; nothing is
// decompressed) for the tricks malware uses: a disk image or program inside,
// a password that hides the contents from scanners, macros, an Android app.
func zipReasons(data []byte) []string {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return []string{"damaged zip (its contents can't be checked)"}
	}
	var reasons, inner []string
	var expanded uint64
	encrypted, macros, manifest, dex := false, false, false, false
	for _, f := range zr.File {
		expanded += f.UncompressedSize64
		if f.Flags&0x1 != 0 {
			encrypted = true
		}
		base := strings.ToLower(filepath.Base(f.Name))
		switch base {
		case "vbaproject.bin":
			macros = true
		case "androidmanifest.xml":
			manifest = true
		case "classes.dex":
			dex = true
		}
		ext := strings.ToLower(filepath.Ext(f.Name))
		switch riskyExtensions[ext] {
		case "disk image":
			reasons = append(reasons, fmt.Sprintf("zip contains disk image %s, a common way to hide malware", filepath.Base(f.Name)))
		case "program or script", "Android app", "macro-enabled Office file", "OneNote file":
			inner = append(inner, filepath.Base(f.Name))
		}
	}
	if len(inner) > 0 {
		if len(inner) > 3 {
			inner = append(inner[:3], "…")
		}
		reasons = append(reasons, "zip contains "+strings.Join(inner, ", "))
	}
	if encrypted {
		reasons = append(reasons, "password-protected zip (its contents can't be checked)")
	}
	if macros {
		reasons = append(reasons, "Office file with macros")
	}
	if manifest && dex {
		reasons = append(reasons, "Android app (APK)")
	}
	if expanded > zipBombBytes && expanded/uint64(max(len(data), 1)) > 100 {
		reasons = append(reasons, fmt.Sprintf("zip expands to %d GB (possible zip bomb)", expanded>>30))
	}
	return reasons
}

func utf16le(s string) []byte {
	out := make([]byte, 0, len(s)*2)
	for _, r := range s {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

// safeMediaFilename turns a sender-chosen file name into one that can only
// name a file inside the chat's media folder: no folders, no control or
// bidi-override characters, nothing Windows refuses. fallback is used when
// nothing usable is left.
func safeMediaFilename(name, fallback string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r), strings.ContainsRune(`<>:"|?*`, r):
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		return fallback
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	if windowsReservedNames[strings.ToUpper(stem)] {
		name = "_" + name
	}
	const maxRunes = 180
	if utf8.RuneCountInString(name) > maxRunes {
		ext := filepath.Ext(name)
		if utf8.RuneCountInString(ext) > 16 {
			ext = ""
		}
		r := []rune(strings.TrimSuffix(name, ext))
		name = string(r[:maxRunes-utf8.RuneCountInString(ext)]) + ext
	}
	return name
}

var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}
