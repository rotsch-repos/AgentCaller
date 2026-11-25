# Working on AgentCaller

For people and coding agents. The files under `docs/upstream/` are the notes of
the project this was forked from; they describe how that code base was built and
are kept for reference, not as rules for this repository.

## Build and test

```sh
go build ./...
go vet ./...
gofmt -l .          # must print nothing
go test ./...       # everything offline; ffmpeg is used if installed
go test -race ./cmd/agentcaller .
```

`examples/cli` is its own module (it needs CGO for audio devices) and is not
part of `go test ./...`.

## Rules

1. **The codec's bitstream does not change by accident.** `mlow` has golden
   tests with a hash over the encoder output. A speed-up that changes one bit is
   a different codec; if a change is meant to alter the bitstream, say so in the
   commit and explain how it was checked against a real peer.
2. **Measure, then claim.** Speed, delay, quality: put the number and how it was
   obtained in the commit message. If a measurement contradicts the idea, the
   measurement wins (packet loss concealment is off by default for exactly this
   reason, see `plc`).
3. **The media loop never blocks.** It sends a frame every 60 ms whatever
   happens. Nothing called from it may wait for a pipe, a model or a lock held
   across I/O. Sources return `(nil, nil)` for "nothing yet", sinks hand off.
4. **Say what was not tested.** Most of this can only be proven in a real call.
   Code that has not seen one keeps a `NOT VALIDATED` comment, and the README
   status table keeps its "no live call yet". Remove either only after a live
   check, and say in the commit what was observed.
5. **A live test rings a real phone.** Never place a call, and never pair a
   device, without the owner of the account asking for it in that moment.
6. **No real data in the repository.** No phone numbers, JIDs, keys, session
   stores, QR codes, packet captures or recordings of people. Test audio and
   video are synthetic.
7. **Keep the credits.** The licence, the copyright line and the upstream
   attributions stay.

## Layout

- `mlow/` codec. `jitter/`, `plc/`, `rx.go` receive path. `resample/`,
  `source.go`, `live.go`, `player.go` send path.
- `engine*.go` call signalling and the media loop. `rtp/`, `srtp/`, `stun/`,
  `relay/`, `signaling/` transport.
- `cmd/agentcaller/` the command: `pair`, `notify`, `talk`, `call`.
