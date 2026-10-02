package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestServer wires a server to a fake upstream.
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
	return newServer(cfg, slog.New(slog.NewTextHandler(logOut, nil)))
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
		name = "audio.wav"
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
	audio := testWAV(1)

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
	if vj.Task != "transcribe" || vj.Language == nil || math.Abs(vj.Duration-1) > 0.01 || vj.Text != "你好世界" || vj.Segments == nil {
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

	// MAX_UPLOAD_MB (default 24): a file just over the cap gets 413.
	rec = postTranscription(t, h, "/v1/audio/transcriptions", nil, make([]byte, 24<<20+1), "")
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "24 MB limit") {
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
		rec := postTranscription(t, h, "/v1/audio/transcriptions", nil, testWAV(1), "")
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
		rec := postTranscription(t, h, "/v1/audio/transcriptions", nil, testWAV(1), auth)
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

func TestConcurrencyLimitRespectsTimeout(t *testing.T) {
	app := newTestApp(t, nil, func(c *Config) { c.Timeout = 100 * time.Millisecond }, io.Discard)
	for range maxConcurrent {
		app.sem <- struct{}{} // all workers busy
	}
	rec := postTranscription(t, app.routes(), "/v1/audio/transcriptions", nil, testWAV(1), "")
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status %d, want 504", rec.Code)
	}
}

func TestRequestLogLine(t *testing.T) {
	_, upstream := newFakeVolcWS(t, modeOK, "secret words")
	var logBuf bytes.Buffer
	h := newTestApp(t, upstream, nil, &logBuf).routes()
	fields := map[string]string{"model": "whisper-1", "language": "zh", "response_format": "json"}
	if rec := postNamed(t, h, "/v1/audio/transcriptions", fields, "Rec.M4A", testWAV(1), ""); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	line := logBuf.String()
	for _, want := range []string{"file_ext=m4a", "file_type=application/octet-stream", "in_bytes=32044",
		"model=whisper-1", "response_format=json", "has_language=true", "has_prompt=false", "audio_sec_est=1", "text_len=12"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "secret words") || strings.Contains(line, "zh") {
		t.Errorf("log line leaks content: %s", line)
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
// mid-recognition, the WebSocket to Volcengine is closed.
func TestClientDisconnectClosesUpstream(t *testing.T) {
	fake, upstream := newFakeVolcWS(t, modeStall, "")
	app := newTestApp(t, upstream, nil, io.Discard)
	body, ct := multipartBody(t, nil, "a.wav", testWAV(1))
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
}

// TestUploadForwardedAsIs: the uploaded file reaches Volcengine byte for
// byte, declared as audio.format "wav".
func TestUploadForwardedAsIs(t *testing.T) {
	fake, upstream := newFakeVolcWS(t, modeOK, "x")
	h := newTestServer(t, upstream, nil)
	data := testWAV(1)
	for i := 44; i < len(data); i++ {
		data[i] = byte(i * 7) // non-trivial sample bytes
	}
	if rec := postNamed(t, h, "/v1/audio/transcriptions", nil, "a.m4a", data, ""); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !bytes.Equal(fake.audio, data) || !fake.sawLast {
		t.Errorf("upstream got %d bytes (last=%v), want the %d uploaded bytes unchanged", len(fake.audio), fake.sawLast, len(data))
	}
	if f := fake.request["audio"].(map[string]any)["format"]; f != "wav" {
		t.Errorf("audio.format = %v, want wav", f)
	}
}

// testWAV returns a 16 kHz mono 16-bit WAV of silence.
func testWAV(seconds float64) []byte {
	n := uint32(seconds * bytesPerSecond)
	le := binary.LittleEndian
	h := []byte("RIFF")
	h = le.AppendUint32(h, 36+n)
	h = append(h, "WAVEfmt "...)
	h = le.AppendUint32(h, 16)
	h = le.AppendUint16(h, 1)
	h = le.AppendUint16(h, 1)
	h = le.AppendUint32(h, sampleRate)
	h = le.AppendUint32(h, bytesPerSecond)
	h = le.AppendUint16(h, 2)
	h = le.AppendUint16(h, 16)
	h = append(h, "data"...)
	h = le.AppendUint32(h, n)
	return append(h, make([]byte, n)...)
}

// TestLongDictation sends a 3-minute WAV through the handler to the fake
// upstream. REQUEST_TIMEOUT is 1s and the fake delays its final result by
// 1.5s, so this only passes if the upstream deadline grows with the size
// estimate of the audio (1s + 180s × 0.5).
func TestLongDictation(t *testing.T) {
	fake, upstream := newFakeVolcWS(t, modeOK, "long")
	fake.finalDelay = 1500 * time.Millisecond
	h := newTestServer(t, upstream, func(c *Config) { c.Timeout = time.Second })
	data := testWAV(180)
	rec := postTranscription(t, h, "/v1/audio/transcriptions", nil, data, "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"text":"long"}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !bytes.Equal(fake.audio, data) || fake.packets != (len(data)+audioChunkBytes-1)/audioChunkBytes || !fake.sawLast {
		t.Errorf("upstream got %d bytes in %d packets (last=%v)", len(fake.audio), fake.packets, fake.sawLast)
	}
}
