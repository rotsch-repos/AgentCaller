package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sse writes one streamed chat chunk.
func sse(w io.Writer, delta string) {
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s}]}\n\n", delta)
}

// The model asks for the weather in pieces, as vLLM streams it; the tool runs, its
// result goes back, and the answer that follows is spoken. While the tool runs the
// agent says that it is looking.
func TestThinkWithTools(t *testing.T) {
	var requests []chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			sse(w, `{"content":"Gute Frage. "}`)
			sse(w, `{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"wetter","arguments":""}}]}`)
			sse(w, `{"tool_calls":[{"index":0,"function":{"arguments":"{\"ort\": \"Wett"}}]}`)
			sse(w, `{"tool_calls":[{"index":0,"function":{"arguments":"ingen\"}"}}]}`)
		} else {
			sse(w, `{"content":"In Wettingen sind es vier Grad. "}`)
			sse(w, `{"content":"Zieh dich warm an."}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	tools := []callTool{{
		Name: "wetter", Description: "Wetter an einem Ort", Timeout: 5,
		Parameters: json.RawMessage(`{"type":"object","properties":{"ort":{"type":"string"}}}`),
		Command:    `printf 'Gefragt: '; cat; printf ' -> 4 Grad, bedeckt'`,
	}}
	var said []string
	var notes []map[string]any
	var looked []chatMessage
	msgs := []chatMessage{{Role: "system", Content: "P"}, {Role: "user", Content: "Wie ist das Wetter in Wettingen?"}}
	err := thinkWithTools(context.Background(), LLMConfig{Endpoint: srv.URL, ModelID: "m", MaxTokens: 50}, tools, "Moment, ich schaue nach.", msgs,
		thinkSink{
			sentence: func(s string) { said = append(said, s) },
			aside:    func(s string) { said = append(said, "("+s+")") },
			looked:   func(m []chatMessage) { looked = append(looked, m...) },
		},
		func(event string, fields map[string]any) { notes = append(notes, fields) })
	if err != nil {
		t.Fatal(err)
	}
	// The model spoke before it looked, so there is nothing to bridge with "one moment".
	if want := []string{"Gute Frage.", "In Wettingen sind es vier Grad.", "Zieh dich warm an."}; !reflect.DeepEqual(said, want) {
		t.Fatalf("said %q, want %q", said, want)
	}
	if len(looked) != 2 || len(looked[0].ToolCalls) != 1 || looked[0].Content != "" || looked[1].Role != "tool" {
		t.Fatalf("the lookup to remember: %+v", looked)
	}
	if len(requests) != 2 || len(requests[0].Tools) != 1 || requests[0].Tools[0].Function.Name != "wetter" {
		t.Fatalf("requests %+v", requests)
	}
	second := requests[1].Messages
	if len(second) != 4 || second[2].Role != "assistant" || second[2].Content != "Gute Frage." || len(second[2].ToolCalls) != 1 || second[2].ToolCalls[0].ID != "call-1" ||
		second[2].ToolCalls[0].Function.Arguments != `{"ort": "Wettingen"}` {
		t.Fatalf("second request does not carry the call: %+v", second)
	}
	if second[3].Role != "tool" || second[3].ToolCallID != "call-1" || second[3].Content != `Gefragt: {"ort": "Wettingen"} -> 4 Grad, bedeckt` {
		t.Fatalf("second request does not carry the result: %+v", second[3])
	}
	if len(notes) != 1 || notes[0]["name"] != "wetter" || notes[0]["error"] != nil {
		t.Fatalf("notes %v", notes)
	}
	if len(msgs) != 2 {
		t.Fatal("the lookup leaked into the caller's messages")
	}
}

// A tool that fails or does not exist is no reason to fall silent: the model is told
// and answers anyway. A model that never stops asking is cut off.
func TestThinkWithToolsFailuresAndLimit(t *testing.T) {
	asked := 0
	var last chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		last = chatRequest{}
		_ = json.NewDecoder(r.Body).Decode(&last)
		if len(last.Tools) > 0 {
			name := "kaputt"
			if asked == 2 {
				name = "gibtsnicht"
			}
			sse(w, fmt.Sprintf(`{"tool_calls":[{"index":0,"id":"c%d","function":{"name":%q,"arguments":"{}"}}]}`, asked, name))
		} else {
			sse(w, `{"content":"Das weiss ich leider nicht."}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	tools := []callTool{{Name: "kaputt", Description: "x", Timeout: 5, Command: "echo Netz weg >&2; exit 3"}}
	var said []string
	var notes []map[string]any
	err := thinkWithTools(context.Background(), LLMConfig{Endpoint: srv.URL, ModelID: "m"}, tools, "", []chatMessage{{Role: "user", Content: "?"}},
		thinkSink{sentence: func(s string) { said = append(said, s) }}, func(_ string, f map[string]any) { notes = append(notes, f) })
	if err != nil {
		t.Fatal(err)
	}
	if asked != maxToolRounds+1 || len(last.Tools) != 0 {
		t.Fatalf("%d requests, the last with %d tools", asked, len(last.Tools))
	}
	if !reflect.DeepEqual(said, []string{"Das weiss ich leider nicht."}) {
		t.Fatalf("said %q", said)
	}
	if len(notes) != maxToolRounds || !strings.Contains(fmt.Sprint(notes[0]["error"]), "Netz weg") || !strings.Contains(fmt.Sprint(notes[1]["error"]), "gibtsnicht") {
		t.Fatalf("notes %v", notes)
	}
	var toolMsgs int
	for _, m := range last.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.Content, "Das Nachschauen ist fehlgeschlagen") {
			toolMsgs++
		}
	}
	if toolMsgs != maxToolRounds {
		t.Fatalf("the model was told of %d failures, want %d", toolMsgs, maxToolRounds)
	}
}

func TestRunToolTimeoutAndCap(t *testing.T) {
	slow := []callTool{{Name: "lahm", Command: "sleep 5", Timeout: 1}}
	start := time.Now()
	var call toolCall
	call.Function.Name = "lahm"
	if _, err := runTool(context.Background(), slow, call); err == nil || !strings.Contains(err.Error(), "within 1 seconds") {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("the timeout did not stop the command")
	}
	big := []callTool{{Name: "viel", Command: "head -c 20000 /dev/zero | tr '\\0' 'ä'", Timeout: 5}}
	call.Function.Name, call.Function.Arguments = "viel", "{}"
	out, err := runTool(context.Background(), big, call)
	if err != nil || len(out) > maxToolOutput+8 || !strings.HasSuffix(out, "…") || strings.ContainsRune(out, '�') {
		t.Fatalf("len %d err %v", len(out), err)
	}
	call.Function.Arguments = "{kein json"
	if _, err := runTool(context.Background(), big, call); err == nil {
		t.Fatal("broken arguments were passed on")
	}
}

func TestLoadTools(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		p := filepath.Join(dir, "tools.json")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tools, err := loadTools(write(`[{"name":"wetter","description":"Wetter","command":"wetter"}]`))
	if err != nil || len(tools) != 1 || tools[0].Timeout != defaultToolTimeout || !json.Valid(tools[0].Parameters) {
		t.Fatalf("%+v %v", tools, err)
	}
	for _, bad := range []string{
		`{"name":"x"}`,
		`[{"name":"mit leerzeichen","description":"d","command":"c"}]`,
		`[{"name":"a","description":"d","command":"c"},{"name":"a","description":"d","command":"c"}]`,
		`[{"name":"a","description":"d"}]`,
		`[{"name":"a","command":"c"}]`,
	} {
		if _, err := loadTools(write(bad)); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
	var stderr strings.Builder
	cfg, err := parseConverseArgs([]string{"-tts-cmd", "say", "-tools", write(`[{"name":"wetter","description":"Wetter","command":"wetter"}]`), "x"}, &stderr)
	if err != nil || len(cfg.tools) != 1 || !strings.HasSuffix(cfg.llm.Prompt, lookupRules) {
		t.Fatalf("%v %+v", err, cfg.tools)
	}
	if _, err := parseConverseArgs([]string{"-tts-cmd", "say", "-tools", filepath.Join(dir, "fehlt.json"), "x"}, &stderr); err == nil {
		t.Fatal("a missing tools file was accepted")
	}
}

// Measured on the live call of 25 November 2026: the first four are what Whisper made
// of clicks, the rest is what was really said.
func TestTooMuchForItsLength(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	for _, c := range []struct {
		text string
		dur  time.Duration
		want bool
	}{
		{"Vielen Dank.", ms(120), true},
		{"Vielen Dank.", ms(360), true},
		{"Vielen Dank.", ms(540), true},
		{"Das ist ja interessant.", ms(300), true},
		{"Was?", ms(360), false},
		{"Ja.", ms(420), false},
		{"Nein.", ms(540), false},
		{"Ja, macht mal.", ms(720), false},
		{"Vielen Dank.", ms(900), false},
		{"Das glaube ich dir nicht.", ms(1080), false},
		{"Ohne Längenangabe.", 0, false},
	} {
		if got := tooMuchForItsLength(c.text, c.dur); got != c.want {
			t.Errorf("tooMuchForItsLength(%q, %v) = %v", c.text, c.dur, got)
		}
	}
}

func TestSpokenNow(t *testing.T) {
	got := spokenNow(time.Date(2026, time.November, 25, 22, 5, 0, 0, time.UTC))
	if want := "Jetzt ist Mittwoch, der 25. November 2026, 22 Uhr 05."; got != want {
		t.Fatalf("%q", got)
	}
}

// A model that looks without a word leaves a silence; the wait phrase fills it, as an
// aside that is spoken but not part of the answer.
func TestThinkWithToolsBridgesSilentLookup(t *testing.T) {
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		w.Header().Set("Content-Type", "text/event-stream")
		if asked == 1 {
			sse(w, `{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"uhr","arguments":"{}"}}]}`)
		} else {
			sse(w, `{"content":"Es ist zehn."}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	tools := []callTool{{Name: "uhr", Description: "Uhrzeit", Timeout: 5, Parameters: json.RawMessage(`{"type":"object","properties":{}}`), Command: "echo zehn"}}
	var answer, asides []string
	err := thinkWithTools(context.Background(), LLMConfig{Endpoint: srv.URL, ModelID: "m", MaxTokens: 50}, tools, "Moment.",
		[]chatMessage{{Role: "user", Content: "Wie spät ist es?"}},
		thinkSink{sentence: func(s string) { answer = append(answer, s) }, aside: func(s string) { asides = append(asides, s) }},
		func(string, map[string]any) {})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(answer, []string{"Es ist zehn."}) || !reflect.DeepEqual(asides, []string{"Moment."}) {
		t.Fatalf("answer %q, asides %q", answer, asides)
	}
}

// The model says it will look and calls nothing. It is reminded once, silently; the
// errand then starts in the background and nothing more is said in this answer.
func TestThinkWithToolsKeepsItsPromise(t *testing.T) {
	var requests []chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "text/event-stream")
		switch len(requests) {
		case 1:
			sse(w, `{"content":"Gleich schaue ich nach."}`)
		case 2:
			sse(w, `{"content":"Mache ich. "}`)
			sse(w, `{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"auftrag","arguments":"{\"auftrag\":\"Chronik lesen\"}"}}]}`)
		default:
			t.Errorf("a third request: %+v", req.Messages)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	tools := []callTool{{Name: "auftrag", Description: "Auftrag", Background: true, Command: "cat",
		Parameters: json.RawMessage(`{"type":"object","properties":{"auftrag":{"type":"string"}}}`)}}
	var said []string
	var started []toolCall
	var looked []chatMessage
	var notes []map[string]any
	sink := thinkSink{
		sentence: func(s string) { said = append(said, s) },
		aside:    func(s string) { said = append(said, "("+s+")") },
		looked:   func(m []chatMessage) { looked = append(looked, m...) },
		later:    func(c toolCall) string { started = append(started, c); return jobStarted },
	}
	cfg := LLMConfig{Endpoint: srv.URL, ModelID: "m", MaxTokens: 50}
	msgs := []chatMessage{{Role: "system", Content: "P"}, {Role: "user", Content: "Was steht in der Chronik?"}}
	if err := thinkWithTools(context.Background(), cfg, tools, "Moment.", msgs, sink,
		func(event string, fields map[string]any) { notes = append(notes, fields) }); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Gleich schaue ich nach."}; !reflect.DeepEqual(said, want) {
		t.Fatalf("said %q, want %q", said, want)
	}
	if len(started) != 1 || started[0].Function.Arguments != `{"auftrag":"Chronik lesen"}` {
		t.Fatalf("started %+v", started)
	}
	if len(requests) != 2 {
		t.Fatalf("%d requests, want 2", len(requests))
	}
	reminder := requests[1].Messages
	if n := len(reminder); n != 4 || reminder[2].Content != "Gleich schaue ich nach." || reminder[3].Content != keepPromise {
		t.Fatalf("the reminder: %+v", reminder)
	}
	if len(looked) != 2 || len(looked[0].ToolCalls) != 1 || looked[1].Content != jobStarted {
		t.Fatalf("the lookup to remember: %+v", looked)
	}
	if len(notes) != 1 || notes[0]["reminded"] != true || notes[0]["background"] != true {
		t.Fatalf("notes %+v", notes)
	}

	// Chit-chat is no promise: nothing is asked a second time.
	requests = nil
	said = nil
	srvTalk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, chatRequest{})
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, `{"content":"Mir geht es gut, danke. Und dir?"}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srvTalk.Close()
	cfg.Endpoint = srvTalk.URL
	if err := thinkWithTools(context.Background(), cfg, tools, "Moment.", msgs, sink, func(string, map[string]any) {}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || len(said) != 1 {
		t.Fatalf("%d requests and %q for chit-chat", len(requests), said)
	}
}

func TestPromise(t *testing.T) {
	for s, want := range map[string]bool{
		"Gleich schaue ich nach.":                                     true,
		"Sehr wohl, ich blättere gerade in Ihren Unterlagen nach.":    true,
		"Ich gebe es an Johann durch, damit es in die Chronik kommt.": true,
		"Ich lasse in Ihren Notizen nachsehen und melde mich.":        true,
		"Ich kümmere mich darum.":                                     true,
		"Das trage ich gleich ein.":                                   true,
		"Mir geht es gut, danke, dass du fragst.":                     false,
		"Das Matterhorn ist 4478 Meter hoch.":                         false,
		"Tschüss, Roger. Gute Nacht.":                                 false,
		"Ich mag es, wenn sich ein Gespräch ergibt.":                  false,
		"Morgen wird es kalt, nach dem Mittag kommt Regen.":           false,
	} {
		if got := promise.MatchString(s); got != want {
			t.Errorf("promise(%q) = %v, want %v", s, got, want)
		}
	}
}
