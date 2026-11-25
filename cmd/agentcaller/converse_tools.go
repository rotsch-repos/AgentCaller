package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// callTool is something the agent can look up while it talks. The chat model decides
// when; the command does the work: it gets the arguments as a JSON object on standard
// input and prints what it found, as text the model can retell.
type callTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Command     string          `json:"command"`
	Timeout     int             `json:"timeout_seconds"`
	// Background marks work that takes longer than a pause in a conversation can: the
	// agent says it is on it, keeps talking, and reports the result when it is there.
	Background bool `json:"background"`
}

type toolSpec struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// What the model is told when work goes to the background, and when it comes back.
const (
	jobStarted = "Der Auftrag ist gestartet, aber noch nicht erledigt: Er läuft im Hintergrund und kann eine Minute oder länger dauern. " +
		"Sag deinem Gegenüber in einem Satz, dass du dich darum kümmerst und dich meldest, sobald es fertig ist, " +
		"und führe das Gespräch weiter. Das Ergebnis bekommst du als Meldung des Systems; bis dahin kennst du es nicht und rätst es nicht."
	jobRefused = "Es laufen schon mehrere Aufträge im Hintergrund. Dieser wurde nicht gestartet; " +
		"sag das deinem Gegenüber und warte auf ein Ergebnis."
	jobDone = "(Meldung des Systems, nicht von deinem Gegenüber gesprochen: Der Auftrag %s %s von vorhin ist fertig. Ergebnis:\n%s\n" +
		"Sag deinem Gegenüber jetzt in zwei bis vier Sätzen das Wesentliche davon, in eigenen Worten; Einzelheiten erst, wenn es nachfragt.)"
	// reportOwed goes along with the next thing the agent says while an outcome that is
	// in the conversation has not reached the peer: the peer talked over the report.
	reportOwed = "(Meldung des Systems, nicht von deinem Gegenüber gesprochen: Weiter oben steht das Ergebnis zu %s. " +
		"Du hast es deinem Gegenüber noch nicht gesagt. Geh zuerst kurz auf das eben Gesagte ein und berichte dann das Wesentliche " +
		"davon in zwei bis vier Sätzen. Nur wenn du es inzwischen doch schon gesagt hast, lass es weg.)"
	// keepPromise goes to a model that announced a lookup or an errand and called no tool.
	keepPromise = "(Meldung des Systems, nicht von deinem Gegenüber gesprochen: Du hast eben angekündigt, etwas nachzusehen, " +
		"weiterzugeben oder zu erledigen, aber kein Werkzeug aufgerufen. Ruf jetzt das passende Werkzeug auf, ohne etwas dazu zu sagen. " +
		"War es keine solche Ankündigung, antworte nur mit dem Wort: nichts.)"
	jobFailed = "(Meldung des Systems, nicht von deinem Gegenüber gesprochen: Der Auftrag %s %s von vorhin ist gescheitert: %s. " +
		"Sag das deinem Gegenüber jetzt, ohne ein Ergebnis zu erfinden.)"
)

const (
	defaultToolTimeout = 20 // seconds
	maxToolOutput      = 6000
	// maxToolRounds bounds how often one answer may go and look something up.
	maxToolRounds = 4
)

var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// promise matches an answer that announces a lookup or an errand: "ich schaue nach",
// "ich kümmere mich darum", "ich gebe es weiter", "ich melde mich".
var promise = regexp.MustCompile(`(?i)\b(schau\w*|seh\w*|sieh\w*|blätter\w*|such\w*|prüf\w*|les\w*|frag\w*|forsch\w*|hak\w*)\b[^.!?]*\bnach\b` +
	`|\bnach(sehen|schauen|schlagen|lesen|fragen|forschen|prüfen)\b|\bkümmer\w* mich\b|\bmeld\w* mich\b|\bgeb\w*\b[^.!?]*\b(weiter|durch)\b` +
	`|\b(trag\w*|schreib\w*|notier\w*|erledig\w*|veranlass\w*|leit\w*)\b[^.!?]*\b(ein|auf|weiter|ich|das)\b`)

// loadTools reads a JSON array of tools.
func loadTools(path string) ([]callTool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tools []callTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for i := range tools {
		t := &tools[i]
		switch {
		case !toolName.MatchString(t.Name):
			return nil, fmt.Errorf("%s: tool %d: name %q must be 1-64 letters, digits, _ or -", path, i+1, t.Name)
		case seen[t.Name]:
			return nil, fmt.Errorf("%s: tool %q is defined twice", path, t.Name)
		case strings.TrimSpace(t.Command) == "":
			return nil, fmt.Errorf("%s: tool %q has no command", path, t.Name)
		case strings.TrimSpace(t.Description) == "":
			return nil, fmt.Errorf("%s: tool %q has no description (the model decides by it)", path, t.Name)
		}
		seen[t.Name] = true
		if len(t.Parameters) == 0 {
			t.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		if t.Timeout <= 0 {
			t.Timeout = defaultToolTimeout
		}
	}
	return tools, nil
}

func toolSpecs(tools []callTool) []toolSpec {
	specs := make([]toolSpec, 0, len(tools))
	for _, t := range tools {
		specs = append(specs, toolSpec{Type: "function", Function: toolFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	return specs
}

func findTool(tools []callTool, name string) *callTool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

// runTool runs the command of the tool the model asked for.
func runTool(ctx context.Context, tools []callTool, call toolCall) (string, error) {
	tool := findTool(tools, call.Function.Name)
	if tool == nil {
		return "", fmt.Errorf("there is no tool %q", call.Function.Name)
	}
	args := strings.TrimSpace(call.Function.Arguments)
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return "", errors.New("the arguments are not valid JSON")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(tool.Timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", tool.Command)
	cmd.Stdin = strings.NewReader(args)
	cmd.WaitDelay = 2 * time.Second
	var out, complaints bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &complaints
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if len(text) > maxToolOutput {
		text = strings.ToValidUTF8(text[:maxToolOutput], "") + " …"
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return "", fmt.Errorf("no result within %d seconds", tool.Timeout)
	case err != nil:
		return "", fmt.Errorf("%w: %s", err, lastLine(complaints.String()))
	case text == "":
		return "", errors.New("the tool returned nothing")
	}
	return text, nil
}

// thinkWithTools answers msgs like streamReply and lets the model look things up on the
// way. While the first lookup runs the agent says wait, unless the model already said
// something itself: silence on a telephone sounds like a dead line.
func thinkWithTools(ctx context.Context, cfg LLMConfig, tools []callTool, wait string, msgs []chatMessage, out thinkSink, note func(string, map[string]any)) error {
	specs := toolSpecs(tools)
	if out.plain {
		specs = nil
	}
	for round := 0; ; round++ {
		if round == maxToolRounds {
			specs = nil // enough looked up: answer with what is there
		}
		var said []string
		calls, err := streamTurn(ctx, cfg, specs, msgs, func(s string) {
			said = append(said, s)
			out.sentence(s)
		})
		if err != nil {
			return err
		}
		kept := false
		if len(calls) == 0 {
			// A model that says "ich schaue nach" and calls nothing leaves its peer waiting
			// for an answer that never comes. It is asked once to do what it announced; what
			// it says to that is not spoken.
			text := strings.Join(said, " ")
			if round != 0 || len(specs) == 0 || !promise.MatchString(text) {
				return nil
			}
			nudged := append(append([]chatMessage(nil), msgs...),
				chatMessage{Role: "assistant", Content: text}, chatMessage{Role: "user", Content: keepPromise})
			calls, err = streamTurn(ctx, cfg, specs, nudged, func(string) {})
			if err != nil || len(calls) == 0 {
				return nil // the answer itself was spoken
			}
			msgs, said, kept = nudged, nil, true
		}
		later := func(call toolCall) bool {
			t := findTool(tools, call.Function.Name)
			return t != nil && t.Background && out.later != nil
		}
		waits := false
		for _, call := range calls {
			waits = waits || !later(call)
		}
		if waits && len(said) == 0 && round == 0 && !kept && wait != "" && out.aside != nil {
			out.aside(wait)
		}
		begin := len(msgs)
		// What was said before looking goes along: a model that does not see it says it again.
		msgs = append(msgs, chatMessage{Role: "assistant", Content: strings.Join(said, " "), ToolCalls: calls})
		for _, call := range calls {
			if later(call) {
				result := out.later(call)
				note("lookup", map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments, "background": result == jobStarted, "reminded": kept})
				msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: call.ID, Content: result})
				continue
			}
			start := time.Now()
			result, err := runTool(ctx, tools, call)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fields := map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments, "ms": time.Since(start).Milliseconds(), "reminded": kept}
			if err != nil {
				fields["error"] = err.Error()
				result = "Das Nachschauen ist fehlgeschlagen: " + err.Error()
			} else {
				fields["result"] = clip(result, 300)
			}
			note("lookup", fields)
			msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: call.ID, Content: result})
		}
		if out.looked != nil {
			keep := append([]chatMessage(nil), msgs[begin:]...)
			keep[0].Content = "" // those sentences are remembered with the answer
			out.looked(keep)
		}
		if kept && !waits {
			return nil // it already said that it is on it; the result is reported when it comes
		}
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
