package main

import (
	"encoding/binary"
	"testing"
)

// oggPage builds one Ogg page (checksum left zero: analyzeOggOpus does not verify it).
func oggPage(seq uint32, granule uint64, payload []byte) []byte {
	b := []byte("OggS")
	b = append(b, 0, 0) // version, header type
	b = binary.LittleEndian.AppendUint64(b, granule)
	b = binary.LittleEndian.AppendUint32(b, 1) // stream serial
	b = binary.LittleEndian.AppendUint32(b, seq)
	b = binary.LittleEndian.AppendUint32(b, 0) // CRC
	var lacing []byte
	n := len(payload)
	for n >= 255 {
		lacing = append(lacing, 255)
		n -= 255
	}
	lacing = append(lacing, byte(n))
	b = append(b, byte(len(lacing)))
	b = append(b, lacing...)
	return append(b, payload...)
}

// voiceNote builds a minimal Ogg Opus stream: OpusHead, OpusTags, one audio page.
func voiceNote(preSkip uint16, inputRate uint32, seconds uint64) []byte {
	head := []byte("OpusHead")
	head = append(head, 1, 1) // version, mono
	head = binary.LittleEndian.AppendUint16(head, preSkip)
	head = binary.LittleEndian.AppendUint32(head, inputRate)
	head = append(head, 0, 0, 0) // output gain, mapping family 0
	tags := []byte("OpusTags")
	tags = binary.LittleEndian.AppendUint32(tags, 4)
	tags = append(tags, "test"...)
	tags = binary.LittleEndian.AppendUint32(tags, 0)
	audio := make([]byte, 300)
	data := oggPage(0, 0, head)
	data = append(data, oggPage(1, 0, tags)...)
	return append(data, oggPage(2, uint64(preSkip)+seconds*48000, audio)...)
}

func TestAnalyzeOggOpusDuration(t *testing.T) {
	// The old parser read pre-skip / sample rate 8 bytes too far (into the next
	// page header) and declared every voice note 1 second long.
	for _, tc := range []struct {
		name      string
		inputRate uint32
		seconds   uint64
	}{
		{"48 kHz source", 48000, 15},
		{"16 kHz source (granules are still 48 kHz)", 16000, 15},
		{"short note", 24000, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, wave, err := analyzeOggOpus(voiceNote(312, tc.inputRate, tc.seconds))
			if err != nil {
				t.Fatalf("analyzeOggOpus: %v", err)
			}
			if uint64(got) != tc.seconds {
				t.Fatalf("duration = %d s, want %d s", got, tc.seconds)
			}
			if len(wave) != 64 {
				t.Fatalf("waveform = %d bytes, want 64", len(wave))
			}
		})
	}
}

func TestOpusHeadNeverReadsPastThePage(t *testing.T) {
	short := append([]byte("xxOpusHead"), 1, 1, 0x38) // truncated header
	if _, _, ok := opusHead(short); ok {
		t.Fatal("a truncated OpusHead must not parse")
	}
	ps, rate, ok := opusHead(voiceNote(312, 48000, 1)[28:])
	if !ok || ps != 312 || rate != 48000 {
		t.Fatalf("opusHead = (%d, %d, %v), want (312, 48000, true)", ps, rate, ok)
	}
}
