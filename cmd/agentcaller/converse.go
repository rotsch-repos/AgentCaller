package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rotsch-repos/agentcaller"
)

// The converse command is a spoken conversation on top of the duplex call: the peer's
// speech is cut at the pauses the speech gate reports, transcribed, answered by a chat
// model sentence by sentence, and each sentence is synthesised into the call while the
// next one is still being written. When the peer starts to talk, the agent stops.
//
// Three services do the work and all three are replaceable: a transcription endpoint
// (OpenAI-compatible /v1/audio/transcriptions), a chat endpoint (OpenAI-compatible
// /v1/chat/completions with streaming) and a speech command (text on stdin, raw PCM on
// stdout).

type converseConfig struct {
	call        callConfig
	llm         LLMConfig
	personaPath string
	greeting    string
	sttURL      string
	sttModel    string
	sttLanguage string
	ttsCmd      string
	noBargeIn   bool
	simulate    string // WAV file played as the peer instead of a real call
	record      string // with simulate: WAV of what the agent said
	recordHeard string // WAV of what the agent heard, as its timeline has it
}

// phoneRules is appended to every persona: the answer is heard, not read.
const phoneRules = "Du führst gerade ein Telefongespräch. Was du schreibst, wird vorgelesen. " +
	"Antworte in ein bis drei kurzen, natürlichen Sätzen in der Sprache deines Gegenübers. " +
	"Keine Listen, kein Markdown, keine Emojis, keine Regieanweisungen. " +
	"Das Gehörte stammt aus einer Spracherkennung und kann Hörfehler enthalten; frag nach, wenn etwas keinen Sinn ergibt."

const defaultPersona = "Du bist eine freundliche, aufmerksame Gesprächspartnerin."

// farewell matches a goodbye at the end of what the peer said; the agent answers once
// more and hangs up.
var farewell = regexp.MustCompile(`(?i)(tschüss|tschüs|tschau|ciao|auf wiederhören|auf wiedersehen|bis bald|bis später|bis morgen|mach's gut|machs gut|gute nacht|bye|goodbye)[\s.!,]*$`)

func parseConverseArgs(args []string, stderr io.Writer) (converseConfig, error) {
	cfg := converseConfig{llm: defaultLLMConfig()}
	defaultStore, err := defaultStorePath()
	if err != nil {
		return cfg, err
	}
	flags := flag.NewFlagSet("converse", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.BoolVar(&cfg.call.answer, "answer", false, "wait for an incoming call from <caller> and answer it, instead of calling")
	flags.StringVar(&cfg.call.storePath, "store", defaultStore, "linked-device SQLite store")
	flags.StringVar(&cfg.call.eventsPath, "events", "", "write JSON-line events here (default stderr)")
	flags.DurationVar(&cfg.call.endSilence, "end-silence", 700*time.Millisecond, "pause after which the peer's turn counts as finished")
	flags.DurationVar(&cfg.call.answerTimeout, "answer-timeout", defaultAnswerTimeout, "maximum wait for the call to be answered (or to arrive, with -answer); 0 disables")
	flags.DurationVar(&cfg.call.maxDuration, "max-duration", 15*time.Minute, "hang up after this long; 0 disables")
	flags.StringVar(&cfg.llm.Endpoint, "llm-url", cfg.llm.Endpoint, "base URL of an OpenAI-compatible chat endpoint")
	flags.StringVar(&cfg.llm.ModelID, "llm-model", cfg.llm.ModelID, "chat model")
	flags.IntVar(&cfg.llm.MaxTokens, "max-tokens", 220, "upper bound for one answer")
	flags.StringVar(&cfg.personaPath, "persona", "", "text file with the agent's character (system prompt); the telephone rules are appended")
	flags.StringVar(&cfg.greeting, "greeting", "", "what the agent says when the call connects (default: the peer speaks first)")
	flags.StringVar(&cfg.sttURL, "stt-url", "http://127.0.0.1:8000", "base URL of an OpenAI-compatible transcription endpoint")
	flags.StringVar(&cfg.sttModel, "stt-model", "large-v3", "transcription model")
	flags.StringVar(&cfg.sttLanguage, "stt-language", "de", "language hint for the transcription")
	flags.StringVar(&cfg.ttsCmd, "tts-cmd", "", "speech command, run by sh: text on stdin, raw s16le mono PCM on stdout (required)")
	flags.IntVar(&cfg.call.inRate, "tts-rate", agentcaller.SampleRate, "sample rate of the speech command's output in Hz")
	flags.BoolVar(&cfg.noBargeIn, "no-barge-in", false, "let the agent finish its answer even when the peer starts to talk")
	flags.StringVar(&cfg.simulate, "simulate", "", "no call: play this WAV as the peer (it waits for the agent at each pause) and run the real chain")
	flags.StringVar(&cfg.record, "record", "", "with -simulate: write what the agent said to this WAV")
	flags.StringVar(&cfg.recordHeard, "record-heard", "", "write what the agent heard to this WAV when the call ends (a recording of the peer: for finding faults)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: agentcaller converse [flags] <target>")
		fmt.Fprintln(stderr, "       agentcaller converse -answer [flags] <caller|any>")
		fmt.Fprintln(stderr, "       agentcaller converse -simulate peer.wav [flags]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return cfg, errors.Join(errUsage, err)
	}
	switch {
	case cfg.simulate != "" && flags.NArg() != 0, cfg.simulate == "" && flags.NArg() != 1:
		flags.Usage()
		return cfg, errUsage
	case cfg.ttsCmd == "":
		return cfg, errors.New("-tts-cmd is required")
	case cfg.call.answerTimeout < 0 || cfg.call.maxDuration < 0 || cfg.call.endSilence <= 0:
		return cfg, errors.New("durations must be non-negative (-end-silence positive)")
	case cfg.call.inRate < 4000 || cfg.call.inRate > 192000:
		return cfg, errors.New("-tts-rate must be between 4000 and 192000")
	case cfg.record != "" && cfg.simulate == "":
		return cfg, errors.New("-record needs -simulate")
	}
	if cfg.simulate == "" {
		cfg.call.target = flags.Arg(0)
	}
	cfg.call.bargeIn = !cfg.noBargeIn
	cfg.llm.Prompt = defaultPersona
	if cfg.personaPath != "" {
		raw, err := os.ReadFile(cfg.personaPath)
		if err != nil {
			return cfg, fmt.Errorf("read -persona: %w", err)
		}
		cfg.llm.Prompt = strings.TrimSpace(string(raw))
	}
	cfg.llm.Prompt += "\n\n" + phoneRules
	return cfg, nil
}

func runConverse(ctx context.Context, cfg converseConfig, stderr io.Writer) error {
	var eventsOut io.Writer = stderr
	if cfg.call.eventsPath != "" {
		f, err := os.OpenFile(cfg.call.eventsPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("open -events: %w", err)
		}
		defer f.Close()
		eventsOut = f
	}
	chain := voiceChain{
		listen: func(ctx context.Context, pcm []byte) (string, error) {
			return transcribeHTTP(ctx, cfg.sttURL, cfg.sttModel, cfg.sttLanguage, pcm)
		},
		think: func(ctx context.Context, msgs []chatMessage, sentence func(string)) error {
			return streamReply(ctx, cfg.llm, msgs, sentence)
		},
		speak: commandSpeaker(cfg.ttsCmd, cfg.call.inRate),
	}
	c := newConversation(cfg, chain, eventsOut)
	if cfg.recordHeard != "" {
		c.tape = &bytes.Buffer{}
		defer func() {
			c.heard.mu.Lock() // the tape is written under the log's lock
			wav := wavBytes(c.tape.Bytes())
			c.heard.mu.Unlock()
			if err := os.WriteFile(cfg.recordHeard, wav, 0o600); err != nil {
				fmt.Fprintln(stderr, "write -record-heard:", err)
			}
		}()
		c.heard.tape = c.tape
	}
	if cfg.simulate != "" {
		peer := readWAV16(cfg.simulate)
		if len(peer) == 0 {
			return fmt.Errorf("-simulate: no audio in %s", cfg.simulate)
		}
		sim := newSimCall(peer, cfg.greeting != "")
		err := c.run(ctx, sim)
		if cfg.record != "" {
			if werr := writeWAV16(cfg.record, sim.recording()); werr != nil {
				return fmt.Errorf("write -record: %w", werr)
			}
		}
		return err
	}
	return placeCall(ctx, cfg.call, c.events, stderr, func(call *agentcaller.Call) error { return c.run(ctx, call) })
}

// voiceChain is what a conversation needs from the outside world.
type voiceChain struct {
	// listen turns 16 kHz s16le mono speech into text.
	listen func(ctx context.Context, pcm []byte) (string, error)
	// think answers the conversation so far and hands over one sentence at a time.
	think func(ctx context.Context, msgs []chatMessage, sentence func(string)) error
	// speak writes text as raw PCM (at the call's input rate) to w until done or cancelled.
	speak func(ctx context.Context, text string, w io.Writer) error
}

// callEvent is what the conversation needs from a duplex event line.
type callEvent struct {
	Event    string  `json:"event"`
	At       float64 `json:"at"`
	Duration float64 `json:"duration"`
}

// eventTap passes event lines on and hands the conversation a copy. It is written from
// the media loop, so it never waits for the conversation.
type eventTap struct {
	next io.Writer
	ch   chan callEvent
}

func (t *eventTap) Write(line []byte) (int, error) {
	if t.next != nil {
		_, _ = t.next.Write(line)
	}
	var e callEvent
	if json.Unmarshal(line, &e) == nil {
		switch e.Event {
		case "ready", "speech_start", "speech_end":
			select {
			case t.ch <- e:
			default:
			}
		}
	}
	return len(line), nil
}

// audioLog keeps the most recent received audio addressable by its position on the
// call timeline, which is how speech_end names a stretch of speech.
type audioLog struct {
	mu   sync.Mutex
	base int // bytes dropped from the front
	buf  []byte
	tape *bytes.Buffer // everything, if the call is recorded
}

const (
	pcmBytesPerSecond = agentcaller.SampleRate * 2
	audioLogKeep      = 90 * pcmBytesPerSecond
)

func (a *audioLog) Write(p []byte) (int, error) {
	a.mu.Lock()
	if a.tape != nil {
		a.tape.Write(p)
	}
	a.buf = append(a.buf, p...)
	if len(a.buf) > 2*audioLogKeep {
		drop := len(a.buf) - audioLogKeep
		a.buf = append(a.buf[:0], a.buf[drop:]...)
		a.base += drop
	}
	a.mu.Unlock()
	return len(p), nil
}

func (a *audioLog) end() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Duration(a.base+len(a.buf)) * time.Second / pcmBytesPerSecond
}

// slice returns the audio between two points of the timeline, as far as it is there.
func (a *audioLog) slice(from, to time.Duration) []byte {
	pos := func(d time.Duration) int { return int(d*pcmBytesPerSecond/time.Second) &^ 1 }
	a.mu.Lock()
	defer a.mu.Unlock()
	lo, hi := pos(from)-a.base, pos(to)-a.base
	lo, hi = max(lo, 0), min(hi, len(a.buf))
	if lo >= hi {
		return nil
	}
	return append([]byte(nil), a.buf[lo:hi]...)
}

// pacedWriter feeds the call no more than a short lead ahead of real time. What is not
// written yet cannot be heard after an interruption, and a sentence that is written is
// within that lead of having been heard.
type pacedWriter struct {
	mu    sync.Mutex
	w     io.Writer
	rate  int
	lead  time.Duration
	until time.Time // when what was written so far will have been played
}

func (p *pacedWriter) write(ctx context.Context, b []byte) error {
	chunk := (p.rate * 2 * 60 / 1000) &^ 1
	for len(b) > 0 {
		n := min(chunk, len(b))
		p.mu.Lock()
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return err
		}
		// Compared, not subtracted: for the zero time the distance saturates.
		if ahead := time.Until(p.until); ahead > p.lead {
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(min(ahead-p.lead, 20*time.Millisecond)):
			}
			continue
		}
		_, err := p.w.Write(b[:n])
		if now := time.Now(); p.until.Before(now) {
			p.until = now
		}
		p.until = p.until.Add(time.Duration(n/2) * time.Second / time.Duration(p.rate))
		p.mu.Unlock()
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// cut forgets what was written; once it returns, no write of a cancelled turn follows.
func (p *pacedWriter) cut() {
	p.mu.Lock()
	p.until = time.Now()
	p.mu.Unlock()
}

func (p *pacedWriter) remaining() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return max(time.Until(p.until), 0)
}

type ctxWriter struct {
	ctx   context.Context
	p     *pacedWriter
	first *time.Time
}

func (w ctxWriter) Write(b []byte) (int, error) {
	if w.first != nil && w.first.IsZero() && len(b) > 0 {
		*w.first = time.Now()
	}
	if err := w.p.write(w.ctx, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

type segment struct {
	greeting bool
	at, dur  time.Duration
	ended    time.Time // when the pause was reported: the start of the answer delay
}

type conversation struct {
	cfg     converseConfig
	chain   voiceChain
	events  *eventLog
	tap     *eventTap
	heard   *audioLog
	tape    *bytes.Buffer
	say     *pacedWriter
	sayPipe *io.PipeReader
	control chan string
	turns   chan segment

	mu         sync.Mutex
	peerTalks  bool
	cancelTurn context.CancelFunc
	history    []chatMessage
	pending    []string // heard but not answered yet
}

func newConversation(cfg converseConfig, chain voiceChain, eventsOut io.Writer) *conversation {
	pr, pw := io.Pipe()
	tap := &eventTap{next: eventsOut, ch: make(chan callEvent, 64)}
	return &conversation{
		cfg:     cfg,
		chain:   chain,
		events:  &eventLog{w: tap, start: time.Now()},
		tap:     tap,
		heard:   &audioLog{},
		say:     &pacedWriter{w: pw, rate: cfg.call.inRate, lead: 600 * time.Millisecond},
		sayPipe: pr,
		control: make(chan string, 8),
		turns:   make(chan segment, 16),
	}
}

// run holds the conversation on an established (or ringing) call until it ends.
func (c *conversation) run(ctx context.Context, call duplexCall) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.sayPipe.CloseWithError(io.ErrClosedPipe)
	d := newDuplex(c.cfg.call, c.events, c.heard)
	go c.follow(ctx)
	go c.work(ctx)
	return d.run(ctx, call, c.sayPipe, c.control)
}

func (c *conversation) command(ctx context.Context, line string) {
	select {
	case c.control <- line:
	case <-ctx.Done():
	}
}

// follow reacts to the call's events. It must stay quick: answering happens in work.
func (c *conversation) follow(ctx context.Context) {
	queue := func(s segment) {
		select {
		case c.turns <- s:
		default: // sixteen unanswered turns: the oldest ones no longer matter
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-c.tap.ch:
			switch e.Event {
			case "ready":
				if c.cfg.greeting != "" {
					queue(segment{greeting: true})
				}
			case "speech_start":
				c.mu.Lock()
				c.peerTalks = true
				stop := c.cancelTurn
				c.mu.Unlock()
				if c.cfg.noBargeIn {
					continue
				}
				if stop != nil {
					stop()
				}
				queued := c.say.remaining() > 0
				c.say.cut()
				if stop != nil || queued {
					// The duplex already emptied its queue on this event; this clears
					// what the stopped turn wrote in between.
					c.command(ctx, "flush")
				}
			case "speech_end":
				c.mu.Lock()
				c.peerTalks = false
				c.mu.Unlock()
				queue(segment{
					at:    time.Duration(e.At * float64(time.Second)),
					dur:   time.Duration(e.Duration * float64(time.Second)),
					ended: time.Now(),
				})
			}
		}
	}
}

func (c *conversation) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-c.turns:
			if s.greeting {
				c.greet(ctx)
			} else {
				c.answer(ctx, s)
			}
		}
	}
}

// beginTurn opens a stretch in which the agent speaks. It refuses while the peer is
// talking, unless forced; the peer starting to talk ends it.
func (c *conversation) beginTurn(ctx context.Context, force bool) (turn context.Context, end func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.peerTalks && !force && !c.cfg.noBargeIn {
		return nil, nil, false
	}
	turn, cancel := context.WithCancel(ctx)
	c.cancelTurn = cancel
	return turn, func() {
		c.mu.Lock()
		c.cancelTurn = nil
		c.mu.Unlock()
		cancel()
	}, true
}

func (c *conversation) greet(ctx context.Context) {
	turn, end, _ := c.beginTurn(ctx, true)
	defer end()
	err := c.chain.speak(turn, c.cfg.greeting, ctxWriter{ctx: turn, p: c.say})
	if err != nil {
		if turn.Err() == nil {
			c.events.emit("reply_error", map[string]any{"error": err.Error()})
		}
		return
	}
	c.remember("", c.cfg.greeting)
	c.events.emit("reply", map[string]any{"text": c.cfg.greeting, "greeting": true})
}

func (c *conversation) remember(user, assistant string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if user != "" {
		c.history = append(c.history, chatMessage{Role: "user", Content: user})
	}
	c.history = append(c.history, chatMessage{Role: "assistant", Content: assistant})
	if extra := len(c.history) - 40; extra > 0 {
		c.history = append(c.history[:0], c.history[extra:]...)
	}
}

func (c *conversation) answer(ctx context.Context, s segment) {
	// The received audio reaches the log through a queue; give the tail a moment.
	const before, after = 240 * time.Millisecond, 300 * time.Millisecond
	want := s.at + s.dur + min(after, c.cfg.call.endSilence)
	for deadline := time.Now().Add(500 * time.Millisecond); c.heard.end() < want && time.Now().Before(deadline) && ctx.Err() == nil; {
		time.Sleep(5 * time.Millisecond)
	}
	pcm := c.heard.slice(s.at-before, want)
	if len(pcm) == 0 {
		return
	}
	sttStart := time.Now()
	text, err := c.chain.listen(ctx, pcm)
	if err != nil {
		if ctx.Err() == nil {
			c.events.emit("stt_error", map[string]any{"error": err.Error()})
		}
		return
	}
	text = strings.TrimSpace(text)
	if text == "" || isTranscriptionNoise(text) {
		c.events.emit("heard", map[string]any{"text": "", "ignored": text, "at": s.at.Seconds()})
		c.mu.Lock()
		owed := len(c.pending) > 0
		c.mu.Unlock()
		if !owed {
			return
		}
		text = "" // a noise cut off the answer to what was said before: it is still owed
	} else {
		c.events.emit("heard", map[string]any{"text": text, "at": s.at.Seconds(), "stt_ms": time.Since(sttStart).Milliseconds()})
	}

	c.mu.Lock()
	if text != "" {
		c.pending = append(c.pending, text)
	}
	user := strings.Join(c.pending, " ")
	msgs := append([]chatMessage{{Role: "system", Content: c.cfg.llm.Prompt}}, c.history...)
	c.mu.Unlock()
	msgs = append(msgs, chatMessage{Role: "user", Content: user})

	turn, end, ok := c.beginTurn(ctx, false)
	if !ok {
		return // the peer is talking again: answer everything at the next pause
	}
	defer end()
	spoken, first, err := c.respond(turn, msgs)
	interrupted := turn.Err() != nil && ctx.Err() == nil
	if err != nil && !interrupted && ctx.Err() == nil {
		c.events.emit("reply_error", map[string]any{"error": err.Error()})
	}
	if len(spoken) == 0 {
		if !interrupted { // nothing came back; do not carry the question forever
			c.mu.Lock()
			c.pending = nil
			c.mu.Unlock()
		}
		return
	}
	reply := strings.Join(spoken, " ")
	c.mu.Lock()
	c.pending = nil
	c.mu.Unlock()
	c.remember(user, reply)
	fields := map[string]any{"text": reply, "interrupted": interrupted}
	if !first.IsZero() {
		fields["first_audio_ms"] = first.Sub(s.ended).Milliseconds()
	}
	c.events.emit("reply", fields)

	if !interrupted && farewell.MatchString(user) {
		select {
		case <-turn.Done(): // the peer had more to say
		case <-time.After(c.say.remaining() + 500*time.Millisecond):
			c.command(ctx, "hangup")
		}
	}
}

// respond speaks the answer to msgs. spoken holds the sentences that went out in full.
func (c *conversation) respond(ctx context.Context, msgs []chatMessage) (spoken []string, firstAudio time.Time, err error) {
	sentences := make(chan string, 32)
	thought := make(chan error, 1)
	go func() {
		thought <- c.chain.think(ctx, msgs, func(s string) {
			select {
			case sentences <- s:
			case <-ctx.Done():
			}
		})
		close(sentences)
	}()
	for s := range sentences {
		if err := c.chain.speak(ctx, s, ctxWriter{ctx: ctx, p: c.say, first: &firstAudio}); err != nil {
			return spoken, firstAudio, err
		}
		spoken = append(spoken, s)
	}
	return spoken, firstAudio, <-thought
}

// isTranscriptionNoise recognises what Whisper invents for breath, clicks and silence.
func isTranscriptionNoise(text string) bool {
	lower := strings.ToLower(text)
	for _, s := range []string{"untertitel", "amara.org", "copyright wdr", "swr 20", "vielen dank fürs zuschauen"} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return strings.Trim(lower, " .!?…-–♪") == ""
}

// simCall stands in for a WhatsApp call: it plays a recording as the peer in real time
// and records what the agent sends. Like a polite caller it waits at each of its pauses
// until the agent has answered.
type simCall struct {
	peer  []float32
	tick  time.Duration // one 60 ms frame takes this long (shorter in tests)
	await bool          // the agent owes an answer (or its greeting)

	mu      sync.Mutex
	onEnd   func(string)
	sink    agentcaller.AudioSink
	said    []float32
	stop    chan struct{}
	stopped sync.Once
}

func newSimCall(peer []float32, greeted bool) *simCall {
	return &simCall{peer: peer, tick: 60 * time.Millisecond, await: greeted, stop: make(chan struct{})}
}

func (c *simCall) OnReady(func())                            {}
func (c *simCall) OnEnd(fn func(string))                     { c.mu.Lock(); c.onEnd = fn; c.mu.Unlock() }
func (c *simCall) OnStateChange(func(agentcaller.CallPhase)) {}
func (c *simCall) State() agentcaller.CallPhase              { return agentcaller.CallPhaseActive }
func (c *simCall) Receive(s agentcaller.AudioSink)           { c.mu.Lock(); c.sink = s; c.mu.Unlock() }
func (c *simCall) ConcealLosses(bool)                        {}
func (c *simCall) ReceiveStats() agentcaller.ReceiveStats    { return agentcaller.ReceiveStats{} }
func (c *simCall) Hangup() error                             { c.stopped.Do(func() { close(c.stop) }); return nil }
func (c *simCall) recording() []float32                      { c.mu.Lock(); defer c.mu.Unlock(); return c.said }
func (c *simCall) Play(src agentcaller.AudioSource) *agentcaller.Player {
	go c.loop(src)
	return agentcaller.NewPlayer()
}

func (c *simCall) loop(src agentcaller.AudioSource) {
	const frame = agentcaller.FrameSamples
	settled, patience := 25*c.tick, 500*c.tick // 1.5 s without agent audio; 30 s without any answer
	silence := make([]float32, frame)
	ticker := time.NewTicker(c.tick)
	defer ticker.Stop()
	c.mu.Lock()
	sink, onEnd := c.sink, c.onEnd
	c.mu.Unlock()
	pos, quiet := 0, 0
	answered := false
	var lastAgent time.Time
	waiting := time.Now()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}
		out, _ := src.ReadFrame()
		if out == nil {
			out = silence
		} else if frameRMS(out) > 0 {
			lastAgent = time.Now()
			answered = c.await
		}
		c.mu.Lock()
		c.said = append(c.said, out...)
		c.mu.Unlock()
		if c.await && (answered && time.Since(lastAgent) > settled || time.Since(waiting) > patience) {
			c.await, answered = false, false
		}

		in := silence
		if pos < len(c.peer) {
			next := c.peer[pos:min(pos+frame, len(c.peer))]
			if len(next) < frame {
				next = append(append(make([]float32, 0, frame), next...), silence[len(next):]...)
			}
			pause := frameRMS(next) < 0.002
			if hold := c.await && (pos == 0 || pause && quiet >= 15); !hold {
				in, pos = next, pos+frame
				if pause {
					quiet++
				} else {
					if !c.await {
						waiting = time.Now()
					}
					quiet, c.await, answered = 0, true, false
				}
			}
		} else if !c.await {
			if onEnd != nil {
				onEnd("simulation finished")
			}
			return
		}
		if sink != nil {
			_ = sink.WriteFrame(in)
		}
	}
}
