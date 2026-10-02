package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// --- hand-built frames, independent of the code under test ---

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// serverResponse builds a full server response: 0x11, type 0b1001 + flags,
// JSON + gzip, then sequence, payload size and gzip payload.
func serverResponse(t *testing.T, flags byte, seq int32, payload string) []byte {
	z := gz(t, []byte(payload))
	return cat([]byte{0x11, 0x90 | flags, 0x11, 0x00}, be32(uint32(seq)), be32(uint32(len(z))), z)
}

// serverError builds an error frame as in the doc example: type 0b1111,
// flags 0, JSON, no compression, then code, message size and message.
func serverError(code uint32, msg string) []byte {
	return cat([]byte{0x11, 0xF0, 0x10, 0x00}, be32(code), be32(uint32(len(msg))), []byte(msg))
}

// clientFrame is a client message decoded by the fake server.
type clientFrame struct {
	header  [4]byte
	payload []byte
}

func parseClientFrame(t *testing.T, data []byte) clientFrame {
	t.Helper()
	if len(data) < 8 {
		t.Fatalf("client frame too short: %d bytes", len(data))
	}
	var f clientFrame
	copy(f.header[:], data)
	size := binary.BigEndian.Uint32(data[4:8])
	if int(size) != len(data)-8 {
		t.Fatalf("payload size %d, frame has %d bytes after header", size, len(data)-8)
	}
	f.payload = gunzip(t, data[8:])
	return f
}

// --- frame codec unit tests ---

func TestEncodeFrame(t *testing.T) {
	cases := []struct {
		msgType, flags, serial byte
		wantHeader             [4]byte
	}{
		{msgFullClientRequest, 0, serialJSON, [4]byte{0x11, 0x10, 0x11, 0x00}},
		{msgAudioOnlyRequest, 0, serialNone, [4]byte{0x11, 0x20, 0x01, 0x00}},
		{msgAudioOnlyRequest, flagLastPacket, serialNone, [4]byte{0x11, 0x22, 0x01, 0x00}},
	}
	var enc frameEncoder // reused across frames, as in sendSession
	for i, tc := range cases {
		payload := bytes.Repeat([]byte(`{"a":1}`), i+1)
		f := parseClientFrame(t, enc.encode(tc.msgType, tc.flags, tc.serial, payload))
		if f.header != tc.wantHeader || !bytes.Equal(f.payload, payload) {
			t.Errorf("header % x payload %q, want % x", f.header, f.payload, tc.wantHeader)
		}
	}
}

func TestDecodeFrame(t *testing.T) {
	f, err := decodeFrame(serverResponse(t, flagSequence, 1, `{"result":{"text":"这是"}}`))
	if err != nil || f.msgType != msgFullServerResponse || f.seq != 1 || f.flags&flagLastPacket != 0 ||
		string(f.payload) != `{"result":{"text":"这是"}}` {
		t.Errorf("response: %+v %v", f, err)
	}

	// Final response: flags 0b0011, negative sequence.
	f, err = decodeFrame(serverResponse(t, flagSequence|flagLastPacket, -3, `{}`))
	if err != nil || f.seq != -3 || f.flags&flagLastPacket == 0 {
		t.Errorf("final: %+v %v", f, err)
	}

	// Uncompressed response without a sequence number.
	f, err = decodeFrame(cat([]byte{0x11, 0x90, 0x10, 0x00}, be32(2), []byte("{}")))
	if err != nil || string(f.payload) != "{}" {
		t.Errorf("uncompressed: %+v %v", f, err)
	}

	f, err = decodeFrame(serverError(45000001, `{"error":"bad params"}`))
	if err != nil || f.msgType != msgServerError || f.code != 45000001 || string(f.payload) != `{"error":"bad params"}` {
		t.Errorf("error frame: %+v %v", f, err)
	}

	for name, bad := range map[string][]byte{
		"short":            {0x11, 0x90},
		"bad version":      {0x21, 0x90, 0x10, 0x00, 0, 0, 0, 0},
		"missing size":     {0x11, 0x91, 0x10, 0x00, 0, 0, 0, 1},
		"truncated":        cat([]byte{0x11, 0x90, 0x10, 0x00}, be32(10), []byte("{}")),
		"bad gzip":         cat([]byte{0x11, 0x90, 0x11, 0x00}, be32(2), []byte("{}")),
		"unknown msg type": {0x11, 0x20, 0x10, 0x00, 0, 0, 0, 0},
	} {
		if _, err := decodeFrame(bad); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestResponseText(t *testing.T) {
	cases := []struct {
		payload   string
		want      string
		hasResult bool
	}{
		{`{"audio_info":{"duration":3696},"result":{"text":"这是字节跳动， 今日头条母公司。"}}`, "这是字节跳动， 今日头条母公司。", true},
		{`{"result":[{"text":"a"},{"text":"b"}]}`, "ab", true},
		{`{"result":{"text":""}}`, "", true},
		{`{"result":{}}`, "", true},
		{`{"audio_info":{"duration":0}}`, "", false},
		{`{"result":null}`, "", false},
		{``, "", false},
	}
	for _, tc := range cases {
		got, has, err := responseText([]byte(tc.payload))
		if err != nil || got != tc.want || has != tc.hasResult {
			t.Errorf("%s: got %q, %v, %v", tc.payload, got, has, err)
		}
	}
}

func TestDecodeFrameRejectsGzipBomb(t *testing.T) {
	big := gz(t, make([]byte, maxServerFrame+1))
	frame := cat([]byte{0x11, 0x90, 0x11, 0x00}, be32(uint32(len(big))), big)
	if _, err := decodeFrame(frame); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("got %v, want too-large error", err)
	}
	ok := gz(t, make([]byte, maxServerFrame))
	if _, err := decodeFrame(cat([]byte{0x11, 0x90, 0x11, 0x00}, be32(uint32(len(ok))), ok)); err != nil {
		t.Errorf("payload at the limit rejected: %v", err)
	}
}

func TestHandshakeErrorSanitizesStatus(t *testing.T) {
	for code, wantIn := range map[string]bool{"45000030": true, "abc": false, "1234567890123": false} {
		resp := &http.Response{StatusCode: 403, Header: http.Header{"X-Api-Status-Code": {code}},
			Body: io.NopCloser(strings.NewReader(strings.Repeat("y", 5000)))}
		msg := handshakeError(resp, "res").Error()
		if strings.Contains(msg, "(status "+code+")") != wantIn {
			t.Errorf("code %q: message %q", code, msg)
		}
		if len(msg) > 600 {
			t.Errorf("message not capped: %d bytes", len(msg))
		}
	}
}

// --- fake Volcengine WebSocket server ---

type fakeMode int

const (
	modeOK               fakeMode = iota
	modeSilence                   // final response with empty text
	modeError                     // error frame after the last audio packet
	modeErrorOnRequest            // error frame right after the full client request
	modeEmptyAudio                // 45000002 error frame
	modeStall                     // never sends a final result
	modeHandshake401              // rejects the upgrade
	modeInterimThenEmpty          // interim "draft" results, then an empty final
	modeErrorMidUpload            // error frame after the first audio packet, then stops reading
	modeEarlyFinal                // final result after the first audio packet, then closes
	modeAbruptClose               // drops the TCP connection after the first audio packet, no frame
)

type fakeVolcWS struct {
	t          *testing.T
	mode       fakeMode
	text       string
	finalDelay time.Duration // modeOK: wait this long before the final result

	mu        sync.Mutex
	headers   http.Header
	request   map[string]any
	audio     []byte
	packets   int
	sawLast   bool
	afterLast bool          // a packet arrived after the last-packet flag
	closed    chan struct{} // receives once per finished WebSocket connection
}

func newFakeVolcWS(t *testing.T, mode fakeMode, text string) (*fakeVolcWS, *httptest.Server) {
	f := &fakeVolcWS{t: t, mode: mode, text: text, closed: make(chan struct{}, 64)}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

func (f *fakeVolcWS) serve(w http.ResponseWriter, r *http.Request) {
	// Each connection is one recognition session; keep the latest one's state.
	f.mu.Lock()
	f.headers = r.Header.Clone()
	f.request, f.audio, f.packets, f.sawLast, f.afterLast = nil, nil, 0, false, false
	f.mu.Unlock()
	if f.mode == modeHandshake401 {
		w.Header().Set("X-Tt-Logid", "logid-401")
		http.Error(w, `{"error":"requested resource not granted"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("X-Tt-Logid", "fake-logid")
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		f.t.Errorf("accept: %v", err)
		return
	}
	defer func() { f.closed <- struct{}{} }()
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	ctx := r.Context()
	send := func(b []byte) { c.Write(ctx, websocket.MessageBinary, b) }

	for seq := int32(1); ; seq++ {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		fr := parseClientFrame(f.t, data)
		f.mu.Lock()
		if f.sawLast {
			f.afterLast = true
		}
		if f.request == nil {
			if fr.header != [4]byte{0x11, 0x10, 0x11, 0x00} {
				f.t.Errorf("first frame header % x, want full client request", fr.header)
			}
			if err := json.Unmarshal(fr.payload, &f.request); err != nil {
				f.t.Errorf("full client request is not JSON: %v", err)
			}
			f.mu.Unlock()
			if f.mode == modeErrorOnRequest {
				send(serverError(45000001, `{"error":"invalid request params"}`))
				c.Close(websocket.StatusNormalClosure, "")
				return
			}
			send(serverResponse(f.t, flagSequence, seq, `{"result":{"text":""}}`))
			continue
		}
		if fr.header[0] != 0x11 || fr.header[1]&0xF0 != 0x20 || fr.header[2] != 0x01 {
			f.t.Errorf("audio frame header % x", fr.header)
		}
		f.audio = append(f.audio, fr.payload...)
		f.packets++
		last := fr.header[1]&0x0F == flagLastPacket
		f.sawLast = f.sawLast || last
		f.mu.Unlock()
		switch {
		case f.mode == modeAbruptClose:
			c.CloseNow() // no close frame, unread data => RST
			return
		case f.mode == modeErrorMidUpload:
			send(serverError(55000031, `{"error":"server busy"}`))
			time.Sleep(3 * time.Second) // stop reading; the client must not wait for its deadline
			return
		case f.mode == modeEarlyFinal:
			send(serverResponse(f.t, flagSequence|flagLastPacket, -seq, `{"result":{"text":"early"}}`))
			time.Sleep(100 * time.Millisecond)
			c.Close(websocket.StatusNormalClosure, "")
			return
		case !last && f.mode == modeInterimThenEmpty:
			send(serverResponse(f.t, flagSequence, seq, `{"result":{"text":"draft"}}`))
			continue
		case !last:
			send(serverResponse(f.t, flagSequence, seq, `{"result":{"text":""}}`))
			continue
		}
		switch f.mode {
		case modeOK:
			time.Sleep(f.finalDelay)
			send(serverResponse(f.t, flagSequence|flagLastPacket, -seq,
				`{"audio_info":{"duration":1000},"result":{"text":"`+f.text+`"}}`))
		case modeSilence, modeInterimThenEmpty:
			send(serverResponse(f.t, flagSequence|flagLastPacket, -seq, `{"audio_info":{"duration":1000},"result":{"text":""}}`))
		case modeEmptyAudio:
			send(serverError(volcCodeEmptyAudio, `{"error":"empty audio"}`))
		case modeError:
			send(serverError(55000031, `{"error":"server busy `+strings.Repeat("x", 1000)+`"}`))
		case modeStall:
			// Never answer; the next Read fails once the client hangs up.
		}
	}
}

func testCfg(endpoint string) *Config {
	return &Config{APIKey: "new-key", Endpoint: endpoint, ResourceID: defaultResourceID,
		ModelName: "bigmodel", EnableDDC: true, Timeout: 5 * time.Second, MaxAudioSec: 600,
		MaxUpload: 128 << 20, PerAudioSec: 0.5}
}

func testPCM(seconds float64) []byte {
	return make([]byte, int(seconds*bytesPerSecond))
}

// --- client tests ---

func TestRecognizeSession(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		fake, srv := newFakeVolcWS(t, modeOK, " 你好世界。 ")
		cfg := testCfg(wsURL(srv))
		cfg.Language = "zh-CN"
		if legacy {
			cfg.AppKey, cfg.AccessKey = "app", "access"
		}
		pcm := testPCM(1.03) // 32960 bytes: five full 200 ms packets + a partial one
		res, err := newVolcClient(cfg).recognize(context.Background(), pcm)
		if err != nil {
			t.Fatal(err)
		}
		if res.Text != "你好世界。" || res.LogID != "fake-logid" {
			t.Errorf("got %+v", res)
		}

		fake.mu.Lock()
		h := fake.headers
		if h.Get("X-Api-Resource-Id") != "volc.seedasr.sauc.duration" || !uuidV4.MatchString(h.Get("X-Api-Request-Id")) ||
			!uuidV4.MatchString(h.Get("X-Api-Connect-Id")) {
			t.Errorf("bad handshake headers: %v", h)
		}
		if legacy {
			if h.Get("X-Api-App-Key") != "app" || h.Get("X-Api-Access-Key") != "access" || h.Get("X-Api-Key") != "" {
				t.Errorf("bad legacy auth headers: %v", h)
			}
		} else if h.Get("X-Api-Key") != "new-key" || h.Get("X-Api-App-Key") != "" {
			t.Errorf("bad api-key auth headers: %v", h)
		}
		req := fake.request
		audio, _ := req["audio"].(map[string]any)
		want := map[string]any{"format": "pcm", "codec": "raw", "rate": 16000.0, "bits": 16.0, "channel": 1.0, "language": "zh-CN"}
		for k, v := range want {
			if audio[k] != v {
				t.Errorf("audio.%s = %v, want %v", k, audio[k], v)
			}
		}
		r, _ := req["request"].(map[string]any)
		if r["model_name"] != "bigmodel" || r["enable_itn"] != true || r["enable_punc"] != true ||
			r["enable_ddc"] != true || len(r) != 4 {
			t.Errorf("request = %v", r)
		}
		if req["user"].(map[string]any)["uid"] != "volc-stt-proxy" {
			t.Errorf("user = %v", req["user"])
		}
		if !bytes.Equal(fake.audio, pcm) || fake.packets != 6 || !fake.sawLast || fake.afterLast {
			t.Errorf("audio: %d bytes in %d packets, last=%v afterLast=%v", len(fake.audio), fake.packets, fake.sawLast, fake.afterLast)
		}
		fake.mu.Unlock()
	}
}

func TestRecognizeRequestOptions(t *testing.T) {
	fake, srv := newFakeVolcWS(t, modeOK, "x")
	cfg := testCfg(wsURL(srv))
	cfg.EnableDDC = false
	if _, err := newVolcClient(cfg).recognize(context.Background(), testPCM(0.1)); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if r := fake.request["request"].(map[string]any); r["enable_ddc"] != false {
		t.Errorf("enable_ddc = %v, want false", r["enable_ddc"])
	}
	if _, ok := fake.request["audio"].(map[string]any)["language"]; ok {
		t.Error("audio.language should be omitted when VOLC_LANGUAGE is unset")
	}
	if fake.packets != 1 || !fake.sawLast {
		t.Errorf("short audio: %d packets, last=%v", fake.packets, fake.sawLast)
	}
}

func TestRecognizeOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		mode      fakeMode
		wantErr   int // upstreamError status, 0 = success with empty text
		wantInMsg string
		wantCode  string
	}{
		{"silence", modeSilence, 0, "", ""},
		{"empty audio code", modeEmptyAudio, 0, "", "45000002"},
		{"error frame", modeError, 502, "volcengine error 55000031: {\"error\":\"server busy", "55000031"},
		{"error on request", modeErrorOnRequest, 502, "45000001", "45000001"},
		{"handshake 401", modeHandshake401, 502, "HTTP 401: {\"error\":\"requested resource not granted\"}; check the key type", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newFakeVolcWS(t, tc.mode, "")
			res, err := newVolcClient(testCfg(wsURL(srv))).recognize(context.Background(), testPCM(0.5))
			if res.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", res.Code, tc.wantCode)
			}
			if tc.wantErr == 0 {
				if err != nil || res.Text != "" {
					t.Fatalf("got %+v, %v", res, err)
				}
				return
			}
			var ue *upstreamError
			if !errors.As(err, &ue) || ue.status != tc.wantErr || !strings.Contains(ue.msg, tc.wantInMsg) {
				t.Fatalf("got %v, want %d containing %q", err, tc.wantErr, tc.wantInMsg)
			}
			if len(ue.msg) > maxErrorMsgLen+100 {
				t.Errorf("error message not capped: %d bytes", len(ue.msg))
			}
			if tc.mode == modeHandshake401 && res.LogID != "logid-401" {
				t.Errorf("handshake logid = %q", res.LogID)
			}
		})
	}
}

func TestRecognizeTimeout(t *testing.T) {
	fake, srv := newFakeVolcWS(t, modeStall, "")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := newVolcClient(testCfg(wsURL(srv))).recognize(ctx, testPCM(0.5))
	var ue *upstreamError
	if !errors.As(err, &ue) || ue.status != http.StatusGatewayTimeout {
		t.Fatalf("got %v, want 504", err)
	}
	select {
	case <-fake.closed:
	case <-time.After(2 * time.Second):
		t.Error("WebSocket was not closed after timeout")
	}
}

func TestRecognizeDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect target must not be contacted")
	}))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	_, err := newVolcClient(testCfg(wsURL(upstream))).recognize(context.Background(), testPCM(0.1))
	var ue *upstreamError
	if !errors.As(err, &ue) || ue.status != http.StatusBadGateway || !strings.Contains(ue.msg, "307") {
		t.Fatalf("got %v, want 502 mentioning HTTP 307", err)
	}
}

func randomPCM(t *testing.T, n int) []byte {
	b := make([]byte, n)
	rand.Read(b) // incompressible, so TCP buffers really fill up
	return b
}

func TestRecognizeInterimThenEmptyFinal(t *testing.T) {
	_, srv := newFakeVolcWS(t, modeInterimThenEmpty, "")
	res, err := newVolcClient(testCfg(wsURL(srv))).recognize(context.Background(), testPCM(1))
	if err != nil || res.Text != "" {
		t.Fatalf("got %+v, %v; want empty text (final snapshot replaces interim draft)", res, err)
	}
}

func TestRecognizeErrorMidUploadDoesNotWaitForDeadline(t *testing.T) {
	_, srv := newFakeVolcWS(t, modeErrorMidUpload, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	res, err := newVolcClient(testCfg(wsURL(srv))).recognize(ctx, randomPCM(t, 8<<20))
	var ue *upstreamError
	if !errors.As(err, &ue) || ue.status != 502 || res.Code != "55000031" {
		t.Fatalf("got %+v, %v; want 502 with code 55000031", res, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v; outstanding writes were not cancelled", d)
	}
}

func TestRecognizeFinalWinsOverSendError(t *testing.T) {
	_, srv := newFakeVolcWS(t, modeEarlyFinal, "")
	res, err := newVolcClient(testCfg(wsURL(srv))).recognize(context.Background(), randomPCM(t, 8<<20))
	if err != nil || res.Text != "early" {
		t.Fatalf("got %+v, %v; want the early final result", res, err)
	}
}

// TestRecognizeAbruptClose: the server drops the connection mid-upload without
// any frame. Both the send and the read side fail; the client must return a
// 502 promptly (not wait for its deadline) with a connection diagnostic.
func TestRecognizeAbruptClose(t *testing.T) {
	_, srv := newFakeVolcWS(t, modeAbruptClose, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := newVolcClient(testCfg(wsURL(srv))).recognize(ctx, randomPCM(t, 8<<20))
	var ue *upstreamError
	if !errors.As(err, &ue) || ue.status != 502 || !strings.Contains(ue.msg, "volcengine") {
		t.Fatalf("got %v, want 502", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %v", d)
	}
	t.Logf("error: %s", ue.msg)
}
