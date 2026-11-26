package agentcaller

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rotsch-repos/agentcaller/diag"
	"github.com/rotsch-repos/agentcaller/rtp"
)

func TestVideoRtpDurationSamples(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     uint32
	}{
		{name: "zero uses fallback", duration: 0, want: defaultVideoRtpStepSamples},
		{name: "negative uses fallback", duration: -time.Millisecond, want: defaultVideoRtpStepSamples},
		{name: "30 fps", duration: time.Second / 30, want: 3000},
		{name: "60 fps", duration: time.Second / 60, want: 1500},
		{name: "sub sample uses fallback", duration: time.Nanosecond, want: defaultVideoRtpStepSamples},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := videoRtpDurationSamples(tt.duration); got != tt.want {
				t.Fatalf("videoRtpDurationSamples(%s) = %d, want %d", tt.duration, got, tt.want)
			}
		})
	}
}

func TestVideoSenderStartsAtIDRAndUsesWhatsappHeaders(t *testing.T) {
	callKey := iota32()
	pipe, err := NewMediaPipeline(callKey, "111111111111111:0@lid", "222222222222222:0@lid", 0x55667788, FrameSamples)
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	sender := &videoSender{
		pipe:             pipe,
		stream:           rtp.NewVideoRtpStream(0x55667788, 4500),
		active:           true,
		keyframeRequired: true,
	}

	delta := []byte{0, 0, 0, 1, 0x41, 1, 2, 3}
	if packets := sender.protectAccessUnit(delta, 50*time.Millisecond); len(packets) != 0 {
		t.Fatalf("dependent frame produced %d packets before an IDR", len(packets))
	}

	idr := []byte{
		0, 0, 0, 1, 0x67, 0x42, 0, 0x1f,
		0, 0, 0, 1, 0x68, 0xce, 6, 0xe2,
		0, 0, 0, 1, 0x65, 1, 2, 3,
	}
	packets := sender.protectAccessUnit(idr, 50*time.Millisecond)
	if len(packets) != 1 {
		t.Fatalf("IDR produced %d packets, want one packed access-unit packet", len(packets))
	}
	receiver, err := NewMediaPipeline(callKey, "222222222222222:0@lid", "111111111111111:0@lid", 0x55667788, FrameSamples)
	if err != nil {
		t.Fatalf("receiver pipe: %v", err)
	}
	var depack rtp.H264Depacketizer
	var reconstructed []byte
	for i, packet := range packets {
		header, ok := rtp.ParseRtpHeader(packet)
		if !ok {
			t.Fatalf("packet %d has no RTP header", i)
		}
		wantHeaderSize := rtp.WhatsappVideoRtpHeaderSize
		if i == 0 {
			wantHeaderSize += 4
		}
		if n, ok := rtp.RtpHeaderByteLength(packet); !ok || n != wantHeaderSize {
			t.Fatalf("packet %d header length = (%d, %v), want (%d, true)", i, n, ok, wantHeaderSize)
		}
		if header.VideoExtension == nil || header.VideoExtension.MediaFrameInfo != rtp.VideoMediaFrameInfoIDR {
			t.Fatalf("packet %d video extension = %+v", i, header.VideoExtension)
		}
		_, payload, ok := receiver.UnprotectAudio(packet)
		if !ok {
			t.Fatalf("packet %d did not unprotect", i)
		}
		for _, nalu := range depack.Depacketize(payload) {
			reconstructed = append(reconstructed, 0, 0, 0, 1)
			reconstructed = append(reconstructed, nalu...)
		}
	}
	if !bytes.Equal(reconstructed, idr) {
		t.Fatalf("reconstructed access unit = %x, want %x", reconstructed, idr)
	}
}

func TestVideoSenderRecordsWireDiagnostics(t *testing.T) {
	dir := t.TempDir()
	rec, err := diag.NewRecorder(dir)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	pipe, err := NewMediaPipeline(iota32(), "111111111111111:0@lid", "222222222222222:0@lid", 0x55667788, FrameSamples)
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	sender := &videoSender{
		pipe: pipe, stream: rtp.NewVideoRtpStream(0x55667788, 4500),
		callID: "test-call", active: true, keyframeRequired: true, diag: rec,
	}
	idr := []byte{0, 0, 0, 1, 0x65, 1, 2, 3}
	if packets := sender.protectAccessUnit(idr, 50*time.Millisecond); len(packets) != 1 {
		t.Fatalf("IDR produced %d packets, want 1", len(packets))
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("close recorder: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "video_wire.jsonl"))
	if err != nil {
		t.Fatalf("read video wire diagnostics: %v", err)
	}
	text := string(data)
	for _, want := range []string{`"event":"access_unit"`, `"event":"packet"`, `"direction":"out"`, `"call_id":"test-call"`, `"header_hex":`, `"payload_hex":`, `"protected_hex":`} {
		if !strings.Contains(text, want) {
			t.Errorf("video wire diagnostics missing %s: %s", want, text)
		}
	}
}

func TestVideoSenderGatesUpgradeUntilPeerAcceptance(t *testing.T) {
	pipe, err := NewMediaPipeline(iota32(), "111111111111111:0@lid", "222222222222222:0@lid", 0x55667788, FrameSamples)
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	sender := &videoSender{pipe: pipe, stream: rtp.NewVideoRtpStream(0x55667788, 4500)}
	idr := []byte{0, 0, 0, 1, 0x65, 1, 2, 3}
	delta := []byte{0, 0, 0, 1, 0x41, 1, 2, 3}

	if packets := sender.protectAccessUnit(idr, 50*time.Millisecond); len(packets) != 0 {
		t.Fatalf("inactive sender produced %d packets", len(packets))
	}
	sender.enable(true)
	if packets := sender.protectAccessUnit(idr, 50*time.Millisecond); len(packets) != 0 {
		t.Fatalf("send-gated sender produced %d packets", len(packets))
	}
	sender.enable(false)
	if packets := sender.protectAccessUnit(delta, 50*time.Millisecond); len(packets) != 0 {
		t.Fatalf("ungated sender sent dependent frame before recovery IDR: %d packets", len(packets))
	}
	if packets := sender.protectAccessUnit(idr, 50*time.Millisecond); len(packets) == 0 {
		t.Fatal("ungated sender did not send recovery IDR")
	}
	sender.disable()
	if packets := sender.protectAccessUnit(idr, 50*time.Millisecond); len(packets) != 0 {
		t.Fatalf("disabled sender produced %d packets", len(packets))
	}
}

func TestMediaSrtcpSenderProtectsVideoReport(t *testing.T) {
	sender, err := newMediaSrtcpSender(iota32(), "111111111111111:0@lid", 0x55667788, true)
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	packet, err := sender.senderReport(rtp.RtcpSenderStats{
		PacketsSent:  3,
		OctetsSent:   400,
		RtpTimestamp: 90000,
	}, 1700000000000, nil)
	if err != nil {
		t.Fatalf("sender report: %v", err)
	}
	if kind := rtp.IsRtcpPacket(packet); !kind {
		t.Fatal("protected sender report is not classified as RTCP")
	}
	if len(packet) != 60+14 {
		t.Fatalf("protected report length = %d, want 74", len(packet))
	}
}

func TestMediaSrtcpReceiverRekeysForAnsweringDevice(t *testing.T) {
	callKey := iota32()
	const ssrc = 0x55667788
	receiver, err := newMediaSrtcpReceiver(callKey, "222222222222222:0@lid")
	if err != nil {
		t.Fatalf("receiver: %v", err)
	}
	sender, err := newMediaSrtcpSender(callKey, "222222222222222:7@lid", ssrc, true)
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	packet, err := sender.senderReport(rtp.RtcpSenderStats{PacketsSent: 1}, 1700000000000, nil)
	if err != nil {
		t.Fatalf("sender report: %v", err)
	}

	if err = receiver.rekey(callKey, "222222222222222:7@lid"); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if _, _, ok := receiver.unprotect(ssrc, packet); !ok {
		t.Fatal("rekeyed SRTCP receiver rejected answering-device report")
	}
}

// What we send to get the picture back after a loss must read, on the other side, as
// the request our own receive path understands from WhatsApp.
func TestMediaSrtcpSenderAsksForKeyframe(t *testing.T) {
	callKey := iota32()
	const self, peerVideo = 0x55667788, 0x0a0b0c0d
	sender, err := newMediaSrtcpSender(callKey, "111111111111111:0@lid", self, true)
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	receiver, err := newMediaSrtcpReceiver(callKey, "111111111111111:0@lid")
	if err != nil {
		t.Fatalf("receiver: %v", err)
	}
	for i := 0; i < 2; i++ { // each request has its own index
		packet, err := sender.pictureLossIndication(peerVideo)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !rtp.IsRtcpPacket(packet) {
			t.Fatalf("request %d is not classified as RTCP", i)
		}
		plain, _, ok := receiver.unprotect(self, packet)
		if !ok {
			t.Fatalf("request %d does not authenticate", i)
		}
		if !rtp.RtcpRequestsKeyframe(plain, peerVideo) {
			t.Fatalf("request %d = %x, not a keyframe request for the peer's video", i, plain)
		}
		if rtp.RtcpRequestsKeyframe(plain, self) {
			t.Fatalf("request %d names our own stream", i)
		}
	}
}

func TestVideoPLIDue(t *testing.T) {
	for _, c := range []struct {
		lost, awaiting bool
		since          time.Duration
		want           bool
	}{
		{lost: true, awaiting: true, since: time.Hour, want: true},               // a loss: ask at once
		{lost: true, awaiting: true, since: 100 * time.Millisecond, want: false}, // not for every packet of a burst
		{awaiting: true, since: 500 * time.Millisecond, want: false},             // asked, give it time
		{awaiting: true, since: 1100 * time.Millisecond, want: true},             // still nothing: ask again
		{since: time.Hour, want: false},                                          // all is well
		{lost: true, since: 400 * time.Millisecond, want: true},                  // a new loss while already recovering
	} {
		if got := videoPLIDue(c.lost, c.awaiting, c.since); got != c.want {
			t.Errorf("videoPLIDue(%v, %v, %v) = %v, want %v", c.lost, c.awaiting, c.since, got, c.want)
		}
	}
}
