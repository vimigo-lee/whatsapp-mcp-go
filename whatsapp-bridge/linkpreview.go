package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// WhatsApp link previews are built by the SENDER: the recipient's phone never
// fetches the URL itself. A plain Conversation with a link therefore arrives
// as bare blue text — every API send looked like that, YouTube Shorts
// included. attachLinkPreview fetches the first link's title and thumbnail and
// sends the text as an ExtendedTextMessage carrying them, the way the phone
// app does. Any failure leaves the message as plain text; a preview is never
// worth failing or delaying a send for long.

const (
	linkPreviewTimeout   = 6 * time.Second
	linkPreviewPageBytes = 512 << 10
	linkPreviewImageMax  = 4 << 20
	linkPreviewThumbEdge = 400
)

var (
	firstLinkRe = regexp.MustCompile(`https?://[^\s<>"']+`)
	youTubeIDRe = regexp.MustCompile(`(?:youtu\.be/|[?&]v=|/shorts/|/embed/|/live/)([A-Za-z0-9_-]{11})`)
	metaTagRe   = regexp.MustCompile(`(?is)<meta\s[^>]*>`)
	metaAttrRe  = regexp.MustCompile(`(?is)(property|name|content)\s*=\s*("([^"]*)"|'([^']*)')`)
	titleTagRe  = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

type linkPreview struct {
	matched     string
	title       string
	description string
	imageURL    string
	video       bool
}

// firstLink is the first http(s) URL in the text, without the sentence
// punctuation that usually follows it ("watch this: https://…/abc).").
func firstLink(text string) string {
	link := firstLinkRe.FindString(text)
	return strings.TrimRight(link, ".,;:!?)]}*_~")
}

func isYouTubeHost(host string) bool {
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	host = strings.TrimPrefix(host, "m.")
	return host == "youtube.com" || host == "youtu.be" || host == "music.youtube.com" || host == "youtube-nocookie.com"
}

func previewHTTPGet(ctx context.Context, rawURL string, limit int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	// The crawler UA most sites already answer with Open Graph tags.
	req.Header.Set("User-Agent", "facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)")
	req.Header.Set("Accept-Language", "en")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return body, resp.Header.Get("Content-Type"), err
}

// youTubePreview uses oEmbed and the static thumbnail rather than the page:
// youtube.com HTML is megabytes of script and often a consent wall from a
// server IP, and /shorts/ pages carry no usable og: tags at all.
func youTubePreview(ctx context.Context, link, videoID string) (*linkPreview, error) {
	watch := "https://www.youtube.com/watch?v=" + videoID
	body, _, err := previewHTTPGet(ctx, "https://www.youtube.com/oembed?format=json&url="+url.QueryEscape(watch), 64<<10)
	if err != nil {
		return nil, err
	}
	var oembed struct {
		Title      string `json:"title"`
		AuthorName string `json:"author_name"`
	}
	if err := json.Unmarshal(body, &oembed); err != nil || oembed.Title == "" {
		return nil, fmt.Errorf("oembed: no title")
	}
	return &linkPreview{
		matched:     link,
		title:       oembed.Title,
		description: oembed.AuthorName,
		imageURL:    "https://i.ytimg.com/vi/" + videoID + "/hqdefault.jpg",
		video:       true,
	}, nil
}

func pagePreview(ctx context.Context, link string) (*linkPreview, error) {
	body, contentType, err := previewHTTPGet(ctx, link, linkPreviewPageBytes)
	if err != nil {
		return nil, err
	}
	if contentType != "" && !strings.Contains(contentType, "html") {
		return nil, fmt.Errorf("not html: %s", contentType)
	}
	meta := map[string]string{}
	for _, tag := range metaTagRe.FindAllString(string(body), -1) {
		var key, content string
		for _, attr := range metaAttrRe.FindAllStringSubmatch(tag, -1) {
			value := attr[3] + attr[4]
			if strings.EqualFold(attr[1], "content") {
				content = value
			} else {
				key = strings.ToLower(value)
			}
		}
		if key != "" && content != "" {
			if _, seen := meta[key]; !seen {
				meta[key] = html.UnescapeString(strings.TrimSpace(content))
			}
		}
	}
	p := &linkPreview{matched: link}
	p.title = firstNonEmpty(meta["og:title"], meta["twitter:title"])
	if p.title == "" {
		if m := titleTagRe.FindSubmatch(body); m != nil {
			p.title = html.UnescapeString(strings.TrimSpace(string(m[1])))
		}
	}
	if p.title == "" {
		return nil, fmt.Errorf("no title")
	}
	p.description = firstNonEmpty(meta["og:description"], meta["twitter:description"], meta["description"])
	if img := firstNonEmpty(meta["og:image"], meta["og:image:url"], meta["twitter:image"]); img != "" {
		if base, err := url.Parse(link); err == nil {
			if ref, err := url.Parse(img); err == nil {
				p.imageURL = base.ResolveReference(ref).String()
			}
		}
	}
	p.video = strings.HasPrefix(meta["og:type"], "video")
	return p, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// previewThumbnail downloads the preview image and re-encodes it as a JPEG no
// larger than linkPreviewThumbEdge on its long side.
func previewThumbnail(ctx context.Context, imageURL string) ([]byte, int, int, error) {
	data, _, err := previewHTTPGet(ctx, imageURL, linkPreviewImageMax)
	if err != nil {
		return nil, 0, 0, err
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return nil, 0, 0, fmt.Errorf("empty image")
	}
	if long := max(w, h); long > linkPreviewThumbEdge {
		w, h = w*linkPreviewThumbEdge/long, h*linkPreviewThumbEdge/long
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*b.Dx()/w, b.Min.Y+y*b.Dy()/h))
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), w, h, nil
}

// attachLinkPreview turns a text-only message that contains a link into an
// ExtendedTextMessage with a title, description and thumbnail. It keeps any
// ContextInfo (mentions, reply) already built. Best effort: on any error the
// message is left exactly as it was.
func attachLinkPreview(client *whatsmeow.Client, msg *waE2E.Message, text string) {
	if msg.ImageMessage != nil || msg.VideoMessage != nil || msg.DocumentMessage != nil || msg.AudioMessage != nil {
		return
	}
	link := firstLink(text)
	if link == "" {
		return
	}
	parsed, err := url.Parse(link)
	if err != nil || parsed.Host == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), linkPreviewTimeout)
	defer cancel()

	var p *linkPreview
	if m := youTubeIDRe.FindStringSubmatch(link); m != nil && isYouTubeHost(parsed.Host) {
		p, err = youTubePreview(ctx, link, m[1])
	} else {
		p, err = pagePreview(ctx, link)
	}
	if err != nil || p == nil {
		fmt.Printf("Link preview skipped for %s: %v\n", link, err)
		return
	}

	ext := msg.ExtendedTextMessage
	if ext == nil {
		ext = &waE2E.ExtendedTextMessage{Text: proto.String(text)}
	}
	ext.MatchedText = proto.String(p.matched)
	ext.Title = proto.String(p.title)
	if p.description != "" {
		ext.Description = proto.String(p.description)
	}
	if p.video {
		ext.PreviewType = waE2E.ExtendedTextMessage_VIDEO.Enum()
	} else {
		ext.PreviewType = waE2E.ExtendedTextMessage_NONE.Enum()
	}

	if p.imageURL != "" {
		if thumb, w, h, err := previewThumbnail(ctx, p.imageURL); err == nil {
			ext.JPEGThumbnail = thumb
			ext.ThumbnailWidth = proto.Uint32(uint32(w))
			ext.ThumbnailHeight = proto.Uint32(uint32(h))
			// Current clients render the large card only from an uploaded
			// thumbnail; the inline JPEG above is the small fallback.
			if up, err := client.Upload(ctx, thumb, whatsmeow.MediaLinkThumbnail); err == nil {
				ext.ThumbnailDirectPath = proto.String(up.DirectPath)
				ext.ThumbnailSHA256 = up.FileSHA256
				ext.ThumbnailEncSHA256 = up.FileEncSHA256
				ext.MediaKey = up.MediaKey
				ext.MediaKeyTimestamp = proto.Int64(time.Now().Unix())
			} else {
				fmt.Printf("Link preview thumbnail upload failed for %s: %v\n", link, err)
			}
		} else {
			fmt.Printf("Link preview thumbnail skipped for %s: %v\n", link, err)
		}
	}

	msg.Conversation = nil
	msg.ExtendedTextMessage = ext
}
