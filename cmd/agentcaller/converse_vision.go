package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rotsch-repos/agentcaller"
)

// What the peer's camera shows, as part of a conversation (converse -vision).
//
// The peer switches its camera on during the call; the call accepts that without
// sending a picture of its own. The H.264 that arrives goes through one long-running
// decoder command to JPEGs, of which the last seconds are kept. When the peer has said
// something, the picture from the end of its words goes to the model with them.
//
// NOT VALIDATED: decoder, timing and the model's part are tested offline and with
// -simulate-video. That a phone's camera arrives this way in a live call (the upgrade
// is accepted, pictures come, the orientation is the right one) has not been seen yet.

// visionRules follows the telephone rules when the agent may see.
const visionRules = "Dein Gegenüber kann im Gespräch die Kamera einschalten. Dann liegt dem, was es sagt, ein Bild bei: " +
	"das, was die Kamera in diesem Augenblick zeigt. Sprich darüber so selbstverständlich, wie wenn du daneben stündest, " +
	"und nur über das, was im Bild wirklich zu erkennen ist; was du nicht erkennst, errätst du nicht. " +
	"Du musst das Bild nicht jedes Mal beschreiben: Geh darauf ein, wenn die Frage es meint oder dir etwas auffällt. " +
	"Liegt kein Bild bei, siehst du in diesem Moment nichts; behaupte dann nicht, etwas zu sehen, " +
	"sondern sag, dass du gerade kein Bild hast."

// sawNote stands in the history where a picture was: later turns do not get it again.
const sawNote = "(Dazu lag ein Bild aus der Kamera vor; du siehst es jetzt nicht mehr.)"

// defaultVisionCmd decodes H.264 (Annex B) on stdin to a stream of JPEGs on stdout, one
// per picture and each as soon as it is decoded. Frame threads and input buffering
// would hold pictures back; -fflags nobuffer is left out on purpose, with it ffmpeg
// loses the parameter sets at the head of the stream and shows nothing until the next
// keyframe.
const defaultVisionCmd = `ffmpeg -hide_banner -loglevel error -threads 1 -flags low_delay -probesize 32 -analyzeduration 0 ` +
	`-f h264 -i pipe:0 -an -vf "scale=w='min(768,iw)':h='min(768,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2" ` +
	`-fps_mode passthrough -c:v mjpeg -q:v 4 -flush_packets 1 -f image2pipe pipe:1`

const (
	eyeSpacing = 200 * time.Millisecond // between the pictures that are kept
	eyeKept    = 40                     // of them: the last eight seconds
	eyeStale   = 4 * time.Second        // nothing newer than this before the words: the camera is off or stuck
	eyeBacklog = 256                    // access units the decoder may be behind before they are dropped
	eyeTries   = 3                      // decoders that may fail in a row before the eye gives up
	eyeTail    = 500 * time.Millisecond // what the decoder still shows after the camera went off is not kept
)

// eyeRetry is how long a decoder that ended stays down before it is started again.
var eyeRetry = 2 * time.Second

// picture is one moment of the peer's camera.
type picture struct {
	jpeg  []byte
	at    time.Time
	turns int // quarter turns clockwise that set it upright
}

// eye is the video sink of a call: it keeps the last seconds of the peer's camera as
// still pictures.
type eye struct {
	ctx    context.Context
	stop   context.CancelFunc
	cmd    string
	events *eventLog
	in     chan []byte   // access units on their way to the decoder
	done   chan struct{} // closed when the eye has ended
	lost   atomic.Uint64 // access units that found the way full

	mu     sync.Mutex
	gen    int       // which decoder runs: pictures of an older one no longer count
	recent []picture // oldest first, eyeSpacing apart except for the newest
	turns  int
	seeing bool
	quiet  time.Time // until then pictures are the tail of a camera that was switched off
}

func newEye(ctx context.Context, cmd string, events *eventLog) *eye {
	e := &eye{cmd: cmd, events: events, in: make(chan []byte, eyeBacklog), done: make(chan struct{})}
	e.ctx, e.stop = context.WithCancel(ctx)
	go e.decode()
	return e
}

// WriteVideo takes one access unit from the call. It never waits: the call's media loop
// is the caller, and the audio of the call runs through it.
func (e *eye) WriteVideo(au []byte) error {
	select {
	case e.in <- au:
	default:
		if e.lost.Add(1) == 1 {
			go e.events.emit("vision_error", map[string]any{"error": "the picture decoder does not keep up; pictures are dropped"})
		}
	}
	return nil
}

// SetOrientation is told by the call how the peer holds its phone.
func (e *eye) SetOrientation(quarterTurns int) {
	e.mu.Lock()
	e.turns = ((quarterTurns % 4) + 4) % 4
	e.mu.Unlock()
}

// Close ends the eye. It does not wait for the decoder: the call may be the caller.
func (e *eye) Close() error {
	e.stop()
	return nil
}

// shut ends the eye and waits until its decoder is gone.
func (e *eye) shut() {
	e.stop()
	<-e.done
}

// blind forgets what was seen: the peer has switched its camera off. The decoder stays;
// a camera that comes back starts with a keyframe and needs no new one.
func (e *eye) blind() {
	e.mu.Lock()
	was := e.seeing
	e.recent, e.seeing = nil, false
	e.quiet = time.Now().Add(eyeTail)
	e.mu.Unlock()
	if was {
		e.events.emit("seeing", map[string]any{"on": false})
	}
}

// decoder is one run of the decoder command.
type decoder struct {
	stdin io.WriteCloser
	gone  chan string // why it ended; empty: it had shown pictures before
	shown atomic.Int64
}

// decode feeds the decoder for as long as the eye lives. The command starts with the
// first picture data and is started again when it ends, unless it never shows anything.
func (e *eye) decode() {
	defer close(e.done)
	var (
		dec      *decoder
		failures int
		retryAt  time.Time
	)
	ended := func(why string) {
		shown := dec.shown.Load() > 0
		dec = nil
		if e.ctx.Err() != nil {
			return
		}
		fields := map[string]any{"error": "the picture decoder ended: " + why}
		if shown {
			failures = 0 // it did work; the next picture data starts another
		} else if failures++; failures >= eyeTries {
			fields["gave_up"] = true
		}
		retryAt = time.Now().Add(eyeRetry)
		e.events.emit("vision_error", fields)
	}
	defer func() {
		if dec != nil {
			_ = dec.stdin.Close()
			<-dec.gone
		}
	}()
	for {
		var au []byte
		select {
		case <-e.ctx.Done():
			return
		case au = <-e.in:
		}
		if dec != nil {
			select {
			case why := <-dec.gone:
				ended(why)
			default:
			}
		}
		if dec == nil {
			if failures >= eyeTries || time.Now().Before(retryAt) {
				continue
			}
			var err error
			if dec, err = e.start(); err != nil {
				dec = &decoder{}
				ended(err.Error())
				continue
			}
		}
		if _, err := dec.stdin.Write(au); err != nil {
			_ = dec.stdin.Close()
			ended(<-dec.gone)
		}
	}
}

// start runs the decoder command; its pictures go to add.
func (e *eye) start() (*decoder, error) {
	cmd := exec.CommandContext(e.ctx, "sh", "-c", e.cmd)
	// Something the command started may outlive it and hold its pipes; that long after
	// its end they are closed anyway.
	cmd.WaitDelay = 2 * time.Second
	complaints := &tailBuffer{max: 2048}
	cmd.Stderr = complaints
	pictures, out := io.Pipe()
	cmd.Stdout = out
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.gen++
	gen := e.gen
	e.mu.Unlock()
	dec := &decoder{stdin: stdin, gone: make(chan string, 1)}
	read := make(chan struct{})
	go func() {
		defer close(read)
		_ = readJPEGs(pictures, func(jpg []byte) {
			dec.shown.Add(1)
			e.add(gen, jpg)
		})
	}()
	go func() {
		err := cmd.Wait() // also ends a write to stdin that the command no longer takes
		_ = out.Close()
		<-read
		why := lastLine(complaints.String())
		if why == "" && err != nil {
			why = err.Error()
		}
		if why == "" {
			why = "without a word"
		}
		dec.gone <- why
	}()
	return dec, nil
}

// add keeps one decoded picture.
func (e *eye) add(gen int, jpg []byte) {
	now := time.Now()
	e.mu.Lock()
	if gen != e.gen || e.ctx.Err() != nil || now.Before(e.quiet) {
		e.mu.Unlock()
		return
	}
	p := picture{jpeg: jpg, at: now, turns: e.turns}
	// The newest is always kept; it only stays when it is eyeSpacing after the one before.
	if n := len(e.recent); n >= 2 && e.recent[n-1].at.Sub(e.recent[n-2].at) < eyeSpacing {
		e.recent[n-1] = p
	} else {
		e.recent = append(e.recent, p)
	}
	if extra := len(e.recent) - eyeKept; extra > 0 {
		e.recent = append(e.recent[:0], e.recent[extra:]...)
	}
	first := !e.seeing
	e.seeing = true
	e.mu.Unlock()
	if first {
		fields := map[string]any{"on": true, "bytes": len(jpg), "turns": p.turns}
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(jpg)); err == nil {
			fields["width"], fields["height"] = cfg.Width, cfg.Height
		}
		e.events.emit("seeing", fields)
	}
}

// pick returns what the camera showed at a moment: the newest picture up to it, or the
// first one after it when the camera only just came on. No picture means the camera is
// off, or what it last showed is too old to stand for that moment.
func (e *eye) pick(moment time.Time) (picture, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.recent) == 0 {
		return picture{}, false
	}
	best := e.recent[0]
	for _, p := range e.recent[1:] {
		if p.at.After(moment) {
			break
		}
		best = p
	}
	if moment.Sub(best.at) > eyeStale {
		return picture{}, false
	}
	return best, true
}

// upright is the picture as the peer sees the scene: turned by the quarter turns the
// call reported for it.
func (p picture) upright() []byte {
	if p.turns%4 == 0 {
		return p.jpeg
	}
	src, err := jpeg.Decode(bytes.NewReader(p.jpeg))
	if err != nil {
		return p.jpeg
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, rotate(src, p.turns), &jpeg.Options{Quality: 88}); err != nil {
		return p.jpeg
	}
	return out.Bytes()
}

// rotate turns a picture clockwise by quarter turns.
func rotate(src image.Image, quarterTurns int) image.Image {
	b := src.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)
	w, h := b.Dx(), b.Dy()
	q := ((quarterTurns % 4) + 4) % 4
	if q == 0 {
		return rgba
	}
	dst := image.NewRGBA(image.Rect(0, 0, h, w))
	if q == 2 {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			from := rgba.Pix[y*rgba.Stride+4*x:][:4:4]
			var tx, ty int
			switch q {
			case 1:
				tx, ty = h-1-y, x
			case 2:
				tx, ty = w-1-x, h-1-y
			default:
				tx, ty = y, w-1-x
			}
			copy(dst.Pix[ty*dst.Stride+4*tx:][:4:4], from)
		}
	}
	return dst
}

// readJPEGs cuts a stream of JPEGs, one after the other, into single pictures. A 0xFF in
// a picture's data is escaped, so the end marker does not occur inside it (it could in
// an embedded thumbnail; the decoder's pictures have none).
func readJPEGs(r io.Reader, each func([]byte)) error {
	var soi, eoi = []byte{0xff, 0xd8}, []byte{0xff, 0xd9}
	const tooLarge = 16 << 20
	var buf []byte
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			start := bytes.Index(buf, soi)
			if start < 0 {
				buf = buf[max(len(buf)-1, 0):] // the last byte may be half a marker
				break
			}
			end := bytes.Index(buf[start+2:], eoi)
			if end < 0 {
				buf = buf[start:]
				break
			}
			end += start + 4
			each(bytes.Clone(buf[start:end]))
			buf = buf[end:]
		}
		if len(buf) > tooLarge {
			buf = nil // not a JPEG stream
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// tailBuffer keeps the end of what is written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if extra := len(t.buf) - t.max; extra > 0 {
		t.buf = append(t.buf[:0], t.buf[extra:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// seeingCall is what a call must be able to do for the agent to see.
type seeingCall interface {
	ReceiveVideo(agentcaller.VideoSink)
	OnVideoState(func(agentcaller.VideoState))
	AcceptVideo() error
}

// watch lets the conversation see through the call: the peer's camera is accepted when
// it is offered, and what it shows goes to the eye.
func (c *conversation) watch(call seeingCall) {
	call.ReceiveVideo(c.eye)
	call.OnVideoState(func(s agentcaller.VideoState) {
		c.events.emit("video_state", map[string]any{"active": s.Active, "upgrade": s.Upgrade, "orientation": s.Orientation, "state": s.Raw})
		switch {
		case s.Upgrade:
			go func() { // not on the signalling goroutine: accepting sends stanzas
				if err := call.AcceptVideo(); err != nil {
					c.events.emit("video_error", map[string]any{"error": err.Error()})
					return
				}
				c.events.emit("video_accepted", nil)
			}()
		case !s.Active && (s.Raw == videoStateDisabled || s.Raw == videoStateStopped):
			c.eye.blind()
		}
	})
}

// The states of a video stanza that mean the peer's camera is off.
const (
	videoStateDisabled = 0
	videoStateStopped  = 6
)

// look attaches to a question what the camera showed when it was asked.
func (c *conversation) look(q *chatMessage, s segment) (seen bool, fields map[string]any) {
	if c.eye == nil || s.ended.IsZero() {
		return false, nil
	}
	// The pause was reported that much after the last word.
	spoke := s.ended.Add(-c.cfg.call.endSilence)
	p, ok := c.eye.pick(spoke)
	if !ok {
		return false, nil
	}
	q.Image = p.upright()
	fields = map[string]any{"picture_bytes": len(q.Image), "picture_ms": p.at.Sub(spoke).Milliseconds()}
	if c.cfg.visionSave != "" {
		c.mu.Lock()
		c.seen++
		n := c.seen
		c.mu.Unlock()
		path := filepath.Join(c.cfg.visionSave, fmt.Sprintf("seen-%03d.jpg", n))
		if err := os.WriteFile(path, q.Image, 0o600); err != nil {
			fields["picture_error"] = err.Error()
		} else {
			fields["picture"] = path
		}
	}
	return true, fields
}
