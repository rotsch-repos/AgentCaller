package jitter

import (
	"math/rand"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

func pkt(seq int) Packet {
	return Packet{Seq: uint16(seq), TS: uint32(seq) * 960, Payload: []byte{byte(seq)}}
}

// seqs flattens events into delivered sequence numbers; a loss of n packets appears as
// n entries of -1.
func seqs(ev []Event) []int {
	var out []int
	for _, e := range ev {
		if e.Kind == Audio {
			out = append(out, int(e.Packet.Seq))
			continue
		}
		for i := 0; i < e.Packets; i++ {
			out = append(out, -1)
		}
	}
	return out
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestInOrderAddsNoDelay(t *testing.T) {
	r := New(Config{})
	for i := 100; i < 200; i++ {
		ev := r.Push(pkt(i), t0, nil)
		if len(ev) != 1 || ev[0].Kind != Audio || int(ev[0].Packet.Seq) != i {
			t.Fatalf("packet %d not delivered at once: %+v", i, ev)
		}
		if _, holding := r.Deadline(); holding {
			t.Fatalf("packet %d: buffer holds packets on an in-order stream", i)
		}
	}
}

func TestSwapIsRepaired(t *testing.T) {
	r := New(Config{})
	var ev []Event
	for _, s := range []int{1, 2, 4, 3, 5} {
		ev = r.Push(pkt(s), t0, ev)
	}
	if got := seqs(ev); !equal(got, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("got %v", got)
	}
	if st := r.Stats(); st.Lost != 0 || st.Reordered != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestLossIsReportedAfterDeadline(t *testing.T) {
	r := New(Config{MaxWait: 80 * time.Millisecond})
	ev := r.Push(pkt(1), t0, nil)
	ev = r.Push(pkt(3), t0.Add(60*time.Millisecond), ev) // 2 missing
	if got := seqs(ev); !equal(got, []int{1}) {
		t.Fatalf("packet behind the gap released early: %v", got)
	}
	dl, holding := r.Deadline()
	if !holding || !dl.Equal(t0.Add(140*time.Millisecond)) {
		t.Fatalf("deadline %v holding %v", dl, holding)
	}
	if ev = r.Flush(t0.Add(139*time.Millisecond), ev); len(ev) != 1 {
		t.Fatalf("flushed before the deadline: %v", seqs(ev))
	}
	ev = r.Flush(t0.Add(140*time.Millisecond), ev)
	if got := seqs(ev); !equal(got, []int{1, -1, 3}) {
		t.Fatalf("got %v", got)
	}
	if ev[1].TSGap != 2*960 {
		t.Fatalf("ts gap %d, want %d", ev[1].TSGap, 2*960)
	}
	// The straggler shows up after all: dropped, never delivered out of order.
	ev = r.Push(pkt(2), t0.Add(200*time.Millisecond), ev)
	if got := seqs(ev); !equal(got, []int{1, -1, 3}) {
		t.Fatalf("late packet delivered: %v", got)
	}
	if st := r.Stats(); st.Late != 1 || st.Lost != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestHeldLimitClosesGapWithoutTimer(t *testing.T) {
	r := New(Config{MaxHeld: 3})
	var ev []Event
	for _, s := range []int{10, 12, 13, 14} {
		ev = r.Push(pkt(s), t0, ev)
	}
	if got := seqs(ev); !equal(got, []int{10, -1, 12, 13, 14}) {
		t.Fatalf("got %v", got)
	}
}

func TestWrapAround(t *testing.T) {
	r := New(Config{})
	var ev []Event
	for _, s := range []int{65534, 65535, 1, 0, 2} {
		ev = r.Push(pkt(s), t0, ev)
	}
	if got := seqs(ev); !equal(got, []int{65534, 65535, 0, 1, 2}) {
		t.Fatalf("got %v", got)
	}
}

func TestDuplicatesAreDropped(t *testing.T) {
	r := New(Config{})
	var ev []Event
	for _, s := range []int{1, 1, 3, 3, 2, 2} {
		ev = r.Push(pkt(s), t0, ev)
	}
	if got := seqs(ev); !equal(got, []int{1, 2, 3}) {
		t.Fatalf("got %v", got)
	}
}

func TestStreamRestartResyncs(t *testing.T) {
	r := New(Config{})
	ev := r.Push(pkt(100), t0, nil)
	ev = r.Push(pkt(40000), t0, ev)
	ev = r.Push(pkt(40001), t0, ev)
	if got := seqs(ev); !equal(got, []int{100, 40000, 40001}) {
		t.Fatalf("got %v", got)
	}
	if st := r.Stats(); st.Resyncs != 1 || st.Lost != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// TestRandomNetwork drives the buffer with a seeded lossy, reordering network and checks
// the invariants that matter to a decoder: strictly increasing order, every packet
// accounted for exactly once (delivered, lost or late), nothing invented.
func TestRandomNetwork(t *testing.T) {
	for seed := int64(1); seed <= 50; seed++ {
		rng := rand.New(rand.NewSource(seed))
		const n = 2000
		type arrival struct {
			seq int
			at  time.Duration
		}
		var arr []arrival
		sent := 0
		for i := 0; i < n; i++ {
			if rng.Float64() < 0.05 {
				continue // dropped by the network
			}
			sent++
			delay := time.Duration(rng.Intn(20)) * time.Millisecond
			if rng.Float64() < 0.05 {
				delay += time.Duration(40+rng.Intn(200)) * time.Millisecond // a straggler
			}
			arr = append(arr, arrival{i, time.Duration(i)*60*time.Millisecond + delay})
		}
		// Stable sort by arrival time.
		for i := 1; i < len(arr); i++ {
			for j := i; j > 0 && arr[j].at < arr[j-1].at; j-- {
				arr[j], arr[j-1] = arr[j-1], arr[j]
			}
		}
		r := New(Config{})
		var ev []Event
		for _, a := range arr {
			now := t0.Add(a.at)
			if dl, holding := r.Deadline(); holding && !now.Before(dl) {
				ev = r.Flush(dl, ev)
			}
			ev = r.Push(pkt(a.seq+500), now, ev)
		}
		ev = r.Flush(t0.Add(time.Hour), ev)

		last := -1
		for _, e := range ev {
			if e.Kind != Audio {
				continue
			}
			s := int(e.Packet.Seq)
			if s <= last {
				t.Fatalf("seed %d: order violated: %d after %d", seed, s, last)
			}
			last = s
		}
		st := r.Stats()
		if st.Delivered+st.Late+st.Duplicates != uint64(sent) {
			t.Fatalf("seed %d: sent %d, delivered %d + late %d", seed, sent, st.Delivered, st.Late)
		}
		// Every sequence number between the first and last delivered one is either
		// delivered or reported lost.
		first := int(ev[0].Packet.Seq)
		if span := uint64(last - first + 1); st.Delivered+st.Lost != span {
			t.Fatalf("seed %d: span %d != delivered %d + lost %d", seed, span, st.Delivered, st.Lost)
		}
	}
}
