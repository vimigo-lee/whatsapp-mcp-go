package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hugeGrayPNG declares w x h at 1 bit per pixel with all-zero rows, so it
// compresses to a few KB while image.Decode would allocate w*h bytes.
func hugeGrayPNG(w, h int) []byte {
	var raw bytes.Buffer
	row := make([]byte, 1+(w+7)/8)
	for y := 0; y < h; y++ {
		raw.Write(row)
	}
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestCompression)
	zw.Write(raw.Bytes())
	zw.Close()

	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(typ string, data []byte) {
		binary.Write(&out, binary.BigEndian, uint32(len(data)))
		out.WriteString(typ)
		out.Write(data)
		binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = 1, 0 // bit depth 1, grayscale
	chunk("IHDR", ihdr)
	chunk("IDAT", z.Bytes())
	chunk("IEND", nil)
	return out.Bytes()
}

// A small file declaring huge dimensions must be refused before decoding,
// while an ordinary image still becomes a thumbnail.
func TestPreviewThumbnailRefusesHugeDimensions(t *testing.T) {
	var small bytes.Buffer
	png.Encode(&small, image.NewRGBA(image.Rect(0, 0, 640, 480)))
	images := map[string][]byte{
		"/huge.png":  hugeGrayPNG(5000, 5000),
		"/small.png": small.Bytes(),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(images[r.URL.Path])
	}))
	defer srv.Close()
	prev := previewClient
	previewClient = srv.Client() // the test server is on loopback, which previews refuse
	defer func() { previewClient = prev }()

	if _, _, _, err := previewThumbnail(context.Background(), srv.URL+"/huge.png"); err == nil {
		t.Error("5000x5000 preview image was decoded, want refused")
	}
	thumb, w, h, err := previewThumbnail(context.Background(), srv.URL+"/small.png")
	if err != nil || len(thumb) == 0 || w != linkPreviewThumbEdge || h != 300 {
		t.Errorf("ordinary image: %d bytes %dx%d err=%v, want a 400x300 thumbnail", len(thumb), w, h, err)
	}
}
