package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	meowcaller "github.com/purpshell/meowcaller"
	"github.com/rs/zerolog"
	"golang.org/x/term"
)

const (
	talkDefaultAnswerTimeout = 45 * time.Second
	talkDefaultMaxListen     = 60 * time.Second

	// interimMessage is the fixed word-for-word line played the instant the callee's
	// speech endpoint is detected, while STT runs in parallel.
	interimMessage = "Einen Moment, Sir, ich werde das verarbeiten."
	// askAgain is played when the first round heard no voice, before the second try.
	askAgain = "Ich habe nichts gehört. Bitte sprechen Sie erneut."
	// fallbackNoSpeech is played when no voice ever reached the threshold.
	fallbackNoSpeech = "Ich habe nichts gehört. Auf Wiederhören."
	// fallbackProcessingError is played when STT, the LLM, or TTS fails mid-call, so
	// the call never ends silent.
	fallbackProcessingError = "Entschuldigung, Sir, ich habe das nicht richtig verstanden. Auf Wiederhören."

	defaultAnnouncementText = "Guten Abend. Dies ist Ihr automatischer Anruf. Erzählen Sie mir in einem Satz, wie es Ihnen geht, und ich werde darauf antworten."

	// playbackGuard bounds how long we wait for the player to finish a line before
	// moving on, so a stuck source can never hang the call.
	playbackGuard = 20 * time.Second
)

type talkConfig struct {
	target        string
	storePath     string
	answerTimeout time.Duration
	maxListen     time.Duration
	prompt        string
	sayPath       string
	sayText       string
	saveAudio     string
	dryRun        bool
	replayPath    string
	listenOnly    bool
	ttsFile       string
	outDir        string

	sttModel     string
	llmEndpoint  string
	llmModel     string
	ttsBackend   string
	piperVoice   string
	vadSilence   time.Duration
	vadMinSpeech time.Duration
}

func parseTalkArgs(args []string, stderr io.Writer) (talkConfig, error) {
	var cfg talkConfig
	flags := newTalkFlagSet(&cfg, stderr)
	if err := flags.Parse(reorderFlagsFirst(flags, args)); err != nil {
		return cfg, errors.Join(errUsage, err)
	}
	switch {
	case cfg.dryRun:
		if flags.NArg() != 1 {
			flags.Usage()
			return cfg, errUsage
		}
		cfg.target = flags.Arg(0)
	case cfg.replayPath != "":
		if flags.NArg() > 0 {
			flags.Usage()
			return cfg, errUsage
		}
	default:
		if flags.NArg() != 1 {
			flags.Usage()
			return cfg, errUsage
		}
		cfg.target = flags.Arg(0)
	}
	if cfg.answerTimeout < 0 || cfg.maxListen < 0 {
		return cfg, errors.Join(errUsage, errors.New("durations must be non-negative"))
	}
	return cfg, nil
}

// newTalkFlagSet registers all talk flags on a fresh FlagSet bound to cfg. It is
// separated from parseTalkArgs so the ordering helper can consult the registered
// flags in tests without running a parse.
func newTalkFlagSet(cfg *talkConfig, stderr io.Writer) *flag.FlagSet {
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("talk", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.storePath, "store", filepath.Join(home, ".config", "meowcaller", "talk-voip.db"), "linked-device SQLite store for talk (separate from the notify store)")
	flags.DurationVar(&cfg.answerTimeout, "answer-timeout", talkDefaultAnswerTimeout, "maximum wait for the recipient to answer; 0 disables")
	flags.DurationVar(&cfg.maxListen, "max-listen", talkDefaultMaxListen, "maximum time to listen per round; 0 disables")
	flags.StringVar(&cfg.prompt, "prompt", DefaultLLMPrompt, "system prompt for the answer LLM")
	flags.StringVar(&cfg.sayPath, "say", "", "announcement audio file (.mp3,.wav,.ogg,.opus,.mulaw,.ulaw); plays instead of the generated announcement")
	flags.StringVar(&cfg.sayText, "say-text", defaultAnnouncementText, "announcement text, synthesized when -say is empty")
	flags.StringVar(&cfg.saveAudio, "save-audio", "", "directory for intermediate audio artifacts (default a /tmp dir)")
	flags.BoolVar(&cfg.dryRun, "dry-run", false, "run STT+LLM+TTS on the target WAV without placing a call")
	flags.StringVar(&cfg.replayPath, "replay", "", "replay a 16 kHz call-frame WAV through the VAD pipeline (deterministic, frame-paced)")
	flags.BoolVar(&cfg.listenOnly, "listen-only", false, "with -replay: stop after the VAD endpointing report")
	flags.StringVar(&cfg.ttsFile, "tts-file", "", "copy the final answer TTS to this path")
	flags.StringVar(&cfg.outDir, "out-dir", "", "directory for artifacts (default honors -save-audio or a /tmp dir)")
	flags.StringVar(&cfg.sttModel, "stt-model", "", "whisper model; default small (fallbacks base, tiny)")
	flags.StringVar(&cfg.llmEndpoint, "llm-endpoint", defaultLLMConfig().Endpoint, "local OpenAI-compatible chat endpoint")
	flags.StringVar(&cfg.llmModel, "llm-model", defaultLLMConfig().ModelID, "LLM model id")
	flags.StringVar(&cfg.ttsBackend, "tts-backend", "piper", "TTS backend: piper (local, default) or edge (cloud fallback)")
	flags.StringVar(&cfg.piperVoice, "piper-voice", defaultTTSConfig().PiperVoice, "piper voice name")
	flags.DurationVar(&cfg.vadSilence, "vad-silence", defaultVADConfig().SilenceDuration, "silence after speech that closes a round")
	flags.DurationVar(&cfg.vadMinSpeech, "vad-min-speech", defaultVADConfig().MinSpeech, "minimum speech duration that counts as an answer")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: meowcaller talk [flags] <target>")
		fmt.Fprintln(stderr, "       meowcaller talk -dry-run [flags] <input.wav>")
		fmt.Fprintln(stderr, "       meowcaller talk -replay <input.wav> [--listen-only] [flags]")
		flags.PrintDefaults()
	}
	return flags
}

// reorderFlagsFirst moves flag arguments (and their values) ahead of positional
// arguments so that Go's flag package, which stops at the first positional, still
// accepts "talk -dry-run <file> -out-dir <dir>". It consults the registered flag
// set, so value-taking flags are detected without a hardcoded list.
func reorderFlagsFirst(flags *flag.FlagSet, args []string) []string {
	var ordered, pos []string
	i := 0
	for i < len(args) {
		a := args[i]
		if len(a) > 1 && a[0] == '-' && a != "-" {
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
			}
			f := flags.Lookup(name)
			takesValue := f != nil && reflect.TypeOf(f.Value).String() != "*flag.boolValue"
			if takesValue && !strings.Contains(a, "=") && i+1 < len(args) {
				ordered = append(ordered, a, args[i+1])
				i += 2
				continue
			}
			ordered = append(ordered, a)
			i++
			continue
		}
		pos = append(pos, a)
		i++
	}
	return append(ordered, pos...)
}

// artifactDir returns the directory intermediate files land in, creating it.
func artifactDir(cfg talkConfig) (string, error) {
	dir := cfg.outDir
	if dir == "" {
		dir = cfg.saveAudio
	}
	if dir == "" {
		created, err := os.MkdirTemp("", "meowcaller-talk-")
		if err != nil {
			return "", err
		}
		dir = created
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// runTalk dispatches to the live, dry-run, or replay pipeline.
func runTalk(ctx context.Context, cfg talkConfig, stderr io.Writer) error {
	log := zerolog.Ctx(ctx)
	switch {
	case cfg.replayPath != "":
		return runReplay(ctx, cfg, *log)
	case cfg.dryRun:
		return runDryRun(ctx, cfg, *log)
	default:
		return runLiveCall(ctx, cfg, *log, stderr)
	}
}

func newVADFromConfig(cfg talkConfig) VADConfig {
	vad := defaultVADConfig()
	if cfg.vadSilence > 0 {
		vad.SilenceDuration = cfg.vadSilence
	}
	if cfg.vadMinSpeech > 0 {
		vad.MinSpeech = cfg.vadMinSpeech
	}
	if cfg.maxListen > 0 {
		vad.MaxListen = cfg.maxListen
	}
	return vad
}

func newSTTFromConfig(cfg talkConfig, log zerolog.Logger) STTBackend {
	stt := defaultSTTConfig()
	if cfg.sttModel != "" {
		stt.Model = cfg.sttModel
	}
	return NewSTTBackend(stt, log)
}

func newLLMFromConfig(cfg talkConfig) LLMConfig {
	llm := defaultLLMConfig()
	llm.Endpoint = cfg.llmEndpoint
	llm.ModelID = cfg.llmModel
	if cfg.prompt != "" {
		llm.Prompt = cfg.prompt
	}
	return llm
}

func newTTSFromConfig(cfg talkConfig, outDir string) TTSConfig {
	tts := defaultTTSConfig()
	if cfg.ttsBackend != "" {
		tts.Backend = cfg.ttsBackend
	}
	if cfg.piperVoice != "" {
		tts.PiperVoice = cfg.piperVoice
	}
	tts.OutputDir = outDir
	return tts
}

// processAudio runs the local chain STT → LLM → TTS on a 16 kHz WAV and writes
// every artifact into outDir. Shared by dry-run and replay.
func processAudio(ctx context.Context, cfg talkConfig, inWav, outDir string, log zerolog.Logger) (transcript, answer, ttsPath string, err error) {
	stt := newSTTFromConfig(cfg, log)
	llm := newLLMFromConfig(cfg)
	tts := newTTSFromConfig(cfg, outDir)

	start := time.Now()
	transcript, err = stt.Transcribe(ctx, inWav)
	log.Info().Str("duration", time.Since(start).String()).Int("transcript_bytes", len(transcript)).Msg("stt finished")
	if err != nil {
		return "", "", "", fmt.Errorf("speech-to-text: %w", err)
	}
	if transcript == "" {
		return "", "", "", errors.New("speech-to-text produced an empty transcript")
	}
	if err := os.WriteFile(filepath.Join(outDir, "transcript.json"), jsonLine(map[string]string{"text": transcript}), 0o600); err != nil {
		return "", "", "", err
	}

	start = time.Now()
	answer, err = generateReply(ctx, llm, transcript)
	log.Info().Str("duration", time.Since(start).String()).Int("answer_bytes", len(answer)).Msg("llm finished")
	if err != nil {
		return transcript, "", "", fmt.Errorf("answer generation: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "answer.txt"), []byte(answer+"\n"), 0o600); err != nil {
		return transcript, "", "", err
	}

	start = time.Now()
	ttsPath, err = synthesize(ctx, tts, answer, "answer")
	log.Info().Str("duration", time.Since(start).String()).Str("path", ttsPath).Msg("tts finished")
	if err != nil {
		return transcript, answer, "", fmt.Errorf("text-to-speech: %w", err)
	}
	if cfg.ttsFile != "" {
		if err := copyFile(ttsPath, cfg.ttsFile); err != nil {
			return transcript, answer, "", fmt.Errorf("copy tts file: %w", err)
		}
	}
	if err := makeMP3(ttsPath, filepath.Join(outDir, "answer.mp3")); err != nil {
		log.Warn().Err(err).Msg("mp3 conversion skipped (ffmpeg missing?)")
	}
	return transcript, answer, ttsPath, nil
}

// readWAV16 decodes a WAV file into 16 kHz mono float32 samples.
func readWAV16(path string) []float32 {
	src, err := meowcaller.WAVFile(path)
	if err != nil {
		return nil
	}
	defer src.Close()
	var out []float32
	for {
		frame, rerr := src.ReadFrame()
		if len(frame) > 0 {
			out = append(out, frame...)
		}
		if rerr != nil {
			return out
		}
	}
}

// makeMP3 converts a WAV to MP3 with ffmpeg when available; the playable
// artifact is the WAV, the MP3 is a convenience for inspection.
func makeMP3(wavPath, mp3Path string) error {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return err
	}
	return runCommand(ffmpeg, "-y", "-v", "error", "-i", wavPath, "-codec:a", "libmp3lame", "-qscale:a", "4", mp3Path)
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", filepath.Base(name), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(dst); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(dst, raw, 0o600)
}

func jsonLine(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

// runReplay replays a real recorded call-frame WAV through the VAD pipeline to
// verify endpointing and the interim-message trigger timing without a live call
// (review point 3). The VAD measures frame count, so frames feed back-to-back and
// the result is deterministic.
func runReplay(ctx context.Context, cfg talkConfig, log zerolog.Logger) error {
	outDir, err := artifactDir(cfg)
	if err != nil {
		return err
	}
	src, err := meowcaller.WAVFile(cfg.replayPath)
	if err != nil {
		return fmt.Errorf("open replay file: %w", err)
	}
	vad := newVADSink(newVADFromConfig(cfg))
	res := feedSinkTo(vad, src)
	recording := filepath.Join(outDir, "replay-recording.wav")
	if err := writeWAV16(recording, vad.Audio()); err != nil {
		return err
	}
	trimmed := filepath.Join(outDir, "replay-speech.wav")
	if err := writeWAV16(trimmed, vad.TrimmedAudio()); err != nil {
		return err
	}
	report := map[string]any{
		"total_s":           res.Total.Seconds(),
		"ended":             res.Ended,
		"reason":            res.Reason,
		"speech_start_s":    res.SpeechStart.Seconds(),
		"speech_end_s":      res.SpeechEnd.Seconds(),
		"speech_duration_s": res.Duration.Seconds(),
		"no_speech":         res.NoSpeech,
		"interim_message":   interimMessage,
		"interim_trigger_s": res.SpeechEnd.Seconds(),
		"recording":         recording,
		"speech_wav":        trimmed,
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "replay result: %s\n", string(raw))
	log.Info().Str("reason", res.Reason).Dur("speech", res.Duration).
		Dur("endpoint", res.SpeechEnd).Msg("replay endpointing verified")
	if cfg.listenOnly {
		return nil
	}
	return runDryRun(ctx, talkConfig{
		target:      cfg.replayPath,
		sttModel:    cfg.sttModel,
		prompt:      cfg.prompt,
		llmEndpoint: cfg.llmEndpoint,
		llmModel:    cfg.llmModel,
		ttsBackend:  cfg.ttsBackend,
		piperVoice:  cfg.piperVoice,
		outDir:      outDir,
		ttsFile:     cfg.ttsFile,
	}, log)
}

// runDryRun runs the full local chain on a WAV without placing a call.
func runDryRun(ctx context.Context, cfg talkConfig, log zerolog.Logger) error {
	outDir, err := artifactDir(cfg)
	if err != nil {
		return err
	}
	samples := readWAV16(cfg.target)
	if samples == nil {
		return fmt.Errorf("read wav: %s", cfg.target)
	}
	inWav := filepath.Join(outDir, "input.wav")
	if err := writeWAV16(inWav, samples); err != nil {
		return err
	}
	log.Info().Float64("peak", maxRMS(samples)).Int("samples", len(samples)).
		Str("out_dir", outDir).Msg("dry run started")
	transcript, answer, ttsPath, err := processAudio(ctx, cfg, inWav, outDir, log)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "transcript: %s\nanswer: %s\naudio: %s\nout_dir: %s\n",
		transcript, answer, ttsPath, outDir)
	return nil
}

// liveCall is the state machine of one interactive call, bound to a real
// meowcaller.Call. It is driven by a single goroutine that waits on channels so
// the announcement, listening rounds, interim line, and answer never overlap on
// the shared player.
type liveCall struct {
	ctx        context.Context
	cfg        talkConfig
	log        zerolog.Logger
	call       *meowcaller.Call
	player     *meowcaller.Player
	outDir     string
	interimWav string
}

// runLiveCall executes the interactive call state machine:
// connect → call → announcement → listen (VAD) → interim+STT/LLM parallel →
// answer → hangup.
func runLiveCall(ctx context.Context, cfg talkConfig, log zerolog.Logger, stderr io.Writer) error {
	session, err := openSession(ctx, cfg.storePath, log)
	if err != nil {
		return err
	}
	defer session.Close()

	client := meowcaller.NewClient(session.client, meowcaller.WithLogger(log))
	allowQR := false
	if terminal, ok := stderr.(*os.File); ok {
		allowQR = term.IsTerminal(int(terminal.Fd()))
	}
	if err := session.Connect(ctx, stderr, allowQR); err != nil {
		return err
	}

	outDir, err := artifactDir(cfg)
	if err != nil {
		return err
	}
	tts := newTTSFromConfig(cfg, outDir)

	// Pre-synthesize the interim line before the call so it can start the instant
	// the endpoint is detected — no synthesis latency in the call path.
	log.Info().Str("out_dir", outDir).Msg("talk: pre-synthesizing interim message")
	interimWav, err := synthesize(ctx, tts, interimMessage, "interim")
	if err != nil {
		return fmt.Errorf("synthesize interim message: %w", err)
	}

	// Announcement: -say file wins, otherwise synthesize -say-text.
	var announceSource meowcaller.AudioSource
	if cfg.sayPath != "" {
		if announceSource, err = openAudioSource(cfg.sayPath); err != nil {
			return err
		}
	} else {
		sayWav, err := synthesize(ctx, tts, cfg.sayText, "announcement")
		if err != nil {
			return fmt.Errorf("synthesize announcement: %w", err)
		}
		if announceSource, err = meowcaller.WAVFile(sayWav); err != nil {
			return err
		}
	}

	call, err := client.Call(ctx, cfg.target)
	if err != nil {
		return fmt.Errorf("place call: %w", err)
	}
	nc := &liveCall{ctx: ctx, cfg: cfg, log: log, call: call, player: meowcaller.NewPlayer(), outDir: outDir, interimWav: interimWav}
	call.Subscribe(nc.player)
	call.Receive(meowcaller.SinkFunc(func([]float32) {})) // discard ringing audio

	ready := make(chan struct{}, 1)
	ended := make(chan string, 1)
	done := make(chan error, 1)
	registerTalkCallbacks(call, func() {
		select {
		case ready <- struct{}{}:
		default:
		}
	}, func(reason string) {
		select {
		case ended <- reason:
		default:
		}
	})

	// Wait for the recipient to answer.
	if err := nc.awaitAnswer(ready, ended); err != nil {
		return err
	}
	log.Info().Str("target", cfg.target).Msg("talk: answered, playing announcement")

	// Drive the post-answer flow in one goroutine; report via done.
	go func() { done <- nc.runConversation(announceSource, ended) }()
	return nc.awaitCompletion(done, ended)
}

// registerTalkCallbacks mirrors registerNotificationCallbacks, replaying the
// current phase when the call is already active or already ended.
func registerTalkCallbacks(call *meowcaller.Call, onReady func(), onEnd func(string)) {
	call.OnReady(onReady)
	call.OnEnd(onEnd)
	switch call.State() {
	case meowcaller.CallPhaseActive:
		onReady()
	case meowcaller.CallPhaseEnded:
		onEnd("ended before callback registration")
	}
}

// awaitAnswer blocks until the recipient answers or the call ends/times out.
func (nc *liveCall) awaitAnswer(ready <-chan struct{}, ended <-chan string) error {
	timer, timerC := optionalTimer(nc.cfg.answerTimeout)
	if timer != nil {
		defer timer.Stop()
	}
	select {
	case <-ready:
		return nil
	case reason := <-ended:
		nc.hangup()
		return fmt.Errorf("call ended before answer: %s", reason)
	case <-timerC:
		nc.hangup()
		return fmt.Errorf("recipient did not answer within %s", nc.cfg.answerTimeout)
	case <-nc.ctx.Done():
		nc.hangup()
		return nc.ctx.Err()
	}
}

// awaitCompletion waits for the conversation goroutine to finish or the peer to
// hang up.
func (nc *liveCall) awaitCompletion(done <-chan error, ended <-chan string) error {
	for {
		select {
		case err := <-done:
			return err
		case reason := <-ended:
			return fmt.Errorf("call ended: %s", reason)
		case <-nc.ctx.Done():
			nc.hangup()
			return nc.ctx.Err()
		}
	}
}

// runConversation plays the announcement, then runs the listening rounds, and
// finally the answer exchange. It returns nil when the call completed its
// intended flow.
func (nc *liveCall) runConversation(announce meowcaller.AudioSource, ended <-chan string) error {
	defer announce.Close()
	announceDone := make(chan struct{})
	nc.player.OnFinish(func() { close(announceDone) })
	nc.player.Play(announce)

	if err := nc.waitForPlay(announceDone, ended); err != nil {
		return err
	}
	nc.log.Info().Msg("talk: announcement finished, listening for the answer")
	return nc.runRounds(ended)
}

// runRounds listens for the answer, up to two rounds, asking once more on silence.
func (nc *liveCall) runRounds(ended <-chan string) error {
	for round := 1; round <= 2; round++ {
		res, endedEarly := nc.listenRound(round, ended)
		if endedEarly {
			return errors.New("call ended during listening")
		}
		if !res.NoSpeech {
			nc.log.Info().Dur("speech", res.Duration).Str("reason", res.Reason).
				Int("round", round).Msg("talk: speech endpoint detected")
			nc.processAndAnswer(round, ended)
			return nil
		}
		nc.log.Warn().Int("round", round).Msg("talk: no speech heard")
		if round == 1 {
			nc.sayLine(askAgain, ended)
		}
	}
	// Two silent rounds: the fixed fallback line, then hangup.
	nc.sayLine(fallbackNoSpeech, ended)
	nc.hangup()
	return nil
}

// listenRound attaches the VAD sink and waits for the round to close.
func (nc *liveCall) listenRound(round int, ended <-chan string) (res VADResult, endedEarly bool) {
	vad := newVADSink(newVADFromConfig(nc.cfg))
	endCh := make(chan VADResult, 1)
	vad.OnEnd(func(r VADResult) { endCh <- r })
	nc.call.Receive(vad)

	listenTimer, listenC := optionalTimer(nc.cfg.maxListen + 10*time.Second)
	if listenTimer != nil {
		defer listenTimer.Stop()
	}
	for {
		select {
		case r := <-endCh:
			nc.saveRoundArtifacts(vad, round)
			return r, false
		case <-ended:
			return VADResult{}, true
		case <-listenC:
			nc.log.Warn().Int("round", round).Msg("talk: listen round timed out")
			nc.saveRoundArtifacts(vad, round)
			return VADResult{Ended: true, Reason: "max_listen", NoSpeech: true}, false
		case <-nc.ctx.Done():
			return VADResult{}, true
		}
	}
}

// saveRoundArtifacts writes the full recording and the trimmed speech segment.
func (nc *liveCall) saveRoundArtifacts(vad *vadSink, round int) {
	recording := filepath.Join(nc.outDir, fmt.Sprintf("recording-r%d.wav", round))
	if err := writeWAV16(recording, vad.Audio()); err != nil {
		nc.log.Warn().Err(err).Msg("write recording")
	}
	speech := filepath.Join(nc.outDir, fmt.Sprintf("speech-r%d.wav", round))
	if err := writeWAV16(speech, vad.TrimmedAudio()); err != nil {
		nc.log.Warn().Err(err).Msg("write speech")
	}
}

// processAndAnswer plays the fixed interim line immediately (on the player) while
// STT → LLM → TTS run on this goroutine. When the answer is synthesized it waits
// for the interim line to finish, plays the answer, then hangs up. On any failure
// it plays the fallback line so the call never ends silent.
func (nc *liveCall) processAndAnswer(round int, ended <-chan string) {
	speechWav := filepath.Join(nc.outDir, fmt.Sprintf("speech-r%d.wav", round))

	interimDone := make(chan struct{})
	go func() {
		nc.waitIdle(3 * time.Second)
		src, err := meowcaller.WAVFile(nc.interimWav)
		if err != nil {
			close(interimDone)
			return
		}
		nc.player.OnFinish(func() { close(interimDone) })
		nc.player.Play(src)
	}()

	transcript, answer, ttsPath, err := nc.process(speechWav)
	if err != nil {
		nc.log.Warn().Err(err).Msg("talk: processing failed, playing fallback line")
		nc.playLineAfter(fallbackProcessingError, interimDone, ended)
		nc.hangup()
		return
	}
	_ = transcript
	_ = answer
	_ = ttsPath
	nc.playAnswerAfter(ttsPath, interimDone, ended)
	nc.hangup()
}

// process runs STT → LLM → TTS on the trimmed speech and writes the artifacts.
func (nc *liveCall) process(speechWav string) (transcript, answer, ttsPath string, err error) {
	cfg := nc.cfg
	log := nc.log
	outDir := nc.outDir
	stt := newSTTFromConfig(cfg, log)
	llm := newLLMFromConfig(cfg)
	tts := newTTSFromConfig(cfg, outDir)

	start := time.Now()
	transcript, err = stt.Transcribe(nc.ctx, speechWav)
	log.Info().Str("duration", time.Since(start).String()).Err(err).Int("transcript_bytes", len(transcript)).Msg("live stt finished")
	if err != nil {
		return "", "", "", fmt.Errorf("speech-to-text: %w", err)
	}
	if transcript == "" {
		return "", "", "", errors.New("empty transcript")
	}
	_ = os.WriteFile(filepath.Join(outDir, "transcript.json"), jsonLine(map[string]string{"text": transcript}), 0o600)

	answer, err = generateReply(nc.ctx, llm, transcript)
	if err != nil {
		return transcript, "", "", fmt.Errorf("answer generation: %w", err)
	}
	_ = os.WriteFile(filepath.Join(outDir, "answer.txt"), []byte(answer+"\n"), 0o600)

	ttsPath, err = synthesize(nc.ctx, tts, answer, "answer")
	if err != nil {
		return transcript, answer, "", fmt.Errorf("text-to-speech: %w", err)
	}
	if cfg.ttsFile != "" {
		_ = copyFile(ttsPath, cfg.ttsFile)
	}
	return transcript, answer, ttsPath, nil
}

// playAnswerAfter waits for the interim line to finish, plays the answer, then
// signals completion.
func (nc *liveCall) playAnswerAfter(answerWav string, interimDone <-chan struct{}, ended <-chan string) {
	nc.waitInterim(interimDone, ended)
	nc.playLine(answerWav, ended)
}

// playLineAfter plays a synthesized text line after the interim line finishes.
func (nc *liveCall) playLineAfter(text string, interimDone <-chan struct{}, ended <-chan string) {
	nc.waitInterim(interimDone, ended)
	nc.sayLine(text, ended)
}

// waitInterim blocks until the interim line finishes or the peer hangs up,
// guarded by playbackGuard.
func (nc *liveCall) waitInterim(interimDone <-chan struct{}, ended <-chan string) {
	select {
	case <-interimDone:
	case <-ended:
	case <-time.After(playbackGuard):
	}
}

// sayLine synthesizes a line and plays it to the call.
func (nc *liveCall) sayLine(text string, ended <-chan string) {
	tts := newTTSFromConfig(nc.cfg, nc.outDir)
	wav, err := synthesize(nc.ctx, tts, text, fmt.Sprintf("line-%d", time.Now().UnixNano()))
	if err != nil {
		nc.log.Warn().Err(err).Msg("synthesize line failed")
		return
	}
	nc.playLine(wav, ended)
}

// playLine plays a WAV to the call and waits for it to finish.
func (nc *liveCall) playLine(wav string, ended <-chan string) {
	nc.waitIdle(5 * time.Second)
	src, err := meowcaller.WAVFile(wav)
	if err != nil {
		nc.log.Warn().Err(err).Msg("open line wav failed")
		return
	}
	done := make(chan struct{})
	nc.player.OnFinish(func() { close(done) })
	nc.player.Play(src)
	select {
	case <-done:
	case <-ended:
	case <-time.After(playbackGuard):
	}
}

// waitIdle blocks until the player is idle or the timeout is reached.
func (nc *liveCall) waitIdle(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for nc.player.State() != meowcaller.PlayerIdle {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForPlay blocks until a playback finishes or the peer hangs up.
func (nc *liveCall) waitForPlay(done <-chan struct{}, ended <-chan string) error {
	select {
	case <-done:
		return nil
	case reason := <-ended:
		nc.hangup()
		return fmt.Errorf("call ended: %s", reason)
	case <-nc.ctx.Done():
		nc.hangup()
		return nc.ctx.Err()
	}
}

func (nc *liveCall) hangup() {
	_ = nc.call.Hangup()
}
