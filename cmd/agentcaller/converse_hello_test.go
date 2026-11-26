package main

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countSpeech wraps the speech command of a chain and counts how often it runs.
func countSpeech(chain voiceChain) (voiceChain, *atomic.Int32) {
	var n atomic.Int32
	speak := chain.speak
	chain.speak = func(ctx context.Context, text string, w io.Writer) error {
		n.Add(1)
		return speak(ctx, text, w)
	}
	return chain, &n
}

// With -greeting-after-hello the agent is silent until whoever picked up has said a
// word. The greeting answers that word at once, from audio made while it rang, without
// asking the model; the word and the greeting are in the history for what follows.
func TestGreetingWaitsForHello(t *testing.T) {
	fake := &fakeChain{
		transcripts:    []string{"Hallo?", "Wie geht es dir?"},
		replies:        [][]string{{"Mir geht es gut."}},
		sentenceFrames: 5,
	}
	chain, spoken := countSpeech(fake.chain())
	cfg := converseConfig{greeting: "Hallo Mara, hier bin ich.", greetHello: 30 * time.Second, llm: LLMConfig{Prompt: "PERSONA"}}
	l := startLine(t, cfg, chain)

	if e := l.await("greeting_ready", 1); e["error"] != nil || e["audio_ms"].(float64) <= 0 {
		t.Fatalf("greeting made ahead: %v", e)
	}
	time.Sleep(300 * time.Millisecond)
	if n, sent := len(l.all("reply")), l.sentFrames(); n != 0 || sent != 0 {
		t.Fatalf("the agent did not wait for the first word: %d replies, %d frames", n, sent)
	}
	l.peerSays(10)
	e := l.await("reply", 1)
	if e["text"] != "Hallo Mara, hier bin ich." || e["greeting"] != true {
		t.Fatalf("greeting after the hello: %v", e)
	}
	if ms, ok := e["first_audio_ms"].(float64); !ok || ms < 0 || ms > 2000 {
		t.Fatalf("greeting delay %v", e["first_audio_ms"])
	}
	if n := len(fake.questions()); n != 0 {
		t.Fatalf("the model was asked %d times for a greeting", n)
	}
	if n := spoken.Load(); n != 1 {
		t.Fatalf("the greeting was spoken %d times, want once, ahead", n)
	}
	l.awaitSent(5)
	l.peerSays(15)
	if e := l.await("reply", 2); e["text"] != "Mir geht es gut." {
		t.Fatalf("reply: %v", e)
	}
	asked := fake.questions()
	if len(asked) != 1 {
		t.Fatalf("%d questions", len(asked))
	}
	got := contents(asked[0])
	want := []string{"system: PERSONA", "user: Hallo?", "assistant: Hallo Mara, hier bin ich.", "user: Wie geht es dir?"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the model was shown\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	l.cancel()
	if err := <-l.result; err != nil {
		t.Fatal(err)
	}
}

// Whoever picks up and says nothing is greeted when the wait is over.
func TestGreetingComesByItself(t *testing.T) {
	fake := &fakeChain{sentenceFrames: 5}
	cfg := converseConfig{greeting: "Hallo, hier bin ich.", greetHello: 150 * time.Millisecond, greetAfter: time.Hour, llm: LLMConfig{Prompt: "P"}}
	l := startLine(t, cfg, fake.chain())
	e := l.await("reply", 1)
	if e["text"] != "Hallo, hier bin ich." || e["greeting"] != true {
		t.Fatalf("greeting: %v", e)
	}
	if _, ok := e["first_audio_ms"]; ok {
		t.Fatalf("nobody was waiting, but the greeting has a delay: %v", e)
	}
	l.awaitSent(5)
	l.cancel()
	if err := <-l.result; err != nil {
		t.Fatal(err)
	}
}

// Who opens with more than a word to pick up has opened the conversation: the model
// answers, and no greeting follows, not even when the wait runs out.
func TestGreetingGivesWayToAQuestion(t *testing.T) {
	fake := &fakeChain{
		transcripts:    []string{"Hallo Mara, hast du kurz Zeit für mich?"},
		replies:        [][]string{{"Ja, klar."}},
		sentenceFrames: 5,
	}
	cfg := converseConfig{greeting: "Hallo, hier bin ich.", greetHello: 400 * time.Millisecond, llm: LLMConfig{Prompt: "P"}}
	l := startLine(t, cfg, fake.chain())
	l.peerSays(15)
	if e := l.await("reply", 1); e["text"] != "Ja, klar." || e["greeting"] != nil {
		t.Fatalf("reply: %v", e)
	}
	time.Sleep(500 * time.Millisecond) // past the wait
	if n := len(l.all("reply")); n != 1 {
		t.Fatalf("%d replies, want the answer alone; events:\n%s", n, l.events.String())
	}
	l.cancel()
	if err := <-l.result; err != nil {
		t.Fatal(err)
	}
}

// The click of picking up is not a first word: the greeting keeps waiting, and comes
// when the wait is over.
func TestGreetingHeldThroughNoise(t *testing.T) {
	fake := &fakeChain{transcripts: []string{"Untertitelung des ZDF, 2020"}, sentenceFrames: 5}
	cfg := converseConfig{greeting: "Hallo, hier bin ich.", greetHello: 1500 * time.Millisecond, llm: LLMConfig{Prompt: "P"}}
	l := startLine(t, cfg, fake.chain())
	l.peerSays(10)
	if e := l.await("heard", 1); e["ignored"] != "Untertitelung des ZDF, 2020" {
		t.Fatalf("heard: %v", e)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(l.all("reply")); n != 0 {
		t.Fatalf("a noise was greeted; events:\n%s", l.events.String())
	}
	if e := l.await("reply", 1); e["greeting"] != true {
		t.Fatalf("greeting after the wait: %v", e)
	}
	l.cancel()
	if err := <-l.result; err != nil {
		t.Fatal(err)
	}
}

// The first word counts for the speaker check like any other, and what a guest said to
// pick up is marked as such for the trusted speaker later.
func TestGreetingAfterHelloKeepsTheGuard(t *testing.T) {
	fake := &fakeChain{
		transcripts:    []string{"Ja, hallo?", "Wer hat vorhin abgenommen?"},
		replies:        [][]string{{"Das weiss ich nicht."}},
		verdicts:       []speakerVerdict{{Score: 0.1}, {Speaker: "Jonas", Trusted: true, Score: 0.8}},
		sentenceFrames: 5,
	}
	cfg := converseConfig{greeting: "Hallo, hier bin ich.", greetHello: 30 * time.Second, llm: LLMConfig{Prompt: "PERSONA"},
		guard: &speakerGuard{prompt: "GAST"}}
	l := startLine(t, cfg, fake.chain())
	l.peerSays(10)
	if e := l.await("reply", 1); e["greeting"] != true {
		t.Fatalf("greeting: %v", e)
	}
	if e := l.await("speaker", 1); e["guest"] != true {
		t.Fatalf("speaker event for the first word: %v", e)
	}
	l.awaitSent(5)
	l.peerSays(15)
	l.await("reply", 2)
	asked := fake.questions()
	if len(asked) != 1 {
		t.Fatalf("%d questions", len(asked))
	}
	text := strings.Join(contents(asked[0]), "\n")
	if asked[0][0].Content != "PERSONA" || !strings.Contains(text, "user: [Unerkannte Stimme sagt:] Ja, hallo?") || !strings.Contains(text, "assistant: Hallo, hier bin ich.") {
		t.Fatalf("the trusted speaker was shown: %s", text)
	}
}

func TestOpensTheCall(t *testing.T) {
	for said, want := range map[string]bool{
		"Hallo?": true, "Ja, bitte?": true, "Hallo, hier ist Jonas.": true, "Ja?": true,
		"Hallo Mara, hast du kurz Zeit für mich?": false, "Kannst du mir sagen, wie spät es ist?": false,
	} {
		if got := opensTheCall(said); got != want {
			t.Errorf("opensTheCall(%q) = %v, want %v", said, got, want)
		}
	}
}

func TestGreetingAfterHelloNeedsAGreeting(t *testing.T) {
	_, err := parseConverseArgs([]string{"-tts-cmd", "cat", "-greeting-after-hello", "5s", "+41790000000"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-greeting-after-hello") {
		t.Fatalf("error %v", err)
	}
}
