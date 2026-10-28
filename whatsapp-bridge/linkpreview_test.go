package main

import "testing"

func TestFirstLink(t *testing.T) {
	cases := map[string]string{
		"watch this: https://youtube.com/shorts/abcDEF12345?si=xyz.": "https://youtube.com/shorts/abcDEF12345?si=xyz",
		"(see https://example.com/a)":                                "https://example.com/a",
		"no link here":                                               "",
	}
	for in, want := range cases {
		if got := firstLink(in); got != want {
			t.Errorf("firstLink(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestYouTubeIDShapes(t *testing.T) {
	for _, link := range []string{
		"https://youtube.com/shorts/abcDEF12345?si=xyz",
		"https://www.youtube.com/shorts/abcDEF12345",
		"https://m.youtube.com/watch?v=abcDEF12345&t=3",
		"https://youtu.be/abcDEF12345",
		"https://www.youtube.com/live/abcDEF12345",
	} {
		m := youTubeIDRe.FindStringSubmatch(link)
		if m == nil || m[1] != "abcDEF12345" {
			t.Errorf("no video id in %q (got %v)", link, m)
		}
	}
}
