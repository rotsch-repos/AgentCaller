package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TTSConfig selects the local text-to-speech engine. Piper is the default and keeps
// the whole call path on this host; edge-tts is an explicit fallback flag (it talks
// to the Microsoft cloud) and OpenAI-TTS is not part of the call path at all.
type TTSConfig struct {
	// Backend is "piper" or "edge".
	Backend string
	// PiperVoice is the voice name under PiperDataDir, e.g. de_DE-thorsten-medium.
	PiperVoice string
	// PiperDataDir holds the onnx model files, default ~/.local/share/piper.
	PiperDataDir string
	// Timeout bounds one synthesis.
	Timeout time.Duration
	// OutputDir receives the rendered WAV/MP3 files.
	OutputDir string
}

func defaultTTSConfig() TTSConfig {
	home, _ := os.UserHomeDir()
	return TTSConfig{
		Backend:      "piper",
		PiperVoice:   "de_DE-thorsten-medium",
		PiperDataDir: filepath.Join(home, ".local", "share", "piper"),
		Timeout:      30 * time.Second,
	}
}

// Synthesize renders text to a playable audio file and returns its path.
func synthesize(ctx context.Context, cfg TTSConfig, text, stem string) (string, error) {
	if cfg.Timeout == 0 {
		cfg = defaultTTSConfig()
	}
	if cfg.OutputDir == "" {
		dir, err := os.MkdirTemp("", "agentcaller-tts-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		cfg.OutputDir = dir
	}
	switch cfg.Backend {
	case "edge":
		return synthesizeEdge(ctx, cfg, text, stem)
	case "piper", "":
		return synthesizePiper(ctx, cfg, text, stem)
	default:
		return "", fmt.Errorf("unknown tts backend %q (want piper or edge)", cfg.Backend)
	}
}

func synthesizePiper(ctx context.Context, cfg TTSConfig, text, stem string) (string, error) {
	voiceDir := filepath.Join(cfg.PiperDataDir, cfg.PiperVoice)
	model := voiceDir + ".onnx"
	if _, err := os.Stat(model); err != nil {
		return "", fmt.Errorf("piper voice %q not found at %s: %w (download it first)", cfg.PiperVoice, model, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	outPath := filepath.Join(cfg.OutputDir, stem+".wav")
	cmd := exec.CommandContext(runCtx, resolvePiperBinary(),
		"-m", model, "-c", model+".json", "-f", outPath, "-i", "/dev/stdin")
	cmd.Stdin = strings.NewReader(text)
	if err := runCtx.Err(); err != nil {
		return "", err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("piper timed out after %s", cfg.Timeout)
		}
		return "", fmt.Errorf("piper failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	info, err := os.Stat(outPath)
	if err != nil || info.Size() == 0 {
		return "", fmt.Errorf("piper produced no audio")
	}
	return outPath, nil
}

func resolvePiperBinary() string {
	if p, err := exec.LookPath("piper"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	local := filepath.Join(home, ".local", "bin", "piper")
	if _, err := os.Stat(local); err == nil {
		return local
	}
	return "piper"
}

func synthesizeEdge(ctx context.Context, cfg TTSConfig, text, stem string) (string, error) {
	bin := "edge-tts"
	if p, err := exec.LookPath("edge-tts"); err == nil {
		bin = p
	}
	outPath := filepath.Join(cfg.OutputDir, stem+".mp3")
	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, "--text", text, "--write-media", outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("edge-tts timed out after %s", cfg.Timeout)
		}
		return "", fmt.Errorf("edge-tts failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return outPath, nil
}
