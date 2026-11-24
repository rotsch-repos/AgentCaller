package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestParseTalkArgsLive verifies the live-call invocation and the separate
// talk store default (review point 4: talk must not reuse the notify store).
func TestParseTalkArgsLive(t *testing.T) {
	var stderr bytes.Buffer
	cfg, err := parseTalkArgs([]string{"+41791234567"}, &stderr)
	if err != nil {
		t.Fatalf("parse live: %v", err)
	}
	if cfg.target != "+41791234567" {
		t.Fatalf("target = %q", cfg.target)
	}
	if cfg.dryRun || cfg.replayPath != "" {
		t.Fatal("a bare target must be a live call, not dry-run/replay")
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".config", "meowcaller", "talk-voip.db")
	if cfg.storePath != want {
		t.Fatalf("store default = %q, want %q (separate from notify)", cfg.storePath, want)
	}
	if cfg.answerTimeout != 45*time.Second {
		t.Fatalf("answer-timeout default = %s, want 45s (parity with notify)", cfg.answerTimeout)
	}
}

// TestParseTalkArgsDryRun verifies -dry-run takes a single WAV argument, with
// flags after the positional (Go's flag package stops at the first positional).
func TestParseTalkArgsDryRun(t *testing.T) {
	var stderr bytes.Buffer
	cfg, err := parseTalkArgs([]string{"-dry-run", "/tmp/audio.wav", "-out-dir", "/tmp/out"}, &stderr)
	if err != nil {
		t.Fatalf("parse dry-run: %v", err)
	}
	if !cfg.dryRun || cfg.target != "/tmp/audio.wav" || cfg.outDir != "/tmp/out" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

// TestParseTalkArgsReplay verifies the replay path: the file is a flag value,
// --listen-only is a bool, and no positional may follow.
func TestParseTalkArgsReplay(t *testing.T) {
	var stderr bytes.Buffer
	cfg, err := parseTalkArgs([]string{"-replay", "/tmp/frames.wav", "--listen-only", "-vad-silence", "2s"}, &stderr)
	if err != nil {
		t.Fatalf("parse replay: %v", err)
	}
	if cfg.replayPath != "/tmp/frames.wav" || !cfg.listenOnly || cfg.vadSilence != 2*time.Second {
		t.Fatalf("cfg = %+v", cfg)
	}
	var stderr2 bytes.Buffer
	if _, err := parseTalkArgs([]string{"-replay", "/tmp/frames.wav", "extra"}, &stderr2); !errors.Is(err, errUsage) {
		t.Fatalf("extra positional with -replay must be a usage error, got %v", err)
	}
}

// TestParseTalkArgsUsageErrors verifies the exit-2 contract (analog notify).
func TestParseTalkArgsUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"a", "b"},
		{"-dry-run"},
		{"-answer-timeout", "-1s", "+41791234567"},
	}
	for _, args := range cases {
		var stderr bytes.Buffer
		if _, err := parseTalkArgs(args, &stderr); !errors.Is(err, errUsage) {
			t.Fatalf("args %v: want errUsage, got %v", args, err)
		}
	}
}

// TestReorderFlagsFirst verifies the flag/positional reordering the Go flag
// package needs, including value-taking flags and the `--flag=value` form.
func TestReorderFlagsFirst(t *testing.T) {
	var cfg talkConfig
	flags := newTalkFlagSet(&cfg, io.Discard)
	got := reorderFlagsFirst(flags, []string{
		"-dry-run", "/tmp/x.wav", "-out-dir", "/tmp/o", "-vad-silence=2s", "trailing",
	})
	want := []string{"-dry-run", "-out-dir", "/tmp/o", "-vad-silence=2s", "/tmp/x.wav", "trailing"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("reorder = %v, want %v", got, want)
	}
	// A value flag at the end without its value must not swallow a positional.
	got = reorderFlagsFirst(flags, []string{"/tmp/x.wav", "-out-dir"})
	want = []string{"-out-dir", "/tmp/x.wav"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("reorder (dangling flag) = %v, want %v", got, want)
	}
}

// TestTalkArtifactDir verifies artifactDir honors -out-dir, then -save-audio,
// and creates a private /tmp directory as the fallback.
func TestTalkArtifactDir(t *testing.T) {
	tmp := t.TempDir()
	dir, err := artifactDir(talkConfig{outDir: tmp + "/custom"})
	if err != nil {
		t.Fatal(err)
	}
	if dir != tmp+"/custom" {
		t.Fatalf("outDir not honored: %s", dir)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir not created private 0700: %v %v", fi, err)
	}
	dir, err = artifactDir(talkConfig{outDir: "", saveAudio: tmp + "/saved"})
	if err != nil || dir != tmp+"/saved" {
		t.Fatalf("saveAudio precedence wrong: %s %v", dir, err)
	}
	dir, err = artifactDir(talkConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if !strings.HasPrefix(dir, os.TempDir()) {
		t.Fatalf("fallback dir %s not under os.TempDir", dir)
	}
}

// TestProcessAudioMissingInput verifies the dry-run chain fails cleanly on a
// missing input instead of panicking or emitting a phantom artifact.
func TestProcessAudioMissingInput(t *testing.T) {
	outDir := t.TempDir()
	cfg := talkConfig{outDir: outDir}
	_, _, _, err := processAudio(context.Background(), cfg, filepath.Join(outDir, "missing.wav"), outDir, zerolog.Nop())
	if err == nil {
		t.Fatal("processAudio on a missing WAV must fail (no silent success)")
	}
	if _, err := os.Stat(filepath.Join(outDir, "transcript.json")); !os.IsNotExist(err) {
		t.Fatal("a failed run must not leave a transcript artifact behind")
	}
}

// TestNewTalkFlagSetDocumentsUsage verifies the usage text mentions all three
// invocation forms, so `meowcaller talk` without arguments is self-describing.
func TestNewTalkFlagSetDocumentsUsage(t *testing.T) {
	var stderr bytes.Buffer
	var cfg talkConfig
	flags := newTalkFlagSet(&cfg, &stderr)
	flags.Usage()
	out := stderr.String()
	for _, want := range []string{"talk [flags] <target>", "-dry-run", "-replay"} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage text missing %q:\n%s", want, out)
		}
	}
	// The registered flags must match what the README documents as implemented.
	for _, name := range []string{"store", "answer-timeout", "max-listen", "prompt",
		"say", "say-text", "save-audio", "dry-run", "replay", "listen-only",
		"tts-file", "out-dir", "stt-model", "llm-endpoint", "llm-model",
		"tts-backend", "piper-voice", "vad-silence", "vad-min-speech"} {
		if flags.Lookup(name) == nil {
			t.Fatalf("flag -%s not registered", name)
		}
	}
}
