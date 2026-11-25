package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rotsch-repos/agentcaller"
)

func TestSentenceSplitter(t *testing.T) {
	var s sentenceSplitter
	var got []string
	text := "Ja. Das ist eine **gute** Frage, wirklich! Am 3. Mai war ich dort 🙂 und es war schön.\nUnd du?"
	for i := 0; i < len(text); i += 5 { // arbitrary pieces, also through the middle of runes
		got = append(got, s.feed(text[i:min(i+5, len(text))])...)
	}
	if rest := s.flush(); rest != "" {
		got = append(got, rest)
	}
	want := []string{"Ja. Das ist eine gute Frage, wirklich!", "Am 3. Mai war ich dort und es war schön.", "Und du?"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sentences %q, want %q", got, want)
	}
}

func TestStreamReply(t *testing.T) {
	var req chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, piece := range []string{"Das ist der erste ", "Satz der Antwort. Und ", "das der zweite."} {
			chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": piece}}}})
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var got []string
	msgs := []chatMessage{{Role: "system", Content: "S"}, {Role: "user", Content: "U"}}
	err := streamReply(context.Background(), LLMConfig{Endpoint: srv.URL + "/", ModelID: "m", MaxTokens: 50}, msgs, func(s string) { got = append(got, s) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Das ist der erste Satz der Antwort.", "Und das der zweite."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sentences %q, want %q", got, want)
	}
	if !req.Stream || req.Model != "m" || len(req.Messages) != 2 || req.ChatTemplateKwargs["enable_thinking"] != false {
		t.Fatalf("request %+v", req)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model is loading", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if err := streamReply(context.Background(), LLMConfig{Endpoint: down.URL}, msgs, func(string) {}); err == nil || !strings.Contains(err.Error(), "model is loading") {
		t.Fatalf("error %v, want the endpoint's message", err)
	}
}

func TestTranscribeHTTP(t *testing.T) {
	pcm := pcm16(tone(5, 0.2))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("no file: %v", err)
			return
		}
		wav, _ := io.ReadAll(file)
		if len(wav) != 44+len(pcm) || string(wav[:4]) != "RIFF" || !bytes.Equal(wav[44:], pcm) {
			t.Errorf("upload is %d bytes, want a WAV around %d bytes of speech", len(wav), len(pcm))
		}
		if r.FormValue("model") != "large-v3" || r.FormValue("language") != "de" || r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("fields %v at %s", r.Form, r.URL.Path)
		}
		fmt.Fprint(w, `{"text": " Guten Abend. "}`)
	}))
	defer srv.Close()
	text, err := transcribeHTTP(context.Background(), srv.URL, "large-v3", "de", pcm)
	if err != nil || text != " Guten Abend. " {
		t.Fatalf("got %q, %v", text, err)
	}
}

func TestAudioLog(t *testing.T) {
	var a audioLog
	second := make([]byte, pcmBytesPerSecond)
	for i := 0; i < 200; i++ { // 200 s: the front is dropped
		for j := range second {
			second[j] = byte(i)
		}
		_, _ = a.Write(second)
	}
	if a.end() != 200*time.Second {
		t.Fatalf("end %v", a.end())
	}
	got := a.slice(150*time.Second+500*time.Millisecond, 151*time.Second+500*time.Millisecond)
	if len(got) != pcmBytesPerSecond || got[0] != 150 || got[len(got)-1] != 151 {
		t.Fatalf("slice of %d bytes from %d to %d", len(got), got[0], got[len(got)-1])
	}
	if old := a.slice(time.Second, 2*time.Second); old != nil {
		t.Fatalf("audio from 199 s ago is still there (%d bytes)", len(old))
	}
	if tail := a.slice(199*time.Second+500*time.Millisecond, 205*time.Second); len(tail) != pcmBytesPerSecond/2 {
		t.Fatalf("tail is %d bytes", len(tail))
	}
	if len(a.buf) > 2*audioLogKeep {
		t.Fatalf("log holds %d bytes", len(a.buf))
	}
}

func TestCommandSpeaker(t *testing.T) {
	var out bytes.Buffer
	speak := commandSpeaker(`n=$(wc -c); head -c $((n * 1000 + 1)) /dev/zero`, 16000)
	if err := speak(context.Background(), "abc", &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 3000 { // the odd last byte is not half a sample worth sending
		t.Fatalf("%d bytes of audio, want 3000", out.Len())
	}
	if err := commandSpeaker(`cat >/dev/null; echo "voice not found" >&2; exit 3`, 16000)(context.Background(), "abc", &out); err == nil || !strings.Contains(err.Error(), "voice not found") {
		t.Fatalf("error %v, want the command's complaint", err)
	}
	if err := commandSpeaker(`cat >/dev/null`, 16000)(context.Background(), "abc", &out); err == nil {
		t.Fatal("a silent command is not an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- commandSpeaker(`cat /dev/zero`, 16000)(ctx, "abc", io.Discard) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("error %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the speech command outlived its turn")
	}
}

// fakeChain is a scripted listener, thinker and speaker.
type fakeChain struct {
	mu             sync.Mutex
	transcripts    []string
	replies        [][]string
	sentenceFrames int
	stall          bool // the first answer never comes: think waits for its turn to be cancelled

	heardBytes []int
	asked      [][]chatMessage
}

func (f *fakeChain) chain() voiceChain {
	return voiceChain{
		listen: func(_ context.Context, pcm []byte) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.heardBytes = append(f.heardBytes, len(pcm))
			if len(f.transcripts) == 0 {
				return "", nil
			}
			text := f.transcripts[0]
			f.transcripts = f.transcripts[1:]
			return text, nil
		},
		think: func(ctx context.Context, msgs []chatMessage, sentence func(string)) error {
			f.mu.Lock()
			n := len(f.asked)
			f.asked = append(f.asked, msgs)
			var reply []string
			if n < len(f.replies) {
				reply = f.replies[n]
			}
			stall := f.stall && n == 0
			f.mu.Unlock()
			if stall {
				<-ctx.Done()
				return ctx.Err()
			}
			for _, s := range reply {
				sentence(s)
			}
			return nil
		},
		speak: func(_ context.Context, _ string, w io.Writer) error {
			_, err := w.Write(pcm16(tone(f.sentenceFrames, 0.3)))
			return err
		},
	}
}

func (f *fakeChain) questions() [][]chatMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]chatMessage(nil), f.asked...)
}

// testLine plays the media loop of a call much faster than real time: it pulls what the
// agent says and feeds what the peer says, silence unless told otherwise.
type testLine struct {
	t      *testing.T
	events lockedBuffer
	call   *fakeDuplexCall
	result chan error
	cancel context.CancelFunc

	mu   sync.Mutex
	peer [][]float32
	sent int // audible frames the agent sent
}

func startLine(t *testing.T, cfg converseConfig, chain voiceChain) *testLine {
	t.Helper()
	cfg.call.inRate, cfg.call.endSilence, cfg.call.answerTimeout = agentcaller.SampleRate, 300*time.Millisecond, time.Second
	cfg.call.bargeIn = !cfg.noBargeIn
	l := &testLine{t: t, call: newFakeDuplexCall(agentcaller.CallPhaseActive), result: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	t.Cleanup(cancel)
	c := newConversation(cfg, chain, &l.events)
	go func() { l.result <- c.run(ctx, l.call) }()
	src := l.call.source(t)
	l.call.mu.Lock()
	sink := l.call.sink
	l.call.mu.Unlock()
	go func() {
		silence := make([]float32, frameSamples)
		for ctx.Err() == nil {
			frame, _ := pull(src)
			in := silence
			l.mu.Lock()
			if frameRMS(frame) > 0 {
				l.sent++
			}
			if len(l.peer) > 0 {
				in, l.peer = l.peer[0], l.peer[1:]
			}
			l.mu.Unlock()
			_ = sink.WriteFrame(in)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return l
}

func (l *testLine) peerSays(frames int) {
	speech := tone(frames, 0.2)
	l.mu.Lock()
	for i := 0; i < frames; i++ {
		l.peer = append(l.peer, speech[i*frameSamples:(i+1)*frameSamples])
	}
	l.mu.Unlock()
}

func (l *testLine) sentFrames() int { l.mu.Lock(); defer l.mu.Unlock(); return l.sent }

func (l *testLine) all(name string) (found []event) {
	for _, line := range strings.Split(l.events.String(), "\n") {
		var e event
		if json.Unmarshal([]byte(line), &e) == nil && e["event"] == name {
			found = append(found, e)
		}
	}
	return found
}

// await waits for the n-th event of a kind (counting from one).
func (l *testLine) await(name string, n int) event {
	l.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if found := l.all(name); len(found) >= n {
			return found[n-1]
		}
	}
	l.t.Fatalf("no %s event number %d; events:\n%s", name, n, l.events.String())
	return nil
}

func (l *testLine) awaitSent(frames int) {
	l.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); l.sentFrames() < frames; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			l.t.Fatalf("the agent sent %d audible frames, want %d", l.sentFrames(), frames)
		}
	}
}

func contents(msgs []chatMessage) (out []string) {
	for _, m := range msgs {
		out = append(out, m.Role+": "+m.Content)
	}
	return out
}

// A whole call: greeting, a question that is answered in two sentences with the
// conversation so far as context, and a goodbye after which the agent hangs up.
func TestConversationTurns(t *testing.T) {
	chain := &fakeChain{
		transcripts:    []string{" Wie geht es dir? ", "Danke, tschüss!"},
		replies:        [][]string{{"Mir geht es gut.", "Und dir?"}, {"Bis bald."}},
		sentenceFrames: 5,
	}
	cfg := converseConfig{greeting: "Hallo, hier bin ich.", llm: LLMConfig{Prompt: "PERSONA"}}
	l := startLine(t, cfg, chain.chain())

	if e := l.await("reply", 1); e["text"] != "Hallo, hier bin ich." || e["greeting"] != true {
		t.Fatalf("greeting: %v", e)
	}
	l.awaitSent(5)
	l.peerSays(15)
	if e := l.await("heard", 1); e["text"] != "Wie geht es dir?" || e["at"] == nil {
		t.Fatalf("heard: %v", e)
	}
	e := l.await("reply", 2)
	if e["text"] != "Mir geht es gut. Und dir?" || e["interrupted"] != false {
		t.Fatalf("reply: %v", e)
	}
	if ms, ok := e["first_audio_ms"].(float64); !ok || ms < 0 || ms > 2000 {
		t.Fatalf("answer delay %v", e["first_audio_ms"])
	}
	l.awaitSent(15)
	l.peerSays(15)
	if e := l.await("reply", 3); e["text"] != "Bis bald." {
		t.Fatalf("reply: %v", e)
	}
	select {
	case err := <-l.result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not hang up after the goodbye")
	}
	if e := l.await("ended", 1); e["reason"] != "hangup command" {
		t.Fatalf("ended: %v", e)
	}
	if l.sentFrames() != 20 {
		t.Fatalf("the peer heard %d frames, want all 20", l.sentFrames())
	}

	asked := chain.questions()
	if len(asked) != 2 {
		t.Fatalf("%d questions to the model", len(asked))
	}
	if got, want := contents(asked[0]), []string{"system: PERSONA", "assistant: Hallo, hier bin ich.", "user: Wie geht es dir?"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first question %q", got)
	}
	if got := contents(asked[1]); len(got) != 5 || got[3] != "assistant: Mir geht es gut. Und dir?" || got[4] != "user: Danke, tschüss!" {
		t.Fatalf("second question %q", got)
	}
	// 0.9 s of speech with 240 ms before and 300 ms after it.
	for _, n := range chain.heardBytes {
		if d := time.Duration(n) * time.Second / pcmBytesPerSecond; d != 1440*time.Millisecond {
			t.Fatalf("the recogniser got %v of audio", d)
		}
	}
}

// The peer interrupts: the agent stops, only what it had said in full counts as said,
// and the interjection is answered next.
func TestConversationBargeIn(t *testing.T) {
	chain := &fakeChain{
		transcripts:    []string{"Erzähl mir etwas.", "Warte mal."},
		replies:        [][]string{{"Satz eins.", "Satz zwei.", "Satz drei.", "Satz vier."}, {"Ja?"}},
		sentenceFrames: 10,
	}
	l := startLine(t, converseConfig{llm: LLMConfig{Prompt: "P"}}, chain.chain())
	l.peerSays(10)
	l.awaitSent(3)
	l.peerSays(10)
	e := l.await("reply", 1)
	if e["interrupted"] != true || e["text"] == "" || strings.Contains(e["text"].(string), "vier") {
		t.Fatalf("reply: %v", e)
	}
	said := e["text"].(string)
	if e := l.await("reply", 2); e["text"] != "Ja?" || e["interrupted"] != false {
		t.Fatalf("second reply: %v", e)
	}
	if len(l.all("flushed")) == 0 {
		t.Fatalf("nothing was flushed; events:\n%s", l.events.String())
	}
	asked := chain.questions()
	want := []string{"system: P", "user: Erzähl mir etwas.", "assistant: " + said, "user: Warte mal."}
	if got := contents(asked[1]); !reflect.DeepEqual(got, want) {
		t.Fatalf("second question %q, want %q", got, want)
	}
	l.awaitSent(1)
	if sent := l.sentFrames(); sent >= 40 {
		t.Fatalf("the peer heard %d frames: the interrupted answer played to the end", sent)
	}
	l.cancel()
	if err := <-l.result; err != nil {
		t.Fatal(err)
	}
}

// The peer pauses and goes on before the agent has said anything: both parts are
// answered as one.
func TestConversationMergesTurns(t *testing.T) {
	chain := &fakeChain{
		transcripts:    []string{"Erstens", "und zweitens."},
		replies:        [][]string{nil, {"Verstanden."}},
		sentenceFrames: 5,
		stall:          true,
	}
	l := startLine(t, converseConfig{llm: LLMConfig{Prompt: "P"}}, chain.chain())
	l.peerSays(10)
	l.await("heard", 1)
	for len(chain.questions()) == 0 {
		time.Sleep(time.Millisecond)
	}
	l.peerSays(10)
	if e := l.await("reply", 1); e["text"] != "Verstanden." {
		t.Fatalf("reply: %v", e)
	}
	asked := chain.questions()
	if got, want := contents(asked[len(asked)-1]), []string{"system: P", "user: Erstens und zweitens."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("question %q, want %q", got, want)
	}
}

// With -no-barge-in the agent finishes its answer and takes the interjection afterwards.
func TestConversationNoBargeIn(t *testing.T) {
	chain := &fakeChain{
		transcripts:    []string{"Erzähl.", "Moment."},
		replies:        [][]string{{"Satz eins.", "Satz zwei.", "Satz drei."}, {"Ja?"}},
		sentenceFrames: 10,
	}
	l := startLine(t, converseConfig{noBargeIn: true, llm: LLMConfig{Prompt: "P"}}, chain.chain())
	l.peerSays(10)
	l.awaitSent(3)
	l.peerSays(10)
	if e := l.await("reply", 1); e["text"] != "Satz eins. Satz zwei. Satz drei." || e["interrupted"] != false {
		t.Fatalf("reply: %v", e)
	}
	l.await("reply", 2)
	l.awaitSent(40)
	if n := len(l.all("flushed")); n != 0 {
		t.Fatalf("%d flushes without barge-in", n)
	}
}

// The simulated caller says two things and waits for an answer after each.
func TestSimulatedCall(t *testing.T) {
	var peer []float32
	for i := 0; i < 2; i++ {
		peer = append(peer, make([]float32, 5*frameSamples)...)
		peer = append(peer, tone(15, 0.2)...)
		peer = append(peer, make([]float32, 30*frameSamples)...)
	}
	chain := &fakeChain{
		transcripts:    []string{"Eins?", "Zwei?"},
		replies:        [][]string{{"Antwort eins."}, {"Antwort zwei."}},
		sentenceFrames: 8,
	}
	cfg := converseConfig{greeting: "Hallo.", llm: LLMConfig{Prompt: "P"}}
	cfg.call.inRate, cfg.call.endSilence, cfg.call.bargeIn = agentcaller.SampleRate, 300*time.Millisecond, true
	var events lockedBuffer
	sim := newSimCall(peer, true)
	sim.tick = 3 * time.Millisecond
	c := newConversation(cfg, chain.chain(), &events)
	done := make(chan error, 1)
	go func() { done <- c.run(context.Background(), sim) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the simulated call did not end; events:\n%s", events.String())
	}
	var replies []string
	for _, line := range strings.Split(events.String(), "\n") {
		var e event
		if json.Unmarshal([]byte(line), &e) == nil && e["event"] == "reply" {
			replies = append(replies, fmt.Sprint(e["text"], " ", e["interrupted"]))
		}
	}
	if want := []string{"Hallo. <nil>", "Antwort eins. false", "Antwort zwei. false"}; !reflect.DeepEqual(replies, want) {
		t.Fatalf("replies %q, want %q; events:\n%s", replies, want, events.String())
	}
	audible := 0
	rec := sim.recording()
	for i := 0; i+frameSamples <= len(rec); i += frameSamples {
		if frameRMS(rec[i:i+frameSamples]) > 0 {
			audible++
		}
	}
	if audible != 3*8 {
		t.Fatalf("%d audible frames recorded, want 24", audible)
	}
}

func TestParseConverseArgs(t *testing.T) {
	var stderr bytes.Buffer
	cfg, err := parseConverseArgs([]string{"-tts-cmd", "say", "-tts-rate", "24000", "-greeting", "Hallo", "+41790000000"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.call.target != "+41790000000" || cfg.call.inRate != 24000 || !cfg.call.bargeIn || !strings.HasSuffix(cfg.llm.Prompt, phoneRules) {
		t.Fatalf("config %+v", cfg)
	}
	for _, bad := range [][]string{
		{"+41790000000"},    // no speech command
		{"-tts-cmd", "say"}, // nobody to call
		{"-tts-cmd", "say", "-record", "a.wav", "x"}, // -record without -simulate
		{"-tts-cmd", "say", "-simulate", "a.wav", "x"},
		{"-tts-cmd", "say", "-simulate", "a.wav", "-announce", "Alice ruft an"},
		{"-tts-cmd", "say", "-answer", "-announce", "Alice ruft an", "any"},
		{"-tts-cmd", "say", "-persona", "/nonexistent/persona.md", "x"},
	} {
		if _, err := parseConverseArgs(bad, &stderr); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
	for text, want := range map[string]bool{
		"Danke, tschüss!": true, "Also dann, bis bald.": true, "Ich bleibe bis später im Büro": false, "Ciao ist italienisch, oder?": false,
	} {
		if farewell.MatchString(text) != want {
			t.Fatalf("farewell(%q) = %v", text, !want)
		}
	}
}

// A short noise right after a question stops the answer that was on its way. It is not
// speech, so the question is still answered.
func TestConversationAnswersAfterNoise(t *testing.T) {
	chain := &fakeChain{
		transcripts:    []string{"Ist alles gut bei dir?", "Untertitelung des ZDF, 2020"},
		replies:        [][]string{nil, {"Ja, alles gut."}},
		sentenceFrames: 5,
		stall:          true,
	}
	l := startLine(t, converseConfig{llm: LLMConfig{Prompt: "P"}}, chain.chain())
	l.peerSays(10)
	l.await("heard", 1)
	for len(chain.questions()) == 0 {
		time.Sleep(time.Millisecond)
	}
	l.peerSays(3)
	if e := l.await("heard", 2); e["ignored"] != "Untertitelung des ZDF, 2020" {
		t.Fatalf("the noise was not ignored: %v", e)
	}
	if e := l.await("reply", 1); e["text"] != "Ja, alles gut." {
		t.Fatalf("reply: %v", e)
	}
	asked := chain.questions()
	if got, want := contents(asked[len(asked)-1]), []string{"system: P", "user: Ist alles gut bei dir?"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("question %q, want %q", got, want)
	}
}

// Noise with nothing unanswered before it gets no answer.
func TestConversationIgnoresLoneNoise(t *testing.T) {
	chain := &fakeChain{transcripts: []string{"Untertitelung des ZDF, 2020", "Hallo?"}, replies: [][]string{{"Hallo."}}, sentenceFrames: 5}
	l := startLine(t, converseConfig{llm: LLMConfig{Prompt: "P"}}, chain.chain())
	l.peerSays(5)
	l.await("heard", 1)
	l.peerSays(8)
	if e := l.await("reply", 1); e["text"] != "Hallo." {
		t.Fatalf("reply: %v", e)
	}
	if asked := chain.questions(); len(asked) != 1 || !reflect.DeepEqual(contents(asked[0]), []string{"system: P", "user: Hallo?"}) {
		t.Fatalf("questions: %v", asked)
	}
}
