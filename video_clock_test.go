package agentcaller

import (
	"testing"
	"time"
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
