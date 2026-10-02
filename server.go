package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxConcurrent = 4 // simultaneous ffmpeg conversions + upstream calls

type server struct {
	cfg  *Config
	volc *volcClient
	log  *slog.Logger
	sem  chan struct{}  // bounds concurrent conversions + upstream calls
	wg   sync.WaitGroup // in-flight transcriptions, for shutdown
	// convert turns an uploaded audio file into raw 16 kHz mono s16le PCM.
	// ext is the lowercase file extension without the dot ("" if none).
	convert func(ctx context.Context, path, ext string) ([]byte, error)
}

func newServer(cfg *Config, logger *slog.Logger) *server {
	return &server{
		cfg:  cfg,
		volc: newVolcClient(cfg),
		log:  logger,
		sem:  make(chan struct{}, maxConcurrent),
		convert: func(ctx context.Context, path, ext string) ([]byte, error) {
			return convertToPCM(ctx, cfg.FFmpegPath, path, ext, cfg.MaxAudioSec)
		},
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	transcribe := s.auth(http.HandlerFunc(s.handleTranscription))
	models := s.auth(http.HandlerFunc(s.handleModels))
	mux.Handle("POST /v1/audio/transcriptions", transcribe)
	mux.Handle("POST /audio/transcriptions", transcribe)
	mux.Handle("GET /v1/models", models)
	mux.Handle("GET /models", models)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok")
	})
	// Log anything else so it's easy to see what a client actually sends.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("unknown route", "method", r.Method, "path", r.URL.Path)
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found",
			fmt.Sprintf("unknown route %s %s", r.Method, r.URL.Path))
	})
	return mux
}

// auth enforces PROXY_API_KEY (if configured) as a Bearer token.
func (s *server) auth(next http.Handler) http.Handler {
	if s.cfg.ProxyAPIKey == "" {
		return next
	}
	want := sha256.Sum256([]byte(s.cfg.ProxyAPIKey))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := strings.TrimSpace(r.Header.Get("Authorization"))
		token := ""
		if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			token = strings.TrimSpace(h[7:])
		}
		got := sha256.Sum256([]byte(token))
		if token == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			s.log.Warn("unauthorized request", "method", r.Method, "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key",
				"invalid or missing API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	s.log.Info("models", "path", r.URL.Path)
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "volc-bigasr", "object": "model", "owned_by": "volcengine"},
		},
	})
}

// reqLog collects the fields of the single log line emitted per transcription.
// It never holds transcript text, audio, prompt/language values, or credentials.
type reqLog struct {
	status     int
	up         *upload
	audioSec   float64
	upstreamMS int64
	volcCode   string
	logID      string
	textLen    int
	errMsg     string
}

func (s *server) handleTranscription(w http.ResponseWriter, r *http.Request) {
	s.wg.Add(1)
	defer s.wg.Done()
	start := time.Now()
	lg := &reqLog{up: &upload{}}
	defer func() {
		u := lg.up
		attrs := []any{
			"route", r.URL.Path, "status", lg.status,
			"file_ext", u.ext, "file_type", u.contentType, "in_bytes", u.size,
			"model", u.model, "response_format", u.responseFormat,
			"has_language", u.hasLanguage, "has_prompt", u.hasPrompt,
			"audio_sec", math.Round(lg.audioSec*100) / 100, "upstream_ms", lg.upstreamMS,
			"volc_code", lg.volcCode, "logid", lg.logID, "text_len", lg.textLen,
			"total_ms", time.Since(start).Milliseconds(),
		}
		if lg.errMsg != "" {
			s.log.Warn("transcription failed", append(attrs, "err", lg.errMsg)...)
		} else {
			s.log.Info("transcription", attrs...)
		}
	}()
	fail := func(status int, typ, code, msg string) {
		lg.status = status
		if lg.errMsg == "" {
			lg.errMsg = msg
		}
		writeError(w, status, typ, code, msg)
	}

	// Deadlines: the upload is bounded by the server's ReadTimeout
	// (REQUEST_TIMEOUT); waiting for a worker plus ffmpeg get REQUEST_TIMEOUT;
	// the Volcengine session gets REQUEST_TIMEOUT + audio × TIMEOUT_PER_AUDIO_SECOND.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload)
	up, err := readUpload(r) // streams the file part to a temp file
	defer up.cleanup()
	lg.up = up
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(http.StatusRequestEntityTooLarge, "invalid_request_error", "file_too_large",
				fmt.Sprintf("request body exceeds the %d MB limit", s.cfg.MaxUpload>>20))
			return
		}
		if isTimeout(err) {
			fail(http.StatusRequestTimeout, "invalid_request_error", "request_timeout",
				"timed out reading the request body")
			return
		}
		fail(http.StatusBadRequest, "invalid_request_error", "invalid_request", err.Error())
		return
	}
	if up.path == "" {
		fail(http.StatusBadRequest, "invalid_request_error", "missing_file", "missing required 'file' field")
		return
	}
	if up.size == 0 {
		fail(http.StatusBadRequest, "invalid_request_error", "empty_file", "uploaded file is empty")
		return
	}
	format := up.responseFormat
	switch format {
	case "":
		format = "json"
	case "json", "text", "verbose_json":
	case "srt", "vtt":
		fail(http.StatusBadRequest, "invalid_request_error", "unsupported_response_format",
			"response_format '"+format+"' is not supported; use json, text or verbose_json")
		return
	default:
		fail(http.StatusBadRequest, "invalid_request_error", "invalid_response_format",
			"invalid response_format '"+format+"'")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		fail(http.StatusGatewayTimeout, "server_error", "timeout", "timed out waiting for a free worker")
		return
	}

	pcm, err := s.convert(ctx, up.path, up.ext)
	if err != nil {
		if ctx.Err() != nil {
			fail(http.StatusGatewayTimeout, "server_error", "timeout", "timed out converting audio")
			return
		}
		if errors.Is(err, errAudioTooLong) {
			fail(http.StatusBadRequest, "invalid_request_error", "audio_too_long",
				fmt.Sprintf("audio is longer than %d seconds", s.cfg.MaxAudioSec))
			return
		}
		lg.errMsg = err.Error() // full (trimmed) ffmpeg stderr goes to the log only
		fail(http.StatusBadRequest, "invalid_request_error", "invalid_audio",
			"could not decode audio file (unsupported or corrupt format)")
		return
	}
	lg.audioSec = pcmDuration(pcm)
	if len(pcm) >= s.cfg.MaxAudioSec*bytesPerSecond { // ffmpeg -t cut it off
		s.log.Warn("audio truncated", "route", r.URL.Path, "kept_sec", s.cfg.MaxAudioSec,
			"in_bytes", up.size, "note", "original was at least this long; raise MAX_AUDIO_SECONDS to keep more")
		w.Header().Set("X-Audio-Truncated", "true")
	}

	text := ""
	if len(pcm) > 0 { // zero-length audio: nothing to recognize
		uctx, ucancel := context.WithTimeout(r.Context(), s.cfg.upstreamTimeout(lg.audioSec))
		defer ucancel()
		t0 := time.Now()
		res, err := s.volc.recognize(uctx, pcm)
		lg.upstreamMS = time.Since(t0).Milliseconds()
		lg.volcCode, lg.logID = res.Code, res.LogID
		if err != nil {
			var ue *upstreamError
			if !errors.As(err, &ue) {
				ue = &upstreamError{http.StatusBadGateway, err.Error()}
			}
			typ := "upstream_error"
			if ue.status == http.StatusGatewayTimeout {
				typ = "timeout"
			}
			fail(ue.status, "server_error", typ, ue.msg)
			return
		}
		text = res.Text
	}
	lg.textLen = utf8.RuneCountInString(text)
	lg.status = http.StatusOK

	switch format {
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, text)
	case "verbose_json":
		writeJSON(w, http.StatusOK, map[string]any{
			"task":     "transcribe",
			"language": "",
			"duration": math.Round(lg.audioSec*1000) / 1000,
			"text":     text,
			"segments": []any{},
		})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"text": text})
	}
}

// upload is the parsed multipart request; the audio is spooled to a temp file.
type upload struct {
	path           string
	size           int64
	ext            string // sanitized lowercase extension of the client's filename
	contentType    string // Content-Type of the file part, for logging
	model          string
	responseFormat string
	hasLanguage    bool
	hasPrompt      bool
}

func (u *upload) cleanup() {
	if u.path != "" {
		os.Remove(u.path)
	}
}

// readUpload streams the multipart body, writing the "file" part to a temp
// file. It always returns a non-nil *upload so the caller can defer cleanup.
// Fields model, language, prompt, temperature etc. are accepted and ignored
// (model is kept for logging only).
func readUpload(r *http.Request) (*upload, error) {
	up := &upload{}
	mr, err := r.MultipartReader()
	if err != nil {
		return up, errors.New("request must be multipart/form-data")
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return up, nil
		}
		if err != nil {
			return up, fmt.Errorf("reading multipart body: %w", err)
		}
		switch part.FormName() {
		case "file":
			if up.path != "" {
				break // ignore duplicate file parts
			}
			up.ext = uploadExt(part.FileName())
			up.contentType = truncate(part.Header.Get("Content-Type"), 64)
			// No extension on the temp file, so ffmpeg never guesses a format from it.
			f, err := os.CreateTemp("", "volc-stt-*")
			if err != nil {
				part.Close()
				return up, fmt.Errorf("creating temp file: %w", err)
			}
			up.path = f.Name()
			up.size, err = io.Copy(f, part)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				part.Close()
				return up, fmt.Errorf("reading file: %w", err)
			}
		case "response_format", "model":
			b, err := io.ReadAll(io.LimitReader(part, 64))
			if err != nil {
				part.Close()
				return up, fmt.Errorf("reading %s: %w", part.FormName(), err)
			}
			if part.FormName() == "model" {
				up.model = strings.TrimSpace(string(b))
			} else {
				up.responseFormat = strings.ToLower(strings.TrimSpace(string(b)))
			}
		case "language":
			up.hasLanguage = true
		case "prompt":
			up.hasPrompt = true
		}
		// Drain whatever is left of this part (e.g. ignored fields).
		if _, err := io.Copy(io.Discard, part); err != nil {
			part.Close()
			return up, fmt.Errorf("reading multipart body: %w", err)
		}
		part.Close()
	}
}

// uploadExt returns the filename's extension, lowercased and without the dot,
// or "" if it is missing or not a short alphanumeric string.
func uploadExt(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext == "" || len(ext) > 8 {
		return ""
	}
	for _, c := range ext {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return ""
		}
	}
	return ext
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes an OpenAI-style error body.
func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
}
