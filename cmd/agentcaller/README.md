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
to warning-only structured logs; `MEOW_LOG_LEVEL` opts into a different level.

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
