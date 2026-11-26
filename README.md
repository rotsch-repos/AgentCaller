# AgentCaller

WhatsApp voice and video calls for software agents, in pure Go.

AgentCaller places and answers WhatsApp calls from a linked device and gives a
program both directions of the conversation at once: what the person says comes
out as PCM, what the agent wants to say goes in as PCM, and the call itself is a
stream of small JSON events. There is no microphone, no speaker and no CGO. The
speech recognition, the language model and the voice stay outside, on the other
end of two pipes, so they can be local, and they can be replaced.

It is a fork of [meowcaller](https://github.com/purpshell/meowcaller) by Rajeh
Taher, which did the hard part: the WhatsApp call signalling and a pure-Go
implementation of the MLow audio codec. AgentCaller's work is on what happens
around the codec so that a call feels immediate. See [Credits](#credits).

> Independent research and interoperability project, not affiliated with Meta or
> WhatsApp. Read the [disclaimer](DISCLAIMER.md) first. Use it with your own
> accounts and with people who know they are talking to a machine.

## Status

| Part | State |
| --- | --- |
| Outbound audio call, send only (`notify`) | live-tested as `meowcaller notify`; not re-run since the fork (the encoder output is bit-identical) |
| One question, one answer (`talk`) | built and replay-tested; no live call yet |
| Two-way pipe (`call`), speech events, barge-in | tested offline against a simulated call; **no live call yet** |
| Spoken conversation (`converse`): recognition, chat model, speech command | tested offline, and end to end with `-simulate` against real services. After a failed first call, **two live calls on 2026-11-25 held a conversation** over several minutes (see the changelog). A later call that day ran 17 minutes with lookups, background errands and the 480 ms interruption rule. Several voices (`-voices`), the speaker check (`-speakers`) and the warm start came after it and are **not yet confirmed on a call** |
| New receive path (ordering, loss handling) | tested offline with simulated loss and reordering; **not yet on real traffic** |
| Video send and receive | tested offline against ffmpeg; **never validated in a live call**, as upstream |

Everything in the "offline" rows is exercised by `go test ./...`. Nothing here
claims more than that.

## Quick start

```sh
git clone https://github.com/rotsch-repos/AgentCaller.git && cd AgentCaller
go build -o ~/.local/bin/agentcaller ./cmd/agentcaller

# once, in a terminal: scan the QR with the phone the AGENT's number lives on
agentcaller pair

# a two-way call: stdin is what the agent says, stdout is what the person says
agentcaller call -in-rate 22050 -events events.jsonl +41790000000 < say.s16le > heard.s16le
```

A conversation is three processes and two pipes:

```sh
mkfifo say heard ctl
agentcaller call -in say -in-rate 22050 -out heard -control ctl -events events.jsonl -barge-in +41790000000 &

# your loop: read `heard` (s16le, 16 kHz, mono), watch events.jsonl for speech_end,
# transcribe, think, synthesise, write s16le into `say`.
echo flush  > ctl   # drop what is still queued (the person interrupted)
echo hangup > ctl
```

Events, one JSON object per line (`t` is seconds since the command started):

```json
{"event":"ready","t":4.212}
{"event":"speech_start","t":6.031,"at":1.74}
{"event":"speech_end","t":8.912,"at":1.74,"duration":2.1}
{"event":"flushed","t":9.4,"cause":"barge-in","ms":1320}
{"event":"ended","t":31.07,"reason":"remote hangup"}
```

`agentcaller call -answer +41790000000` waits for a call from that number and
answers it; calls from anyone else are left ringing on the phone. All flags:
`agentcaller call -h`, and [cmd/agentcaller/README.md](cmd/agentcaller/README.md).

### The loop, built in: `converse`

`agentcaller converse` is that loop in one process. It needs three things you run
yourself: a transcription endpoint and a chat endpoint that speak the OpenAI HTTP
protocol (for example faster-whisper behind a small server, and vLLM or llama.cpp), and
any command that reads text on stdin and writes raw PCM to stdout.

```sh
agentcaller converse \
  -persona persona.md -greeting "Hallo, ich bins." \
  -stt-url http://127.0.0.1:8000 -llm-url http://127.0.0.1:8002 -llm-model qwen3.8-27b \
  -tts-cmd "piper --model de_DE-thorsten-medium.onnx --output-raw" -tts-rate 22050 \
  +41790000000
```

The answer is spoken sentence by sentence while the model is still writing, the agent
stops when the person talks into it, a pause that turns out not to be the end is
merged with what follows, and a goodbye ends the call. To hear the whole chain without
a phone, let a recording play the caller.

Three settings came out of real calls. A sound has to last 480 ms before the agent stops
for it (`-barge-in-after`): line noise and the echo of the agent's own voice lasted 120 to
360 ms and cut answers off in the middle of a sentence; they are still listened to, and
thrown away when they turn out to be nothing. `-greeting-delay 1500ms` waits for the
phone to reach the ear, and a greeting that a noise did cut off is said again. Or let the
one who picks up speak first: with `-greeting-after-hello 6s` the greeting follows their
"Hallo?" at once (it is spoken ahead while it rings), and comes by itself after six
seconds of silence. `-hangup`
says when the agent ends the call: after a `goodbye` (default), only when `asked` to hang
up, or `never`. A noise once came out of the recogniser as "Bye." and ended a call.
The outcome of a background errand is owed until it has been spoken: if the person talks
over the report, it goes along with the next answer.


```sh
agentcaller converse -simulate caller.wav -record agent.wav -tts-cmd "..." -events events.jsonl
```

The agent can look things up while it talks. `-tools tools.json` names commands the
chat model may call (it must support OpenAI-style tool calls):

```json
[{"name": "weather", "description": "Current weather and forecast for a place.",
  "parameters": {"type": "object", "properties": {"place": {"type": "string"}}},
  "command": "/usr/local/bin/weather-lookup", "timeout_seconds": 15}]
```

The command gets the arguments as JSON on stdin and answers with text on stdout. If the
model starts a lookup without a word, `-tools-wait "One moment."` is spoken to bridge
the silence. A lookup and its result stay in the conversation, so the next question can
build on them. A command here runs with your rights on whatever the model sends it:
give it read-only things to do, and treat what a web search returns as hearsay.

Work that takes longer than a pause in a conversation gets `"background": true`: the
command starts, the model is told that it is running and goes on talking, and when the
result is there the agent reports it at the next quiet moment (never over its peer, and
after any question that is waiting). At most three such errands run at once; a result
that arrives after the call has ended is not spoken. This is how a fast voice model
hands a task to a slower agent that has the tools and the memory: the command is that
agent's command line. What the model hears on the phone can be misheard, so let the
agent on the other end ask before it does anything that cannot be undone. A model that
says "I'll look" and calls nothing is reminded once, silently, to do what it announced.
Events: `lookup` (`background`, `reminded`), `lookup_done`, and `reply` with `report`.

Several characters can hold the same call. `-voices voices.json` replaces `-persona`,
`-tts-cmd` and `-tts-rate`; `-voice Name` says who starts:

```json
[{"name": "Alice", "about": "the one to talk and think with", "persona": "alice.md",
  "tts_cmd": "alice-voice --pcm 16000", "tts_rate": 16000, "tools_wait": "One moment."},
 {"name": "Johann", "about": "the butler, for getting things done", "persona": "johann.md",
  "tts_cmd": "johann-voice --pcm", "tts_rate": 24000}]
```

The model gets a tool `verbinden` ("put through"): when the person asks for the other
one, the voice that is speaking finishes its sentence, and the next answer comes from
the other persona in the other voice. The conversation and everything that was looked
up stay; the one that takes over is told that the answers so far were not its own.
Tool commands find the name of the voice that is speaking in `AGENTCALLER_VOICE`.
Events: `handover` (`from`, `to`); `reply` carries `voice`.

The agent can be made to mind who is speaking. `-speakers speakers.json` names a command
that runs for the length of the call:

```json
{"command": "/usr/local/bin/speaker-check", "guest_persona": "guest.md", "password": "..."}
```

For every stretch of speech it gets one line `{"wav": "/path.wav", "seconds": 2.4}` and
answers one line, for instance `{"speaker": "Sam", "trusted": true, "score": 0.83}`;
`"unclear": true` when it cannot tell, `"short": true` when there was too little to
hear, `"note"` for what is known about a guest. It runs next to the speech recognition
and costs the answer no time. The call starts locked. Until a trusted voice is heard,
the model works from the guest persona and sees nothing of what was said with trusted
speakers; it is offered only the tools marked `"guests": true`, never the handover, and
outcomes of errands wait until the trusted speaker is back. This is done by what the
model is sent, not by asking it to keep quiet: it cannot tell what it is not shown.
A check that fails or times out counts as a guest. The password, spoken, unlocks the
call for whoever says it. It is matched in the transcript (case, hyphens and spaces
aside) and replaced by `[Passwort]` before the model or the event log see the sentence,
so the model cannot give it away; the file must be readable by its owner alone.
A voice profile tells who is probably speaking. It is not a lock: a recording or a
cloned voice passes it. Events: `speaker` (`trusted`, `guest`, `score`, `speaker`).

Before the first word, the prompts and tool lists of the call are sent to the chat
endpoint once with a one-token answer, so that a server with a prefix cache has read
the persona by the time the person speaks (event `warmed` with `ms`).

Measured that way on one machine (Whisper large-v3 and a 27B model on local GPUs), two
runs of three turns each: the first sound of the answer left 0.9 s after the pause was
reported in four turns and 1.4 and 1.9 s in the two where the recogniser was slow. The
pause itself (`-end-silence`, 700 ms by default) comes on top. These are numbers from
the simulation, not from a call.

## What "instant" means here

Measured on the development machine (Threadripper PRO 7985WX, one core), not in
a live call. Network and the other phone come on top.

**Sending.** WhatsApp's codec works in 60 ms frames; that is the floor.

| Step | Delay |
| --- | --- |
| Waiting for a frame to fill and for the next 60 ms tick | 60 to 120 ms |
| Resampling the input to 16 kHz (if needed) | 2 ms look-ahead |
| Encoding one frame | 0.9 ms (was 3.3 ms; output bit-identical) |
| Codec look-ahead, measured through encoder and decoder | 2.5 ms |

The send loop never waits for the producer. A voice that synthesises in bursts
or a slow pipe cannot stall the call: missing audio is sent as silence, a
partial frame waits one tick and then goes out.

**Receiving.** Packets in sequence are decoded and delivered immediately: zero
added delay, no playout buffer. Only when a packet is missing does the receiver
hold what came after it, for at most 80 ms or four packets, and then declares
the packet lost. The timeline stays intact either way, so one second of call is
always 16000 samples.

**Lost packets are silence, on purpose.** The usual trick is to synthesise a
plausible continuation. For a human ear that is right. For a speech recogniser
it is not, and the default here serves the recogniser:

| Loss | Word errors, silent gaps | Word errors, concealment |
| --- | --- | --- |
| 10 % | 9 of 108 | 15 of 108 |
| 20 % | 21 of 108 | 30 of 108 |

(German sentence through the codec, three loss patterns per rate, Whisper
`small`.) `-conceal`, or `Call.ConcealLosses(true)`, turns the continuation on
for calls a person listens to.

**Interrupting.** `-barge-in` drops the agent's queued audio once the person has
been speaking for `-barge-in-after` (480 ms; the event is `speech_sustained`).
`speech_start` still comes after 180 ms, so nothing is lost for the recogniser. It
listens to the incoming audio only: on live calls the echo of the agent's own voice and
line noise opened it for 120 to 360 ms, which is why the first version, cutting at
`speech_start`, stopped the agent in the middle of sentences. Still opt-in for `call`.

**Input quality.** WAV, MP3 and the `-in` stream are converted to 16 kHz with a
band-limited polyphase resampler (everything above 8.2 kHz is down by 86 dB or
more; the straight-line interpolation it replaces folded those frequencies into
the speech band almost unattenuated). Raw G.711 input (`notify -`) still uses
sample doubling.

## Video

AgentCaller does not encode pixels. It carries H.264 that something else has
encoded: ffmpeg, a hardware encoder, a browser.

```sh
ffmpeg -re -i clip.mp4 -an -c:v libx264 -profile:v baseline -tune zerolatency \
       -g 30 -bsf:v h264_mp4toannexb -f h264 video.fifo &
agentcaller call -video-in video.fifo -video-out peer.h264 +41790000000
```

- `rtp.AccessUnitSplitter` cuts an encoder's byte stream into pictures. A byte
  stream has no frame lengths, so a picture is complete only when the next one
  begins; the splitter decides that on the first two bytes of the next picture.
  The cost of feeding video through a pipe is therefore one frame interval.
- RTP timestamps follow the real frame rate (`SendVideo` stamps the moment of
  the call, `SendVideoAt` takes a presentation time). The fixed 15 fps step this
  replaces made a 30 fps stream play at half speed.
- Checked: split, packetise, depacketise, and ffmpeg decodes the result to the
  same pictures as the original.
- Not checked: whether a phone shows the picture. The live video path has never
  been validated, here or upstream, and an audio call that the person upgrades
  to video is still work in progress upstream. Treat the flags as experimental.

## Library

```go
client := agentcaller.NewClient(wa) // wa is a connected *whatsmeow.Client

call, _ := client.Call(ctx, "+41790000000")

voice := agentcaller.NewLiveSource(agentcaller.LiveConfig{Rate: 22050})
call.OnReady(func() { call.Play(voice) })
call.Receive(agentcaller.SinkFunc(func(pcm []float32) { /* 60 ms, 16 kHz, mono */ }))

voice.WritePCM16(chunk) // from any goroutine, never blocks
voice.Flush()           // the person interrupted
```

| Package | Purpose |
| --- | --- |
| `agentcaller` | client, calls, sources and sinks, `LiveSource` |
| `mlow` | the MLow speech codec (encoder and decoder) |
| `jitter` | puts received packets in order without a playout delay |
| `plc` | waveform continuation for lost packets (human listeners) |
| `resample` | band-limited streaming sample-rate conversion |
| `rtp`, `srtp`, `stun`, `relay`, `signaling` | the transport underneath |

## Pairing and the session store

`agentcaller pair` shows a QR code. Scan it under *Linked devices* in the
WhatsApp account the agent calls **from**, not the one it calls. The session is
kept in `<user config dir>/agentcaller/whatsapp.db` (override with `-store`);
it is a credential: file mode `0600`, directory `0700`, never in a repository.
QR codes and media are not logged. `AGENTCALLER_LOG_LEVEL=debug` turns on
protocol logging.

## Credits

- [meowcaller](https://github.com/purpshell/meowcaller) by Rajeh Taher
  (purpshell): the WhatsApp VoIP stack and the pure-Go MLow codec this is built
  on. If AgentCaller is useful to you, [sponsor the original](https://purpshell.dev/sponsor).
- [steipete/meowcaller](https://github.com/steipete/meowcaller): the headless
  `pair` and `notify` commands this fork started from.
- [WaCalls](https://github.com/JotaDev66/WaCalls) by jotadev66: the design of
  the video call path.
- The [WhatsApp Calls Research Group](https://wacrg.org) and the whatsapp-rust
  port that the Go code was validated against.
- [whatsmeow](https://github.com/tulir/whatsmeow) by Tulir Asokan: the WhatsApp
  Web client underneath.

The codec work follows the habits of the people who built FFmpeg: keep the
bitstream exact and prove it with a hash, measure before and after, and write
down what was not tested.

## License

MIT, see [LICENSE](LICENSE). Copyright of the original work remains with its
authors.
