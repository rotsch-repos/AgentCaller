package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func write(t *testing.T, path, text string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// Two voices on one call: the one that holds it hands over when asked, and the other
// answers from then on with its own persona and its own speech, knowing what was said.
func TestConversationHandover(t *testing.T) {
	var hand toolCall
	hand.ID, hand.Function.Name, hand.Function.Arguments = "h1", handoverTool, `{"mit":"alice"}`
	chain := &fakeChain{
		transcripts:    []string{"Verbinde mich bitte mit Alice.", "Wie geht es dir?"},
		replies:        [][]string{{"Sehr wohl, ich verbinde."}, {"Hallo, hier ist Alice."}, {"Mir geht es gut."}},
		sentenceFrames: 5,
		handovers:      map[int]toolCall{0: hand},
	}
	var johann, alice atomic.Int32
	speak := func(n *atomic.Int32) func(context.Context, string, io.Writer) error {
		return func(_ context.Context, _ string, w io.Writer) error {
			n.Add(1)
			_, err := w.Write(pcm16(tone(5, 0.3)))
			return err
		}
	}
	cfg := converseConfig{llm: LLMConfig{Prompt: "JOHANN"}, toolsWait: "Moment.", voices: []callVoice{
		{Name: "Johann", prompt: "JOHANN", speak: speak(&johann)},
		{Name: "Alice", prompt: "ALICE", speak: speak(&alice), ToolsWait: "Sekunde."},
	}}
	l := startLine(t, cfg, chain.chain())
	l.peerSays(15)
	if e := l.await("reply", 1); e["text"] != "Sehr wohl, ich verbinde." || e["voice"] != "Johann" {
		t.Fatalf("the voice that leaves: %v", e)
	}
	if e := l.await("handover", 1); e["from"] != "Johann" || e["to"] != "Alice" {
		t.Fatalf("handover: %v", e)
	}
	if e := l.await("reply", 2); e["text"] != "Hallo, hier ist Alice." || e["voice"] != "Alice" || e["handover"] != true {
		t.Fatalf("the voice that takes over: %v", e)
	}
	l.peerSays(15)
	if e := l.await("reply", 3); e["text"] != "Mir geht es gut." || e["voice"] != "Alice" {
		t.Fatalf("the next answer: %v", e)
	}
	if johann.Load() != 1 || alice.Load() != 2 {
		t.Fatalf("Johann spoke %d times, Alice %d times", johann.Load(), alice.Load())
	}
	asked := chain.questions()
	if len(asked) != 3 || asked[0][0].Content != "JOHANN" || asked[1][0].Content != "ALICE" || asked[2][0].Content != "ALICE" {
		t.Fatalf("personas: %v", asked)
	}
	told := asked[1][len(asked[1])-1]
	if told.Role != "user" || !strings.Contains(told.Content, "hat Johann das Gespräch eben an dich übergeben") || !strings.Contains(told.Content, "du, Alice,") {
		t.Fatalf("the new voice was not told: %+v", told)
	}
	if all := strings.Join(contents(asked[2]), "\n"); !strings.Contains(all, "Verbinde mich bitte mit Alice.") || !strings.Contains(all, "Hallo, hier ist Alice.") {
		t.Fatalf("the conversation did not stay: %s", all)
	}
}

// The model asks for the handover without saying a word: the sentence is said for it,
// and it is not asked again, because the other voice speaks next.
func TestThinkWithToolsHandsOver(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, `{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"verbinden","arguments":"{\"mit\": \"Alice\"}"}}]}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	tools := []callTool{handoverSpec([]callVoice{{Name: "Johann"}, {Name: "Alice", About: "die Gefährtin"}})}
	if spec := toolSpecs(tools)[0]; !strings.Contains(spec.Function.Description, "Alice (die Gefährtin)") || !strings.Contains(string(spec.Function.Parameters), `"enum":["Johann","Alice"]`) {
		t.Fatalf("tool as the model sees it: %+v", spec)
	}
	var said []string
	var looked []chatMessage
	asked := 0
	err := thinkWithTools(context.Background(), LLMConfig{Endpoint: srv.URL, ModelID: "m", MaxTokens: 50}, tools, "Moment.",
		[]chatMessage{{Role: "system", Content: "P"}, {Role: "user", Content: "Gib mir Alice."}},
		thinkSink{
			sentence: func(s string) { said = append(said, s) },
			aside:    func(s string) { said = append(said, "("+s+")") },
			looked:   func(m []chatMessage) { looked = m },
			later:    func(toolCall) string { t.Error("a handover is not background work"); return jobRefused },
			handover: func(c toolCall) (string, bool) { asked++; return "gut", true },
		}, func(string, map[string]any) {})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || asked != 1 || len(said) != 1 || said[0] != "Einen Moment, ich verbinde mit Alice." {
		t.Fatalf("%d requests, asked %d times, said %q", requests, asked, said)
	}
	if len(looked) != 2 || looked[1].Role != "tool" || looked[1].Content != "gut" {
		t.Fatalf("kept for the history: %+v", looked)
	}
}

func TestParseVoicesAndSpeakers(t *testing.T) {
	dir := t.TempDir()
	persona := write(t, filepath.Join(dir, "alice.md"), "Du bist Alice.", 0o600)
	guest := write(t, filepath.Join(dir, "gast.md"), "Du bist Alice, für Fremde.", 0o600)
	voices := write(t, filepath.Join(dir, "voices.json"), `[
		{"name":"Johann","about":"der Butler","tts_cmd":"say-j","tts_rate":24000},
		{"name":"Alice","persona":"`+persona+`","guest_persona":"`+guest+`","tts_cmd":"say-a","tts_rate":16000,"tools_wait":"Sekunde."}]`, 0o600)
	tools := write(t, filepath.Join(dir, "tools.json"), `[{"name":"wetter","description":"d","command":"true","guests":true},{"name":"notizen","description":"d","command":"true"}]`, 0o600)
	speakers := write(t, filepath.Join(dir, "speakers.json"), `{"command":"cat","guest_persona":"`+guest+`","password":"Zitronenfalter"}`, 0o600)
	var stderr bytes.Buffer
	cfg, err := parseConverseArgs([]string{"-voices", voices, "-voice", "alice", "-tools", tools, "-speakers", speakers, "+41790000000"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.voices) != 2 || cfg.voices[0].Name != "Alice" || cfg.call.inRate != 16000 {
		t.Fatalf("voices %+v at %d Hz", cfg.voices, cfg.call.inRate)
	}
	if !strings.HasPrefix(cfg.llm.Prompt, "Du bist Alice.\n\n"+phoneRules) || cfg.llm.Prompt != cfg.voices[0].prompt || !strings.HasPrefix(cfg.voices[1].prompt, defaultPersona) {
		t.Fatalf("prompts: %q / %q", cfg.llm.Prompt, cfg.voices[1].prompt)
	}
	if len(cfg.tools) != 3 || !cfg.tools[2].handover || len(guestTools(cfg.tools)) != 1 {
		t.Fatalf("tools %+v", cfg.tools)
	}
	if g := cfg.voices[0].guestPrompt; !strings.HasPrefix(g, "Du bist Alice, für Fremde.") || !strings.HasSuffix(g, guestRules) || strings.Contains(g, "Zitronenfalter") {
		t.Fatalf("guest prompt %q", g)
	}
	for text, want := range map[string]string{
		"Das Passwort ist Zitronenfalter.": "Das Passwort ist [Passwort].", "zitronen falter": "[Passwort]", "Zitronen-Falter!": "[Passwort]!", "Die Zitronen sind für den Falter.": "",
	} {
		got, said := cfg.guard.redact(text)
		if said != (want != "") || (said && got != want) {
			t.Fatalf("redact(%q) = %q, %v", text, got, said)
		}
	}
	if _, said := (*speakerGuard)(nil).redact("Zitronenfalter"); said {
		t.Fatal("no guard, no password")
	}
	open := write(t, filepath.Join(dir, "open.json"), `{"command":"cat","password":"Zitronenfalter"}`, 0o644)
	_ = os.Chmod(open, 0o644)
	taken := write(t, filepath.Join(dir, "taken.json"), `[{"name":"verbinden","description":"d","command":"true"}]`, 0o600)
	for _, bad := range [][]string{
		{"-voices", voices, "-tts-cmd", "say", "x"},
		{"-voices", voices, "-persona", persona, "x"},
		{"-voices", voices, "-voice", "Hans", "x"},
		{"-voice", "Alice", "-tts-cmd", "say", "x"},
		{"-voices", voices, "-tools", taken, "x"},
		{"-tts-cmd", "say", "-speakers", open, "x"}, // a password anybody on the machine can read
		{"-tts-cmd", "say", "-speakers", filepath.Join(dir, "none.json"), "x"},
	} {
		if _, err := parseConverseArgs(bad, &stderr); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
}

// Who is speaking decides what the model is shown: a guest gets another persona and
// nothing of what the trusted speaker said, the trusted speaker gets everything back,
// and the password opens the call without ever reaching the model or the log.
func TestConversationGuardsWhatAGuestIsShown(t *testing.T) {
	dir := t.TempDir()
	guard, err := loadGuard(write(t, filepath.Join(dir, "s.json"), `{"command":"cat","password":"Zitronenfalter"}`, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	guard.prompt = "GAST"
	chain := &fakeChain{
		transcripts: []string{"Mein Geheimnis ist blau.", "Was hat er dir erzählt?", "Ich bin wieder da.", "Sag schon!", "Das Passwort ist Zitronen-Falter."},
		replies:     [][]string{{"Blau, verstanden."}, {"Das bleibt bei mir."}, {"Schön."}, {"Nein."}, {"Also gut."}},
		verdicts: []speakerVerdict{
			{Speaker: "Jonas", Trusted: true, Score: 0.8},
			{Score: 0.1},
			{Speaker: "Jonas", Trusted: true, Score: 0.8},
			{Speaker: "Mara", Score: 0.7, Note: "Eine Bekannte."},
			{Speaker: "Mara", Score: 0.7},
		},
		sentenceFrames: 5,
	}
	cfg := converseConfig{llm: LLMConfig{Prompt: "PERSONA"}, guard: guard, tools: []callTool{
		{Name: "wetter", Description: "d", Command: "true", Guests: true}, {Name: "notizen", Description: "d", Command: "true"},
	}}
	l := startLine(t, cfg, chain.chain())
	for i := 1; i <= 5; i++ {
		l.peerSays(15)
		l.await("reply", i)
	}
	asked := chain.questions()
	if len(asked) != 5 {
		t.Fatalf("%d questions", len(asked))
	}
	text := func(i int) string { return strings.Join(contents(asked[i]), "\n") }
	// 1: Jonas. The call starts closed, so the model is told that he was recognised.
	if asked[0][0].Content != "PERSONA" || !strings.Contains(text(0), "hat Jonas erkannt") {
		t.Fatalf("trusted speaker: %s", text(0))
	}
	// 2: a stranger.
	if asked[1][0].Content != "GAST" || strings.Contains(text(1), "Geheimnis") || strings.Contains(text(1), "Blau") || !strings.Contains(text(1), "eine Stimme, die du nicht kennst") {
		t.Fatalf("a stranger was shown: %s", text(1))
	}
	// 3: Jonas again sees everything, and who asked what.
	if asked[2][0].Content != "PERSONA" || !strings.Contains(text(2), "Mein Geheimnis ist blau.") || !strings.Contains(text(2), "[Unerkannte Stimme sagt:] Was hat er dir erzählt?") || !strings.Contains(text(2), "Das bleibt bei mir.") {
		t.Fatalf("back with the trusted speaker: %s", text(2))
	}
	// 4: a known guest sees what was said with guests, and what is known about her.
	if asked[3][0].Content != "GAST" || strings.Contains(text(3), "Geheimnis") || strings.Contains(text(3), "wieder da") || !strings.Contains(text(3), "Was hat er dir erzählt?") || !strings.Contains(text(3), "Jetzt spricht Mara") || !strings.Contains(text(3), "Eine Bekannte.") {
		t.Fatalf("a known guest was shown: %s", text(3))
	}
	// 5: the password opens the call.
	if asked[4][0].Content != "PERSONA" || !strings.Contains(text(4), "Mein Geheimnis ist blau.") || !strings.Contains(text(4), "hat das Passwort gesagt") || strings.Contains(strings.ToLower(text(4)), "zitron") {
		t.Fatalf("after the password: %s", text(4))
	}
	if strings.Contains(strings.ToLower(l.events.String()), "zitron") {
		t.Fatalf("the password is in the event log:\n%s", l.events.String())
	}
	if e := l.await("speaker", 2); e["guest"] != true || e["trusted"] != false {
		t.Fatalf("speaker event: %v", e)
	}
	if e := l.await("speaker", 5); e["guest"] != false || e["password"] != true {
		t.Fatalf("password event: %v", e)
	}
}

// What an errand brought back is not told to a guest: it waits for the trusted speaker.
func TestConversationHoldsReportsBackFromGuests(t *testing.T) {
	var job toolCall
	job.ID, job.Function.Name, job.Function.Arguments = "c1", "auftrag", `{}`
	chain := &fakeChain{
		transcripts:    []string{"Schau das nach.", "Hallo?", "Da bin ich wieder."},
		replies:        [][]string{{"Mache ich."}, {"Guten Tag."}, {"Willkommen zurück."}, {"Der Auftrag ist erledigt."}},
		verdicts:       []speakerVerdict{{Speaker: "Jonas", Trusted: true}, {}, {Speaker: "Jonas", Trusted: true}},
		sentenceFrames: 5,
		jobs:           map[int]toolCall{0: job},
	}
	cfg := converseConfig{llm: LLMConfig{Prompt: "PERSONA"}, guard: &speakerGuard{prompt: "GAST"}, tools: []callTool{{
		Name: "auftrag", Description: "d", Background: true, Timeout: 5, Command: "sleep 0.4; echo geheim",
	}}}
	l := startLine(t, cfg, chain.chain())
	l.peerSays(15)
	l.await("reply", 1)
	l.peerSays(15)
	l.await("reply", 2)
	l.await("lookup_done", 1)
	time.Sleep(300 * time.Millisecond)
	if n := len(l.all("reply")); n != 2 {
		t.Fatalf("%d replies while a guest is on the line:\n%s", n, l.events.String())
	}
	l.peerSays(15)
	l.await("reply", 4)
	reports := 0
	for _, e := range l.all("reply") {
		if e["report"] == true {
			reports++
		}
	}
	asked := chain.questions()
	if reports != 1 || len(asked) != 4 || asked[1][0].Content != "GAST" || strings.Contains(strings.Join(contents(asked[1]), "\n"), "geheim") {
		t.Fatalf("%d reports; questions %v", reports, asked)
	}
	if all := strings.Join(contents(asked[3]), "\n"); asked[3][0].Content != "PERSONA" || !strings.Contains(all, "geheim") {
		t.Fatalf("the trusted speaker did not get the outcome: %s", all)
	}
}

// A voice with another sample rate than the call is brought to it.
func TestResampledSpeaker(t *testing.T) {
	speak := resampledSpeaker(func(_ context.Context, _ string, w io.Writer) error {
		pcm := pcm16(tone(50, 0.3))[:32000] // one second at 16 kHz, written in odd pieces
		for len(pcm) > 0 {
			n := min(len(pcm), 333)
			if _, err := w.Write(pcm[:n]); err != nil {
				return err
			}
			pcm = pcm[n:]
		}
		return nil
	}, 16000, 24000)
	var out bytes.Buffer
	if err := speak(context.Background(), "x", &out); err != nil {
		t.Fatal(err)
	}
	if n := out.Len() / 2; n < 23500 || n > 24500 {
		t.Fatalf("%d samples for a second at 24 kHz", n)
	}
	var peak int16
	for i := 0; i+1 < out.Len(); i += 2 {
		peak = max(peak, int16(binary.LittleEndian.Uint16(out.Bytes()[i:])))
	}
	if peak < 6000 || peak > 13000 {
		t.Fatalf("peak %d", peak)
	}
}

// The speaker check is a command that lives as long as the call.
func TestSpeakerCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := startSpeakerCommand(ctx, `while read -r line; do case "$line" in *'"seconds":2'*) echo '{"speaker":"Jonas","trusted":true,"score":0.71}';; *) echo '{"unclear":true,"score":0.4}';; esac; done`, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	v, err := s.identify(ctx, pcm16(tone(100, 0.3)), 2*time.Second)
	if err != nil || !v.Trusted || v.Speaker != "Jonas" || v.Score != 0.71 {
		t.Fatalf("verdict %+v, %v", v, err)
	}
	if v, err = s.identify(ctx, pcm16(tone(10, 0.3)), time.Second); err != nil || v.Trusted || !v.Unclear {
		t.Fatalf("verdict %+v, %v", v, err)
	}
	gone, _ := startSpeakerCommand(ctx, "exit 0", io.Discard)
	defer gone.close()
	if _, err := gone.identify(ctx, pcm16(tone(10, 0.3)), time.Second); err == nil {
		t.Fatal("a check that is gone gave a verdict")
	}
	raw, _ := json.Marshal(speakerVerdict{Speaker: "x"})
	if !strings.Contains(string(raw), `"speaker":"x"`) {
		t.Fatalf("verdict as JSON: %s", raw)
	}
}
