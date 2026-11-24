package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	meowcaller "github.com/purpshell/meowcaller"
)

// frameSamples is the repo's 60 ms frame size at 16 kHz (see audio.go).
const frameSamples = 960

// wavPath resolves a testdata fixture relative to this file.
func wavPath(name string) string {
	return filepath.Join("testdata", name)
}

// TestVADOnRealOpusDecodedAudio feeds the Opus-decoded 16 kHz fixture
// (low noise, then synthetic speech) through the VAD and verifies that
// speech is detected where it actually is (review point 2).
func TestVADOnRealOpusDecodedAudio(t *testing.T) {
	path := wavPath("call-opus16k.wav")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	src, err := meowcaller.WAVFile(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	vad := newVADSink(defaultVADConfig())
	res := feedSinkTo(vad, src)

	if res.NoSpeech {
		t.Fatalf("expected speech in the fixture, got none (reason=%s, total=%v)", res.Reason, res.Total)
	}
	if res.SpeechStart < 1900*time.Millisecond || res.SpeechStart > 2300*time.Millisecond {
		t.Fatalf("speech start %v outside the recorded speech onset [1.9s, 2.3s]", res.SpeechStart)
	}
	if res.Duration < 5000*time.Millisecond {
		t.Fatalf("speech duration %v too short; the recording holds ~6s of voice", res.Duration)
	}
	if res.Ended && res.Reason != "max_listen" {
		t.Fatalf("unexpected end reason %q for a recording that ends mid-speech", res.Reason)
	}
	// TrimmedAudio must be a strict sub-window of the full recording.
	full, trimmed := len(vad.Audio()), len(vad.TrimmedAudio())
	if trimmed <= 0 || trimmed >= full {
		t.Fatalf("trimmed audio (%d) not a strict sub-window of full recording (%d)", trimmed, full)
	}
}

// syntheticFrames builds frames: silence (zero), sine voice at the given amplitude.
func syntheticFrames(silence, voice, tailSilence time.Duration, amp float64) [][]float32 {
	var frames [][]float32
	push := func(d time.Duration, amp float64) {
		n := int(d / (60 * time.Millisecond))
		phase := 0.0
		for i := 0; i < n; i++ {
			f := make([]float32, frameSamples)
			for j := range f {
				if amp > 0 {
					phase += 2 * math.Pi * 220 / 16000
					f[j] = float32(amp * 0.5 * math.Sin(phase))
				}
			}
			frames = append(frames, f)
		}
	}
	push(silence, 0)
	push(voice, amp)
	push(tailSilence, 0)
	return frames
}

func feedFrames(vad *vadSink, frames [][]float32) {
	for _, f := range frames {
		if _, ended := vad.process(f); ended {
			break
		}
	}
}

// TestVADSilenceEndpointing verifies the round closes on the configured silence
// after a long-enough utterance, with the reported timings matching the input.
func TestVADSilenceEndpointing(t *testing.T) {
	vad := newVADSink(defaultVADConfig())
	frames := syntheticFrames(500*time.Millisecond, 2000*time.Millisecond, 1500*time.Millisecond, 0.2)
	feedFrames(vad, frames)
	res := vad.result()

	if !res.Ended || res.Reason != "silence" {
		t.Fatalf("expected ended=silence, got ended=%v reason=%q", res.Ended, res.Reason)
	}
	if res.NoSpeech {
		t.Fatal("2 s of voice must count as speech (MinSpeech 1.5 s)")
	}
	// Speech opens after OpenDuration (200 ms) inside the 0.5–2.5 s voice region;
	// SpeechStart is the first voiced frame (frame-quantized).
	if res.SpeechStart < 400*time.Millisecond || res.SpeechStart > 560*time.Millisecond {
		t.Fatalf("speech start %v outside [0.4s, 0.56s]", res.SpeechStart)
	}
	// Speech end is the last voiced frame (~2.5 s, frame quantization ±120 ms).
	if res.SpeechEnd < 2300*time.Millisecond || res.SpeechEnd > 2600*time.Millisecond {
		t.Fatalf("speech end %v outside [2.3s, 2.6s]", res.SpeechEnd)
	}
	if res.Duration < 1400*time.Millisecond {
		t.Fatalf("speech duration %v below expectation (~2 s)", res.Duration)
	}
}

// TestVADNoSpeechOnShortBurst verifies a burst shorter than MinSpeech is
// reported as no-speech when the round is cut by MaxListen (the ask-again path).
// While the silence after a short burst lasts, the VAD keeps listening because
// the speaker may simply be pausing mid-sentence.
func TestVADNoSpeechOnShortBurst(t *testing.T) {
	cfg := defaultVADConfig()
	cfg.MaxListen = 3 * time.Second
	vad := newVADSink(cfg)
	frames := syntheticFrames(300*time.Millisecond, 400*time.Millisecond, 2500*time.Millisecond, 0.2)
	feedFrames(vad, frames)
	res := vad.result()

	if !res.Ended || res.Reason != "max_listen" {
		t.Fatalf("expected ended=max_listen, got ended=%v reason=%q", res.Ended, res.Reason)
	}
	if !res.NoSpeech {
		t.Fatal("0.4 s burst below MinSpeech (1.5 s) must be reported as no-speech")
	}
}

// TestVADMaxListen verifies a never-ending utterance is cut off by MaxListen.
func TestVADMaxListen(t *testing.T) {
	cfg := defaultVADConfig()
	cfg.MaxListen = 1 * time.Second
	vad := newVADSink(cfg)
	// 3 s of continuous voice.
	frames := syntheticFrames(0, 3*time.Second, 0, 0.2)
	feedFrames(vad, frames)
	res := vad.result()

	if !res.Ended || res.Reason != "max_listen" {
		t.Fatalf("expected ended=max_listen, got ended=%v reason=%q", res.Ended, res.Reason)
	}
	if res.Total < 900*time.Millisecond || res.Total > 1100*time.Millisecond {
		t.Fatalf("round ran %v, expected ~1 s (MaxListen)", res.Total)
	}
}

// TestVADPureSilence verifies a silent round ends at MaxListen with NoSpeech.
func TestVADPureSilence(t *testing.T) {
	cfg := defaultVADConfig()
	cfg.MaxListen = 1 * time.Second
	vad := newVADSink(cfg)
	frames := syntheticFrames(1500*time.Millisecond, 0, 0, 0)
	feedFrames(vad, frames)
	res := vad.result()

	if !res.Ended || res.Reason != "max_listen" {
		t.Fatalf("expected ended=max_listen, got ended=%v reason=%q", res.Ended, res.Reason)
	}
	if !res.NoSpeech {
		t.Fatal("pure silence must be reported as no-speech")
	}
}
