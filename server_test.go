package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestServer wires a server to a fake upstream and a fake converter that
// returns one second of silence (so tests don't need ffmpeg).
func newTestServer(t *testing.T, upstream *httptest.Server, mutate func(*Config)) http.Handler {
	t.Helper()
	return newTestApp(t, upstream, mutate, io.Discard).routes()
}

func newTestApp(t *testing.T, upstream *httptest.Server, mutate func(*Config), logOut io.Writer) *server {
	t.Helper()
	cfg := testCfg("")
	if upstream != nil {
		cfg.Endpoint = wsURL(upstream)
	}
	if mutate != nil {
		mutate(cfg)
	}
	s := newServer(cfg, slog.New(slog.NewTextHandler(logOut, nil)))
	s.convert = func(ctx context.Context, path, ext string) ([]byte, error) {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("temp upload missing: %v", err)
		}
		return testPCM(1), nil
	}
	return s
}

// multipartBody builds an OpenAI-style multipart request body.
func multipartBody(t *testing.T, fields map[string]string, fileName string, file []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	if fileName != "" {
		fw, _ := mw.CreateFormFile("file", fileName)
		fw.Write(file)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func postTranscription(t *testing.T, h http.Handler, path string, fields map[string]string, file []byte, auth string) *httptest.ResponseRecorder {
	t.Helper()
	name := ""
	if file != nil {
		name = "audio.m4a"
	}
	return postNamed(t, h, path, fields, name, file, auth)
}

func postNamed(t *testing.T, h http.Handler, path string, fields map[string]string, name string, file []byte, auth string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, fields, name, file)
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", ct)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTranscriptionFormats(t *testing.T) {
	_, upstream := newFakeVolcWS(t, modeOK, " 你好世界 ")
	h := newTestServer(t, upstream, nil)
	audio := []byte("fake audio bytes")

	for _, path := range []string{"/v1/audio/transcriptions", "/audio/transcriptions"} {
		rec := postTranscription(t, h, path, map[string]string{"model": "whisper-1", "language": "zh"}, audio, "Bearer anything")
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: status %d: %s", path, rec.Code, rec.Body)
		}
		var got map[string]any
		json.Unmarshal(rec.Body.Bytes(), &got)
		if got["text"] != "你好世界" || len(got) != 1 {
			t.Errorf("%s: json body = %s", path, rec.Body)
		}
	}

	rec := postTranscription(t, h, "/v1/audio/transcriptions", map[string]string{"response_format": "text"}, audio, "")
	if rec.Code != 200 || rec.Body.String() != "你好世界" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("text: %d %q %s", rec.Code, rec.Body, rec.Header().Get("Content-Type"))
	}

	rec = postTranscription(t, h, "/v1/audio/transcriptions", map[string]string{"response_format": "verbose_json"}, audio, "")
	var vj struct {
		Task     string  `json:"task"`
		Language *string `json:"language"`
		Duration float64 `json:"duration"`
		Text     string  `json:"text"`
		Segments []any   `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &vj); err != nil || rec.Code != 200 {
		t.Fatalf("verbose_json: %d %s", rec.Code, rec.Body)
	}
	if vj.Task != "transcribe" || vj.Language == nil || vj.Duration != 1 || vj.Text != "你好世界" || vj.Segments == nil {
		t.Errorf("verbose_json body = %s", rec.Body)
	}

	for _, f := range []string{"srt", "vtt", "bogus"} {
		rec = postTranscription(t, h, "/v1/audio/transcriptions", map[string]string{"response_format": f}, audio, "")
		if rec.Code != 400 {
			t.Errorf("%s: status %d, want 400", f, rec.Code)
		}
		assertOpenAIError(t, rec)
	}
}

func assertOpenAIError(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error.Message == "" || e.Error.Type == "" {
		t.Errorf("not an OpenAI error body: %s", rec.Body)
	}
}

func TestTranscriptionBadInput(t *testing.T) {
	h := newTestServer(t, nil, nil)

	rec := postTranscription(t, h, "/v1/audio/transcriptions", map[string]string{"model": "x"}, nil, "")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "file") {
		t.Errorf("missing file: %d %s", rec.Code, rec.Body)
	}
	assertOpenAIError(t, rec)

	rec = postTranscription(t, h, "/v1/audio/transcriptions", nil, []byte{}, "")
	if rec.Code != 400 {
		t.Errorf("empty file: %d %s", rec.Code, rec.Body)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(`{"file":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("non-multipart: %d %s", rec.Code, rec.Body)
	}

	// MAX_UPLOAD_MB: a 1 MB cap rejects 1 MB + 1 byte of file (plus multipart framing).
	small := newTestServer(t, nil, func(c *Config) { c.MaxUpload = 1 << 20 })
	rec = postTranscription(t, small, "/v1/audio/transcriptions", nil, make([]byte, 1<<20+1), "")
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "1 MB limit") {
		t.Errorf("too large: %d %s", rec.Code, rec.Body)
	}
}

func TestTranscriptionUpstreamErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		mode fakeMode
		want int
	}{
		"silence":       {modeSilence, 200},
		"empty audio":   {modeEmptyAudio, 200},
		"error frame":   {modeError, 502},
		"handshake 401": {modeHandshake401, 502},
	} {
		_, upstream := newFakeVolcWS(t, tc.mode, "")
		h := newTestServer(t, upstream, nil)
		rec := postTranscription(t, h, "/v1/audio/transcriptions", nil, []byte("x"), "")
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d: %s", name, rec.Code, tc.want, rec.Body)
		}
		if tc.want == 200 && strings.TrimSpace(rec.Body.String()) != `{"text":""}` {
			t.Errorf("%s: body %s", name, rec.Body)
		}
		if tc.want != 200 {
			assertOpenAIError(t, rec)
		}
	}
}

func TestProxyAPIKey(t *testing.T) {
	_, upstream := newFakeVolcWS(t, modeOK, "ok")
	h := newTestServer(t, upstream, func(c *Config) { c.ProxyAPIKey = "secret" })

	for auth, want := range map[string]int{
		"":              401,
		"Bearer wrong":  401,
		"secret":        401,
		"Bearer secret": 200,
		"bearer secret": 200,
	} {
		rec := postTranscription(t, h, "/v1/audio/transcriptions", nil, []byte("x"), auth)
		if rec.Code != want {
			t.Errorf("auth %q: status %d, want %d", auth, rec.Code, want)
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("models auth %q: status %d, want %d", auth, rec.Code, want)
		}
	}

	// /health is never authenticated.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Errorf("health: %d %s", rec.Code, rec.Body)
	}
}

func TestModelsAndHealth(t *testing.T) {
	h := newTestServer(t, nil, nil)
	for _, path := range []string{"/v1/models", "/models"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		want := `{"data":[{"id":"volc-bigasr","object":"model","owned_by":"volcengine"}],"object":"list"}`
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Errorf("health: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != 404 {
		t.Errorf("unknown route: %d", rec.Code)
	}
	assertOpenAIError(t, rec)
}

// TestEndToEndWithFFmpeg runs a real m4a through ffmpeg and the handler.
func TestEndToEndWithFFmpeg(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	src := filepath.Join(t.TempDir(), "in.m4a")
	genAudio(t, ffmpeg, src, 1.5)
	data, _ := os.ReadFile(src)

	fake, upstream := newFakeVolcWS(t, modeOK, "hi")
	cfg := testCfg(wsURL(upstream))
	cfg.FFmpegPath = ffmpeg
	h := newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).routes()
	rec := postTranscription(t, h, "/v1/audio/transcriptions", map[string]string{"response_format": "verbose_json"}, data, "")
	var vj struct {
		Duration float64 `json:"duration"`
		Text     string  `json:"text"`
	}
	json.Unmarshal(rec.Body.Bytes(), &vj)
	if rec.Code != 200 || vj.Text != "hi" || vj.Duration < 1.4 || vj.Duration > 1.6 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	fake.mu.Lock()
	if n := len(fake.audio); n < 1.4*bytesPerSecond || n > 1.6*bytesPerSecond || bytes.HasPrefix(fake.audio, []byte("RIFF")) {
		t.Errorf("upstream got %d bytes of audio, want ~1.5 s of raw PCM", n)
	}
	fake.mu.Unlock()

	// Garbage input should be rejected as a client error.
	rec = postTranscription(t, h, "/v1/audio/transcriptions", nil, []byte("definitely not audio"), "")
	if rec.Code != 400 {
		t.Errorf("garbage audio: %d %s", rec.Code, rec.Body)
	}
}

// TestPlaylistUploadRejected uploads an HLS playlist pointing at a local wav,
// under several names; it must be rejected, never transcribed.
func TestPlaylistUploadRejected(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.aac")
	genAudio(t, ffmpeg, secret, 1)
	pl := filepath.Join(dir, "evil.m3u8")
	writePlaylist(t, pl, secret)
	data, _ := os.ReadFile(pl)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a playlist upload")
	}))
	defer upstream.Close()
	cfg := testCfg(upstream.URL)
	cfg.FFmpegPath = ffmpeg
	h := newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).routes()
	for _, name := range []string{"evil.m3u8", "evil.wav", "evil", "evil.m4a"} {
		rec := postNamed(t, h, "/v1/audio/transcriptions", nil, name, data, "")
		if rec.Code != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, rec.Code, rec.Body)
		}
	}
}

// TestTempFileRemoved checks the upload is deleted after success and after
// the client goes away mid-request.
func TestTempFileRemoved(t *testing.T) {
	_, upstream := newFakeVolcWS(t, modeOK, "x")
	app := newTestApp(t, upstream, nil, io.Discard)
	var seen string
	app.convert = func(ctx context.Context, path, ext string) ([]byte, error) {
		seen = path
		return testPCM(1), nil
	}
	h := app.routes()
	if rec := postTranscription(t, h, "/v1/audio/transcriptions", nil, []byte("x"), ""); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Errorf("temp file %s still exists after success", seen)
	}

	app.convert = func(ctx context.Context, path, ext string) ([]byte, error) {
		seen = path
		<-ctx.Done() // like ffmpeg being killed by exec.CommandContext
		return nil, ctx.Err()
	}
	body, ct := multipartBody(t, nil, "a.wav", []byte("x"))
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body).WithContext(ctx)
	req.Header.Set("Content-Type", ct)
	time.AfterFunc(50*time.Millisecond, cancel)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("cancelled: status %d", rec.Code)
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Errorf("temp file %s still exists after cancel", seen)
	}
}

func TestConcurrencyLimitRespectsTimeout(t *testing.T) {
	app := newTestApp(t, nil, func(c *Config) { c.Timeout = 100 * time.Millisecond }, io.Discard)
	for range maxConcurrent {
		app.sem <- struct{}{} // all workers busy
	}
	rec := postTranscription(t, app.routes(), "/v1/audio/transcriptions", nil, []byte("x"), "")
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status %d, want 504", rec.Code)
	}
}

func TestRequestLogLine(t *testing.T) {
	_, upstream := newFakeVolcWS(t, modeOK, "secret words")
	var logBuf bytes.Buffer
	h := newTestApp(t, upstream, nil, &logBuf).routes()
	fields := map[string]string{"model": "whisper-1", "language": "zh", "response_format": "json"}
	if rec := postNamed(t, h, "/v1/audio/transcriptions", fields, "Rec.M4A", []byte("abc"), ""); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	line := logBuf.String()
	for _, want := range []string{"file_ext=m4a", "file_type=application/octet-stream", "in_bytes=3",
		"model=whisper-1", "response_format=json", "has_language=true", "has_prompt=false", "audio_sec=1", "text_len=12"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "secret words") || strings.Contains(line, "zh") {
		t.Errorf("log line leaks content: %s", line)
	}
}

func requireFFmpeg(t *testing.T) string {
	t.Helper()
	p, err := resolveFFmpeg(os.Getenv("FFMPEG_PATH"))
	if err != nil {
		t.Skip("ffmpeg not found")
	}
	return p
}

// genAudio writes a 44.1 kHz stereo sine wave of the given length to dst
// (container chosen by extension), with the moov atom at the end for m4a.
func genAudio(t *testing.T, ffmpeg, dst string, seconds float64) {
	t.Helper()
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100",
		"-ac", "2", "-t", strconv.FormatFloat(seconds, 'f', -1, 64), dst).CombinedOutput()
	if err != nil {
		t.Fatalf("generating %s: %v: %s", dst, err, out)
	}
}

// TestStalledUploadTimesOut checks a client that stops sending mid-upload
// gets 408 once the server's ReadTimeout fires.
func TestStalledUploadTimesOut(t *testing.T) {
	ts := httptest.NewUnstartedServer(newTestServer(t, nil, nil))
	ts.Config.ReadTimeout = 200 * time.Millisecond
	ts.Start()
	defer ts.Close()

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Promise a large body, send the start of the file part, then stall.
	fmt.Fprint(conn, "POST /v1/audio/transcriptions HTTP/1.1\r\nHost: x\r\n"+
		"Content-Type: multipart/form-data; boundary=B\r\nContent-Length: 100000\r\n\r\n"+
		"--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n\r\npartial audio")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestTimeout {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status %d, want 408: %s", resp.StatusCode, body)
	}
}

// TestClientDisconnectClosesUpstream checks that when the client goes away
// mid-recognition, the WebSocket to Volcengine is closed and the upload removed.
func TestClientDisconnectClosesUpstream(t *testing.T) {
	fake, upstream := newFakeVolcWS(t, modeStall, "")
	app := newTestApp(t, upstream, nil, io.Discard)
	var seen string
	app.convert = func(ctx context.Context, path, ext string) ([]byte, error) {
		seen = path
		return testPCM(1), nil
	}
	body, ct := multipartBody(t, nil, "a.wav", []byte("x"))
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body).WithContext(ctx)
	req.Header.Set("Content-Type", ct)
	time.AfterFunc(200*time.Millisecond, cancel)
	done := make(chan struct{})
	go func() {
		app.routes().ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}
	select {
	case <-fake.closed:
	case <-time.After(2 * time.Second):
		t.Error("upstream WebSocket still open after client disconnect")
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Errorf("temp file %s still exists", seen)
	}
}

func TestTruncationHeader(t *testing.T) {
	_, upstream := newFakeVolcWS(t, modeOK, "x")
	for seconds, want := range map[float64]string{1: "true", 0.5: ""} {
		app := newTestApp(t, upstream, func(c *Config) { c.MaxAudioSec = 1 }, io.Discard)
		app.convert = func(ctx context.Context, path, ext string) ([]byte, error) { return testPCM(seconds), nil }
		rec := postTranscription(t, app.routes(), "/v1/audio/transcriptions", nil, []byte("x"), "")
		if rec.Code != 200 || rec.Header().Get("X-Audio-Truncated") != want {
			t.Errorf("%v s: status %d, X-Audio-Truncated=%q, want %q", seconds, rec.Code, rec.Header().Get("X-Audio-Truncated"), want)
		}
	}
}

// TestLongDictation sends ~3 minutes of audio through ffmpeg and the handler
// to the fake upstream. REQUEST_TIMEOUT is 1s and the fake delays its final
// result by 1.5s, so this only passes if the upstream deadline grows with
// the audio length (1s + 180s × 0.5).
func TestLongDictation(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	src := filepath.Join(t.TempDir(), "long.m4a")
	genAudio(t, ffmpeg, src, 180)
	data, _ := os.ReadFile(src)

	fake, upstream := newFakeVolcWS(t, modeOK, "long")
	fake.finalDelay = 1500 * time.Millisecond
	cfg := testCfg(wsURL(upstream))
	cfg.FFmpegPath = ffmpeg
	cfg.Timeout = time.Second
	h := newServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).routes()
	rec := postTranscription(t, h, "/v1/audio/transcriptions", nil, data, "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"text":"long"}` || rec.Header().Get("X-Audio-Truncated") != "" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	n := len(fake.audio)
	if n < 179*bytesPerSecond || n > 181*bytesPerSecond || fake.packets != (n+audioChunkBytes-1)/audioChunkBytes || !fake.sawLast {
		t.Errorf("upstream got %d bytes in %d packets (last=%v)", n, fake.packets, fake.sawLast)
	}
}
