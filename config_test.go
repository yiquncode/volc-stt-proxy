package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDotEnv(t *testing.T) {
	in := `
# comment
VOLC_API_KEY=abc123
export LISTEN_ADDR = 127.0.0.1:9000
QUOTED="hello world"
SINGLE='a # not a comment'
INLINE=value # trailing comment
DQ_COMMENT="abc" # comment
SQ_COMMENT='abc' # c
EMPTY=
`
	got, err := parseDotEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"VOLC_API_KEY": "abc123",
		"LISTEN_ADDR":  "127.0.0.1:9000",
		"QUOTED":       "hello world",
		"SINGLE":       "a # not a comment",
		"INLINE":       "value",
		"DQ_COMMENT":   "abc",
		"SQ_COMMENT":   "abc",
		"EMPTY":        "",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	for _, bad := range []string{"NOEQUALS\n", "A=1\nB=\"abc\n", "B='abc\n", "B=\"abc\"junk\n"} {
		_, err := parseDotEnv(strings.NewReader(bad))
		if err == nil || !strings.Contains(err.Error(), "line ") {
			t.Errorf("%q: expected error with line number, got %v", bad, err)
		}
	}
}

func TestLoadDotEnvRealEnvWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("VSP_TEST_A=fromfile\nVSP_TEST_B=fromfile\n"), 0o600)
	t.Setenv("VSP_TEST_A", "fromenv")
	t.Setenv("VSP_TEST_B", "") // registers cleanup; empty counts as unset
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if v := os.Getenv("VSP_TEST_A"); v != "fromenv" {
		t.Errorf("VSP_TEST_A = %q, want fromenv", v)
	}
	if v := os.Getenv("VSP_TEST_B"); v != "fromfile" {
		t.Errorf("VSP_TEST_B = %q, want fromfile", v)
	}
	if err := loadDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("missing .env should be ignored, got %v", err)
	}
}

func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfig(t *testing.T) {
	cfg, err := loadConfig(envFunc(map[string]string{"VOLC_API_KEY": "k"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.useLegacyAuth() || cfg.ResourceID != "volc.seedasr.sauc.duration" ||
		cfg.Endpoint != "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_nostream" || !cfg.EnableDDC ||
		cfg.ModelName != "bigmodel" || cfg.ListenAddr != "127.0.0.1:8090" || cfg.Timeout != 60*time.Second ||
		cfg.MaxUpload != 24<<20 || cfg.PerAudioSec != 0.5 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}

	cfg, err = loadConfig(envFunc(map[string]string{
		"VOLC_API_KEY": "k", "VOLC_APP_KEY": "app", "VOLC_ACCESS_KEY": "acc", "REQUEST_TIMEOUT": "5s",
		"VOLC_ENABLE_DDC": "false", "VOLC_RESOURCE_ID": "volc.seedasr.sauc.concurrent",
		"MAX_UPLOAD_MB": "10", "TIMEOUT_PER_AUDIO_SECOND": "0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.useLegacyAuth() || cfg.Timeout != 5*time.Second || cfg.EnableDDC ||
		cfg.ResourceID != "volc.seedasr.sauc.concurrent" || cfg.MaxUpload != 10<<20 || cfg.PerAudioSec != 0 {
		t.Errorf("expected legacy auth and 5s timeout: %+v", cfg)
	}

	for name, env := range map[string]map[string]string{
		"no credentials":   {},
		"only app key":     {"VOLC_APP_KEY": "app"},
		"bad timeout":      {"VOLC_API_KEY": "k", "REQUEST_TIMEOUT": "soon"},
		"negative timeout": {"VOLC_API_KEY": "k", "REQUEST_TIMEOUT": "-1s"},
		"bad ddc":          {"VOLC_API_KEY": "k", "VOLC_ENABLE_DDC": "maybe"},
		"bad upload":       {"VOLC_API_KEY": "k", "MAX_UPLOAD_MB": "0"},
		"bad per-second":   {"VOLC_API_KEY": "k", "TIMEOUT_PER_AUDIO_SECOND": "-1"},
		"NaN per-second":   {"VOLC_API_KEY": "k", "TIMEOUT_PER_AUDIO_SECOND": "NaN"},
		"Inf per-second":   {"VOLC_API_KEY": "k", "TIMEOUT_PER_AUDIO_SECOND": "+Inf"},
		"-Inf per-second":  {"VOLC_API_KEY": "k", "TIMEOUT_PER_AUDIO_SECOND": "-Inf"},
	} {
		if _, err := loadConfig(envFunc(env)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestDerivedTimeouts(t *testing.T) {
	cfg, err := loadConfig(envFunc(map[string]string{"VOLC_API_KEY": "k"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.upstreamTimeout(600); got != 360*time.Second { // 60 + 600 × 0.5
		t.Errorf("upstreamTimeout(600) = %v, want 6m0s", got)
	}
	if got := cfg.upstreamTimeout(5); got != 62500*time.Millisecond {
		t.Errorf("upstreamTimeout(5) = %v, want 62.5s", got)
	}
	// upload (60) + worker wait (60) + session for a 24 MiB upload
	// (60 + 786.432 s × 0.5) + 15s margin
	if got := cfg.maxRequestDuration(); got != 588216*time.Millisecond {
		t.Errorf("maxRequestDuration = %v, want 9m48.216s", got)
	}
}
