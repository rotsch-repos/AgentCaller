package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rotsch-repos/agentcaller"
	"github.com/rotsch-repos/agentcaller/resample"
)

// callVoice is one of several characters that can hold the same call: its own persona,
// its own speech command. The peer asks the one that is speaking to be put through to
// another ("verbinde mich mit Alice"); the conversation and what was looked up stay.
type callVoice struct {
	Name      string `json:"name"`
	About     string `json:"about"`      // one line for the other voices: who this is
	Persona   string `json:"persona"`    // text file, as with -persona
	TTSCmd    string `json:"tts_cmd"`    // as -tts-cmd
	TTSRate   int    `json:"tts_rate"`   // as -tts-rate
	ToolsWait string `json:"tools_wait"` // as -tools-wait; empty: the flag's value
	// GuestPersona is who this voice is for somebody it does not trust (see -speakers).
	GuestPersona string `json:"guest_persona"`

	guestPrompt string

	prompt string
	speak  func(ctx context.Context, text string, w io.Writer) error
}

// handoverTool is the name under which the model asks for another voice to take over.
const handoverTool = "verbinden"

const (
	// handedOver tells the voice that takes over where it stands: everything above was
	// said by somebody else, and it is its turn now.
	handedOver = "(Meldung des Systems, nicht von deinem Gegenüber gesprochen: Bis hierher hat %[1]s mit deinem Gegenüber gesprochen; " +
		"die Antworten weiter oben stammen von %[1]s, nicht von dir. Auf Wunsch deines Gegenübers hat %[1]s das Gespräch eben an dich übergeben. " +
		"Ab jetzt sprichst du, %[2]s, mit deiner eigenen Stimme und in deinem eigenen Ton. Du hast alles mitgehört. " +
		"Melde dich jetzt in ein bis zwei Sätzen und geh auf den Anlass ein, wenn es einen gab.)"
	handoverAccepted = "Die Übergabe an %s folgt, sobald du ausgesprochen hast. Sag nichts weiter."
	handoverSame     = "Du bist %s. Verbinden kannst du nur mit: %s."
	handoverUnknown  = "Eine Stimme %q gibt es in diesem Gespräch nicht. Verbinden kannst du mit: %s."
	// handoverLine is said by a model that asked for the handover without a word.
	handoverLine = "Einen Moment, ich verbinde mit %s."
)

// loadVoices reads a JSON array of voices and their personas.
func loadVoices(path string) ([]callVoice, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var voices []callVoice
	if err := json.Unmarshal(raw, &voices); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(voices) == 0 {
		return nil, fmt.Errorf("%s: no voices", path)
	}
	for i := range voices {
		v := &voices[i]
		v.Name = strings.TrimSpace(v.Name)
		if v.TTSRate == 0 {
			v.TTSRate = agentcaller.SampleRate
		}
		switch {
		case v.Name == "":
			return nil, fmt.Errorf("%s: voice %d has no name", path, i+1)
		case findVoice(voices[:i], v.Name) >= 0:
			return nil, fmt.Errorf("%s: voice %q is defined twice", path, v.Name)
		case strings.TrimSpace(v.TTSCmd) == "":
			return nil, fmt.Errorf("%s: voice %q has no tts_cmd", path, v.Name)
		case v.TTSRate < 4000 || v.TTSRate > 192000:
			return nil, fmt.Errorf("%s: voice %q: tts_rate must be between 4000 and 192000", path, v.Name)
		}
		v.prompt = defaultPersona
		if v.Persona != "" {
			text, err := os.ReadFile(v.Persona)
			if err != nil {
				return nil, fmt.Errorf("%s: voice %q: %w", path, v.Name, err)
			}
			v.prompt = strings.TrimSpace(string(text))
		}
	}
	return voices, nil
}

// findVoice returns the index of the voice called name, or -1. Case does not matter:
// the name comes from a model.
func findVoice(voices []callVoice, name string) int {
	for i := range voices {
		if strings.EqualFold(voices[i].Name, strings.TrimSpace(name)) {
			return i
		}
	}
	return -1
}

// voicesRate is the rate all voices are brought to: their own if they agree, else the
// rate of the call, so that nothing is resampled twice.
func voicesRate(voices []callVoice) int {
	for _, v := range voices[1:] {
		if v.TTSRate != voices[0].TTSRate {
			return agentcaller.SampleRate
		}
	}
	return voices[0].TTSRate
}

// handoverSpec is the tool by which the voice that speaks hands the call to another.
func handoverSpec(voices []callVoice) callTool {
	var who, names []string
	for _, v := range voices {
		names = append(names, v.Name)
		if v.About != "" {
			who = append(who, v.Name+" ("+v.About+")")
		} else {
			who = append(who, v.Name)
		}
	}
	enum, _ := json.Marshal(names)
	return callTool{
		Name: handoverTool,
		Description: "Übergibt dieses Telefongespräch an eine andere Stimme, die am selben Anruf weiterspricht und alles Bisherige kennt: " +
			strings.Join(who, "; ") + ". Nur wenn dein Gegenüber ausdrücklich darum bittet, mit ihr oder ihm zu sprechen oder verbunden zu werden. " +
			"Sag im selben Zug in einem Satz, dass du verbindest; danach spricht die andere Stimme.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"mit":{"type":"string","enum":` + string(enum) + `,"description":"Name der Stimme, die übernimmt."}},"required":["mit"]}`),
		handover:   true,
	}
}

// askHandover takes the model's request to be replaced by another voice. The switch
// itself happens when the answer has been spoken (see handOver).
func (c *conversation) askHandover(call toolCall) (result string, ok bool) {
	var args struct {
		Mit string `json:"mit"`
	}
	_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
	c.mu.Lock()
	defer c.mu.Unlock()
	var others []string
	for i, v := range c.cfg.voices {
		if i != c.voice {
			others = append(others, v.Name)
		}
	}
	to := findVoice(c.cfg.voices, args.Mit)
	switch {
	case to < 0:
		return fmt.Sprintf(handoverUnknown, args.Mit, strings.Join(others, ", ")), false
	case to == c.voice:
		return fmt.Sprintf(handoverSame, c.cfg.voices[to].Name, strings.Join(others, ", ")), false
	}
	c.handTo = to + 1
	return fmt.Sprintf(handoverAccepted, c.cfg.voices[to].Name), true
}

// takeHandover returns the voice that was asked to take over, once.
func (c *conversation) takeHandover() (to int, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	to, c.handTo = c.handTo-1, 0
	return to, to >= 0
}

// handOver gives the call to another voice: its persona answers from here on, its
// speech command speaks, and it says hello knowing what was talked about.
func (c *conversation) handOver(ctx context.Context, to int) {
	for c.say.remaining() > 0 && ctx.Err() == nil { // the voice that leaves finishes its sentence
		time.Sleep(20 * time.Millisecond)
	}
	c.mu.Lock()
	from, next := c.cfg.voices[c.voice], c.cfg.voices[to]
	c.voice = to
	c.cfg.llm.Prompt = next.prompt
	c.push(false, chatMessage{Role: "user", Content: fmt.Sprintf(handedOver, from.Name, next.Name)})
	msgs := c.view()
	c.mu.Unlock()
	// Tools are told who is speaking: an errand may be worded differently for each.
	_ = os.Setenv("AGENTCALLER_VOICE", next.Name)
	c.events.emit("handover", map[string]any{"from": from.Name, "to": next.Name})

	turn, end, ok := c.beginTurn(ctx, false)
	if !ok {
		return // the peer is talking: the new voice answers at the next pause
	}
	defer end()
	start := time.Now()
	spoken, looked, first, err := c.respond(turn, msgs, true)
	interrupted := turn.Err() != nil && ctx.Err() == nil
	if err != nil && !interrupted && ctx.Err() == nil {
		c.events.emit("reply_error", map[string]any{"error": err.Error()})
	}
	if len(spoken) == 0 {
		return
	}
	reply := strings.Join(spoken, " ")
	c.remember("", looked, reply)
	fields := c.voiced(map[string]any{"text": reply, "interrupted": interrupted, "handover": true})
	if !first.IsZero() {
		fields["first_audio_ms"] = first.Sub(start).Milliseconds() // since the old voice fell silent
	}
	c.events.emit("reply", fields)
}

// speech is the speech command of the voice that holds the call.
func (c *conversation) speech() func(ctx context.Context, text string, w io.Writer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cfg.voices) > 0 && c.cfg.voices[c.voice].speak != nil {
		return c.cfg.voices[c.voice].speak
	}
	return c.chain.speak
}

// toolsWait is what the voice that holds the call says while a lookup runs.
func (c *conversation) toolsWait() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cfg.voices) > 0 && c.cfg.voices[c.voice].ToolsWait != "" {
		return c.cfg.voices[c.voice].ToolsWait
	}
	return c.cfg.toolsWait
}

// voiced adds who spoke to the fields of a reply, on calls with more than one voice.
func (c *conversation) voiced(fields map[string]any) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cfg.voices) > 0 {
		fields["voice"] = c.cfg.voices[c.voice].Name
	}
	return fields
}

// resampledSpeaker brings a speech command's output from one sample rate to another.
func resampledSpeaker(speak func(ctx context.Context, text string, w io.Writer) error, from, to int) func(ctx context.Context, text string, w io.Writer) error {
	if from == to {
		return speak
	}
	return func(ctx context.Context, text string, w io.Writer) error {
		rw := &resampleWriter{w: w, res: resample.New(from, to)}
		if err := speak(ctx, text, rw); err != nil {
			return err
		}
		return rw.flush()
	}
}

// resampleWriter takes s16le mono PCM at one rate and writes it on at another.
type resampleWriter struct {
	w    io.Writer
	res  *resample.Resampler
	odd  []byte // half a sample from the last write
	in   []float32
	out  []float32
	wire []byte
}

func (r *resampleWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(r.odd) > 0 {
		p = append(r.odd, p...)
		r.odd = nil
	}
	if len(p)%2 == 1 {
		r.odd = []byte{p[len(p)-1]}
		p = p[:len(p)-1]
	}
	r.in = r.in[:0]
	for i := 0; i+1 < len(p); i += 2 {
		r.in = append(r.in, float32(int16(binary.LittleEndian.Uint16(p[i:])))/32768)
	}
	r.out = r.res.Process(r.in, r.out[:0])
	return n, r.send()
}

func (r *resampleWriter) flush() error {
	r.out = r.res.Flush(r.out[:0])
	return r.send()
}

func (r *resampleWriter) send() error {
	if len(r.out) == 0 {
		return nil
	}
	r.wire = r.wire[:0]
	for _, s := range r.out {
		v := int32(s * 32768)
		v = max(min(v, 32767), -32768)
		r.wire = binary.LittleEndian.AppendUint16(r.wire, uint16(int16(v)))
	}
	_, err := r.w.Write(r.wire)
	return err
}
