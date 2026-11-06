package main

import (
	"encoding/binary"
	"strings"

	"go.mau.fi/whatsmeow"
)

// maxStickerBytes is WhatsApp's documented cap for animated stickers (static
// ones are capped lower, 100 KB, but the phone app still renders larger ones).
const maxStickerBytes = 500 * 1024

// checkSendAs validates a /send request's send_as against the media it carries.
// It returns a readable error, or "" when the combination is sendable.
func checkSendAs(sendAs string, mediaType whatsmeow.MediaType, mimeType string, data []byte, caption string) string {
	switch sendAs {
	case "", "media":
		return ""
	case "gif":
		// WhatsApp "GIFs" are MP4s flagged for looping playback; a real .gif file
		// would need transcoding, which the bridge does not do.
		if mediaType != whatsmeow.MediaVideo || !strings.Contains(mimeType, "mp4") {
			return "send_as=gif needs an MP4 video (WhatsApp GIFs are MP4s); convert the .gif to .mp4 first"
		}
		return ""
	case "sticker":
		// Judge by the bytes, not the caller's MIME: a sticker must really be WebP.
		if mediaType != whatsmeow.MediaImage || !isWebP(data) {
			return "send_as=sticker needs a WebP image (512x512 recommended, animated WebP for a moving sticker)"
		}
		if len(data) > maxStickerBytes {
			return "sticker exceeds WhatsApp's 500 KB limit"
		}
		if caption != "" {
			return "stickers cannot carry a caption; send the text as a separate message"
		}
		return ""
	}
	return "send_as must be one of: media, gif, sticker"
}

func isWebP(b []byte) bool {
	return len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP"
}

// webpInfo reads the canvas size and animation flag from a WebP header. Zero
// sizes mean the header was not one we parse; the sticker still sends.
func webpInfo(b []byte) (width, height uint32, animated bool) {
	if !isWebP(b) || len(b) < 30 {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8X": // extended: flags byte, then 24-bit (w-1), (h-1)
		animated = b[20]&0x02 != 0
		width = 1 + (uint32(b[24]) | uint32(b[25])<<8 | uint32(b[26])<<16)
		height = 1 + (uint32(b[27]) | uint32(b[28])<<8 | uint32(b[29])<<16)
	case "VP8 ": // lossy: 14-bit sizes after the 3-byte frame tag + start code
		width = uint32(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff)
		height = uint32(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
	case "VP8L": // lossless: 14-bit (w-1), (h-1) packed after the 0x2f signature
		bits := binary.LittleEndian.Uint32(b[21:25])
		width = 1 + bits&0x3fff
		height = 1 + (bits>>14)&0x3fff
	}
	return width, height, animated
}
