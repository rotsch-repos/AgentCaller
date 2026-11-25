package main

import (
	"encoding/binary"
	"math"
	"os"
	"sync"
	"time"

	agentcaller "github.com/rotsch-repos/agentcaller"
)

// VADConfig tunes the energy-based voice-activity detector. Thresholds are expressed
// against per-frame RMS of 16 kHz mono float32 frames (FrameSamples, 60 ms each) and
// were calibrated on Opus-decoded 16 kHz audio from a real call. That recording is
// not part of the repository; testdata/call-opus16k.wav is a synthetic stand-in
// with the same shape (two seconds of low noise, then speech).
type VADConfig struct {
	// AbsoluteThreshold is the minimum RMS that ever counts as voice.
	AbsoluteThreshold float64
	// NoiseMultiplier scales the adaptive noise floor into the effective threshold.
	NoiseMultiplier float64
	// NoiseFloorInit seeds the adaptive noise floor.
	NoiseFloorInit float64
	// NoiseFloorAlpha is the EMA weight for new quiet frames.
	NoiseFloorAlpha float64
	// OpenDuration is how long consecutive voice must last before speech is accepted.
	OpenDuration time.Duration
	// SilenceDuration is the silence required after voice to close speech.
	SilenceDuration time.Duration
	// MinSpeech is the minimum speech duration that counts as an answer.
	MinSpeech time.Duration
	// MaxListen caps one listening round.
	MaxListen time.Duration
}

func defaultVADConfig() VADConfig {
	return VADConfig{
		AbsoluteThreshold: 0.012,
		NoiseMultiplier:   3.0,
		NoiseFloorInit:    0.003,
		NoiseFloorAlpha:   0.05,
		OpenDuration:      200 * time.Millisecond,
		SilenceDuration:   1000 * time.Millisecond,
		MinSpeech:         1500 * time.Millisecond,
		MaxListen:         60 * time.Second,
	}
}

// VADResult reports the detector outcome for one listening round.
type VADResult struct {
	// Ended reports whether the round finished (speech closed or max listen hit).
	Ended bool
	// Reason is "silence" or "max_listen".
	Reason string
	// SpeechStart is when voice opened, or -1 for never.
	SpeechStart time.Duration
	// SpeechEnd is the last voice instant.
	SpeechEnd time.Duration
	// Duration is SpeechEnd - SpeechStart when voice occurred.
	Duration time.Duration
	// Total is the full time the sink saw frames.
	Total time.Duration
	// NoSpeech reports a closed round with less than MinSpeech of voice.
	NoSpeech bool
}

type vadSink struct {
	cfg VADConfig

	mu          sync.Mutex
	frames      []float32
	frameCount  int
	noiseFloor  float64
	openedAt    int // frame index of the current voice streak start, -1 if none
	speaking    bool
	speechStart int
	lastVoice   int
	ended       bool
	endReason   string
	endFrame    int
	onEnd       chan VADResult
}

func newVADSink(cfg VADConfig) *vadSink {
	if cfg.MaxListen <= 0 {
		cfg.MaxListen = defaultVADConfig().MaxListen
	}
	return &vadSink{
		cfg:         cfg,
		noiseFloor:  cfg.NoiseFloorInit,
		openedAt:    -1,
		speechStart: -1,
		lastVoice:   -1,
		onEnd:       make(chan VADResult, 1),
	}
}

// OnEnd registers a callback fired exactly once when the round ends.
func (v *vadSink) OnEnd(fn func(VADResult)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ended {
		fn(v.result())
		return
	}
	go func() {
		res := <-v.onEnd
		fn(res)
	}()
}

// WriteFrame implements agentcaller.AudioSink; it consumes one 60 ms mono frame.
func (v *vadSink) WriteFrame(frame []float32) error {
	res, endNow := v.process(frame)
	if endNow {
		select {
		case v.onEnd <- res:
		default:
		}
	}
	return nil
}

// Close implements agentcaller.AudioSink; the buffer is kept for inspection.
func (v *vadSink) Close() error { return nil }

func (v *vadSink) process(frame []float32) (VADResult, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.frames = append(v.frames, frame...)
	v.frameCount++
	if v.ended {
		return v.result(), false
	}
	rms := frameRMS(frame)
	eff := math.Max(v.cfg.AbsoluteThreshold, v.cfg.NoiseMultiplier*v.noiseFloor)
	if rms < eff {
		v.noiseFloor = (1-v.cfg.NoiseFloorAlpha)*v.noiseFloor + v.cfg.NoiseFloorAlpha*rms
	}
	isVoice := rms > eff
	switch {
	case !v.speaking:
		if isVoice {
			if v.openedAt < 0 {
				v.openedAt = v.frameCount - 1
			}
			if v.timeOf(v.frameCount-1)-v.timeOf(v.openedAt) >= v.cfg.OpenDuration {
				v.speaking = true
				v.speechStart = v.openedAt
				v.lastVoice = v.frameCount - 1
			}
		} else {
			v.openedAt = -1
		}
	case isVoice:
		v.lastVoice = v.frameCount - 1
	default:
		silence := v.timeOf(v.frameCount-1) - v.timeOf(v.lastVoice)
		spoken := v.timeOf(v.lastVoice) - v.timeOf(v.speechStart)
		if silence >= v.cfg.SilenceDuration && spoken >= v.cfg.MinSpeech {
			v.end("silence", v.lastVoice)
		}
	}
	if !v.ended && v.timeOf(v.frameCount-1) >= v.cfg.MaxListen {
		v.end("max_listen", v.frameCount-1)
	}
	return v.result(), v.ended
}

func (v *vadSink) end(reason string, atFrame int) {
	v.ended = true
	v.endReason = reason
	v.endFrame = atFrame
}

// timeOf maps a frame index onto wall time at the 60 ms frame cadence.
func (v *vadSink) timeOf(frame int) time.Duration {
	return time.Duration(frame) * 60 * time.Millisecond
}

func (v *vadSink) result() VADResult {
	res := VADResult{
		Ended:       v.ended,
		Reason:      v.endReason,
		SpeechStart: -1,
		SpeechEnd:   -1,
		Total:       v.timeOf(v.frameCount - 1),
	}
	if v.speechStart >= 0 {
		res.SpeechStart = v.timeOf(v.speechStart)
		res.SpeechEnd = v.timeOf(v.lastVoice)
		res.Duration = res.SpeechEnd - res.SpeechStart
	}
	res.NoSpeech = v.ended && (v.speechStart < 0 || res.Duration < v.cfg.MinSpeech)
	return res
}

// Audio returns the full recorded frame buffer.
func (v *vadSink) Audio() []float32 {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]float32, len(v.frames))
	copy(out, v.frames)
	return out
}

// TrimmedAudio returns the recorded segment around the speech: 400 ms pre-roll and
// 500 ms tail, or the full buffer when no voice occurred. speechStart/lastVoice are
// frame indices; the buffer holds samples, so they are mapped onto samples first.
func (v *vadSink) TrimmedAudio() []float32 {
	v.mu.Lock()
	defer v.mu.Unlock()
	start, end := 0, len(v.frames)
	if v.speechStart >= 0 {
		sample := func(frame int) int { return frame * agentcaller.FrameSamples }
		start = sample(v.speechStart) - 400
		if start < 0 {
			start = 0
		}
		end = sample(v.lastVoice+1) + 500
		if end > len(v.frames) {
			end = len(v.frames)
		}
	}
	out := make([]float32, end-start)
	copy(out, v.frames[start:end])
	return out
}

// frameRMS returns the root mean square of a mono float32 frame.
func frameRMS(frame []float32) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sum float64
	for _, s := range frame {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(frame)))
}

// writeWAV16 writes 16 kHz mono float32 samples as a 16-bit PCM WAV file.
func writeWAV16(path string, samples []float32) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dataLen := uint32(len(samples) * 2)
	var hdr [44]byte
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], 36+dataLen)
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)
	binary.LittleEndian.PutUint16(hdr[20:22], 1)
	binary.LittleEndian.PutUint16(hdr[22:24], 1)
	binary.LittleEndian.PutUint32(hdr[24:28], agentcaller.SampleRate)
	binary.LittleEndian.PutUint32(hdr[28:32], agentcaller.SampleRate*2)
	binary.LittleEndian.PutUint16(hdr[32:34], 2)
	binary.LittleEndian.PutUint16(hdr[34:36], 16)
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], dataLen)
	if _, err := f.Write(hdr[:]); err != nil {
		return err
	}
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		v := s * 32768.0
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(int16(v)))
	}
	_, err = f.Write(buf)
	return err
}

// feedSinkTo drives an AudioSink from an AudioSource frame by frame until the sink
// signals the round ended or the source is exhausted. It returns the final result.
func feedSinkTo(sink *vadSink, src agentcaller.AudioSource) VADResult {
	defer src.Close()
	for {
		frame, err := src.ReadFrame()
		if len(frame) > 0 {
			if _, ended := sink.process(frame); ended {
				return sink.result()
			}
		}
		if err != nil {
			return sink.result()
		}
	}
}
