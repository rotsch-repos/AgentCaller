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
	"unicode"

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
	greetAfter  time.Duration
	sttURL      string
	sttModel    string
	sttLanguage string
	ttsCmd      string
	noBargeIn   bool
	hangup      string // when the agent ends the call: goodbye, asked, never
	simulate    string // WAV file played as the peer instead of a real call
	record      string // with simulate: WAV of what the agent said
	recordHeard string // WAV of what the agent heard, as its timeline has it
	toolsPath   string
	tools       []callTool
	toolsWait   string // said while the first lookup of an answer runs
	voicesPath  string
	voices      []callVoice // several characters on one call; empty: the one of -persona and -tts-cmd
	voiceName   string      // which of them starts
	guardPath   string
	guard       *speakerGuard    // who is speaking decides what the agent knows; nil: everybody is trusted
	clock       func() time.Time // tells the model date and time with every question; nil: it is not told
}

// phoneRules is appended to every persona: the answer is heard, not read.
const phoneRules = "Du führst gerade ein Telefongespräch. Was du schreibst, wird vorgelesen. " +
	"Sprich wie in einem guten Gespräch unter vier Augen, in der Sprache deines Gegenübers: in ganzen, vollständigen Sätzen, " +
	"meist zwei bis vier. Will dein Gegenüber etwas erklärt, beschrieben oder erzählt haben, nimm dir Raum, fünf bis acht Sätze, " +
	"und gib dann das Wort zurück: Es ist ein Gespräch, kein Vortrag. " +
	"Antworte nie mit einem einzelnen Wort oder einem halben Satz, ausser auf einen Dank oder einen Abschied. " +
	"Trag das Gespräch mit: Geh auf das Gesagte ein, bring eigene Gedanken, frag nach, wenn dich etwas interessiert. " +
	"Keine Listen, kein Markdown, keine Emojis, keine Regieanweisungen. Zahlen schreibst du als Ziffern, nicht als Zahlwörter, " +
	"die Stimme liest sie richtig vor; Messwerte rundest du so, wie man sie im Gespräch sagt. " +
	"Das Gehörte stammt aus einer Spracherkennung und kann Hörfehler enthalten; ergibt etwas keinen Sinn, frag nach."

// lookupRules follows phoneRules when the agent has tools, noLookupRules when it has none.
const lookupRules = "Du kannst während des Gesprächs nachschauen; dafür hast du Werkzeuge. Geht es um eine Tatsache, die sich ändern kann " +
	"oder die du nicht sicher weisst, etwa Wetter, Aktuelles, wer gerade ein Amt hat, Resultate, Preise oder Messwerte, " +
	"rufst du sofort das passende Werkzeug auf, statt zu raten oder abzulehnen. Kündige das Nachschauen nicht an, " +
	"und behaupte nie, nachgeschaut zu haben, ohne ein Werkzeug aufgerufen zu haben. " +
	"Für Plaudern, Meinungen und gesichertes Allgemeinwissen brauchst du kein Werkzeug. " +
	"Vom Ergebnis erzählst du in eigenen Worten nur das, wonach gefragt war. " +
	"Was sich auch so nicht klären lässt, erfindest du nicht: Dann sagst du, dass du es nicht weisst."

const noLookupRules = "Nachschauen kannst du in diesem Gespräch nichts. Erfinde keine Tatsachen und behaupte nie, " +
	"etwas nachgesehen oder erledigt zu haben; weisst du etwas nicht, sagst du das schlicht."

const defaultPersona = "Du bist eine freundliche, aufmerksame Gesprächspartnerin."

// farewell matches a goodbye at the end of what the peer said; the agent answers once
// more and hangs up.
// hangupAsked is the narrower rule of -hangup asked: only a request to hang up, not a
// goodbye. A noise on a real call once came out of the recogniser as "Bye." and ended it.
var hangupAsked = regexp.MustCompile(`(?i)(aufhängen|auflegen|(häng|leg)(e|en|st|t)? (\S+ ){0,3}auf)[\s.!,]*$`)

// hangsUp reports whether what the peer said ends the call from the agent's side.
func (cfg converseConfig) hangsUp(said string) bool {
	switch cfg.hangup {
	case "never":
		return false
	case "asked":
		return hangupAsked.MatchString(said)
	}
	return farewell.MatchString(said)
}

var farewell = regexp.MustCompile(`(?i)(tschüss|tschüs|tschau|ciao|auf wiederhören|auf wiedersehen|bis bald|bis später|bis morgen|mach's gut|machs gut|gute nacht|bye|goodbye|aufhängen|auflegen|(häng|leg)(e|en|st|t)? (\S+ ){0,3}auf)[\s.!,]*$`)

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
	flags.StringVar(&cfg.call.announce, "announce", "", "send the target this text message two seconds before calling (the ringing screen only shows the account)")
	flags.DurationVar(&cfg.call.endSilence, "end-silence", 700*time.Millisecond, "pause after which the peer's turn counts as finished")
	flags.DurationVar(&cfg.call.answerTimeout, "answer-timeout", defaultAnswerTimeout, "maximum wait for the call to be answered (or to arrive, with -answer); 0 disables")
	flags.DurationVar(&cfg.call.maxDuration, "max-duration", 15*time.Minute, "hang up after this long; 0 disables")
	flags.StringVar(&cfg.llm.Endpoint, "llm-url", cfg.llm.Endpoint, "base URL of an OpenAI-compatible chat endpoint")
	flags.StringVar(&cfg.llm.ModelID, "llm-model", cfg.llm.ModelID, "chat model")
	flags.IntVar(&cfg.llm.MaxTokens, "max-tokens", 320, "upper bound for one answer")
	flags.StringVar(&cfg.toolsPath, "tools", "", "JSON file with what the agent may look up during the call: [{name, description, parameters, command, timeout_seconds}]; the command gets the arguments as JSON on stdin and prints its findings")
	flags.StringVar(&cfg.toolsWait, "tools-wait", "Einen Moment, ich schaue nach.", "what the agent says while a lookup runs (empty: nothing)")
	flags.StringVar(&cfg.voicesPath, "voices", "", "JSON file with several voices that can hold this call, instead of -persona, -tts-cmd and -tts-rate: [{name, about, persona, guest_persona, tts_cmd, tts_rate, tools_wait}]; the one that speaks hands over when the peer asks for another")
	flags.StringVar(&cfg.voiceName, "voice", "", "with -voices: the voice that starts (default: the first)")
	flags.StringVar(&cfg.guardPath, "speakers", "", "JSON file {command, guest_persona, password}: the command tells who is speaking (it gets {\"wav\": path, \"seconds\": n} per line and answers {speaker, trusted, score, note}); until it trusts the voice the agent has the guest persona, no history with trusted speakers and only the tools marked \"guests\": true; the password, spoken, unlocks the call")
	flags.StringVar(&cfg.personaPath, "persona", "", "text file with the agent's character (system prompt); the telephone rules are appended")
	flags.StringVar(&cfg.greeting, "greeting", "", "what the agent says when the call connects (default: the peer speaks first)")
	flags.DurationVar(&cfg.greetAfter, "greeting-delay", 0, "how long the agent waits after the call connects before it greets: the phone has to reach the ear")
	flags.StringVar(&cfg.sttURL, "stt-url", "http://127.0.0.1:8000", "base URL of an OpenAI-compatible transcription endpoint")
	flags.StringVar(&cfg.sttModel, "stt-model", "large-v3", "transcription model")
	flags.StringVar(&cfg.sttLanguage, "stt-language", "de", "language hint for the transcription")
	flags.StringVar(&cfg.ttsCmd, "tts-cmd", "", "speech command, run by sh: text on stdin, raw s16le mono PCM on stdout (required)")
	flags.IntVar(&cfg.call.inRate, "tts-rate", agentcaller.SampleRate, "sample rate of the speech command's output in Hz")
	flags.StringVar(&cfg.hangup, "hangup", "goodbye", "when the agent ends the call after its answer: goodbye (the peer said goodbye), asked (only when the peer asks it to hang up), never")
	flags.DurationVar(&cfg.call.bargeAfter, "barge-in-after", defaultBargeAfter, "how long the peer must have been speaking before the agent stops for it: a cough, a short \"ja\" or an echo of the agent's own voice is shorter")
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
	case cfg.ttsCmd == "" && cfg.voicesPath == "":
		return cfg, errors.New("-tts-cmd is required")
	case cfg.voicesPath != "" && (cfg.ttsCmd != "" || cfg.personaPath != ""):
		return cfg, errors.New("-voices replaces -persona and -tts-cmd: give one or the other")
	case cfg.voiceName != "" && cfg.voicesPath == "":
		return cfg, errors.New("-voice needs -voices")
	case cfg.hangup != "goodbye" && cfg.hangup != "asked" && cfg.hangup != "never":
		return cfg, errors.New("-hangup must be goodbye, asked or never")
	case cfg.call.answerTimeout < 0 || cfg.call.maxDuration < 0 || cfg.call.endSilence <= 0:
		return cfg, errors.New("durations must be non-negative (-end-silence positive)")
	case cfg.call.inRate < 4000 || cfg.call.inRate > 192000:
		return cfg, errors.New("-tts-rate must be between 4000 and 192000")
	case cfg.call.announce != "" && (cfg.call.answer || cfg.simulate != ""):
		return cfg, errors.New("-announce is for calls that are placed, not with -answer or -simulate")
	case cfg.record != "" && cfg.simulate == "":
		return cfg, errors.New("-record needs -simulate")
	}
	if cfg.simulate == "" {
		cfg.call.target = flags.Arg(0)
	}
	cfg.call.bargeIn = !cfg.noBargeIn
	persona := defaultPersona
	if cfg.personaPath != "" {
		raw, err := os.ReadFile(cfg.personaPath)
		if err != nil {
			return cfg, fmt.Errorf("read -persona: %w", err)
		}
		persona = strings.TrimSpace(string(raw))
	}
	if cfg.toolsPath != "" {
		tools, err := loadTools(cfg.toolsPath)
		if err != nil {
			return cfg, fmt.Errorf("read -tools: %w", err)
		}
		cfg.tools = tools
	}
	if cfg.voicesPath != "" {
		voices, err := loadVoices(cfg.voicesPath)
		if err != nil {
			return cfg, fmt.Errorf("read -voices: %w", err)
		}
		start := 0
		if cfg.voiceName != "" {
			if start = findVoice(voices, cfg.voiceName); start < 0 {
				return cfg, fmt.Errorf("-voice: there is no voice %q in %s", cfg.voiceName, cfg.voicesPath)
			}
		}
		voices[0], voices[start] = voices[start], voices[0] // the one that starts comes first
		cfg.voices = voices
		cfg.call.inRate = voicesRate(voices)
		if len(voices) > 1 {
			if findTool(cfg.tools, handoverTool) != nil {
				return cfg, fmt.Errorf("-tools: the name %q is taken by the handover between voices", handoverTool)
			}
			cfg.tools = append(cfg.tools, handoverSpec(voices))
		}
		persona = voices[0].prompt
	}
	if cfg.guardPath != "" {
		guard, err := loadGuard(cfg.guardPath)
		if err != nil {
			return cfg, fmt.Errorf("read -speakers: %w", err)
		}
		cfg.guard = guard
	}
	cfg.clock = time.Now
	cfg.llm.Prompt = phonePrompt(persona, len(cfg.tools) > 0)
	for i := range cfg.voices {
		v := &cfg.voices[i]
		v.prompt = phonePrompt(v.prompt, len(cfg.tools) > 0)
		if cfg.guard != nil {
			v.guestPrompt = cfg.guard.prompt
			if v.GuestPersona != "" {
				raw, err := os.ReadFile(v.GuestPersona)
				if err != nil {
					return cfg, fmt.Errorf("read -voices: voice %q: %w", v.Name, err)
				}
				v.guestPrompt = strings.TrimSpace(string(raw))
			}
			v.guestPrompt = phonePrompt(v.guestPrompt, len(guestTools(cfg.tools)) > 0) + " " + guestRules
		}
	}
	if cfg.guard != nil {
		cfg.guard.prompt = phonePrompt(cfg.guard.prompt, len(guestTools(cfg.tools)) > 0) + " " + guestRules
	}
	return cfg, nil
}

// phonePrompt is a persona with the rules of the telephone.
func phonePrompt(persona string, tools bool) string {
	if tools {
		return persona + "\n\n" + phoneRules + " " + lookupRules
	}
	return persona + "\n\n" + phoneRules + " " + noLookupRules
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
	var c *conversation
	chain := voiceChain{
		listen: func(ctx context.Context, pcm []byte) (string, error) {
			return transcribeHTTP(ctx, cfg.sttURL, cfg.sttModel, cfg.sttLanguage, pcm)
		},
		think: func(ctx context.Context, msgs []chatMessage, out thinkSink) error {
			return thinkWithTools(ctx, cfg.llm, c.toolsNow(), c.toolsWait(), msgs, out, func(event string, fields map[string]any) {
				c.events.emit(event, fields)
			})
		},
	}
	if len(cfg.voices) > 0 {
		for i := range cfg.voices {
			v := &cfg.voices[i]
			v.speak = resampledSpeaker(commandSpeaker(v.TTSCmd, v.TTSRate), v.TTSRate, cfg.call.inRate)
		}
		_ = os.Setenv("AGENTCALLER_VOICE", cfg.voices[0].Name)
	} else {
		chain.speak = commandSpeaker(cfg.ttsCmd, cfg.call.inRate)
	}
	if cfg.guard != nil {
		listener, err := startSpeakerCommand(ctx, cfg.guard.Command, stderr)
		if err != nil {
			return fmt.Errorf("-speakers: %w", err)
		}
		defer listener.close()
		chain.who = listener.identify
	}
	chain.warm = func(ctx context.Context, msgs []chatMessage, tools []callTool) error {
		once := cfg.llm
		once.MaxTokens = 1
		_, err := streamTurn(ctx, once, toolSpecs(tools), msgs, func(string) {})
		return err
	}
	c = newConversation(cfg, chain, eventsOut)
	// While it rings, the model reads what it will be asked with: the personas and the
	// tools. The first answer then starts from a warm cache instead of reading them.
	go c.warm(ctx)
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
	think func(ctx context.Context, msgs []chatMessage, out thinkSink) error
	// speak writes text as raw PCM (at the call's input rate) to w until done or cancelled.
	speak func(ctx context.Context, text string, w io.Writer) error
	// who tells whose voice this speech is; nil: nobody checks.
	who func(ctx context.Context, pcm []byte, dur time.Duration) (speakerVerdict, error)
	// warm lets the model read a start of conversation ahead of time; nil: it does not.
	warm func(ctx context.Context, msgs []chatMessage, tools []callTool) error
}

// thinkSink takes what think produces.
type thinkSink struct {
	// sentence is the next sentence of the answer.
	sentence func(string)
	// aside is something to say that is not part of the answer: "one moment" while a
	// lookup runs. It is spoken and forgotten; kept, the model would learn to say it
	// instead of looking.
	aside func(string)
	// looked is a finished lookup, the model's request and what came back. It is kept
	// with the answer, so that the next question can build on it.
	looked func([]chatMessage)
	// later takes work that outlasts the answer and returns what to tell the model about
	// it. Without it, such work is waited for like any lookup.
	later func(toolCall) string
	// handover takes the request to give the call to another voice; false: refused,
	// with the reason for the model.
	handover func(toolCall) (string, bool)
	// plain asks for an answer without tools: a report tells what came back and looks
	// nothing up on its own.
	plain bool
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
		case "ready", "speech_start", "speech_sustained", "speech_end":
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
	report   *jobReport // background work has come back
	deferred int        // how often the report let the peer go first
	at, dur  time.Duration
	ended    time.Time // when the pause was reported: the start of the answer delay
}

// jobReport is the outcome of work the agent did in the background.
type jobReport struct {
	call   toolCall
	result string
	err    error
}

// maxJobs is how much background work may run at once.
const maxJobs = 3

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
	jobs       int      // background work under way
	greetOwed  bool     // the greeting is due and has not been said to the end
	unsaid     []string // background work whose outcome is in the history but was not spoken
	life       context.Context
	voice      int          // which of cfg.voices holds the call
	handTo     int          // the voice asked to take over, plus one; 0: none
	open       []bool       // per message of history: said with a guest, so a guest may be shown it
	owner      bool         // with a guard: the voice heard last is a trusted one
	unlocked   bool         // the password was said: trusted for the rest of the call
	speaker    string       // name of the voice heard last, if it is a known one
	held       []*jobReport // outcomes that wait for a trusted speaker
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
	c.mu.Lock()
	c.life = ctx
	c.mu.Unlock()
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
				// The agent does not start to talk into this, but it is not cut off
				// either until the sound has lasted (speech_sustained).
				c.mu.Lock()
				c.peerTalks = true
				c.mu.Unlock()
			case "speech_sustained":
				c.mu.Lock()
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
			switch {
			case s.greeting:
				c.greet(ctx)
			case s.report != nil:
				c.report(ctx, s)
			default:
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
	// Whoever accepts a call on a phone still has to lift it: a greeting that starts with
	// the connection is half over by then.
	c.mu.Lock()
	c.greetOwed = true
	c.mu.Unlock()
	select {
	case <-time.After(c.cfg.greetAfter):
	case <-ctx.Done():
		return
	}
	c.sayGreeting(ctx)
}

// sayGreeting speaks the greeting if it is still owed. A sound from the other end cuts
// it short like any answer; if that sound turns out to be no speech (the click of
// picking up), answer says the greeting again, and if it was speech, the peer has
// opened the conversation and gets an answer instead.
func (c *conversation) sayGreeting(ctx context.Context) {
	c.mu.Lock()
	owed := c.greetOwed
	c.mu.Unlock()
	if !owed {
		return
	}
	turn, end, _ := c.beginTurn(ctx, true)
	defer end()
	err := c.speech()(turn, c.cfg.greeting, ctxWriter{ctx: turn, p: c.say})
	if err != nil {
		if turn.Err() == nil {
			c.events.emit("reply_error", map[string]any{"error": err.Error()})
		}
		return
	}
	c.mu.Lock()
	c.greetOwed = false
	c.mu.Unlock()
	c.remember("", nil, c.cfg.greeting)
	c.events.emit("reply", c.voiced(map[string]any{"text": c.cfg.greeting, "greeting": true}))
}

// startJob runs a background tool for as long as the call lasts, not just the turn that
// asked for it, and queues its outcome as a turn of its own.
func (c *conversation) startJob(call toolCall) string {
	c.mu.Lock()
	life := c.life
	if c.jobs >= maxJobs || life == nil {
		c.mu.Unlock()
		return jobRefused
	}
	c.jobs++
	c.mu.Unlock()
	go func() {
		start := time.Now()
		result, err := runTool(life, c.cfg.tools, call)
		c.mu.Lock()
		c.jobs--
		c.mu.Unlock()
		if life.Err() != nil {
			return
		}
		fields := map[string]any{"name": call.Function.Name, "ms": time.Since(start).Milliseconds()}
		if err != nil {
			fields["error"] = err.Error()
		} else {
			fields["result"] = clip(result, 300)
		}
		c.events.emit("lookup_done", fields)
		select {
		case c.turns <- segment{report: &jobReport{call: call, result: result, err: err}, ended: time.Now()}:
		case <-life.Done():
		}
	}()
	return jobStarted
}

// report tells the peer what background work brought back, in a gap of the conversation.
func (c *conversation) report(ctx context.Context, s segment) {
	for {
		if len(c.turns) > 0 && s.deferred < 3 {
			// The peer said something in the meantime: that is answered first.
			s.deferred++
			select {
			case c.turns <- s:
				return
			default:
			}
		}
		c.mu.Lock()
		talking := c.peerTalks && !c.cfg.noBargeIn
		c.mu.Unlock()
		if !talking && c.say.remaining() == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	r := s.report
	if c.holdReport(r) {
		return // a guest is on the line: the outcome waits for somebody who may hear it
	}
	note := fmt.Sprintf(jobDone, r.call.Function.Name, r.call.Function.Arguments, r.result)
	if r.err != nil {
		note = fmt.Sprintf(jobFailed, r.call.Function.Name, r.call.Function.Arguments, r.err)
	}
	// The outcome is part of the conversation from here on, and it is owed to the peer
	// until it has been spoken: if the peer cuts in before the report is out, it goes
	// along with the next answer (see owedNote). On a real call 4 s to first audio were
	// enough for the peer to talk over most reports, and those outcomes were never said.
	c.mu.Lock()
	owed, before := c.owedNote()
	c.push(false, chatMessage{Role: "user", Content: note})
	c.unsaid = append(c.unsaid, r.call.Function.Name+" "+clip(r.call.Function.Arguments, 160))
	msgs := c.view()
	c.mu.Unlock()
	if owed != "" {
		msgs = append(msgs, chatMessage{Role: "user", Content: owed})
	}

	turn, end, ok := c.beginTurn(ctx, false)
	if !ok {
		return
	}
	defer end()
	spoken, looked, first, err := c.respond(turn, msgs, true)
	interrupted := turn.Err() != nil && ctx.Err() == nil
	if err != nil && !interrupted && ctx.Err() == nil {
		c.events.emit("reply_error", map[string]any{"error": err.Error()})
	}
	if len(spoken) == 0 {
		return
	}
	c.said(before + 1)
	reply := strings.Join(spoken, " ")
	c.remember("", looked, reply)
	fields := c.voiced(map[string]any{"text": reply, "interrupted": interrupted, "report": true})
	if !first.IsZero() {
		fields["first_audio_ms"] = first.Sub(s.ended).Milliseconds() // since the work came back
	}
	c.events.emit("reply", fields)
}

// owedNote is the reminder of outcomes that are in the history and were not spoken, and
// how many it covers. The caller holds c.mu.
func (c *conversation) owedNote() (note string, n int) {
	if len(c.unsaid) == 0 {
		return "", 0
	}
	return fmt.Sprintf(reportOwed, strings.Join(c.unsaid, " und zu ")), len(c.unsaid)
}

// said strikes the n oldest outcomes off the list of those still owed to the peer.
func (c *conversation) said(n int) {
	c.mu.Lock()
	c.unsaid = c.unsaid[min(n, len(c.unsaid)):]
	c.mu.Unlock()
}

// maxHistory is how many messages of the conversation the model is shown again.
const maxHistory = 60

func (c *conversation) remember(user string, looked []chatMessage, assistant string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	open := c.locked()
	if user != "" {
		c.push(open, chatMessage{Role: "user", Content: user})
	}
	c.push(open, looked...)
	c.push(open, chatMessage{Role: "assistant", Content: assistant})
	extra := max(len(c.history)-maxHistory, 0)
	// Never start in the middle of a lookup: a result without its request is an error to the model.
	for extra > 0 && extra < len(c.history) && (c.history[extra].Role == "tool" || len(c.history[extra].ToolCalls) > 0) {
		extra++
	}
	if extra > 0 {
		c.history = append(c.history[:0], c.history[extra:]...)
		c.open = append(c.open[:0], c.open[extra:]...)
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
	var verdict chan speakerVerdict
	if c.chain.who != nil {
		// Whose voice it is, is found out while the words are: it costs the answer no time.
		verdict = make(chan speakerVerdict, 1)
		go func() {
			limit, done := context.WithTimeout(ctx, 3*time.Second)
			defer done()
			v, err := c.chain.who(limit, pcm, s.dur)
			if err != nil {
				v = speakerVerdict{err: err}
			}
			verdict <- v
		}()
	}
	sttStart := time.Now()
	text, err := c.chain.listen(ctx, pcm)
	if err != nil {
		if ctx.Err() == nil {
			c.events.emit("stt_error", map[string]any{"error": err.Error()})
		}
		return
	}
	text, password := c.cfg.guard.redact(strings.TrimSpace(text))
	if text == "" || isTranscriptionNoise(text) || tooMuchForItsLength(text, s.dur) {
		c.events.emit("heard", map[string]any{"text": "", "ignored": text, "at": s.at.Seconds()})
		c.mu.Lock()
		owed := len(c.pending) > 0
		c.mu.Unlock()
		if !owed {
			c.sayGreeting(ctx) // if that noise cut off the greeting
			return
		}
		text = "" // a noise cut off the answer to what was said before: it is still owed
	} else {
		c.events.emit("heard", map[string]any{"text": text, "at": s.at.Seconds(), "stt_ms": time.Since(sttStart).Milliseconds()})
		c.mu.Lock()
		c.greetOwed = false // the peer spoke first
		c.mu.Unlock()
	}
	whose := ""
	if verdict != nil && text != "" {
		select {
		case v := <-verdict:
			whose = c.judge(v, password)
		case <-ctx.Done():
			return
		}
	}

	c.mu.Lock()
	if text != "" {
		if c.locked() {
			text = c.guestTag() + text // whoever is trusted later sees who said it
		}
		c.pending = append(c.pending, text)
	}
	user := strings.Join(c.pending, " ")
	msgs := c.view()
	owed, owedN := c.owedNote()
	if c.locked() {
		owed, owedN = "", 0 // outcomes are not for a guest
	}
	c.mu.Unlock()
	asked := user
	if c.cfg.clock != nil {
		// With the question, not in the system prompt: what the model has already read
		// stays the same from turn to turn, and the server need not read it again.
		asked = "(" + spokenNow(c.cfg.clock()) + " Das ist eine Angabe des Systems, dein Gegenüber hat sie nicht gesagt.)\n\n" + user
	}
	if whose != "" {
		asked = "(" + whose + " Das ist eine Angabe des Systems, dein Gegenüber hat sie nicht gesagt.)\n\n" + asked
	}
	if owed != "" {
		asked += "\n\n" + owed
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: asked})

	turn, end, ok := c.beginTurn(ctx, false)
	if !ok {
		return // the peer is talking again: answer everything at the next pause
	}
	defer end()
	spoken, looked, first, err := c.respond(turn, msgs, false)
	interrupted := turn.Err() != nil && ctx.Err() == nil
	if err != nil && !interrupted && ctx.Err() == nil {
		c.events.emit("reply_error", map[string]any{"error": err.Error()})
	}
	if len(spoken) == 0 {
		c.takeHandover()  // it was not said, so it does not happen
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
	c.remember(user, looked, reply)
	fields := c.voiced(map[string]any{"text": reply, "interrupted": interrupted})
	if len(looked) > 0 {
		fields["looked_up"] = true
	}
	if owedN > 0 && !interrupted {
		c.said(owedN)
		fields["reports"] = owedN // outcomes of background work that went along with this answer
	}
	if !first.IsZero() {
		fields["first_audio_ms"] = first.Sub(s.ended).Milliseconds()
	}
	c.events.emit("reply", fields)

	if to, ok := c.takeHandover(); ok {
		end()
		c.handOver(ctx, to)
		return
	}
	if !interrupted && c.cfg.hangsUp(user) {
		select {
		case <-turn.Done(): // the peer had more to say
		case <-time.After(c.say.remaining() + 500*time.Millisecond):
			c.command(ctx, "hangup")
		}
	}
}

// respond speaks the answer to msgs. spoken holds the sentences of the answer that went
// out in full, looked what was looked up for it.
func (c *conversation) respond(ctx context.Context, msgs []chatMessage, plain bool) (spoken []string, looked []chatMessage, firstAudio time.Time, err error) {
	type piece struct {
		text  string
		aside bool
	}
	pieces := make(chan piece, 32)
	thought := make(chan error, 1)
	var found []chatMessage // written by think, read once it has returned
	say := func(p piece) {
		select {
		case pieces <- p:
		case <-ctx.Done():
		}
	}
	go func() {
		err := c.chain.think(ctx, msgs, thinkSink{
			sentence: func(s string) { say(piece{text: s}) },
			aside:    func(s string) { say(piece{text: s, aside: true}) },
			looked:   func(m []chatMessage) { found = append(found, m...) },
			later:    c.startJob,
			handover: c.askHandover,
			plain:    plain,
		})
		close(pieces)
		thought <- err
	}()
	for p := range pieces {
		if err != nil {
			continue // speaking failed or the peer cut in: let think finish, say no more
		}
		if err = c.speech()(ctx, p.text, ctxWriter{ctx: ctx, p: c.say, first: &firstAudio}); err == nil && !p.aside {
			spoken = append(spoken, p.text)
		}
	}
	// Wait for think even after an interruption (it stops with the turn): what was looked
	// up belongs to the sentences that did go out, or they stand in the history as
	// numbers from nowhere and teach the model to answer without looking.
	if thinkErr := <-thought; err == nil {
		err = thinkErr
	}
	return spoken, found, firstAudio, err
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

// tooMuchForItsLength catches the other thing Whisper does with a cough or a click: it
// writes a polite phrase ("Vielen Dank.") that nobody could have said in that time. On
// the first long live call every such phrase came from a sound of 120 to 360 ms, while
// what was really said that briefly was a word: "Was?" in 360 ms, "Ja." in 420.
func tooMuchForItsLength(text string, dur time.Duration) bool {
	if dur <= 0 || dur >= 600*time.Millisecond {
		return false
	}
	letters := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	const fastest = 25 // letters per second; quick German speech stays under 20
	if float64(letters) > fastest*dur.Seconds() {
		return true
	}
	return strings.Trim(strings.ToLower(text), " .!?…") == "vielen dank"
}

var (
	weekdaysDE = [...]string{"Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"}
	monthsDE   = [...]string{"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"}
)

// spokenNow tells the model the date and time: the one thing it is asked on a telephone
// that needs no lookup.
func spokenNow(t time.Time) string {
	return fmt.Sprintf("Jetzt ist %s, der %d. %s %d, %d Uhr %02d.", weekdaysDE[t.Weekday()], t.Day(), monthsDE[t.Month()-1], t.Year(), t.Hour(), t.Minute())
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
