# agentcaller command

Pair once in an interactive terminal, using the same store path the automation will use:

```text
agentcaller pair --store <path>
```

Scan the QR in WhatsApp under **Linked devices**. A successful command prints
`AgentCaller linked device ready` and exits; later `pair` runs verify that the saved linked
device can still connect.

`agentcaller notify` is the headless, send-only command surface for automation:

```text
agentcaller notify [flags] <target> <audio.mp3|wav|ogg|opus|mulaw|ulaw|->
```

`target` may be a phone number, phone JID, or LID JID. `-` reads raw 8 kHz mono G.711 PCMU from stdin. Files use their extension; `.mulaw` and `.ulaw` are also raw PCMU. The command opens no local audio/video device, attaches no inbound sink, sends the input once after the recipient answers, then hangs up.

Flags:

- `--store PATH`: linked-device SQLite store; defaults to the platform user-config directory. On Unix, its parent directory must be user-private (`0700`).
- `--answer-timeout DURATION`: maximum ringing time; default `45s`, `0` disables.
- `--max-duration DURATION`: maximum playback after answer; default `5m`, `0` streams until EOF.

Pairing is an explicit terminal step so the QR never enters service logs. `notify` refuses an
unpaired store when run headlessly. The command does not offer diagnostic capture and defaults
to warning-only structured logs; `AGENTCALLER_LOG_LEVEL` opts into a different level.

`agentcaller talk` is the interactive voice menu. It calls `target`, plays a short announcement,
listens for the recipient's answer, and replies with a generated spoken answer:

```text
agentcaller talk [flags] <target>
agentcaller talk -dry-run [flags] <input.wav>
agentcaller talk -replay <input.wav> [--listen-only] [flags]
```

(-replay is a flag, not a positional argument; the same holds for all of the following lines —
flags may be placed after a positional argument as well.)

Flags: -store (default ~/.config/agentcaller/talk-voip.db, separate from the notify store),
-answer-timeout (45 s), -max-listen (60 s, per VAD round), -prompt, -say <datei>, -say-text,
-dry-run, -replay <datei>, --listen-only, -tts-file <datei>, -out-dir <verzeichnis>
(artifacts: transcript.json, answer.txt, answer.wav/mp3, per-round recordings),
-save-audio <verzeichnis> (superseded by -out-dir), -stt-model, -llm-endpoint, -llm-model,
-tts-backend (piper|edge), -piper-voice, -vad-silence, -vad-min-speech.

Pipeline: announcement → energy-based VAD endpointing (fixed interim line the moment speech
closes) → local whisper STT → local LLM answer → local piper TTS → hangup. `-dry-run` runs the
STT+LLM+TTS chain on a WAV without a call; `-replay` feeds a recorded 16 kHz call-frame WAV
through the VAD (frame count rather than wall clock — deterministic, no real-time pacing) and can stop after the endpointing report with
`--listen-only`. Artifacts (transcript.json, answer.txt, answer.wav/mp3, per-round recordings)
land in an output directory for inspection.

## call: both directions as pipes

```text
agentcaller call [flags] <target>
agentcaller call -answer [flags] <caller|any>
```

The command carries audio and reports what happens; it does not transcribe, think or
speak. Whatever drives the conversation reads and writes the pipes.

| Flag | Meaning |
| --- | --- |
| `-in PATH` | audio to send: raw s16le mono PCM. `-` is stdin (default), `none` sends silence. A FIFO is opened when the call is answered. |
| `-in-rate HZ` | sample rate of `-in` (default 16000); converted band-limited |
| `-out PATH` | the peer's audio: raw s16le mono 16 kHz. `-` is stdout (default, unless it is a terminal), `none` discards |
| `-events PATH` | JSON-line events (default stderr) |
| `-control PATH` | commands, one per line: `flush`, `hangup`, `stats` (also `{"cmd":"flush"}`). A FIFO is reopened after each writer. |
| `-barge-in` | drop the queued outgoing audio when the peer starts to speak |
| `-end-silence D` | silence that ends a turn (`speech_end`), default 700 ms |
| `-max-buffer D` | bound the outgoing queue, dropping the oldest audio; for a live microphone. Default 0: queue everything (for synthesised speech), the reader is held back at two seconds. |
| `-conceal` | synthesise lost packets instead of silence; for human listeners |
| `-hangup-on-eof` | hang up when `-in` has ended and everything was sent |
| `-answer-timeout D`, `-max-duration D` | limits; `0` disables |
| `-video-in PATH`, `-video-fps N`, `-video-out PATH` | experimental H.264 Annex-B pass-through |
| `-store PATH` | linked-device store |

Events: `connected`, `calling` or `waiting`/`incoming`/`ignored_call`, `state`, `ready`,
`speech_start {at}`, `speech_end {at, duration}`, `flushed {cause, ms}`, `input_eof`,
`played`, `stats {...}`, `ended {reason}`. `at` is seconds of received audio, so it
indexes straight into the `-out` stream (`at * 16000 * 2` bytes).

With `-answer`, only the named caller is answered. Other calls are neither answered nor
rejected: a reject from a linked device declines the call on the phone too.

Audio that arrives on `-in` before the call is answered is not read, so nothing stale is
spoken. If the reader of `-out` falls more than three seconds behind, frames are dropped
and counted in `stats.out_dropped_frames`; the call itself is never held up.

## converse: a spoken conversation

```text
agentcaller converse [flags] <target>
agentcaller converse -answer [flags] <caller|any>
agentcaller converse -simulate peer.wav [-record agent.wav] [flags]
```

`converse` runs the `call` duplex in-process and drives it:

1. `speech_end` names a stretch of the peer's audio; it is cut out (with 240 ms before and
   300 ms after) and posted as a WAV to `<-stt-url>/v1/audio/transcriptions` (`file`, `model`,
   `language`; the answer is `{"text": ...}`).
2. The conversation so far goes to `<-llm-url>/v1/chat/completions` with `stream: true`. The
   system prompt is the `-persona` file followed by fixed telephone rules (short spoken
   sentences, no markup). `chat_template_kwargs.enable_thinking` is sent as `false`.
3. Each complete sentence is handed to `-tts-cmd` (run by `sh -c`): text on stdin, raw s16le
   mono PCM at `-tts-rate` on stdout. The audio is passed into the call as it arrives, at
   most 600 ms ahead of real time.

Turn taking:

- The peer starts to talk while the agent speaks or thinks: the turn is cancelled, the speech
  command is killed and queued audio is flushed. Only sentences that went out in full are
  kept as the agent's side of the history. `-no-barge-in` lets the agent finish instead.
- The peer goes on after a pause before the agent has said anything: both parts are answered
  as one.
- What the recogniser returns for noise (empty text, the usual Whisper subtitle credits) is
  ignored.
- A goodbye at the end of what the peer said (tschüss, bis bald, bye, ...) is answered once,
  then the agent hangs up. `-max-duration` (default 15 min) ends a call that does not.

Additional events: `heard {text, at, stt_ms}`, `reply {text, interrupted, first_audio_ms}`
(`first_audio_ms` counts from the `speech_end` event to the first audio written into the
call), `stt_error`, `reply_error`.

`-simulate peer.wav` replaces the WhatsApp call with a recording that plays in real time as
the peer and, like a polite caller, waits at each of its pauses (0.9 s or longer) until the
agent has answered. Everything else is the real chain, so it measures the real answer delay
and `-record` gives you the agent's side to listen to. It does not exercise the network path,
the codec, or echo: on a real call the agent's own voice coming back through the peer's
loudspeaker may trigger barge-in. That has not been tried yet; `-no-barge-in` is the way out.

Flags: `-store`, `-answer`, `-answer-timeout`, `-max-duration`, `-end-silence`, `-events`,
`-persona FILE`, `-greeting TEXT`, `-llm-url`, `-llm-model`, `-max-tokens`, `-stt-url`,
`-stt-model`, `-stt-language`, `-tts-cmd`, `-tts-rate`, `-no-barge-in`, `-simulate`, `-record`.
