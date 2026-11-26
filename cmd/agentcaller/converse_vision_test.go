package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/rotsch-repos/agentcaller"
)

// testJPEG is a picture of one colour.
func testJPEG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func testEye(t *testing.T, cmd string) (*eye, *lockedBuffer) {
	t.Helper()
	out := &lockedBuffer{}
	e := newEye(context.Background(), cmd, &eventLog{w: out, start: time.Now()})
	t.Cleanup(e.shut)
	return e, out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waited in vain for %s", what)
		}
	}
}

func TestReadJPEGsCutsAStream(t *testing.T) {
	a, b, c := testJPEG(t, 8, 8, color.White), testJPEG(t, 16, 4, color.Black), testJPEG(t, 4, 4, color.Gray{Y: 90})
	stream := append([]byte("noise"), a...)
	stream = append(stream, b...)
	stream = append(stream, 0xff) // half a marker between two pictures
	stream = append(stream, c...)
	var whole [][]byte
	if err := readJPEGs(bytes.NewReader(stream), func(j []byte) { whole = append(whole, j) }); err != nil {
		t.Fatal(err)
	}
	if len(whole) != 3 || !bytes.Equal(whole[0], a) || !bytes.Equal(whole[1], b) || !bytes.Equal(whole[2], c) {
		t.Fatalf("at once: %d pictures", len(whole))
	}
	var got [][]byte
	if err := readJPEGs(iotest.OneByteReader(bytes.NewReader(stream)), func(j []byte) { got = append(got, j) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) || !bytes.Equal(got[2], c) {
		t.Fatalf("byte by byte: %d pictures", len(got))
	}
}

// A picture two wide and three high, every pixel its own value, turned by quarter turns
// clockwise: the top left corner goes to the top right, the bottom right, the bottom left.
func TestRotateQuarterTurns(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 2; x++ {
			src.Set(x, y, color.RGBA{R: uint8(10*y + x), A: 255})
		}
	}
	at := func(img image.Image, x, y int) int { r, _, _, _ := img.At(x, y).RGBA(); return int(r >> 8) }
	for turns, want := range map[int]struct{ w, h, x, y int }{
		0: {2, 3, 0, 0}, 1: {3, 2, 2, 0}, 2: {2, 3, 1, 2}, 3: {3, 2, 0, 1}, 5: {3, 2, 2, 0}, -1: {3, 2, 0, 1},
	} {
		got := rotate(src, turns)
		if b := got.Bounds(); b.Dx() != want.w || b.Dy() != want.h {
			t.Fatalf("%d turns: %v", turns, b)
		}
		if v := at(got, want.x, want.y); v != 0 {
			t.Fatalf("%d turns: the top left corner is not at %d,%d (there: %d)", turns, want.x, want.y, v)
		}
	}
	// One turn: the left column, read upwards, becomes the top row.
	one := rotate(src, 1)
	for x, want := range []int{20, 10, 0} {
		if v := at(one, x, 0); v != want {
			t.Fatalf("top row after one turn: %d at %d, want %d", v, x, want)
		}
	}
	back := rotate(rotate(src, 1), 3)
	for y := 0; y < 3; y++ {
		for x := 0; x < 2; x++ {
			if at(back, x, y) != 10*y+x {
				t.Fatalf("one turn and three more is not the picture again at %d,%d", x, y)
			}
		}
	}
}

func TestPictureUprightTurnsTheJPEG(t *testing.T) {
	jpg := testJPEG(t, 32, 16, color.White)
	if got := (picture{jpeg: jpg}).upright(); !bytes.Equal(got, jpg) {
		t.Fatal("an upright picture was encoded again")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(picture{jpeg: jpg, turns: 1}.upright()))
	if err != nil || cfg.Width != 16 || cfg.Height != 32 {
		t.Fatalf("turned picture: %dx%d %v", cfg.Width, cfg.Height, err)
	}
	if got := (picture{jpeg: []byte("no picture"), turns: 1}).upright(); string(got) != "no picture" {
		t.Fatal("what cannot be decoded should be passed on as it is")
	}
}

func TestChatMessageCarriesAPicture(t *testing.T) {
	plain, err := json.Marshal(chatMessage{Role: "user", Content: "Hallo"})
	if err != nil || string(plain) != `{"role":"user","content":"Hallo"}` {
		t.Fatalf("plain message: %s %v", plain, err)
	}
	raw, err := json.Marshal([]chatMessage{{Role: "user", Content: "Was ist das?", Image: []byte{1, 2, 3}}})
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Role    string
		Content []struct {
			Type     string
			Text     string
			ImageURL struct{ URL string } `json:"image_url"`
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	parts := got[0].Content
	if got[0].Role != "user" || len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "Was ist das?" || parts[1].Type != "image_url" {
		t.Fatalf("message with a picture: %s", raw)
	}
	if want := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte{1, 2, 3}); parts[1].ImageURL.URL != want {
		t.Fatalf("picture: %q", parts[1].ImageURL.URL)
	}
}

// The decoder here is cat: what goes in as picture data comes out as the picture.
func TestEyeKeepsWhatTheDecoderShows(t *testing.T) {
	e, events := testEye(t, "cat")
	white := testJPEG(t, 24, 16, color.White)
	e.SetOrientation(5)
	if _, ok := e.pick(time.Now()); ok {
		t.Fatal("a picture before the camera showed one")
	}
	if err := e.WriteVideo(white); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first picture", func() bool { _, ok := e.pick(time.Now()); return ok })
	p, _ := e.pick(time.Now())
	if !bytes.Equal(p.jpeg, white) || p.turns != 1 {
		t.Fatalf("picture of %d bytes with %d turns", len(p.jpeg), p.turns)
	}
	if !strings.Contains(events.String(), `"event":"seeing"`) || !strings.Contains(events.String(), `"width":24`) {
		t.Fatalf("events: %s", events.String())
	}
	if _, ok := e.pick(time.Now().Add(eyeStale + time.Second)); ok {
		t.Fatal("a picture that old does not show the moment")
	}
	if got, ok := e.pick(p.at.Add(-time.Second)); !ok || !got.at.Equal(p.at) {
		t.Fatal("the camera came on just after the words: its first picture is the one")
	}

	e.blind()
	if _, ok := e.pick(time.Now()); ok {
		t.Fatal("a picture after the camera went off")
	}
	if !strings.Contains(events.String(), `"on":false`) {
		t.Fatalf("events: %s", events.String())
	}
}

// Pictures come 15 to 30 times a second; a fifth of a second apart is enough to keep,
// but the newest must always be there.
func TestEyeKeepsPicturesApart(t *testing.T) {
	e, _ := testEye(t, "cat")
	for i := 0; i < 20; i++ {
		e.add(0, []byte{byte(i)})
	}
	e.mu.Lock()
	n, last := len(e.recent), e.recent[len(e.recent)-1].jpeg[0]
	e.mu.Unlock()
	if n != 2 || last != 19 {
		t.Fatalf("kept %d pictures, the newest is number %d", n, last)
	}
	e.mu.Lock()
	base := time.Now().Add(-time.Minute)
	e.recent = nil
	for i := 0; i < eyeKept+5; i++ {
		e.recent = append(e.recent, picture{jpeg: []byte{byte(i)}, at: base.Add(time.Duration(i) * eyeSpacing)})
	}
	e.mu.Unlock()
	e.add(0, []byte{200})
	e.mu.Lock()
	n, first := len(e.recent), e.recent[0].jpeg[0]
	e.mu.Unlock()
	if n != eyeKept || first != 6 {
		t.Fatalf("kept %d pictures from number %d on", n, first)
	}
	if p, ok := e.pick(base.Add(10*eyeSpacing + eyeSpacing/2)); !ok || p.jpeg[0] != 10 {
		t.Fatalf("picked %v", p.jpeg)
	}
	e.add(7, []byte{201}) // from a decoder that is no longer the one
	e.mu.Lock()
	last = e.recent[len(e.recent)-1].jpeg[0]
	e.mu.Unlock()
	if last != 200 {
		t.Fatal("a picture of an old decoder was kept")
	}
}

// A decoder that cannot work is tried a few times and then left alone; the call goes on.
func TestEyeGivesUpOnABrokenDecoder(t *testing.T) {
	old := eyeRetry
	eyeRetry = 5 * time.Millisecond
	t.Cleanup(func() { eyeRetry = old })
	e, events := testEye(t, "echo kein Decoder >&2; exit 3")
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(events.String(), `"gave_up":true`) {
		if time.Now().After(deadline) {
			t.Fatalf("it never gave up: %s", events.String())
		}
		_ = e.WriteVideo([]byte{0, 0, 1, 9})
		time.Sleep(2 * time.Millisecond)
	}
	if n := strings.Count(events.String(), "vision_error"); n != eyeTries {
		t.Fatalf("%d complaints, want %d: %s", n, eyeTries, events.String())
	}
	if !strings.Contains(events.String(), "kein Decoder") {
		t.Fatalf("the decoder's own words are missing: %s", events.String())
	}
	for i := 0; i < 50; i++ {
		_ = e.WriteVideo([]byte{0, 0, 1, 9})
	}
	time.Sleep(50 * time.Millisecond)
	if n := strings.Count(events.String(), "vision_error"); n != eyeTries {
		t.Fatalf("it was started again after giving up: %s", events.String())
	}
}

// WriteVideo is called from the media loop of the call: it must return at once even
// when the decoder takes nothing.
func TestEyeNeverHoldsUpTheCall(t *testing.T) {
	e, events := testEye(t, "sleep 30")
	big := make([]byte, 256<<10)
	start := time.Now()
	for i := 0; i < 4*eyeBacklog; i++ {
		_ = e.WriteVideo(big)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("writing took %v", took)
	}
	if e.lost.Load() == 0 {
		t.Fatal("nothing was dropped although the decoder took nothing")
	}
	waitFor(t, "the warning", func() bool { return strings.Contains(events.String(), "does not keep up") })
	done := make(chan struct{})
	go func() { e.shut(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the eye did not end while its decoder hung")
	}
}

// fakeSeeingCall is a call whose peer has a camera.
type fakeSeeingCall struct {
	mu       sync.Mutex
	sink     agentcaller.VideoSink
	state    func(agentcaller.VideoState)
	accepted int
}

func (f *fakeSeeingCall) ReceiveVideo(s agentcaller.VideoSink) {
	f.mu.Lock()
	f.sink = s
	f.mu.Unlock()
}
func (f *fakeSeeingCall) OnVideoState(fn func(agentcaller.VideoState)) {
	f.mu.Lock()
	f.state = fn
	f.mu.Unlock()
}
func (f *fakeSeeingCall) AcceptVideo() error { f.mu.Lock(); f.accepted++; f.mu.Unlock(); return nil }

func TestWatchAcceptsThePeersCamera(t *testing.T) {
	var events lockedBuffer
	c := newConversation(converseConfig{}, voiceChain{}, &events)
	c.eye = newEye(context.Background(), "cat", c.events)
	t.Cleanup(c.eye.shut)
	call := &fakeSeeingCall{}
	c.watch(call)
	if call.sink != agentcaller.VideoSink(c.eye) {
		t.Fatal("the eye is not the video sink of the call")
	}
	if _, ok := call.sink.(agentcaller.VideoOrientationSink); !ok {
		t.Fatal("the eye is not told how the phone is held")
	}

	call.state(agentcaller.VideoState{Upgrade: true, Raw: 3})
	waitFor(t, "the acceptance", func() bool { return strings.Contains(events.String(), "video_accepted") })
	if call.accepted != 1 {
		t.Fatalf("accepted %d times", call.accepted)
	}
	call.state(agentcaller.VideoState{Active: true, Raw: 1})
	if call.accepted != 1 {
		t.Fatal("a running camera was accepted again")
	}

	_ = call.sink.WriteVideo(testJPEG(t, 8, 8, color.White))
	waitFor(t, "a picture", func() bool { _, ok := c.eye.pick(time.Now()); return ok })
	call.state(agentcaller.VideoState{Raw: videoStateStopped})
	if _, ok := c.eye.pick(time.Now()); ok {
		t.Fatal("the camera was switched off and there still is a picture")
	}
	if n := strings.Count(events.String(), `"event":"video_state"`); n != 3 {
		t.Fatalf("%d video_state events: %s", n, events.String())
	}
}

// What the camera shows goes to the model with the words it belongs to, and only with
// them: the history keeps a note in its place. A goodbye still ends the call.
func TestConversationSeesWithTheQuestion(t *testing.T) {
	chain := &fakeChain{
		transcripts:    []string{"Was halte ich da?", "Und jetzt?", "Danke, tschüss!"},
		replies:        [][]string{{"Eine Tasse."}, {"Jetzt sehe ich nichts."}, {"Bis bald."}},
		sentenceFrames: 5,
	}
	saved := t.TempDir()
	cfg := converseConfig{llm: LLMConfig{Prompt: "PERSONA"}, vision: true, visionSave: saved}
	white := testJPEG(t, 32, 16, color.White)
	var seen *eye
	l := startLineWith(t, cfg, chain.chain(), func(c *conversation) {
		c.eye = newEye(context.Background(), "cat", c.events)
		t.Cleanup(c.eye.shut)
		c.eye.SetOrientation(1)
		seen = c.eye
	})
	_ = seen.WriteVideo(white)
	l.await("seeing", 1)

	l.peerSays(15)
	e := l.await("reply", 1)
	if e["text"] != "Eine Tasse." || e["picture_bytes"] == nil || e["picture_ms"] == nil {
		t.Fatalf("reply: %v", e)
	}
	path, _ := e["picture"].(string)
	if path != filepath.Join(saved, "seen-001.jpg") {
		t.Fatalf("picture kept at %q", path)
	}
	kept, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if dim, err := jpeg.DecodeConfig(bytes.NewReader(kept)); err != nil || dim.Width != 16 || dim.Height != 32 {
		t.Fatalf("the kept picture is not the upright one: %dx%d %v", dim.Width, dim.Height, err)
	}
	l.awaitSent(5)

	seen.blind() // the camera goes off
	time.Sleep(eyeTail)
	l.peerSays(15)
	if e := l.await("reply", 2); e["text"] != "Jetzt sehe ich nichts." || e["picture_bytes"] != nil {
		t.Fatalf("reply: %v", e)
	}
	l.awaitSent(10)

	_ = seen.WriteVideo(white) // and on again
	l.await("seeing", 3)
	l.peerSays(15)
	if e := l.await("reply", 3); e["text"] != "Bis bald." || e["picture_bytes"] == nil {
		t.Fatalf("reply: %v", e)
	}
	select {
	case err := <-l.result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a goodbye with the camera on did not end the call")
	}

	asked := chain.questions()
	if len(asked) != 3 {
		t.Fatalf("%d questions to the model", len(asked))
	}
	first := asked[0][len(asked[0])-1]
	if first.Content != "Was halte ich da?" || !bytes.Equal(first.Image, kept) {
		t.Fatalf("first question: %q with %d bytes of picture", first.Content, len(first.Image))
	}
	second := asked[1]
	for _, m := range second {
		if len(m.Image) > 0 {
			t.Fatalf("a picture in the second question, which had none: %q", m.Content)
		}
	}
	if got := contents(second); len(got) != 4 || got[1] != "user: Was halte ich da?\n\n"+sawNote || got[3] != "user: Und jetzt?" {
		t.Fatalf("second question %q", got)
	}
	third := asked[2]
	for i, m := range third[:len(third)-1] {
		if len(m.Image) > 0 {
			t.Fatalf("message %d of the history still carries its picture", i)
		}
	}
	if len(third[len(third)-1].Image) == 0 {
		t.Fatal("the third question came without the picture")
	}
}

func TestVisionFlags(t *testing.T) {
	var stderr bytes.Buffer
	for _, bad := range [][]string{
		{"-tts-cmd", "say", "-vision-save", "seen", "+41790000000"},                 // without -vision
		{"-tts-cmd", "say", "-vision-cmd", "cat", "+41790000000"},                   // without -vision
		{"-tts-cmd", "say", "-vision", "-vision-cmd", " ", "+41790000000"},          // no decoder
		{"-tts-cmd", "say", "-vision", "-simulate-video", "x.h264", "+41790000000"}, // without -simulate
		{"-tts-cmd", "say", "-simulate", "a.wav", "-simulate-video", "x.h264"},      // without -vision
		{"-tts-cmd", "say", "-vision", "-simulate", "a.wav", "-simulate-video", "x.h264", "-simulate-video-fps", "0"},
	} {
		if _, err := parseConverseArgs(bad, &stderr); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
	cfg, err := parseConverseArgs([]string{"-tts-cmd", "say", "-vision", "+41790000000"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.llm.Prompt, " "+visionRules) || cfg.visionCmd != defaultVisionCmd {
		t.Fatalf("prompt %q", cfg.llm.Prompt)
	}
	if _, err := parseConverseArgs([]string{"-tts-cmd", "say", "-vision", "-simulate", "a.wav", "-simulate-video", "x.h264"}, &stderr); err != nil {
		t.Fatal(err)
	}
	cfg, err = parseConverseArgs([]string{"-tts-cmd", "say", "+41790000000"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg.llm.Prompt, "Kamera") {
		t.Fatal("the agent is told about a camera it does not have")
	}
}

// The real decoder: a synthetic camera of two seconds, played in real time, must come
// out as pictures of the right size while it still plays.
func TestEyeWithFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	file := filepath.Join(t.TempDir(), "camera.h264")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=15",
		"-t", "2", "-c:v", "libx264", "-profile:v", "baseline", "-pix_fmt", "yuv420p", "-g", "15", "-bf", "0", "-f", "h264", file)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("no test video (%v): %s", err, out)
	}
	camera, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer camera.Close()

	e, events := testEye(t, defaultVisionCmd)
	start := time.Now()
	on := make(chan struct{})
	close(on)
	played := make(chan struct{})
	var count int
	go func() {
		defer close(played)
		pumpVideo(context.Background(), func(au []byte, _ int) error { count++; return e.WriteVideo(au) }, camera, 15, on, &eventLog{w: &lockedBuffer{}, start: start})
	}()
	waitFor(t, "the first picture", func() bool { _, ok := e.pick(time.Now()); return ok })
	first := time.Since(start)
	<-played
	time.Sleep(300 * time.Millisecond)
	if count < 28 {
		t.Fatalf("the test video has only %d pictures", count)
	}
	if first > time.Second {
		t.Fatalf("the first picture came after %v: the decoder holds pictures back", first)
	}
	e.mu.Lock()
	n := len(e.recent)
	newest := e.recent[n-1]
	e.mu.Unlock()
	// Two seconds a fifth of a second apart.
	if n < 8 || n > 12 {
		t.Fatalf("kept %d pictures of two seconds", n)
	}
	if age := time.Since(newest.at); age > 700*time.Millisecond {
		t.Fatalf("the newest picture is %v old just after the video ended", age)
	}
	if dim, err := jpeg.DecodeConfig(bytes.NewReader(newest.jpeg)); err != nil || dim.Width != 320 || dim.Height != 240 {
		t.Fatalf("picture: %dx%d %v", dim.Width, dim.Height, err)
	}
	if strings.Contains(events.String(), "vision_error") {
		t.Fatalf("events: %s", events.String())
	}
}
