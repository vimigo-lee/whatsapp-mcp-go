package helpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stubFFmpeg records each ffmpeg invocation and runs a program that does not
// exist instead, so a conversion that gets as far as ffmpeg fails.
func stubFFmpeg(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	prev := ffmpegCommand
	ffmpegCommand = func(args ...string) *exec.Cmd {
		calls = append(calls, args)
		return exec.Command(filepath.Join(t.TempDir(), "no-such-ffmpeg"))
	}
	t.Cleanup(func() { ffmpegCommand = prev })
	return &calls
}

// send_audio_message's media_path reaches ffmpeg, which fetches URLs given to
// -i. A URL must be refused before ffmpeg runs: a long one fails os.Stat with
// ENAMETOOLONG on Linux, and any "http://" fails with ERROR_INVALID_NAME on
// Windows, neither of which is "not exist".
func TestSendAudioRefusesURLsBeforeFFmpeg(t *testing.T) {
	t.Setenv("IS_HTTP", "false")
	calls := stubFFmpeg(t)
	short := "http://169.254.169.254/latest/meta-data/iam/security-credentials/"
	long := "http://10.0.0.1/" + strings.Repeat("a", 5000) + ".mp3"
	for _, p := range []string{short, long, "concat:/etc/hostname|/etc/hostname", t.TempDir()} {
		if ok, msg := SendAudioVoiceMessage("60123456789", p); ok {
			t.Errorf("%.60s: accepted (%s)", p, msg)
		}
		if _, err := ConvertToOpusOgg(p, "", "32k", 48000); err == nil {
			t.Errorf("%.60s: ConvertToOpusOgg accepted it", p)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("ffmpeg ran %d times for refused paths: %v", len(*calls), *calls)
	}
}

// A remote (HTTP) server cannot read the caller's disk, so it takes no path.
func TestSendAudioRefusedOverHTTP(t *testing.T) {
	t.Setenv("IS_HTTP", "true")
	calls := stubFFmpeg(t)
	f := filepath.Join(t.TempDir(), "note.mp3")
	if err := os.WriteFile(f, []byte("ID3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := SendAudioVoiceMessage("60123456789", f); ok || len(*calls) != 0 {
		t.Errorf("HTTP mode: ok=%v ffmpeg calls=%d, want refused before ffmpeg", ok, len(*calls))
	}
}

// ffmpeg is told to read only the local file, and a failed conversion is
// reported (not swallowed) with its temp file removed.
func TestConvertToOpusOggTempReadsOnlyTheFileAndReportsFailure(t *testing.T) {
	tmp := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, tmp)
	}
	calls := stubFFmpeg(t)
	in := filepath.Join(t.TempDir(), "note.mp3")
	if err := os.WriteFile(in, []byte("ID3"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := ConvertToOpusOggTemp(in)
	if err == nil {
		t.Fatalf("conversion failure was swallowed, got %q", out)
	}
	if len(*calls) != 1 {
		t.Fatalf("ffmpeg calls = %d, want 1", len(*calls))
	}
	args := (*calls)[0]
	i := slices.Index(args, "-i")
	if i < 0 || i+1 >= len(args) || args[i+1] != "file:"+in {
		t.Errorf("input arg = %v, want -i file:%s", args, in)
	}
	if j := slices.Index(args, "-protocol_whitelist"); j < 0 || j > i || args[j+1] != "file" {
		t.Errorf("args %v lack -protocol_whitelist file before -i", args)
	}
	if !slices.Contains(args[:i], "-nostdin") {
		t.Errorf("args %v lack -nostdin", args)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}
