package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentcaller "github.com/rotsch-repos/agentcaller"
	"github.com/rotsch-repos/agentcaller/rtp"
	"github.com/rs/zerolog"
	"golang.org/x/term"
)

// The call command is the two-way pipe: raw PCM in, raw PCM out, events as JSON lines.
// It contains no speech recognition, language model or synthesis; whatever drives the
// conversation sits on the other end of the pipes and can be swapped freely.

type callConfig struct {
	target        string // number to call, or with -answer the caller to accept ("any" = everyone)
	answer        bool
	answerCheck   string // with -answer: a command that decides, while the call rings, whether it is answered
	storePath     string
	inPath        string
	outPath       string
	eventsPath    string
	controlPath   string
	inRate        int
	maxBuffer     time.Duration
	conceal       bool
	bargeIn       bool
	bargeAfter    time.Duration // how long the peer must have been talking before it counts as an interruption
	endSilence    time.Duration
	answerTimeout time.Duration
	maxDuration   time.Duration
	hangupOnEOF   bool
	videoIn       string
	videoOut      string
	videoFPS      float64
	announce      string // text message sent to the target just before the call
}

// announceLead is how long the message has to show up on the phone before it rings.
const announceLead = 2 * time.Second

// announceCall tells the target by text who is about to call. A message that cannot be
// sent is reported and does not stop the call.
func announceCall(ctx context.Context, cfg callConfig, events *eventLog, send func(context.Context, string, string) error, lead time.Duration) {
	if cfg.announce == "" {
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := send(sendCtx, cfg.target, cfg.announce)
	cancel()
	if err != nil {
		events.emit("announce_error", map[string]any{"error": err.Error()})
		return
	}
	events.emit("announced", map[string]any{"text": cfg.announce})
	select {
	case <-time.After(lead):
	case <-ctx.Done():
	}
}

func parseCallArgs(args []string, stderr io.Writer) (callConfig, error) {
	var cfg callConfig
	defaultStore, err := defaultStorePath()
	if err != nil {
		return cfg, err
	}
	flags := flag.NewFlagSet("call", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.BoolVar(&cfg.answer, "answer", false, "wait for an incoming call from <caller> and answer it, instead of calling")
	flags.StringVar(&cfg.answerCheck, "answer-check", "", answerCheckUsage)
	flags.StringVar(&cfg.storePath, "store", defaultStore, "linked-device SQLite store")
	flags.StringVar(&cfg.inPath, "in", "-", "audio to send: raw s16le mono PCM from this file or FIFO; - is stdin, none sends silence")
	flags.StringVar(&cfg.outPath, "out", "-", "received audio: raw s16le mono 16 kHz PCM to this file or FIFO; - is stdout, none discards")
	flags.StringVar(&cfg.eventsPath, "events", "", "write JSON-line events here (default stderr)")
	flags.StringVar(&cfg.announce, "announce", "", "send the target this text message two seconds before calling (the ringing screen only shows the account)")
	flags.StringVar(&cfg.controlPath, "control", "", "read commands from this file or FIFO, one per line: flush, hangup, stats")
	flags.IntVar(&cfg.inRate, "in-rate", agentcaller.SampleRate, "sample rate of -in in Hz (resampled to 16 kHz)")
	flags.DurationVar(&cfg.maxBuffer, "max-buffer", 0, "bound the queued outgoing audio, dropping the oldest (for a live microphone); 0 queues everything (for speech synthesis)")
	flags.BoolVar(&cfg.conceal, "conceal", false, "fill lost packets with a synthesised continuation instead of silence (for human listeners, worse for speech recognition)")
	flags.BoolVar(&cfg.bargeIn, "barge-in", false, "discard queued outgoing audio the moment the peer starts to speak")
	flags.DurationVar(&cfg.bargeAfter, "barge-in-after", defaultBargeAfter, "how long the peer must have been speaking before -barge-in discards anything: a cough or an echo is shorter")
	flags.DurationVar(&cfg.endSilence, "end-silence", 700*time.Millisecond, "silence after speech that produces a speech_end event")
	flags.DurationVar(&cfg.answerTimeout, "answer-timeout", defaultAnswerTimeout, "maximum wait for the call to be answered (or to arrive, with -answer); 0 disables")
	flags.DurationVar(&cfg.maxDuration, "max-duration", 0, "hang up after this long; 0 disables")
	flags.BoolVar(&cfg.hangupOnEOF, "hangup-on-eof", false, "hang up once -in has ended and everything queued was sent")
	flags.StringVar(&cfg.videoIn, "video-in", "", "EXPERIMENTAL: H.264 Annex-B byte stream to send (file or FIFO, e.g. from ffmpeg -f h264)")
	flags.StringVar(&cfg.videoOut, "video-out", "", "EXPERIMENTAL: write the peer's H.264 video to this Annex-B file")
	flags.Float64Var(&cfg.videoFPS, "video-fps", 0, "frame rate of -video-in for exact timestamps; 0 stamps pictures on arrival")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: agentcaller call [flags] <target>")
		fmt.Fprintln(stderr, "       agentcaller call -answer [flags] <caller|any>")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return cfg, errors.Join(errUsage, err)
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return cfg, errUsage
	}
	cfg.target = flags.Arg(0)
	switch {
	case cfg.answer && cfg.announce != "":
		return cfg, errors.New("-announce is for calls that are placed, not with -answer")
	case !cfg.answer && cfg.answerCheck != "":
		return cfg, errors.New("-answer-check decides about calls that come in; it needs -answer")
	case cfg.answerTimeout < 0 || cfg.maxDuration < 0 || cfg.maxBuffer < 0 || cfg.endSilence <= 0:
		return cfg, errors.New("durations must be non-negative (-end-silence positive)")
	case cfg.inRate < 4000 || cfg.inRate > 192000:
		return cfg, errors.New("-in-rate must be between 4000 and 192000")
	case cfg.videoFPS < 0:
		return cfg, errors.New("-video-fps must not be negative")
	case cfg.hangupOnEOF && cfg.inPath == "none":
		return cfg, errors.New("-hangup-on-eof needs an -in stream")
	}
	return cfg, nil
}

// eventLog writes one JSON object per line: {"t": seconds since start, "event": name, ...}.
type eventLog struct {
	mu    sync.Mutex
	w     io.Writer
	start time.Time
}

func (l *eventLog) emit(event string, fields map[string]any) {
	rec := map[string]any{"event": event, "t": math.Round(time.Since(l.start).Seconds()*1000) / 1000}
	for k, v := range fields {
		rec[k] = v
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	_, _ = l.w.Write(append(line, '\n'))
	l.mu.Unlock()
}

// speechGate is a streaming voice detector for turn taking: it reports when the peer
// starts and stops talking. Same energy rule and thresholds as the talk command's
// one-shot detector (calibrated on real call audio), without its buffering.
type speechGate struct {
	cfg        VADConfig
	openFrames int // consecutive voiced frames that open speech
	endFrames  int // consecutive quiet frames that close it
	noise      float64
	frame      int
	streak     int
	quiet      int
	speaking   bool
	startFrame int
	// holdFrames is how long speech must have lasted before it counts as an interruption;
	// sustained is true on the one frame where it gets there. A cough, a click or the
	// agent's own echo opens the gate for three or four frames: enough to listen to,
	// not enough to cut the agent off in the middle of a sentence.
	holdFrames int
	sustained  bool
}

// defaultBargeAfter: on real calls the sounds that were not speech (line noise, the
// agent's own echo) lasted 120 to 360 ms, the shortest real interjections 420 to 600 ms.
const defaultBargeAfter = 480 * time.Millisecond

func newSpeechGate(endSilence, bargeAfter time.Duration) *speechGate {
	cfg := defaultVADConfig()
	if bargeAfter <= 0 {
		bargeAfter = defaultBargeAfter
	}
	return &speechGate{
		cfg:        cfg,
		openFrames: 3, // 180 ms: a click on a real call lasts up to two frames and must not count as speech
		holdFrames: max(3, int((bargeAfter+59*time.Millisecond)/(60*time.Millisecond))),
		endFrames:  max(1, int((endSilence+59*time.Millisecond)/(60*time.Millisecond))),
		noise:      cfg.NoiseFloorInit,
	}
}

// feed consumes one 60 ms frame. started is true on the frame that opens speech, ended
// on the frame that closes it; at and length locate the speech on the call timeline.
func (g *speechGate) feed(frame []float32) (started, ended bool, at, length time.Duration) {
	const tick = 60 * time.Millisecond
	rms := frameRMS(frame)
	threshold := math.Max(g.cfg.AbsoluteThreshold, g.cfg.NoiseMultiplier*g.noise)
	voiced := rms > threshold
	if !voiced {
		g.noise = (1-g.cfg.NoiseFloorAlpha)*g.noise + g.cfg.NoiseFloorAlpha*rms
	}
	idx := g.frame
	g.frame++
	g.sustained = false
	switch {
	case !g.speaking && voiced:
		g.streak++
		if g.streak >= g.openFrames {
			g.speaking, g.quiet = true, 0
			g.startFrame = idx - g.streak + 1
			g.sustained = g.streak == g.holdFrames || g.holdFrames <= g.openFrames
			return true, false, time.Duration(g.startFrame) * tick, 0
		}
	case !g.speaking:
		g.streak = 0
	case voiced:
		g.quiet = 0
		g.streak++
		g.sustained = g.streak == g.holdFrames
	default:
		g.quiet++
		if g.quiet >= g.endFrames {
			g.speaking, g.streak = false, 0
			end := idx - g.quiet + 1
			return false, true, time.Duration(g.startFrame) * tick, time.Duration(end-g.startFrame) * tick
		}
	}
	return false, false, 0, 0
}

// pcmPipe is the sink for received audio. The media loop must never wait for whoever
// reads the pipe, so frames go through a queue and are dropped (and counted) if the
// reader falls more than three seconds behind.
type pcmPipe struct {
	mu      sync.Mutex
	closed  bool
	ch      chan []byte
	dropped atomic.Uint64
	done    chan struct{}
}

func newPCMPipe(w io.Writer) *pcmPipe {
	p := &pcmPipe{ch: make(chan []byte, 50), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for b := range p.ch {
			if _, err := w.Write(b); err != nil {
				for range p.ch { // reader gone: keep draining so nobody blocks
				}
				return
			}
		}
	}()
	return p
}

func (p *pcmPipe) write(frame []float32) {
	b := make([]byte, 2*len(frame))
	for i, s := range frame {
		v := int32(math.Round(float64(s) * 32767))
		v = max(-32768, min(32767, v))
		b[2*i], b[2*i+1] = byte(v), byte(v>>8)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed { // the media loop may deliver a last frame after the call ended
		return
	}
	select {
	case p.ch <- b:
	default:
		p.dropped.Add(1)
	}
}

// close stops accepting frames and gives the reader a moment to take what is queued.
func (p *pcmPipe) close() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.ch)
	}
	p.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
}

// lazyReader opens its stream on the first Read. Opening a FIFO blocks until a writer
// appears, so it has to happen on the goroutine that reads, not before the call.
type lazyReader struct {
	open func() (io.ReadCloser, error)
	r    io.ReadCloser
}

func (l *lazyReader) Read(p []byte) (int, error) {
	if l.r == nil {
		r, err := l.open()
		if err != nil {
			return 0, err
		}
		l.r = r
	}
	return l.r.Read(p)
}

// drained wraps the outgoing source and reports when its last frame has been sent.
type drained struct {
	agentcaller.AudioSource
	once sync.Once
	done chan struct{}
}

func (s *drained) ReadFrame() ([]float32, error) {
	frame, err := s.AudioSource.ReadFrame()
	if errors.Is(err, io.EOF) {
		s.once.Do(func() { close(s.done) })
	}
	return frame, err
}

// duplexCall is what the command needs from a live call (agentcaller.Call in production).
type duplexCall interface {
	OnReady(func())
	OnEnd(func(string))
	OnStateChange(func(agentcaller.CallPhase))
	State() agentcaller.CallPhase
	Play(agentcaller.AudioSource) *agentcaller.Player
	Receive(agentcaller.AudioSink)
	ConcealLosses(bool)
	ReceiveStats() agentcaller.ReceiveStats
	Hangup() error
}

// duplex runs one call: input stream → live source → call, call → speech gate → output.
type duplex struct {
	cfg    callConfig
	events *eventLog
	live   *agentcaller.LiveSource
	out    *pcmPipe // nil: received audio is only analysed
	gate   *speechGate
	ready  chan struct{} // closed when media flows

	// The received audio as one timeline of 60 ms frames that keeps up with the clock.
	// The network delivers it in other shapes: frames of 10 to 120 ms, several in one
	// packet, and few or none while the peer's side is silent.
	rxMu      sync.Mutex
	rxPend    []float32 // received, not yet a whole frame
	rxStart   time.Time // arrival of the first audio
	rxSamples uint64    // length of the timeline so far
	rxAudio   uint64    // of which received
	rxFill    uint64    // of which silence for gaps without packets
	rxSizes   map[int]int
}

// rxSlack is how far the received audio may fall behind the clock before the timeline
// is continued with silence: more than the network's jitter, less than what would
// delay noticing that the peer has stopped talking.
const rxSlack = 180 * time.Millisecond

func newDuplex(cfg callConfig, events *eventLog, out io.Writer) *duplex {
	d := &duplex{
		cfg:    cfg,
		events: events,
		live:   agentcaller.NewLiveSource(agentcaller.LiveConfig{Rate: cfg.inRate, MaxBuffered: cfg.maxBuffer}),
		gate:   newSpeechGate(cfg.endSilence, cfg.bargeAfter),
		ready:  make(chan struct{}),
	}
	if out != nil {
		d.out = newPCMPipe(out)
	}
	return d
}

// WriteFrame receives the peer's audio from the media loop: whatever one packet held.
func (d *duplex) WriteFrame(frame []float32) error {
	now := time.Now()
	d.rxMu.Lock()
	defer d.rxMu.Unlock()
	if d.rxStart.IsZero() {
		d.rxStart = now
		d.rxSizes = map[int]int{}
	}
	d.rxAudio += uint64(len(frame))
	if len(d.rxSizes) < 16 || d.rxSizes[len(frame)] > 0 {
		d.rxSizes[len(frame)]++
	}
	d.rxPend = append(d.rxPend, frame...)
	for len(d.rxPend) >= agentcaller.FrameSamples {
		d.take(d.rxPend[:agentcaller.FrameSamples])
		d.rxPend = d.rxPend[agentcaller.FrameSamples:]
	}
	return nil
}

// fillGap continues the timeline with silence where the stream has fallen behind the
// clock. Without it a pause is as long as the packets that happen to arrive in it, and
// may only be noticed once the peer talks again.
func (d *duplex) fillGap(now time.Time) {
	d.rxMu.Lock()
	defer d.rxMu.Unlock()
	if d.rxStart.IsZero() || now.Sub(d.rxStart) <= rxSlack {
		return
	}
	target := uint64((now.Sub(d.rxStart) - rxSlack) * agentcaller.SampleRate / time.Second)
	if d.rxSamples+uint64(len(d.rxPend))+agentcaller.FrameSamples > target {
		return
	}
	silence := make([]float32, agentcaller.FrameSamples)
	if n := len(d.rxPend); n > 0 { // finish the frame that was begun
		d.rxFill += uint64(agentcaller.FrameSamples - n)
		d.take(append(append(make([]float32, 0, agentcaller.FrameSamples), d.rxPend...), silence[n:]...))
		d.rxPend = d.rxPend[:0]
	}
	for d.rxSamples < target {
		d.rxFill += agentcaller.FrameSamples
		d.take(silence)
	}
}

// take puts one 60 ms frame on the timeline. Callers hold rxMu.
func (d *duplex) take(frame []float32) {
	d.rxSamples += uint64(len(frame))
	if d.out != nil {
		d.out.write(frame)
	}
	started, ended, at, length := d.gate.feed(frame)
	switch {
	case started:
		d.events.emit("speech_start", map[string]any{"at": at.Seconds()})
	case ended:
		d.events.emit("speech_end", map[string]any{"at": at.Seconds(), "duration": length.Seconds()})
	}
	if d.gate.sustained {
		// Only now is it an interruption: see speechGate.holdFrames.
		d.events.emit("speech_sustained", nil)
		if d.cfg.bargeIn {
			if cut := d.live.Flush(); cut > 0 {
				d.events.emit("flushed", map[string]any{"cause": "barge-in", "ms": cut.Milliseconds()})
			}
		}
	}
}

// Close implements agentcaller.AudioSink.
func (d *duplex) Close() error { return nil }

// pumpInput copies the input stream into the live source. A producer that is faster
// than real time (a file, speech synthesis) is held back once two seconds are queued,
// so memory stays bounded and a flush never throws away more than that.
func (d *duplex) pumpInput(ctx context.Context, in io.Reader) error {
	buf := make([]byte, 3200)
	for {
		for d.cfg.maxBuffer == 0 && d.live.Buffered() > 2*time.Second {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
		n, err := in.Read(buf)
		if n > 0 {
			d.live.WritePCM16(buf[:n])
		}
		if err != nil {
			d.live.CloseWrite()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// command executes one control line and reports whether the call should end.
func (d *duplex) command(line string, call duplexCall) (hangup bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	cmd := line
	if strings.HasPrefix(line, "{") {
		var msg struct {
			Cmd string `json:"cmd"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			d.events.emit("control_error", map[string]any{"error": err.Error()})
			return false
		}
		cmd = msg.Cmd
	}
	switch strings.ToLower(cmd) {
	case "flush":
		d.events.emit("flushed", map[string]any{"cause": "control", "ms": d.live.Flush().Milliseconds()})
	case "stats":
		d.events.emit("stats", d.stats(call))
	case "hangup":
		return true
	default:
		d.events.emit("control_error", map[string]any{"error": "unknown command: " + cmd})
	}
	return false
}

func (d *duplex) stats(call duplexCall) map[string]any {
	tx, rx := d.live.Stats(), call.ReceiveStats()
	ms := func(samples uint64) uint64 { return samples * 1000 / agentcaller.SampleRate }
	out := map[string]any{
		"tx_written_ms": ms(tx.Written), "tx_played_ms": ms(tx.Played), "tx_dropped_ms": ms(tx.Dropped),
		"tx_flushed_ms": ms(tx.Flushed), "tx_underruns": tx.Underruns, "tx_queued_ms": d.live.Buffered().Milliseconds(),
		"rx_packets": rx.Received, "rx_lost": rx.Lost, "rx_reordered": rx.Reordered, "rx_late": rx.Late,
		"rx_duplicates": rx.Duplicates, "rx_filled_ms": ms(rx.ConcealedSamples),
		"rx_speech_frames": rx.ActiveFrames, "rx_low_rate_frames": rx.LowRate, "rx_unsupported_frames": rx.Unsupported,
	}
	if d.out != nil {
		out["out_dropped_frames"] = d.out.dropped.Load()
	}
	d.rxMu.Lock()
	out["rx_audio_ms"], out["rx_gap_fill_ms"] = ms(d.rxAudio), ms(d.rxFill)
	sizes := map[string]int{} // samples per delivered packet → how often
	for n, count := range d.rxSizes {
		sizes[strconv.Itoa(n)] = count
	}
	out["rx_packet_samples"] = sizes
	d.rxMu.Unlock()
	return out
}

// run drives the call until it ends and returns why. in may be nil (send silence);
// control may be nil.
func (d *duplex) run(ctx context.Context, call duplexCall, in io.Reader, control <-chan string) error {
	ended := make(chan string, 1)
	inputDone := make(chan error, 1)
	var readyOnce sync.Once
	src := &drained{AudioSource: d.live, done: make(chan struct{})}
	played := src.done

	call.ConcealLosses(d.cfg.conceal)
	call.Receive(d)
	call.OnStateChange(func(p agentcaller.CallPhase) { d.events.emit("state", map[string]any{"phase": p.String()}) })
	onReady := func() { readyOnce.Do(func() { close(d.ready) }) }
	onEnd := func(reason string) {
		select {
		case ended <- reason:
		default:
		}
	}
	call.OnReady(onReady)
	call.OnEnd(onEnd)
	switch call.State() {
	case agentcaller.CallPhaseActive:
		onReady()
	case agentcaller.CallPhaseEnded:
		onEnd("ended before start")
	}

	finish := func(reason string, err error) error {
		_ = call.Hangup()
		d.events.emit("stats", d.stats(call))
		d.events.emit("ended", map[string]any{"reason": reason})
		if d.out != nil {
			d.out.close()
		}
		return err
	}

	answerTimer, answerC := optionalTimer(d.cfg.answerTimeout)
	if answerTimer != nil {
		defer answerTimer.Stop()
	}
	select {
	case <-d.ready:
	case reason := <-ended:
		return finish(reason, fmt.Errorf("call ended before it was answered: %s", reason))
	case <-answerC:
		return finish("answer-timeout", fmt.Errorf("not answered within %s", d.cfg.answerTimeout))
	case <-ctx.Done():
		return finish("interrupted", ctx.Err())
	}
	d.events.emit("ready", nil)

	// Audio written before this point would be stale by the time the peer hears it, so
	// the input is only read once media flows.
	call.Play(src)
	inCtx, stopInput := context.WithCancel(ctx)
	defer stopInput()
	if in != nil {
		go func() { inputDone <- d.pumpInput(inCtx, in) }()
	} else {
		played = nil // nothing will ever be queued, so nothing ever finishes
	}

	durTimer, durC := optionalTimer(d.cfg.maxDuration)
	if durTimer != nil {
		defer durTimer.Stop()
	}
	gaps := time.NewTicker(60 * time.Millisecond)
	defer gaps.Stop()
	for {
		select {
		case now := <-gaps.C:
			d.fillGap(now)
		case reason := <-ended:
			return finish(reason, nil)
		case err := <-inputDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				d.events.emit("input_error", map[string]any{"error": err.Error()})
			}
			d.events.emit("input_eof", map[string]any{"queued_ms": d.live.Buffered().Milliseconds()})
			inputDone = nil
		case <-played:
			d.events.emit("played", nil)
			played = nil
			if d.cfg.hangupOnEOF {
				return finish("input finished", nil)
			}
		case line, ok := <-control:
			if !ok {
				control = nil
				continue
			}
			if d.command(line, call) {
				return finish("hangup command", nil)
			}
		case <-durC:
			return finish("max-duration", nil)
		case <-ctx.Done():
			return finish("interrupted", nil)
		}
	}
}

// inputReader resolves an -in / -video-in argument to a stream that is opened when it
// is first read, or nil if there is nothing to read.
func inputReader(path string) io.Reader {
	switch path {
	case "", "none":
		return nil
	case "-":
		if term.IsTerminal(int(os.Stdin.Fd())) {
			return nil // a keyboard is not an audio source
		}
		return os.Stdin
	}
	return &lazyReader{open: func() (io.ReadCloser, error) { return os.Open(path) }}
}

func openOutput(path string) (io.WriteCloser, error) {
	switch path {
	case "", "none":
		return nil, nil
	case "-":
		if term.IsTerminal(int(os.Stdout.Fd())) {
			return nil, nil // never dump binary audio into a terminal
		}
		return os.Stdout, nil
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
}

// controlLines reads commands from path and reopens it at end of file if it is a FIFO,
// so several short-lived writers (`echo flush > fifo`) can follow each other.
func controlLines(ctx context.Context, path string) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		for ctx.Err() == nil {
			f, err := os.Open(path)
			if err != nil {
				return
			}
			info, _ := f.Stat()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				select {
				case ch <- sc.Text():
				case <-ctx.Done():
					f.Close()
					return
				}
			}
			f.Close()
			if info == nil || info.Mode()&os.ModeNamedPipe == 0 {
				return
			}
		}
	}()
	return ch
}

// pumpVideo cuts an H.264 byte stream into pictures and sends them once media flows.
// With a frame rate the pictures are also paced to it, so a plain file plays in real
// time; without one they go out as they arrive (a live encoder paces itself).
func pumpVideo(ctx context.Context, send func(au []byte, n int) error, in io.Reader, fps float64, ready <-chan struct{}, events *eventLog) {
	select {
	case <-ready:
	case <-ctx.Done():
		return
	}
	var split rtp.AccessUnitSplitter
	buf := make([]byte, 32<<10)
	sent, refused := 0, 0
	start := time.Now()
	emit := func(au []byte) {
		n := sent + refused
		if fps > 0 {
			if wait := time.Until(start.Add(time.Duration(float64(n) / fps * float64(time.Second)))); wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return
				}
			}
		}
		if err := send(au, n); err != nil {
			refused++
			return
		}
		sent++
	}
	var readErr error
	for ctx.Err() == nil && readErr == nil {
		var n int
		n, readErr = in.Read(buf)
		for _, au := range split.Write(buf[:n]) {
			emit(au)
		}
	}
	if au := split.Flush(); au != nil && ctx.Err() == nil {
		emit(au)
	}
	fields := map[string]any{"pictures_sent": sent, "pictures_refused": refused}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		fields["error"] = readErr.Error()
	}
	events.emit("video_input_eof", fields)
}

func runCall(ctx context.Context, cfg callConfig, stderr io.Writer) error {
	events := &eventLog{w: stderr, start: time.Now()}
	if cfg.eventsPath != "" {
		f, err := os.OpenFile(cfg.eventsPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("open -events: %w", err)
		}
		defer f.Close()
		events.w = f
	}
	out, err := openOutput(cfg.outPath)
	if err != nil {
		return fmt.Errorf("open -out: %w", err)
	}
	in := inputReader(cfg.inPath)
	if in == nil && cfg.hangupOnEOF {
		return errors.New("-hangup-on-eof needs an -in stream, and stdin is a terminal")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var outW io.Writer
	if out != nil {
		outW = out
	}
	var control <-chan string
	if cfg.controlPath != "" {
		control = controlLines(ctx, cfg.controlPath)
	}
	return placeCall(ctx, cfg, events, stderr, func(call *agentcaller.Call) error {
		return newDuplex(cfg, events, outW).runWithVideo(ctx, call, in, control)
	})
}

// placeCall connects, places or answers the call described by cfg and hands it to run.
func placeCall(ctx context.Context, cfg callConfig, events *eventLog, stderr io.Writer, run func(*agentcaller.Call) error) error {
	log := zerolog.Ctx(ctx)
	session, err := openSession(ctx, cfg.storePath, *log)
	if err != nil {
		return err
	}
	defer session.Close()
	client := agentcaller.NewClient(session.client, agentcaller.WithLogger(*log))
	incoming := make(chan *agentcaller.Call, 4)
	if cfg.answer {
		client.OnIncomingCall(func(c *agentcaller.Call) {
			select {
			case incoming <- c:
			default:
			}
		})
	}
	if err := session.Connect(ctx, stderr, false); err != nil {
		return err
	}
	events.emit("connected", nil)

	var call *agentcaller.Call
	if cfg.answer {
		call, err = awaitCaller(ctx, client, cfg, incoming, events)
	} else {
		announceCall(ctx, cfg, events, client.SendText, announceLead)
		events.emit("calling", map[string]any{"target": cfg.target})
		call, err = client.Call(ctx, cfg.target)
	}
	if err != nil {
		return err
	}
	return run(call)
}

// runWithVideo is run plus the optional video streams of the call command. The peer's
// camera is accepted when there is somewhere to write it (-video-out); our own picture
// is offered as an upgrade of the audio call once media flows (-video-in) and goes out
// when the peer accepts.
func (d *duplex) runWithVideo(ctx context.Context, call *agentcaller.Call, in io.Reader, control <-chan string) error {
	cfg, events := d.cfg, d.events
	if cfg.videoOut != "" {
		rec, err := agentcaller.AnnexBRecorder(cfg.videoOut)
		if err != nil {
			_ = call.Hangup()
			return fmt.Errorf("open -video-out: %w", err)
		}
		defer rec.Close()
		call.ReceiveVideo(rec)
	}
	if cfg.videoOut != "" || cfg.videoIn != "" {
		call.OnVideoState(func(s agentcaller.VideoState) {
			events.emit("video_state", map[string]any{"active": s.Active, "upgrade": s.Upgrade, "orientation": s.Orientation, "state": s.Raw})
			if s.Upgrade && cfg.videoOut != "" {
				go func() { // not on the signalling goroutine: accepting sends stanzas
					if err := call.AcceptVideo(); err != nil {
						events.emit("video_error", map[string]any{"error": err.Error()})
					}
				}()
			}
		})
	}
	if v := inputReader(cfg.videoIn); v != nil {
		send := func(au []byte, n int) error {
			if cfg.videoFPS > 0 {
				return call.SendVideoAt(au, time.Duration(float64(n)/cfg.videoFPS*float64(time.Second)))
			}
			return call.SendVideo(au)
		}
		go func() {
			select {
			case <-d.ready:
			case <-ctx.Done():
				return
			}
			if !call.IsSendingVideo() {
				if err := call.StartVideo(); err != nil {
					events.emit("video_error", map[string]any{"error": err.Error()})
				}
			}
		}()
		go pumpVideo(ctx, send, v, cfg.videoFPS, d.ready, events)
	}
	return d.run(ctx, call, in, control)
}

const (
	answerCheckUsage = "with -answer: run this command (sh -c) while a call from <caller> rings; exit status 75 declines the call and the wait goes on, anything else answers it"
	// answerCheckDecline is EX_TEMPFAIL from sysexits.h: busy now, try again later.
	answerCheckDecline = 75
	// answerCheckLimit is well below the time a call rings before the caller's phone gives up.
	answerCheckLimit = 15 * time.Second
)

// answerVerdict runs the -answer-check command and reports whether the ringing call is
// to be answered. Only exit status 75 declines. A command that fails in any other way,
// or has no verdict within limit, answers and reports why: a broken check must not cost
// the caller the call.
func answerVerdict(ctx context.Context, command, callID string, limit time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(), "AGENTCALLER_CALL_ID="+callID)
	cmd.WaitDelay = 2 * time.Second
	var complaints bytes.Buffer
	cmd.Stderr = &complaints
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case ctx.Err() == context.DeadlineExceeded:
		return true, fmt.Errorf("no verdict within %s", limit)
	case errors.As(err, &exit) && exit.ExitCode() == answerCheckDecline:
		return false, nil
	default:
		return true, fmt.Errorf("%w: %s", err, lastLine(complaints.String()))
	}
}

// awaitCaller waits for an incoming call from the expected peer and answers it. Calls
// from anyone else are left alone: rejecting them would also silence the phone. With
// -answer-check the expected peer's call is declined when the command says so, and the
// wait goes on.
func awaitCaller(ctx context.Context, client *agentcaller.Client, cfg callConfig, incoming <-chan *agentcaller.Call, events *eventLog) (*agentcaller.Call, error) {
	want := ""
	if cfg.target != "any" {
		lid, err := client.ResolvePeer(ctx, cfg.target)
		if err != nil {
			return nil, fmt.Errorf("resolve caller: %w", err)
		}
		want = lid.User
	}
	events.emit("waiting", map[string]any{"caller": cfg.target})
	timer, timeout := optionalTimer(cfg.answerTimeout)
	if timer != nil {
		defer timer.Stop()
	}
	for {
		select {
		case c := <-incoming:
			if want != "" && c.Peer().User != want {
				events.emit("ignored_call", map[string]any{"call_id": c.ID()})
				continue
			}
			if cfg.answerCheck != "" {
				events.emit("ringing", map[string]any{"call_id": c.ID(), "video": c.IsVideo()})
				answer, err := answerVerdict(ctx, cfg.answerCheck, c.ID(), answerCheckLimit)
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if c.State() == agentcaller.CallPhaseEnded {
					// The caller gave up while the command was deciding.
					events.emit("missed", map[string]any{"call_id": c.ID()})
					continue
				}
				if !answer {
					note := map[string]any{"call_id": c.ID()}
					if err := c.Reject(); err != nil {
						note["error"] = err.Error()
					}
					events.emit("declined", note)
					continue
				}
				if err != nil {
					events.emit("answer_check_failed", map[string]any{"call_id": c.ID(), "error": err.Error()})
				}
			}
			events.emit("incoming", map[string]any{"call_id": c.ID(), "video": c.IsVideo()})
			if err := c.Answer(); err != nil {
				return nil, fmt.Errorf("answer: %w", err)
			}
			return c, nil
		case <-timeout:
			return nil, fmt.Errorf("no call from %s within %s", cfg.target, cfg.answerTimeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
