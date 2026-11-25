package mlow

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// encodeGoldenSHA256 is the SHA-256 over every packet the encoder produces for the
// speech fixture, taken from the unoptimised reference port (the fork commit). Speed work
// on the encoder must not move a single bit: the far end is WhatsApp's own decoder, and
// the port was validated against it in exactly this form.
const encodeGoldenSHA256 = "4f707800918b7290067b74afcf11bc817da5a77b7fe97fb8445d6e1d39b65589"

// encodeFixtureDigest encodes the fixture twice with two fresh encoders (interleaved, so
// a buffer shared between instances would corrupt one of them) and returns the digest of
// the first stream after checking both agree.
func encodeFixtureDigest(t *testing.T) string {
	t.Helper()
	frames := benchSpeech(t)
	a, b := NewMlowEncoder(), NewMlowEncoder()
	h := sha256.New()
	for i, f := range frames {
		pa, err := a.Encode(f)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		pb, err := b.Encode(f)
		if err != nil {
			t.Fatalf("frame %d (second encoder): %v", i, err)
		}
		if string(pa) != string(pb) {
			t.Fatalf("frame %d: two encoders fed the same audio disagree", i)
		}
		h.Write([]byte{byte(len(pa)), byte(len(pa) >> 8)})
		h.Write(pa)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestEncodeBitExactGolden(t *testing.T) {
	if got := encodeFixtureDigest(t); got != encodeGoldenSHA256 {
		t.Fatalf("encoder output changed: digest %s, want %s", got, encodeGoldenSHA256)
	}
}
