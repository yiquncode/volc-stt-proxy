package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCappedBuffer(t *testing.T) {
	strict := &cappedBuffer{max: 4, strict: true}
	if _, err := strict.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Write([]byte("e")); !errors.Is(err, errAudioTooLong) || !strict.overflow {
		t.Errorf("strict overflow: err=%v", err)
	}
	loose := &cappedBuffer{max: 4}
	if n, err := loose.Write([]byte("abcdef")); n != 6 || err != nil || loose.String() != "abcd" {
		t.Errorf("loose: n=%d err=%v buf=%q", n, err, loose.String())
	}
}

// TestCappedBufferViaCopy guards against io.Copy bypassing Write through a
// ReadFrom method (os/exec copies child output with io.Copy).
func TestCappedBufferViaCopy(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 100_000)
	onlyReader := func() io.Reader { return struct{ io.Reader }{bytes.NewReader(big)} } // hide WriterTo

	strict := &cappedBuffer{max: 1000, strict: true}
	if _, err := io.Copy(strict, onlyReader()); !errors.Is(err, errAudioTooLong) || !strict.overflow || strict.Len() > 1000 {
		t.Errorf("strict via io.Copy: err=%v overflow=%v len=%d", err, strict.overflow, strict.Len())
	}
	loose := &cappedBuffer{max: 1000}
	if _, err := io.Copy(loose, onlyReader()); err != nil || loose.Len() != 1000 {
		t.Errorf("loose via io.Copy: err=%v len=%d", err, loose.Len())
	}
}

// TestCappedBufferChildProcess checks the caps hold for real child output.
func TestCappedBufferChildProcess(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "head -c 1000000 /dev/zero; head -c 1000000 /dev/zero >&2")
	stdout := &cappedBuffer{max: 1000, strict: true}
	stderr := &cappedBuffer{max: 1000}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if err == nil || !stdout.overflow || stdout.Len() > 1000 {
		t.Errorf("stdout cap not enforced: err=%v overflow=%v len=%d", err, stdout.overflow, stdout.Len())
	}
	if stderr.Len() > 1000 {
		t.Errorf("stderr cap not enforced: len=%d", stderr.Len())
	}
}

// TestConvertToPCM converts 44.1 kHz stereo inputs to 16 kHz mono, in every
// allowlisted container (m4a has the moov atom at the end, which can't be
// read from a pipe), both with the right extension and with none.
func TestConvertToPCM(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	for _, ext := range []string{"wav", "mp3", "m4a", "aac", "ogg", "opus", "webm", "flac", "aiff", "caf"} {
		src := filepath.Join(t.TempDir(), "in."+ext)
		genAudio(t, ffmpeg, src, 1)
		for _, hint := range []string{ext, ""} {
			pcm, err := convertToPCM(context.Background(), ffmpeg, src, hint, 600)
			if err != nil {
				t.Fatalf("%s (hint %q): %v", ext, hint, err)
			}
			if bytes.HasPrefix(pcm, []byte("RIFF")) || len(pcm)%2 != 0 {
				t.Errorf("%s: output is not raw s16le PCM", ext)
			}
			// 16 kHz mono 16-bit: 1 s of input must be ~32000 bytes.
			if d := pcmDuration(pcm); d < 0.95 || d > 1.1 {
				t.Errorf("%s (hint %q): duration %v, want ~1s", ext, hint, d)
			}
		}
	}

	// A mislabeled file (wav content, .m4a name) falls back to probing.
	src := filepath.Join(t.TempDir(), "in.wav")
	genAudio(t, ffmpeg, src, 1)
	if _, err := convertToPCM(context.Background(), ffmpeg, src, "m4a", 600); err != nil {
		t.Errorf("mislabeled wav: %v", err)
	}

	// MAX_AUDIO_SECONDS truncates long audio.
	long := filepath.Join(t.TempDir(), "long.wav")
	genAudio(t, ffmpeg, long, 3)
	pcm, err := convertToPCM(context.Background(), ffmpeg, long, "wav", 1)
	if err != nil || pcmDuration(pcm) > 1.01 {
		t.Errorf("max seconds: err=%v duration=%v", err, pcmDuration(pcm))
	}

	bad := filepath.Join(t.TempDir(), "bad.m4a")
	os.WriteFile(bad, []byte("not audio at all"), 0o600)
	_, err = convertToPCM(context.Background(), ffmpeg, bad, "m4a", 600)
	var fe *ffmpegError
	if !errors.As(err, &fe) || fe.stderr == "" {
		t.Errorf("want ffmpegError with stderr, got %v", err)
	}
}

// writePlaylist writes an HLS playlist referencing target to path.
func writePlaylist(t *testing.T, path, target string) {
	t.Helper()
	pl := "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n" + target + "\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(path, []byte(pl), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestConvertRejectsPlaylists makes sure an uploaded playlist can't make
// ffmpeg read another local file, whatever extension it is uploaded with.
func TestConvertRejectsPlaylists(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.aac") // .aac is an allowed HLS segment extension
	genAudio(t, ffmpeg, secret, 1)
	pl := filepath.Join(dir, "evil.m3u8")
	writePlaylist(t, pl, secret)

	// Sanity check: unrestricted ffmpeg does follow this playlist.
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-i", pl,
		"-f", "s16le", "pipe:1").Output(); err != nil || len(out) == 0 {
		t.Fatalf("test setup: unrestricted ffmpeg should read the playlist: %v", err)
	}

	noExt := filepath.Join(dir, "evil")
	writePlaylist(t, noExt, secret)
	for _, path := range []string{pl, noExt} {
		for _, ext := range []string{"m3u8", "wav", "m4a", ""} {
			if pcm, err := convertToPCM(context.Background(), ffmpeg, path, ext, 600); err == nil {
				t.Errorf("%s with ext hint %q was decoded (%v s)", filepath.Base(path), ext, pcmDuration(pcm))
			}
		}
	}
}
