# Test audio

`call-opus16k.wav` is synthetic. No recording of a person is part of this
repository.

It has the shape of the start of a call as the receiver sees it: about two
seconds of low noise, then continuous speech that is cut off in the middle of a
sentence (16 kHz mono, 8.0 s, 16-bit PCM).

How it was made:

1. A German sentence spoken by [Piper](https://github.com/rhasspy/piper) with
   the `de_DE-thorsten-medium` voice.
2. Encoded with libopus at 16 kbit/s in 60 ms frames and decoded again
   (`ffmpeg -c:a libopus -b:a 16k -application voip -frame_duration 60`), so the
   samples carry real codec artefacts.
3. Scaled to an RMS of 0.15 and laid over smoothed white noise at an RMS of
   0.0018, starting at 2.04 s.

What depends on it:

- `vad_test.go` and `call_test.go` expect the speech to start between 1.9 s and
  2.3 s and to run to the end of the file.
- `mlow/golden_test.go` hashes the encoder output for exactly these samples.
  Replacing the file means taking a new hash from the unoptimised encoder (the
  fork commit) and checking that the current encoder still matches it.
- `mlow/bench_test.go` uses it as benchmark input.

The detector thresholds in `vad.go` were calibrated on a recording of a real
call. That recording stays private. The speed figures in the changelog were
measured on it too; on this file the same machine measures 3.28 ms and 0.93 ms
per frame for the reference and the current encoder.
