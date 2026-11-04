package helpers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ConvertToOpusOggTemp – creates a temporary .ogg file in Opus format
// Uses reasonable defaults for WhatsApp voice messages
func ConvertToOpusOggTemp(inputFile string) (string, error) {
	// WhatsApp voice messages usually use:
	//   bitrate  → 24k–32k is very common and good quality/size balance
	//   sample rate → 48000 Hz (Opus native)
	const defaultBitrate = "32k"
	const defaultSampleRate = 48000

	return ConvertToOpusOggTempWithParams(inputFile, defaultBitrate, defaultSampleRate)
}

// ffmpegCommand builds the ffmpeg invocation; tests replace it to observe
// what would run.
var ffmpegCommand = func(args ...string) *exec.Cmd {
	return exec.Command("ffmpeg", args...)
}

// ConvertToOpusOgg converts an audio file to Opus format in an Ogg container.
//
// ffmpeg's -i takes URLs as readily as files (http, ftp, concat, ...), so a
// media_path that is really a URL would have it fetch from the server's own
// network. Only an existing regular file is accepted, and ffmpeg is told to
// read nothing but that file.
func ConvertToOpusOgg(inputFile string, outputFile string, bitrate string, sampleRate int) (string, error) {
	absInput, err := filepath.Abs(inputFile)
	if err != nil {
		return "", fmt.Errorf("invalid input path %s: %w", inputFile, err)
	}
	// Any Stat error is a refusal: a long URL fails with ENAMETOOLONG and a
	// Windows "http://" with ERROR_INVALID_NAME, neither of which is NotExist.
	info, err := os.Stat(absInput)
	if err != nil {
		return "", fmt.Errorf("input file not readable: %s", inputFile)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("input is not a regular file: %s", inputFile)
	}

	// If no output file is specified, replace extension with .ogg
	if outputFile == "" {
		ext := filepath.Ext(absInput)
		outputFile = strings.TrimSuffix(absInput, ext) + ".ogg"
	}

	// Ensure output directory exists
	outputDir := filepath.Dir(outputFile)
	if outputDir != "." {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	// Build the ffmpeg command
	cmd := ffmpegCommand(
		"-nostdin",
		"-protocol_whitelist", "file",
		"-i", "file:"+absInput,
		"-c:a", "libopus",
		"-b:a", bitrate,
		"-ar", fmt.Sprintf("%d", sampleRate),
		"-application", "voip",
		"-vbr", "on",
		"-compression_level", "10",
		"-frame_duration", "60",
		"-y",
		outputFile,
	)

	// Run command and capture output for error reporting
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to convert audio. ffmpeg error: %s (%w)", string(output), err)
	}

	return outputFile, nil
}

// ConvertToOpusOggTempWithParams converts audio to a temporary .ogg file.
func ConvertToOpusOggTempWithParams(inputFile string, bitrate string, sampleRate int) (string, error) {
	// Create a temporary file
	tempFile, err := os.CreateTemp("", "audio-*.ogg")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	err = tempFile.Close()
	if err != nil {
		return "", err
	} // Close it so ffmpeg can write to the path

	// Convert the audio
	result, err := ConvertToOpusOgg(inputFile, tempPath, bitrate, sampleRate)
	if err != nil {
		// Clean up on failure; the conversion error is the one that matters.
		_ = os.Remove(tempPath)
		return "", err
	}

	return result, nil
}
