package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os/exec"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rotsch-repos/agentcaller"
)

// wavBytes wraps 16 kHz s16le mono PCM in a WAV header.
func wavBytes(pcm []byte) []byte {
	var hdr [44]byte
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+len(pcm)))
	copy(hdr[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1)
	binary.LittleEndian.PutUint16(hdr[22:], 1)
	binary.LittleEndian.PutUint32(hdr[24:], agentcaller.SampleRate)
	binary.LittleEndian.PutUint32(hdr[28:], agentcaller.SampleRate*2)
	binary.LittleEndian.PutUint16(hdr[32:], 2)
	binary.LittleEndian.PutUint16(hdr[34:], 16)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(len(pcm)))
	return append(hdr[:], pcm...)
}

// transcribeHTTP sends speech to an OpenAI-compatible /v1/audio/transcriptions endpoint.
func transcribeHTTP(ctx context.Context, base, model, language string, pcm []byte) (string, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "speech.wav")
	if err != nil {
		return "", err
	}
	_, _ = part.Write(wavBytes(pcm))
	_ = form.WriteField("model", model)
	if language != "" {
		_ = form.WriteField("language", language)
	}
	if err := form.Close(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("transcription request failed (is %s running?): %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("transcription endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode transcription: %w", err)
	}
	return out.Text, nil
}

// streamReply asks the chat endpoint for an answer and passes it on sentence by
// sentence while it is being written.
func streamReply(ctx context.Context, cfg LLMConfig, msgs []chatMessage, sentence func(string)) error {
	_, err := streamTurn(ctx, cfg, nil, msgs, sentence)
	return err
}

// streamTurn is streamReply for a model that may ask for tools: what it says goes to
// sentence, what it wants looked up comes back as calls.
func streamTurn(ctx context.Context, cfg LLMConfig, tools []toolSpec, msgs []chatMessage, sentence func(string)) (calls []toolCall, err error) {
	body, err := json.Marshal(chatRequest{
		Model:              cfg.ModelID,
		Messages:           msgs,
		MaxTokens:          cfg.MaxTokens,
		Temperature:        0.7,
		Stream:             true,
		Tools:              tools,
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	})
	if err != nil {
		return nil, fmt.Errorf("encode LLM request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.Endpoint, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build LLM request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("LLM request failed (is %s running?): %w", cfg.Endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("LLM endpoint %s returned %s: %s", cfg.Endpoint, resp.Status, strings.TrimSpace(string(msg)))
	}
	var split sentenceSplitter
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for lines.Scan() {
		data, ok := strings.CutPrefix(lines.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		for _, s := range split.feed(chunk.Choices[0].Delta.Content) {
			sentence(s)
		}
		// A tool call arrives in pieces too: name first, then the arguments bit by bit.
		for _, part := range chunk.Choices[0].Delta.ToolCalls {
			if part.Index < 0 || part.Index > 16 {
				continue
			}
			for len(calls) <= part.Index {
				calls = append(calls, toolCall{Type: "function"})
			}
			call := &calls[part.Index]
			if part.ID != "" {
				call.ID = part.ID
			}
			call.Function.Name += part.Function.Name
			call.Function.Arguments += part.Function.Arguments
		}
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("read LLM stream: %w", err)
	}
	if s := split.flush(); s != "" {
		sentence(s)
	}
	return calls, nil
}

// sentenceSplitter cuts a text that arrives in pieces into speakable sentences. Short
// ones stay attached to what follows: a lone "Ja." sounds clipped when synthesised alone.
type sentenceSplitter struct{ buf string }

const minSentenceRunes = 24

func (s *sentenceSplitter) feed(delta string) (out []string) {
	s.buf += delta
	for {
		cut := s.boundary()
		if cut < 0 {
			return out
		}
		if text := cleanSpoken(s.buf[:cut]); text != "" {
			out = append(out, text)
		}
		s.buf = s.buf[cut:]
	}
}

func (s *sentenceSplitter) flush() string {
	text := cleanSpoken(s.buf)
	s.buf = ""
	return text
}

// boundary returns the byte offset just after the first complete sentence, or -1.
func (s *sentenceSplitter) boundary() int {
	runes := 0
	var prev rune
	for i, r := range s.buf {
		runes++
		size := utf8.RuneLen(r)
		switch r {
		case '\n':
			if strings.TrimSpace(s.buf[:i]) != "" {
				return i + size
			}
		case '.', '!', '?', '…':
			rest := s.buf[i+size:]
			if rest == "" || runes < minSentenceRunes || (r == '.' && unicode.IsDigit(prev)) {
				break
			}
			if next, _ := utf8.DecodeRuneInString(rest); unicode.IsSpace(next) {
				return i + size
			}
		}
		prev = r
	}
	return -1
}

// cleanSpoken removes what a speech synthesiser would read aloud or stumble over.
func cleanSpoken(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '*' || r == '_' || r == '#' || r == '`':
		case r >= 0x1F000 || (r >= 0x2600 && r <= 0x27BF) || r == 0xFE0F || r == 0x200D:
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// commandSpeaker runs a speech command for each text: the text goes to its standard
// input, raw s16le mono PCM comes from its standard output and is passed on as it arrives.
func commandSpeaker(command string, rate int) func(ctx context.Context, text string, w io.Writer) error {
	return func(ctx context.Context, text string, w io.Writer) error {
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Stdin = strings.NewReader(text)
		cmd.WaitDelay = 2 * time.Second
		var complaints bytes.Buffer
		cmd.Stderr = &complaints
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start speech command: %w", err)
		}
		buf := make([]byte, (rate*2*60/1000)&^1)
		total := 0
		var werr error
		for {
			n, rerr := io.ReadFull(stdout, buf)
			if n &^= 1; n > 0 && werr == nil {
				total += n
				_, werr = w.Write(buf[:n])
			}
			if rerr != nil || werr != nil {
				break
			}
		}
		if werr != nil {
			_ = cmd.Process.Kill()
		}
		waitErr := cmd.Wait()
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case werr != nil:
			return werr
		case waitErr != nil:
			return fmt.Errorf("speech command failed: %w: %s", waitErr, lastLine(complaints.String()))
		case total == 0:
			return errors.New("speech command produced no audio")
		}
		return nil
	}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
