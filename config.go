package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	defaultResourceID = "volc.seedasr.sauc.duration"
	defaultEndpoint   = "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_nostream"
	defaultModelName  = "bigmodel"
	defaultListenAddr = "127.0.0.1:8090"
	defaultTimeout    = "60s"
	defaultMaxAudio   = "600"
	defaultMaxUpload  = "128"
	defaultPerAudioS  = "0.5"
)

// Config holds all runtime settings, read from environment variables.
type Config struct {
	APIKey      string // VOLC_API_KEY: new console, sent as X-Api-Key
	AppKey      string // VOLC_APP_KEY: legacy console, sent as X-Api-App-Key
	AccessKey   string // VOLC_ACCESS_KEY: legacy console, sent as X-Api-Access-Key
	ResourceID  string
	Endpoint    string
	ModelName   string
	Language    string // optional audio.language for Volcengine
	EnableDDC   bool   // VOLC_ENABLE_DDC: request.enable_ddc (语义顺滑)
	ListenAddr  string
	ProxyAPIKey string // optional bearer token required from clients
	FFmpegPath  string
	Timeout     time.Duration
	MaxAudioSec int     // MAX_AUDIO_SECONDS: longer audio is truncated by ffmpeg
	MaxUpload   int64   // MAX_UPLOAD_MB, in bytes
	PerAudioSec float64 // TIMEOUT_PER_AUDIO_SECOND: extra upstream time per second of audio
}

// upstreamTimeout is the deadline for the Volcengine session of a request
// with audioSec seconds of audio: REQUEST_TIMEOUT + audio × TIMEOUT_PER_AUDIO_SECOND.
func (c *Config) upstreamTimeout(audioSec float64) time.Duration {
	return c.Timeout + time.Duration(audioSec*c.PerAudioSec*float64(time.Second))
}

// maxRequestDuration bounds a whole request (used as the server's
// WriteTimeout): upload (REQUEST_TIMEOUT) + conversion (REQUEST_TIMEOUT) +
// the longest possible upstream session + a margin.
func (c *Config) maxRequestDuration() time.Duration {
	return 2*c.Timeout + c.upstreamTimeout(float64(c.MaxAudioSec)) + 15*time.Second
}

// useLegacyAuth reports whether the legacy App Key + Access Key pair is used.
func (c *Config) useLegacyAuth() bool {
	return c.AppKey != "" && c.AccessKey != ""
}

func (c *Config) authMode() string {
	if c.useLegacyAuth() {
		return "legacy (X-Api-App-Key + X-Api-Access-Key)"
	}
	return "api-key (X-Api-Key)"
}

// loadConfig builds a Config from getenv (os.Getenv in production).
func loadConfig(getenv func(string) string) (*Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	c := &Config{
		APIKey:      get("VOLC_API_KEY", ""),
		AppKey:      get("VOLC_APP_KEY", ""),
		AccessKey:   get("VOLC_ACCESS_KEY", ""),
		ResourceID:  get("VOLC_RESOURCE_ID", defaultResourceID),
		Endpoint:    get("VOLC_ENDPOINT", defaultEndpoint),
		ModelName:   get("VOLC_MODEL_NAME", defaultModelName),
		Language:    get("VOLC_LANGUAGE", ""),
		ListenAddr:  get("LISTEN_ADDR", defaultListenAddr),
		ProxyAPIKey: get("PROXY_API_KEY", ""),
		FFmpegPath:  get("FFMPEG_PATH", ""),
	}
	if !c.useLegacyAuth() && c.APIKey == "" {
		return nil, errors.New("no Volcengine credentials configured: set VOLC_API_KEY (new console), " +
			"or both VOLC_APP_KEY and VOLC_ACCESS_KEY (legacy console); see .env.example")
	}
	d, err := time.ParseDuration(get("REQUEST_TIMEOUT", defaultTimeout))
	if err != nil || d <= 0 {
		return nil, fmt.Errorf("invalid REQUEST_TIMEOUT %q: want a positive duration like 60s", getenv("REQUEST_TIMEOUT"))
	}
	c.Timeout = d
	n, err := strconv.Atoi(get("MAX_AUDIO_SECONDS", defaultMaxAudio))
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("invalid MAX_AUDIO_SECONDS %q: want a positive integer", getenv("MAX_AUDIO_SECONDS"))
	}
	c.MaxAudioSec = n
	mb, err := strconv.Atoi(get("MAX_UPLOAD_MB", defaultMaxUpload))
	if err != nil || mb <= 0 {
		return nil, fmt.Errorf("invalid MAX_UPLOAD_MB %q: want a positive integer", getenv("MAX_UPLOAD_MB"))
	}
	c.MaxUpload = int64(mb) << 20
	f, err := strconv.ParseFloat(get("TIMEOUT_PER_AUDIO_SECOND", defaultPerAudioS), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 60 {
		return nil, fmt.Errorf("invalid TIMEOUT_PER_AUDIO_SECOND %q: want a number like 0.5", getenv("TIMEOUT_PER_AUDIO_SECOND"))
	}
	c.PerAudioSec = f
	if c.EnableDDC, err = strconv.ParseBool(get("VOLC_ENABLE_DDC", "true")); err != nil {
		return nil, fmt.Errorf("invalid VOLC_ENABLE_DDC %q: want true or false", getenv("VOLC_ENABLE_DDC"))
	}
	return c, nil
}

// resolveFFmpeg returns a usable ffmpeg path. launchd jobs get a minimal PATH,
// so common Homebrew locations are tried when PATH lookup fails.
func resolveFFmpeg(explicit string) (string, error) {
	if explicit != "" {
		if _, err := exec.LookPath(explicit); err != nil {
			return "", fmt.Errorf("FFMPEG_PATH %q is not an executable: %w", explicit, err)
		}
		return explicit, nil
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
		if _, err := exec.LookPath(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("ffmpeg not found: install it (brew install ffmpeg) or set FFMPEG_PATH")
}

// loadDotEnv reads KEY=VALUE pairs from path (if it exists) into the process
// environment. Variables that are already set (non-empty) are left untouched.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	vals, err := parseDotEnv(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range vals {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return nil
}

// parseDotEnv parses a minimal .env format: blank lines and # comments are
// ignored, an optional "export " prefix is allowed, values may be wrapped in
// single or double quotes, and values may carry a trailing " # comment".
// An unterminated quote is an error.
func parseDotEnv(r io.Reader) (map[string]string, error) {
	vals := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", n)
		}
		val = strings.TrimSpace(val)
		if val != "" && (val[0] == '"' || val[0] == '\'') {
			end := strings.IndexByte(val[1:], val[0])
			if end < 0 {
				return nil, fmt.Errorf("line %d: unterminated quote in value of %s", n, key)
			}
			rest := strings.TrimSpace(val[end+2:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return nil, fmt.Errorf("line %d: unexpected text after quoted value of %s", n, key)
			}
			val = val[1 : end+1]
		} else if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		vals[key] = val
	}
	return vals, sc.Err()
}
