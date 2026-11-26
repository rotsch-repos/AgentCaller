package agentcaller

import (
	"testing"
	"time"

	"github.com/rotsch-repos/agentcaller/rtp"
)

func TestVideoClockFollowsRealFrameRate(t *testing.T) {
	for _, fps := range []int{15, 30, 60} {
		var c videoClock
		var prev uint32
		for i := 0; i < 5*fps; i++ {
			ts := c.stamp(time.Duration(i) * time.Second / time.Duration(fps))
			if i > 0 && ts-prev != uint32(videoClockRate/fps) {
				t.Fatalf("%d fps: step %d at picture %d, want %d", fps, ts-prev, i, videoClockRate/fps)
			}
			prev = ts
		}
		if want := uint32(5*videoClockRate - videoClockRate/fps); prev != want {
			t.Fatalf("%d fps: timestamp %d after 5 s, want %d", fps, prev, want)
		}
	}
}

func TestVideoClockNeverRunsBackwards(t *testing.T) {
	var c videoClock
	a := c.stamp(10 * time.Second) // first picture defines the origin
	b := c.stamp(10*time.Second + 40*time.Millisecond)
	d := c.stamp(10*time.Second + 40*time.Millisecond) // same instant twice
	e := c.stamp(10*time.Second + 10*time.Millisecond) // late picture
	f := c.stamp(10*time.Second + 80*time.Millisecond)
	if a != 0 || b != 3600 || d != b+1 || e != d+1 || f != 7200 {
		t.Fatalf("timestamps %d %d %d %d %d", a, b, d, e, f)
	}
}

// A sender that is told when each picture is shown puts it there on the RTP clock, and
// starts that clock with the first picture that really goes out: pictures dropped while
// it waits for a keyframe must not open a gap.
func TestVideoSenderTimesPicturesByPresentationTime(t *testing.T) {
	pipe, err := NewMediaPipeline(iota32(), "111111111111111:0@lid", "222222222222222:0@lid", 0x55667788, FrameSamples)
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	sender := &videoSender{pipe: pipe, stream: rtp.NewVideoRtpStream(0x55667788, 4500), active: true, keyframeRequired: true}
	delta := []byte{0, 0, 0, 1, 0x41, 1, 2, 3}
	idr := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0x1f, 0, 0, 0, 1, 0x68, 0xce, 6, 0xe2, 0, 0, 0, 1, 0x65, 1, 2, 3}
	stamp := func(au []byte, pts time.Duration) (uint32, bool) {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		packets := sender.protectAccessUnitLocked(au, videoTiming{byPTS: true, pts: pts})
		if len(packets) == 0 {
			return 0, false
		}
		h, ok := rtp.ParseRtpHeader(packets[len(packets)-1])
		if !ok || !h.Marker {
			t.Fatalf("last packet of the picture at %s has no marker", pts)
		}
		return h.Timestamp, true
	}
	if _, sent := stamp(delta, 0); sent {
		t.Fatal("a dependent picture went out before a keyframe")
	}
	base, sent := stamp(idr, 5*time.Second)
	if !sent {
		t.Fatal("keyframe was not sent")
	}
	for i, pts := range []time.Duration{5*time.Second + time.Second/30, 5*time.Second + 100*time.Millisecond, 6 * time.Second} {
		ts, sent := stamp(delta, pts)
		want := base + []uint32{3000, 9000, 90000}[i]
		if !sent || ts != want {
			t.Fatalf("picture at %s: timestamp %d (sent %v), want %d", pts, ts, sent, want)
		}
	}
}
