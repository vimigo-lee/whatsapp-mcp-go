package main

import (
	"encoding/binary"
	"testing"
)

// chainPage builds one Ogg page with an explicit serial and header type
// (checksum left zero: analyzeOggOpus does not verify it).
func chainPage(serial, seq uint32, headerType byte, granule uint64, payload []byte) []byte {
	b := []byte("OggS")
	b = append(b, 0, headerType)
	b = binary.LittleEndian.AppendUint64(b, granule)
	b = binary.LittleEndian.AppendUint32(b, serial)
	b = binary.LittleEndian.AppendUint32(b, seq)
	b = binary.LittleEndian.AppendUint32(b, 0)
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

// opusChainStream is one complete logical stream: BOS OpusHead page, OpusTags,
// an audio page half-way through, and a final page at the full length. When
// lastGranule is non-zero it replaces the final page's granule.
func opusChainStream(serial uint32, preSkip uint16, seconds float64, bos bool, lastGranule uint64) []byte {
	head := []byte("OpusHead")
	head = append(head, 1, 1)
	head = binary.LittleEndian.AppendUint16(head, preSkip)
	head = binary.LittleEndian.AppendUint32(head, 48000)
	head = append(head, 0, 0, 0)
	tags := append([]byte("OpusTags"), make([]byte, 16)...)
	audio := make([]byte, 200)
	full := uint64(seconds*48000) + uint64(preSkip)
	final := full
	if lastGranule != 0 {
		final = lastGranule
	}
	var first byte
	if bos {
		first = 0x02
	}
	var s []byte
	s = append(s, chainPage(serial, 0, first, 0, head)...)
	s = append(s, chainPage(serial, 1, 0, 0, tags)...)
	s = append(s, chainPage(serial, 2, 0, full/2, audio)...)
	s = append(s, chainPage(serial, 3, 0x04, final, audio)...)
	return s
}

// interleavedWithSkeleton alternates a 9 s Opus stream (serial 1) page by page
// with a non-Opus stream (serial 99) whose granules are large and unrelated.
func interleavedWithSkeleton() []byte {
	opus := opusChainStream(1, 312, 9, true, 0)
	skel := [][]byte{
		chainPage(99, 0, 0x02, 0, []byte("fishead\x00skeleton-index")),
		chainPage(99, 1, 0, 5_000_000, make([]byte, 40)),
		chainPage(99, 2, 0x04, 9_000_000, make([]byte, 40)),
	}
	// Split the Opus stream back into its four pages to interleave them.
	var pages [][]byte
	for i := 0; i < len(opus); {
		n := int(opus[i+26])
		size := 27 + n
		for _, l := range opus[i+27 : i+27+n] {
			size += int(l)
		}
		pages = append(pages, opus[i:i+size])
		i += size
	}
	var out []byte
	for i, p := range pages {
		out = append(out, p...)
		if i < len(skel) {
			out = append(out, skel[i]...)
		}
	}
	return out
}

func TestAnalyzeOggOpusChainedStreams(t *testing.T) {
	join := func(parts ...[]byte) []byte {
		var out []byte
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	cases := []struct {
		name string
		data []byte
		want uint32
	}{
		{"single 15s stream", opusChainStream(1, 312, 15, true, 0), 15},
		{"two chained 7.5s streams", join(opusChainStream(1, 312, 7.5, true, 0), opusChainStream(2, 312, 7.5, true, 0)), 15},
		{"three chained 10+10+2s", join(opusChainStream(1, 312, 10, true, 0), opusChainStream(2, 312, 10, true, 0), opusChainStream(3, 312, 2, true, 0)), 22},
		{"each stream's own pre-skip", join(opusChainStream(1, 312, 5, true, 0), opusChainStream(2, 3840, 5, true, 0)), 10},
		{"chained by serial change, no BOS flag", join(opusChainStream(7, 312, 6, false, 0), opusChainStream(8, 312, 6, false, 0)), 12},
		{"chained by BOS, same serial", join(opusChainStream(5, 312, 4, true, 0), opusChainStream(5, 312, 4, true, 0)), 8},
		{"final page granule -1 keeps the last real one", opusChainStream(1, 312, 15, true, ^uint64(0)), 8},
		{"interleaved non-Opus stream is not counted", interleavedWithSkeleton(), 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, wave, err := analyzeOggOpus(tc.data)
			if err != nil {
				t.Fatalf("analyzeOggOpus: %v", err)
			}
			if got != tc.want {
				t.Fatalf("duration = %d s, want %d s", got, tc.want)
			}
			if len(wave) == 0 {
				t.Fatalf("no waveform")
			}
		})
	}
}
