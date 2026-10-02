package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Volcengine "一句话识别" (bigmodel_nostream) over WebSocket. The binary frame
// layout follows the streaming ASR doc ("WebSocket 二进制协议"): a 4-byte
// header, then (depending on the message type) a sequence number or error
// code, a big-endian uint32 payload size, and the payload.

const (
	protoVersion = 0b0001
	headerWords  = 0b0001 // header size = 1 * 4 bytes

	msgFullClientRequest  = 0b0001
	msgAudioOnlyRequest   = 0b0010
	msgFullServerResponse = 0b1001
	msgServerError        = 0b1111

	flagSequence   = 0b0001 // header is followed by a sequence number
	flagLastPacket = 0b0010 // last packet (负包)

	serialNone = 0b0000
	serialJSON = 0b0001

	compressNone = 0b0000
	compressGzip = 0b0001

	sampleRate      = 16000
	bytesPerSecond  = sampleRate * 2     // 16 kHz mono 16-bit
	audioChunkBytes = bytesPerSecond / 5 // 200 ms of audio per packet
	maxServerFrame  = 4 << 20

	volcCodeEmptyAudio = 45000002 // "空音频": treated as no speech
	maxErrorMsgLen     = 300
)

// volcClient talks to the Volcengine nostream ASR endpoint.
type volcClient struct {
	cfg  *Config
	http *http.Client
}

func newVolcClient(cfg *Config) *volcClient {
	return &volcClient{cfg: cfg, http: &http.Client{
		// No Timeout: the WebSocket session is bounded by the request context.
		// Never resend credentials to wherever a redirect points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// volcResult carries the transcript plus diagnostics for logging.
type volcResult struct {
	Text  string
	Code  string // server error code, if any
	LogID string // X-Tt-Logid from the handshake
}

// upstreamError maps a Volcengine failure to the HTTP status we return.
type upstreamError struct {
	status int // 502 or 504
	msg    string
}

func (e *upstreamError) Error() string { return e.msg }

func badGateway(format string, a ...any) error {
	return &upstreamError{http.StatusBadGateway, fmt.Sprintf(format, a...)}
}

type volcRequest struct {
	User struct {
		UID string `json:"uid"`
	} `json:"user"`
	Audio struct {
		Format   string `json:"format"`
		Codec    string `json:"codec"`
		Rate     int    `json:"rate"`
		Bits     int    `json:"bits"`
		Channel  int    `json:"channel"`
		Language string `json:"language,omitempty"`
	} `json:"audio"`
	Request struct {
		ModelName  string `json:"model_name"`
		EnableITN  bool   `json:"enable_itn"`
		EnablePunc bool   `json:"enable_punc"`
		EnableDDC  bool   `json:"enable_ddc"`
	} `json:"request"`
}

func (c *volcClient) buildRequest() volcRequest {
	var r volcRequest
	r.User.UID = "volc-stt-proxy"
	r.Audio.Format = "wav" // the upload is forwarded as-is; Volcengine parses the WAV
	r.Audio.Codec = "raw"
	r.Audio.Rate = sampleRate
	r.Audio.Bits = 16
	r.Audio.Channel = 1
	r.Audio.Language = c.cfg.Language
	r.Request.ModelName = c.cfg.ModelName
	r.Request.EnableITN = true
	r.Request.EnablePunc = true
	r.Request.EnableDDC = c.cfg.EnableDDC
	return r
}

// recognize runs one WebSocket session: full client request, the WAV file in
// 200 ms audio-only packets (the last one flagged), then waits for the final result.
func (c *volcClient) recognize(ctx context.Context, audio []byte) (volcResult, error) {
	var res volcResult
	id := newUUID()
	hdr := http.Header{}
	hdr.Set("X-Api-Resource-Id", c.cfg.ResourceID)
	hdr.Set("X-Api-Request-Id", id)
	hdr.Set("X-Api-Connect-Id", id)
	if c.cfg.useLegacyAuth() {
		hdr.Set("X-Api-App-Key", c.cfg.AppKey)
		hdr.Set("X-Api-Access-Key", c.cfg.AccessKey)
	} else {
		hdr.Set("X-Api-Key", c.cfg.APIKey)
	}

	conn, resp, err := websocket.Dial(ctx, c.cfg.Endpoint, &websocket.DialOptions{
		HTTPClient: c.http,
		HTTPHeader: hdr,
	})
	if resp != nil {
		res.LogID = resp.Header.Get("X-Tt-Logid")
	}
	if err != nil {
		if ctx.Err() != nil || isTimeout(err) {
			return res, &upstreamError{http.StatusGatewayTimeout, "timed out connecting to volcengine"}
		}
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return res, handshakeError(resp, c.cfg.ResourceID)
		}
		return res, badGateway("connecting to volcengine: %v", err)
	}
	ok := false
	defer func() {
		if ok {
			// Polite close handshake in the background: it may wait up to 5s
			// for the server's close frame, which shouldn't delay the reply.
			go conn.Close(websocket.StatusNormalClosure, "")
		} else {
			conn.CloseNow()
		}
	}()
	conn.SetReadLimit(maxServerFrame)

	payload, err := json.Marshal(c.buildRequest())
	if err != nil {
		return res, err
	}

	// Read concurrently with sending, so server responses to each packet
	// never back up while a long recording is being uploaded. Once the reader
	// reaches a terminal outcome (final result, error frame, close), pending
	// writes are cancelled so we never wait for the request deadline.
	type outcome struct {
		text string
		code string
		err  error
	}
	writeCtx, cancelWrites := context.WithCancel(ctx)
	defer cancelWrites()
	done := make(chan outcome, 1)
	go func() {
		text, code, err := readFinalResult(ctx, conn)
		cancelWrites()
		done <- outcome{text, code, err}
	}()

	werr := sendSession(writeCtx, conn, payload, audio)
	var o outcome
	if werr == nil || writeCtx.Err() != nil {
		o = <-done // sent everything, or the reader finished first and cancelled the writes
	} else {
		// The send failed on its own (e.g. the server hung up). Reader and
		// writer share the connection, so the reader sees the same failure
		// (or a server error/close frame, which is more informative); give
		// it a moment, and fall back to the send error only if it saw nothing.
		select {
		case o = <-done:
		case <-time.After(2 * time.Second):
			conn.CloseNow()
			<-done
			return res, wrapSessionError(ctx, fmt.Errorf("sending audio: %w", werr))
		}
	}
	res.Code = o.code
	if o.err == nil {
		// A final result (or accepted empty audio) wins even if the send side
		// failed afterwards, e.g. because the server closed early.
		res.Text = strings.TrimSpace(o.text)
		ok = werr == nil
		return res, nil
	}
	return res, wrapSessionError(ctx, o.err)
}

// sendSession writes the full client request and the audio packets.
// Each 200 ms chunk is gzipped on its own with one reused encoder, so no
// second full copy of the audio is ever built.
func sendSession(ctx context.Context, conn *websocket.Conn, request, audio []byte) error {
	var enc frameEncoder
	if err := conn.Write(ctx, websocket.MessageBinary,
		enc.encode(msgFullClientRequest, 0, serialJSON, request)); err != nil {
		return err
	}
	for off := 0; ; off += audioChunkBytes {
		end := min(off+audioChunkBytes, len(audio))
		var flags byte
		if end == len(audio) {
			flags = flagLastPacket
		}
		if err := conn.Write(ctx, websocket.MessageBinary,
			enc.encode(msgAudioOnlyRequest, flags, serialNone, audio[off:end])); err != nil {
			return err
		}
		if flags == flagLastPacket {
			return nil
		}
	}
}

// readFinalResult reads server frames until the last-packet response and
// returns its transcript. With the default result_type "full" every response
// carries all text so far, so the final one holds the complete transcript.
func readFinalResult(ctx context.Context, conn *websocket.Conn) (text, code string, err error) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if status := websocket.CloseStatus(err); status != -1 {
				return text, "", badGateway("volcengine closed the connection before the final result (close status %d)", status)
			}
			return text, "", err
		}
		if typ != websocket.MessageBinary {
			continue
		}
		f, err := decodeFrame(data)
		if err != nil {
			return text, "", badGateway("invalid frame from volcengine: %v", err)
		}
		switch f.msgType {
		case msgServerError:
			code := strconv.FormatUint(uint64(f.code), 10)
			if f.code == volcCodeEmptyAudio {
				return "", code, nil
			}
			return "", code, badGateway("volcengine error %s: %s", code, truncate(strings.TrimSpace(string(f.payload)), maxErrorMsgLen))
		case msgFullServerResponse:
			t, hasResult, err := responseText(f.payload)
			if err != nil {
				return text, "", badGateway("invalid response from volcengine: %v", err)
			}
			if hasResult { // each result is a full snapshot, even when empty
				text = t
			}
			if f.flags&flagLastPacket != 0 {
				return text, "", nil
			}
		}
	}
}

// responseText extracts result.text from a server response payload;
// hasResult reports whether the payload carried a result at all. The doc
// lists result as an object in its example (and as a "list" in the field
// table), so both shapes are accepted.
func responseText(payload []byte) (text string, hasResult bool, err error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return "", false, nil
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil {
		return "", false, err
	}
	type item struct {
		Text string `json:"text"`
	}
	raw := bytes.TrimSpace(resp.Result)
	switch {
	case len(raw) == 0 || string(raw) == "null":
		return "", false, nil
	case raw[0] == '[':
		var items []item
		if err := json.Unmarshal(raw, &items); err != nil {
			return "", false, err
		}
		var sb strings.Builder
		for _, it := range items {
			sb.WriteString(it.Text)
		}
		return sb.String(), true, nil
	default:
		var it item
		if err := json.Unmarshal(raw, &it); err != nil {
			return "", false, err
		}
		return it.Text, true, nil
	}
}

func wrapSessionError(ctx context.Context, err error) error {
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue
	}
	if ctx.Err() != nil || isTimeout(err) {
		return &upstreamError{http.StatusGatewayTimeout, "timed out waiting for volcengine"}
	}
	return badGateway("volcengine session failed: %v", err)
}

// handshakeError describes a rejected WebSocket upgrade. The response body
// (coder/websocket keeps up to 1 KB of it) is server-generated and holds no
// credentials.
func handshakeError(resp *http.Response, resourceID string) error {
	msg := fmt.Sprintf("volcengine handshake failed: HTTP %d", resp.StatusCode)
	if code := resp.Header.Get("X-Api-Status-Code"); isShortNumber(code) {
		msg += " (status " + code + ")"
	}
	detail := resp.Header.Get("X-Api-Message")
	if detail == "" && resp.Body != nil {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		detail = strings.TrimSpace(string(b))
	}
	if detail != "" {
		msg += ": " + truncate(detail, 200)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		msg += "; check the key type (new-console API Key vs legacy App Key + Access Key) and that " +
			"豆包流式语音识别模型2.0 is activated for resource " + truncate(resourceID, 64)
	}
	return badGateway("%s", truncate(msg, 600))
}

// isShortNumber accepts 1–12 ASCII digits (a plausible status code).
func isShortNumber(s string) bool {
	if s == "" || len(s) > 12 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// frameEncoder builds client frames: header, payload size, gzip payload.
// Client frames never carry a sequence number (flags 0b0000 / 0b0010).
// The returned slice is reused by the next encode call.
type frameEncoder struct {
	buf bytes.Buffer
	zw  *gzip.Writer
}

func (e *frameEncoder) encode(msgType, flags, serial byte, payload []byte) []byte {
	e.buf.Reset()
	e.buf.Write([]byte{protoVersion<<4 | headerWords, msgType<<4 | flags, serial<<4 | compressGzip, 0, 0, 0, 0, 0})
	if e.zw == nil {
		e.zw = gzip.NewWriter(&e.buf)
	} else {
		e.zw.Reset(&e.buf)
	}
	e.zw.Write(payload)
	e.zw.Close()
	frame := e.buf.Bytes()
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(frame)-8))
	return frame
}

// serverFrame is a decoded server message.
type serverFrame struct {
	msgType byte
	flags   byte
	seq     int32  // only if flags&flagSequence
	code    uint32 // only for msgServerError
	payload []byte // decompressed
}

func decodeFrame(data []byte) (serverFrame, error) {
	var f serverFrame
	if len(data) < 4 {
		return f, errors.New("frame shorter than header")
	}
	if v := data[0] >> 4; v != protoVersion {
		return f, fmt.Errorf("unsupported protocol version %d", v)
	}
	hsize := int(data[0]&0x0f) * 4
	if hsize < 4 || len(data) < hsize {
		return f, errors.New("bad header size")
	}
	f.msgType, f.flags = data[1]>>4, data[1]&0x0f
	compression := data[2] & 0x0f
	rest := data[hsize:]

	next := func(what string) (uint32, error) {
		if len(rest) < 4 {
			return 0, errors.New("missing " + what)
		}
		v := binary.BigEndian.Uint32(rest)
		rest = rest[4:]
		return v, nil
	}
	var err error
	switch f.msgType {
	case msgFullServerResponse:
		if f.flags&flagSequence != 0 {
			var seq uint32
			if seq, err = next("sequence"); err != nil {
				return f, err
			}
			f.seq = int32(seq)
		}
	case msgServerError:
		if f.code, err = next("error code"); err != nil {
			return f, err
		}
	default:
		return f, fmt.Errorf("unexpected message type %#x", f.msgType)
	}
	size, err := next("payload size")
	if err != nil {
		return f, err
	}
	if uint64(size) > uint64(len(rest)) {
		return f, errors.New("truncated payload")
	}
	f.payload = rest[:size]
	if compression == compressGzip && len(f.payload) > 0 {
		zr, err := gzip.NewReader(bytes.NewReader(f.payload))
		if err != nil {
			return f, fmt.Errorf("gzip: %w", err)
		}
		defer zr.Close()
		if f.payload, err = io.ReadAll(io.LimitReader(zr, maxServerFrame+1)); err != nil {
			return f, fmt.Errorf("gzip: %w", err)
		}
		if len(f.payload) > maxServerFrame {
			return f, errors.New("decompressed payload too large")
		}
	}
	return f, nil
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// newUUID returns a random RFC 4122 version 4 UUID.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
