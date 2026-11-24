package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// STTBackend transcribes 16 kHz mono WAV files. The default local backend shells out
// to the whisper CLI; keeping it behind an interface lets dry-run and tests substitute
// fakes without touching the GPU path.
type STTBackend interface {
	Transcribe(ctx context.Context, wavPath string) (string, error)
}

// STTConfig controls the local whisper-CLI backend.
type STTConfig struct {
	// Binary is the whisper executable; empty resolves $PATH plus ~/.local/bin.
	Binary string
	// Model is the preferred model; the guard may step down via Fallbacks.
	Model string
	// Fallbacks are tried in order when the preferred model fails or lacks VRAM.
	Fallbacks []string
	// ModelDir is the whisper model cache directory.
	ModelDir string
	// Language is forced for the transcription.
	Language string
	// Timeout bounds one whisper run.
	Timeout time.Duration
	// MinFreeMiB is the VRAM headroom the GPU guard requires before a GPU run.
	MinFreeMiB int
	// GPU is the CUDA device to pin; -1 means auto-pick, 0+ is a fixed index, "cpu" forces CPU.
	GPU string
}

func defaultSTTConfig() STTConfig {
	return STTConfig{
		Model:      "small",
		Fallbacks:  []string{"base", "tiny"},
		ModelDir:   whisperModelDir(),
		Language:   "de",
		Timeout:    60 * time.Second,
		MinFreeMiB: 3072,
		GPU:        "auto",
	}
}

func whisperModelDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "whisper")
}

// whisperCLIBackend runs the local whisper CLI. The whole chain stays on this host:
// no cloud API sits between the call audio and the transcript.
type whisperCLIBackend struct {
	cfg STTConfig
	log zerolog.Logger
}

// NewSTTBackend returns the local whisper-CLI backend.
func NewSTTBackend(cfg STTConfig, log zerolog.Logger) STTBackend {
	if cfg.Model == "" {
		cfg.Model = "small"
	}
	if len(cfg.Fallbacks) == 0 {
		cfg.Fallbacks = []string{"base", "tiny"}
	}
	if cfg.Language == "" {
		cfg.Language = "de"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.MinFreeMiB == 0 {
		cfg.MinFreeMiB = 3072
	}
	if cfg.GPU == "" {
		cfg.GPU = "auto"
	}
	return &whisperCLIBackend{cfg: cfg, log: log}
}

type whisperResult struct {
	Text string `json:"text"`
}

func (b *whisperCLIBackend) Transcribe(ctx context.Context, wavPath string) (string, error) {
	bin := b.cfg.Binary
	if bin == "" {
		bin = resolveWhisperBinary()
	}
	for _, model := range append([]string{b.cfg.Model}, b.cfg.Fallbacks...) {
		device := "cuda"
		env := os.Environ()
		gpuID, ok := pickGPU(b.cfg.GPU, b.cfg.MinFreeMiB, b.log)
		if ok {
			env = append(env, "CUDA_VISIBLE_DEVICES="+strconv.Itoa(gpuID))
		} else if b.cfg.GPU == "cpu" {
			device = "cpu"
		} else if runtime.GOOS == "linux" {
			// No nvidia-smi or no headroom: run on CPU. vLLM is never touched.
			device = "cpu"
			env = append(env, "CUDA_VISIBLE_DEVICES=")
		}
		out, err := b.runWhisper(ctx, bin, wavPath, model, device, env)
		if err == nil {
			return out, nil
		}
		b.log.Warn().Err(err).Str("model", model).Str("device", device).
			Msg("stt run failed; trying fallback model")
	}
	return "", fmt.Errorf("whisper failed for model %s and all fallbacks", b.cfg.Model)
}

func (b *whisperCLIBackend) runWhisper(ctx context.Context, bin, wavPath, model, device string, env []string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	outDir, err := os.MkdirTemp("", "meowcaller-stt-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(outDir)
	args := []string{
		wavPath,
		"--model", model,
		"--language", b.cfg.Language,
		"--output_format", "json",
		"--output_dir", outDir,
		"--device", device,
	}
	if device == "cuda" {
		args = append(args, "--fp16", "True")
	} else {
		args = append(args, "--fp16", "False")
	}
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Env = env
	cmd.Stderr = os.Stderr
	var stdout strings.Builder
	cmd.Stdout = &stdout
	start := time.Now()
	runErr := cmd.Run()
	b.log.Debug().Err(runErr).Str("model", model).Str("device", device).
		Dur("duration", time.Since(start)).Msg("whisper run finished")
	if runCtx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("whisper timed out after %s", b.cfg.Timeout)
	}
	if runErr != nil {
		return "", runErr
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return "", err
	}
	var result whisperResult
	found := false
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(outDir, e.Name()))
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return "", fmt.Errorf("parse whisper output: %w", err)
		}
		found = true
		break
	}
	if !found {
		return "", fmt.Errorf("whisper produced no JSON output")
	}
	return strings.TrimSpace(result.Text), nil
}

func resolveWhisperBinary() string {
	if p, err := exec.LookPath("whisper"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	local := filepath.Join(home, ".local", "bin", "whisper")
	if _, err := os.Stat(local); err == nil {
		return local
	}
	return "whisper"
}

type gpuInfo struct {
	deviceID int
	freeMiB  int
	totalMiB int
}

// parseNVSMISmi parses `nvidia-smi --query-gpu=...` CSV output.
func parseNVSMISmi(out string) []gpuInfo {
	var outLines []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "index,") {
			continue
		}
		outLines = append(outLines, line)
	}
	var gpus []gpuInfo
	for _, line := range outLines {
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		id, err1 := strconv.Atoi(strings.TrimSpace(fields[0]))
		used, err2 := parseMiB(fields[1])
		total, err3 := parseMiB(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		gpus = append(gpus, gpuInfo{deviceID: id, freeMiB: total - used, totalMiB: total})
	}
	return gpus
}

func parseMiB(s string) (int, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "MiB")
	s = strings.TrimSpace(s)
	return strconv.Atoi(s)
}

// pickGPU picks the CUDA device for a whisper run. It never schedules vLLM's
// processes: the guard only reads free VRAM and refuses to start when no card has
// enough headroom, so the vLLM service is never put at risk.
func pickGPU(mode string, minFreeMiB int, log zerolog.Logger) (int, bool) {
	if mode == "cpu" {
		return 0, false
	}
	if !strings.Contains(mode, "auto") && mode != "" {
		if id, err := strconv.Atoi(mode); err == nil {
			return id, true
		}
	}
	smi, err := exec.LookPath("nvidia-smi")
	if err != nil {
		log.Debug().Msg("no nvidia-smi; whisper will run on CPU")
		return 0, false
	}
	raw, err := exec.Command(smi,
		"--query-gpu=index,memory.used,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		log.Warn().Err(err).Msg("nvidia-smi query failed; whisper will run on CPU")
		return 0, false
	}
	gpus := parseNVSMISmi(string(raw))
	if len(gpus) == 0 {
		log.Warn().Msg("nvidia-smi reported no GPU; whisper will run on CPU")
		return 0, false
	}
	best := -1
	bestFree := -1
	for _, g := range gpus {
		if g.freeMiB > bestFree {
			bestFree = g.freeMiB
			best = g.deviceID
		}
	}
	if bestFree < minFreeMiB {
		log.Warn().Int("best_free_mib", bestFree).Int("need_mib", minFreeMiB).
			Msg("insufficient VRAM headroom; whisper will run on CPU")
		return 0, false
	}
	log.Debug().Int("device", best).Int("free_mib", bestFree).Msg("pinned whisper to GPU with headroom")
	return best, true
}

// maxRMS is the largest sample magnitude in a mono float32 buffer, for sanity checks.
func maxRMS(samples []float32) float64 {
	var m float64
	for i := 0; i < len(samples); i += 7 {
		if v := math.Abs(float64(samples[i])); v > m {
			m = v
		}
	}
	return m
}
