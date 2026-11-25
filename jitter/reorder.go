// Package jitter orders inbound RTP audio for a listener that is a program, not a
// loudspeaker.
//
// A classic jitter buffer delays every packet by a playout margin so a sound card never
// runs dry. An agent has no sound card: speech recognition and voice-activity detection
// only need the audio complete and in order. So this buffer adds no delay at all while
// packets arrive in sequence. Only when a gap opens does it hold the packets behind the
// gap, and only for a bounded time, before it reports the missing packets as lost so the
// caller can conceal them and move on.
package jitter

import "time"

// Packet is one received RTP audio packet. Payload is retained until delivered, so the
// caller must not reuse its backing array.
type Packet struct {
	Seq     uint16
	TS      uint32
	Payload []byte
}

// Kind tells an Event apart.
type Kind uint8

const (
	// Audio carries a packet to decode.
	Audio Kind = iota
	// Lost reports packets that never arrived (or arrived too late): conceal them.
	Lost
)

// Event is one step of the ordered output stream.
type Event struct {
	Kind   Kind
	Packet Packet // Kind == Audio
	// Packets is the number of consecutive missing packets (Kind == Lost).
	Packets int
	// TSGap is the RTP timestamp distance from the last delivered packet to the next one
	// that follows the loss (Kind == Lost), or 0 when unknown. It includes the duration
	// of the last delivered packet.
	TSGap uint32
}

// Config bounds the buffer. Zero values select the defaults.
type Config struct {
	// MaxWait is how long packets behind a gap are held for the missing one. One packet
	// interval is enough to absorb the reordering a relayed UDP path produces.
	MaxWait time.Duration // default 80 ms
	// MaxHeld forces the gap closed once this many packets wait behind it.
	MaxHeld int // default 4
	// MaxGap is the largest forward sequence jump treated as loss. A larger jump is a
	// stream restart: the buffer resynchronises on the new sequence without reporting
	// thousands of lost packets.
	MaxGap int // default 50 (3 s of 60 ms packets)
}

func (c Config) withDefaults() Config {
	if c.MaxWait <= 0 {
		c.MaxWait = 80 * time.Millisecond
	}
	if c.MaxHeld <= 0 {
		c.MaxHeld = 4
	}
	if c.MaxGap <= 0 {
		c.MaxGap = 50
	}
	return c
}

// Stats counts what the buffer saw.
type Stats struct {
	Received   uint64 // packets pushed
	Delivered  uint64 // packets handed out as Audio
	Lost       uint64 // packets reported as Lost
	Reordered  uint64 // packets that arrived out of order but were delivered in order
	Late       uint64 // packets that arrived after their loss was reported (dropped)
	Duplicates uint64 // packets seen twice (dropped)
	Resyncs    uint64 // stream restarts
}

type held struct {
	p Packet
}

// Reorder is the buffer. It is not safe for concurrent use; the caller serialises Push
// and Flush.
type Reorder struct {
	cfg      Config
	started  bool
	next     uint16 // next sequence number to deliver
	lastTS   uint32 // timestamp of the last delivered packet
	held     map[uint16]held
	deadline time.Time // when the current gap is given up; zero when nothing is held
	stats    Stats
}

// New returns a buffer with cfg (zero value: defaults).
func New(cfg Config) *Reorder {
	return &Reorder{cfg: cfg.withDefaults(), held: make(map[uint16]held)}
}

// Stats returns the counters.
func (r *Reorder) Stats() Stats { return r.stats }

// Deadline reports when Flush has to be called at the latest, if packets are held.
func (r *Reorder) Deadline() (time.Time, bool) {
	return r.deadline, len(r.held) > 0
}

// Push adds a packet that arrived at now and appends every event that became
// deliverable to out, in stream order.
func (r *Reorder) Push(p Packet, now time.Time, out []Event) []Event {
	r.stats.Received++
	if !r.started {
		r.started = true
		r.next = p.Seq
	}
	ahead := int(int16(p.Seq - r.next)) // signed distance, wraps correctly at 65535→0
	switch {
	case ahead == 0:
		out = r.deliver(p, out)
		out = r.drain(out)
	case ahead < 0:
		// Behind the delivery point: its loss was already reported, or it is a copy.
		if -ahead > r.cfg.MaxGap {
			out = r.resync(p, out)
			break
		}
		r.stats.Late++
	case ahead > r.cfg.MaxGap:
		out = r.resync(p, out)
	default:
		if _, dup := r.held[p.Seq]; dup {
			r.stats.Duplicates++
			break
		}
		if len(r.held) == 0 {
			r.deadline = now.Add(r.cfg.MaxWait)
		}
		r.held[p.Seq] = held{p: p}
		for len(r.held) >= r.cfg.MaxHeld {
			out = r.skip(out)
		}
	}
	if len(r.held) == 0 {
		r.deadline = time.Time{}
	}
	return out
}

// Flush gives up on the current gap if its deadline has passed at now, appending the
// resulting events to out. Call it when Deadline expires without a new packet.
func (r *Reorder) Flush(now time.Time, out []Event) []Event {
	for len(r.held) > 0 && !now.Before(r.deadline) {
		out = r.skip(out)
		if len(r.held) > 0 {
			// The packets still waiting sit behind a second gap. They have already
			// waited a full MaxWait behind the first one, so close it at once too.
			r.deadline = now
		}
	}
	if len(r.held) == 0 {
		r.deadline = time.Time{}
	}
	return out
}

func (r *Reorder) deliver(p Packet, out []Event) []Event {
	r.stats.Delivered++
	r.lastTS = p.TS
	r.next = p.Seq + 1
	return append(out, Event{Kind: Audio, Packet: p})
}

// drain delivers held packets that are now in sequence.
func (r *Reorder) drain(out []Event) []Event {
	for {
		h, ok := r.held[r.next]
		if !ok {
			return out
		}
		delete(r.held, r.next)
		r.stats.Reordered++
		out = r.deliver(h.p, out)
	}
}

// skip reports the packets up to the nearest held one as lost and resumes there.
func (r *Reorder) skip(out []Event) []Event {
	best, bestAhead := uint16(0), -1
	for seq := range r.held {
		if a := int(int16(seq - r.next)); bestAhead < 0 || a < bestAhead {
			best, bestAhead = seq, a
		}
	}
	if bestAhead <= 0 {
		return r.drain(out)
	}
	h := r.held[best]
	r.stats.Lost += uint64(bestAhead)
	out = append(out, Event{Kind: Lost, Packets: bestAhead, TSGap: h.p.TS - r.lastTS})
	r.next = best
	return r.drain(out)
}

// resync restarts on p after a sequence jump too large to be loss.
func (r *Reorder) resync(p Packet, out []Event) []Event {
	r.stats.Resyncs++
	clear(r.held)
	return r.deliver(p, out)
}
