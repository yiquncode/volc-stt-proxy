package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	sampleRate     = 16000
	bytesPerSecond = sampleRate * 2 // 16-bit mono
)

// demuxerByExt maps allowed upload extensions to the ffmpeg demuxer used to
// read them. Only these demuxers are ever enabled (see ffmpegFormatWhitelist),
// so playlist/reference formats such as hls or concat can't be used to make
// ffmpeg read other files.
var demuxerByExt = map[string]string{
	"wav": "wav", "mp3": "mp3", "m4a": "mov", "mp4": "mov", "mov": "mov", "aac": "aac",
	"ogg": "ogg", "opus": "ogg", "oga": "ogg", "webm": "matroska", "flac": "flac",
	"aif": "aiff", "aiff": "aiff", "caf": "caf",
}

const ffmpegFormatWhitelist = "wav,mp3,mov,aac,ogg,matroska,flac,aiff,caf"

// ffmpegError is returned when ffmpeg could not decode the input.
type ffmpegError struct {
	err    error
	stderr string
}

func (e *ffmpegError) Error() string { return fmt.Sprintf("ffmpeg: %v: %s", e.err, e.stderr) }

var errAudioTooLong = errors.New("decoded audio exceeds the size limit")

// cappedBuffer stores at most max bytes. When strict, writing past the cap is
// an error (which makes ffmpeg exit on a broken pipe); otherwise the excess is
// silently dropped. The bytes.Buffer is deliberately not embedded: its
// promoted ReadFrom would let io.Copy (used by os/exec) bypass Write.
type cappedBuffer struct {
	buf      bytes.Buffer
	max      int
	strict   bool
	overflow bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if len(p) <= room {
		return c.buf.Write(p)
	}
	c.overflow = true
	if c.strict {
		return 0, errAudioTooLong
	}
	c.buf.Write(p[:max(room, 0)])
	return len(p), nil
}

func (c *cappedBuffer) Len() int       { return c.buf.Len() }
func (c *cappedBuffer) Bytes() []byte  { return c.buf.Bytes() }
func (c *cappedBuffer) String() string { return c.buf.String() }

// convertToPCM decodes an uploaded audio file into raw 16 kHz mono s16le
// PCM, keeping at most maxSeconds of audio. The input is read from a file (not
// a pipe) because some containers, e.g. m4a with the moov atom at the end,
// need a seekable input. ext (lowercase, without dot) selects the demuxer; if
// that fails, or ext is unknown, ffmpeg probes the format, restricted to the
// allowlisted demuxers.
func convertToPCM(ctx context.Context, ffmpegPath, inputPath, ext string, maxSeconds int) ([]byte, error) {
	var formats []string
	if d, ok := demuxerByExt[ext]; ok {
		formats = append(formats, d)
	}
	formats = append(formats, "") // "" = probe within the whitelist
	var err error
	for _, format := range formats {
		var pcm []byte
		if pcm, err = runFFmpeg(ctx, ffmpegPath, inputPath, format, maxSeconds); err == nil {
			return pcm, nil
		}
		if ctx.Err() != nil || errors.Is(err, errAudioTooLong) {
			return nil, err
		}
	}
	return nil, err
}

func runFFmpeg(ctx context.Context, ffmpegPath, inputPath, format string, maxSeconds int) ([]byte, error) {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-protocol_whitelist", "file", "-format_whitelist", ffmpegFormatWhitelist}
	if format != "" {
		args = append(args, "-f", format)
	}
	args = append(args, "-i", inputPath,
		"-vn", "-ac", "1", "-ar", "16000", "-acodec", "pcm_s16le",
		"-t", strconv.Itoa(maxSeconds), "-f", "s16le", "pipe:1")
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.WaitDelay = 2 * time.Second
	stdout := &cappedBuffer{max: maxSeconds*bytesPerSecond + bytesPerSecond, strict: true}
	stderr := &cappedBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if stdout.overflow {
			return nil, errAudioTooLong
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500] + "..."
		}
		return nil, &ffmpegError{err: err, stderr: msg}
	}
	return stdout.Bytes(), nil
}

// pcmDuration returns the duration in seconds of 16 kHz mono s16le PCM.
func pcmDuration(pcm []byte) float64 {
	return float64(len(pcm)) / bytesPerSecond
}
