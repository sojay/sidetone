package player

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sojay/sidetone/internal/cw"
)

// fakeSink records what it was asked to play instead of touching a device.
type fakeSink struct {
	mu      sync.Mutex
	lengths []int
	err     error

	// gate, when non-nil, holds each Play until the test releases it.
	gate chan struct{}
}

func (f *fakeSink) Play(samples []float64) error {
	if f.gate != nil {
		<-f.gate
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.lengths = append(f.lengths, len(samples))
	return f.err
}

func (f *fakeSink) played() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.lengths...)
}

// msg builds a Message at the given level. The text is what distinguishes
// messages in playback assertions: Render is deterministic, so a longer text
// means proportionally more samples.
func msg(level, text string) Message {
	return Message{Text: text, Profile: cw.ProfileFor(level)}
}

func warning(text string) Message  { return msg(cw.LevelWarning, text) }
func errorMsg(text string) Message { return msg(cw.LevelError, text) }
func fatal(text string) Message    { return msg(cw.LevelFatal, text) }

// levels reads the queue's levels in order, for drop-policy assertions.
func levels(p *Player) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]string, 0, len(p.queue))
	for _, m := range p.queue {
		out = append(out, m.Profile.Level)
	}
	return out
}

// texts reads the queue's texts in order.
func texts(p *Player) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]string, 0, len(p.queue))
	for _, m := range p.queue {
		out = append(out, m.Text)
	}
	return out
}

func TestNewDefaults(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}})

	if p.maxDepth != DefaultMaxDepth {
		t.Errorf("maxDepth = %d, want %d", p.maxDepth, DefaultMaxDepth)
	}
	if p.sampleRate != DefaultSampleRate {
		t.Errorf("sampleRate = %d, want %d", p.sampleRate, DefaultSampleRate)
	}
	// A zero Volume must mean "unset", not "silent" — otherwise the obvious
	// player.Config{Sink: s} would play nothing at all.
	if p.volume != DefaultVolume {
		t.Errorf("volume = %g, want %g", p.volume, DefaultVolume)
	}
	if p.logger == nil {
		t.Error("logger is nil; New must supply a discarding one")
	}
}

func TestEnqueueFIFO(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}})

	for _, text := range []string{"A", "B", "C"} {
		p.Enqueue(errorMsg(text))
	}

	if got, want := texts(p), []string{"A", "B", "C"}; !equalStrings(got, want) {
		t.Errorf("queue = %v, want %v", got, want)
	}
	if got := p.Depth(); got != 3 {
		t.Errorf("Depth() = %d, want 3", got)
	}
}

// TestEnqueueNeverBlocks is the property the webhook handler depends on: it has
// to answer Sentry immediately, with nothing draining the queue.
func TestEnqueueNeverBlocks(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			p.Enqueue(warning("E"))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked with no consumer running")
	}

	if got := p.Depth(); got != DefaultMaxDepth {
		t.Errorf("Depth() = %d after 1000 enqueues, want the cap of %d", got, DefaultMaxDepth)
	}
	if got, want := p.Dropped(), 1000-DefaultMaxDepth; got != want {
		t.Errorf("Dropped() = %d, want %d", got, want)
	}
}

func TestDropOldestWarningFirst(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}, MaxDepth: 3})

	p.Enqueue(warning("oldest"))
	p.Enqueue(warning("middle"))
	p.Enqueue(warning("newest"))

	dropped, ok := p.Enqueue(warning("incoming"))
	if !ok {
		t.Fatal("nothing dropped past capacity")
	}
	if dropped.Text != "oldest" {
		t.Errorf("dropped %q, want the oldest warning", dropped.Text)
	}
	if got, want := texts(p), []string{"middle", "newest", "incoming"}; !equalStrings(got, want) {
		t.Errorf("queue = %v, want %v", got, want)
	}
}

// TestDropWarningsBeforeErrors checks the priority order, including that a
// *newer* warning is dropped before an older error.
func TestDropWarningsBeforeErrors(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}, MaxDepth: 3})

	p.Enqueue(errorMsg("old error"))
	p.Enqueue(errorMsg("newer error"))
	p.Enqueue(warning("warning"))

	dropped, ok := p.Enqueue(errorMsg("incoming"))
	if !ok {
		t.Fatal("nothing dropped past capacity")
	}
	if dropped.Text != "warning" {
		t.Errorf("dropped %q, want the warning even though it is newer", dropped.Text)
	}
	if got, want := texts(p), []string{"old error", "newer error", "incoming"}; !equalStrings(got, want) {
		t.Errorf("queue = %v, want %v", got, want)
	}
}

func TestDropOldestErrorWhenNoWarnings(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}, MaxDepth: 2})

	p.Enqueue(fatal("fatal"))
	p.Enqueue(errorMsg("old error"))

	dropped, ok := p.Enqueue(errorMsg("incoming"))
	if !ok {
		t.Fatal("nothing dropped past capacity")
	}
	if dropped.Text != "old error" {
		t.Errorf("dropped %q, want the oldest error", dropped.Text)
	}
	if got, want := levels(p), []string{cw.LevelFatal, cw.LevelError}; !equalStrings(got, want) {
		t.Errorf("levels = %v, want %v", got, want)
	}
}

// TestNeverDropsFatal is the rule from the spec, in its two forms: a fatal is
// never the victim, and when the queue is nothing but fatals the cap yields
// rather than the alert.
func TestNeverDropsFatal(t *testing.T) {
	t.Run("fatals survive a flood of warnings", func(t *testing.T) {
		p := New(Config{Sink: &fakeSink{}, MaxDepth: 5})

		p.Enqueue(fatal("keep me"))
		for i := 0; i < 200; i++ {
			p.Enqueue(warning("noise"))
		}

		if got := p.Depth(); got != 5 {
			t.Errorf("Depth() = %d, want 5", got)
		}
		if got := texts(p)[0]; got != "keep me" {
			t.Errorf("queue starts with %q, want the fatal", got)
		}
		for _, level := range levels(p)[1:] {
			if level != cw.LevelWarning {
				t.Errorf("unexpected level %q left in queue", level)
			}
		}
	})

	t.Run("an all-fatal queue grows past the cap", func(t *testing.T) {
		p := New(Config{Sink: &fakeSink{}, MaxDepth: 3})

		for i := 0; i < 10; i++ {
			if dropped, ok := p.Enqueue(fatal("fatal")); ok {
				t.Fatalf("dropped a fatal: %+v", dropped)
			}
		}

		if got := p.Depth(); got != 10 {
			t.Errorf("Depth() = %d, want all 10 fatals kept", got)
		}
		if got := p.Dropped(); got != 0 {
			t.Errorf("Dropped() = %d, want 0", got)
		}
	})

	t.Run("an incoming warning behind fatals is itself the victim", func(t *testing.T) {
		p := New(Config{Sink: &fakeSink{}, MaxDepth: 2})

		p.Enqueue(fatal("f1"))
		p.Enqueue(fatal("f2"))

		dropped, ok := p.Enqueue(warning("incoming"))
		if !ok {
			t.Fatal("nothing dropped")
		}
		if dropped.Text != "incoming" {
			t.Errorf("dropped %q, want the incoming warning", dropped.Text)
		}
		if got, want := texts(p), []string{"f1", "f2"}; !equalStrings(got, want) {
			t.Errorf("queue = %v, want %v", got, want)
		}
	})
}

// TestDropUnknownLevel covers a Message built without cw.ProfileFor: only fatal
// is protected, so an unranked level must still be droppable.
func TestDropUnknownLevel(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}, MaxDepth: 2})

	p.Enqueue(Message{Text: "mystery", Profile: cw.Profile{Level: "debug", WPM: 20, Freq: 700}})
	p.Enqueue(fatal("f"))

	dropped, ok := p.Enqueue(fatal("f2"))
	if !ok {
		t.Fatal("nothing dropped past capacity")
	}
	if dropped.Text != "mystery" {
		t.Errorf("dropped %q, want the unranked message", dropped.Text)
	}
}

func TestRunPlaysInOrderThenStops(t *testing.T) {
	sink := &fakeSink{}
	p := New(Config{Sink: sink, StopWhenEmpty: true})

	// Distinct lengths make playback order observable: more units, more samples.
	for _, text := range []string{"E", "EE", "EEE"} {
		p.Enqueue(errorMsg(text))
	}

	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	got := sink.played()
	if len(got) != 3 {
		t.Fatalf("played %d messages, want 3", len(got))
	}
	if !(got[0] < got[1] && got[1] < got[2]) {
		t.Errorf("sample counts %v are not increasing; playback was out of order", got)
	}
	if p.Depth() != 0 {
		t.Errorf("Depth() = %d after draining, want 0", p.Depth())
	}
	if p.Played() != 3 {
		t.Errorf("Played() = %d, want 3", p.Played())
	}
}

func TestRunStopsWhenEmptyWithNothingQueued(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}, StopWhenEmpty: true})

	done := make(chan error, 1)
	go func() { done <- p.Run(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on an empty queue")
	}
}

// TestRunWaitsForWork is the server behaviour: Run must sit idle rather than
// return, then pick up a message enqueued later.
func TestRunWaitsForWork(t *testing.T) {
	sink := &fakeSink{}
	p := New(Config{Sink: sink})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	select {
	case <-done:
		t.Fatal("Run returned on an empty queue without StopWhenEmpty")
	case <-time.After(50 * time.Millisecond):
	}

	p.Enqueue(errorMsg("E"))

	deadline := time.After(5 * time.Second)
	for len(sink.played()) == 0 {
		select {
		case <-deadline:
			t.Fatal("message enqueued after Run started was never played")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() = %v, want nil on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// TestRunCancelledBeforeStart checks an already-cancelled context stops Run
// without playing anything.
func TestRunCancelledBeforeStart(t *testing.T) {
	sink := &fakeSink{}
	p := New(Config{Sink: sink})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p.Enqueue(errorMsg("E"))
	if err := p.Run(ctx); err != nil {
		t.Errorf("Run() = %v, want nil", err)
	}
	if got := sink.played(); len(got) != 0 {
		t.Errorf("played %d messages with a cancelled context, want 0", len(got))
	}
}

// TestRunSurvivesSinkError checks one glitching message does not stop the rest:
// ambient telemetry that goes quiet after a single failure is worse than useless.
func TestRunSurvivesSinkError(t *testing.T) {
	sink := &fakeSink{err: errors.New("device busy")}
	p := New(Config{Sink: sink, StopWhenEmpty: true})

	p.Enqueue(errorMsg("A"))
	p.Enqueue(errorMsg("B"))

	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	if got := len(sink.played()); got != 2 {
		t.Errorf("attempted %d messages, want 2", got)
	}
	if got := p.Played(); got != 0 {
		t.Errorf("Played() = %d, want 0 when every send failed", got)
	}
}

// TestRunSkipsUnkeyableMessage covers a message with nothing Morse-mappable in
// it: it must be discarded, not sent as silence.
func TestRunSkipsUnkeyableMessage(t *testing.T) {
	sink := &fakeSink{}
	p := New(Config{Sink: sink, StopWhenEmpty: true})

	p.Enqueue(errorMsg("🔥🔥🔥"))
	p.Enqueue(errorMsg("E"))

	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	if got := sink.played(); len(got) != 1 {
		t.Errorf("played %d messages, want only the keyable one", len(got))
	}
}

// TestQueueDrainsWhilePlaying checks the queue keeps accepting work during a
// transmission — the whole reason Enqueue and Run are separate.
func TestQueueDrainsWhilePlaying(t *testing.T) {
	gate := make(chan struct{})
	sink := &fakeSink{gate: gate}
	p := New(Config{Sink: sink, MaxDepth: 4})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.Enqueue(errorMsg("first"))

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	// While "first" is held in Play, fill the queue past its cap.
	for i := 0; i < 20; i++ {
		p.Enqueue(warning("noise"))
	}
	if got := p.Depth(); got > 4 {
		t.Errorf("Depth() = %d during playback, want the cap of 4", got)
	}

	close(gate)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// TestConcurrentEnqueue is the race-detector target: many producers, one
// consumer, as the webhook server will be.
func TestConcurrentEnqueue(t *testing.T) {
	sink := &fakeSink{}
	p := New(Config{Sink: sink})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.Run(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				p.Enqueue(msg(levelFor(j), "E"))
				p.Depth()
				p.Dropped()
			}
		}(i)
	}
	wg.Wait()

	assertQueueInvariant(t, p, DefaultMaxDepth)
}

// assertQueueInvariant checks the exact bound the drop policy provides. The cap
// is not a bound on total depth, because a fatal is never dropped: what holds is
// that the queue only ever exceeds the cap when it is *entirely* fatals, so the
// droppable part is always capped.
func assertQueueInvariant(t *testing.T, p *Player, maxDepth int) {
	t.Helper()

	var nonFatal int
	for _, level := range levels(p) {
		if level != cw.LevelFatal {
			nonFatal++
		}
	}

	if nonFatal > maxDepth {
		t.Errorf("%d non-fatal messages queued, over the cap of %d", nonFatal, maxDepth)
	}
	if p.Depth() > maxDepth && nonFatal > 0 {
		t.Errorf("queue is %d deep (over the cap of %d) but holds %d non-fatal messages; "+
			"only a run of fatals may exceed the cap", p.Depth(), maxDepth, nonFatal)
	}
}

// TestQueueInvariantUnderMixedLoad states that bound directly, against a mix
// heavy enough that fatals alone would blow the cap.
func TestQueueInvariantUnderMixedLoad(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}, MaxDepth: 5})

	for i := 0; i < 300; i++ {
		p.Enqueue(msg(levelFor(i), "E"))
	}

	assertQueueInvariant(t, p, 5)

	// And the fatals really are all still there: 300/3 = 100 of them.
	var fatals int
	for _, level := range levels(p) {
		if level == cw.LevelFatal {
			fatals++
		}
	}
	if fatals != 100 {
		t.Errorf("%d fatals survived, want all 100", fatals)
	}
}

func levelFor(n int) string {
	switch n % 3 {
	case 0:
		return cw.LevelWarning
	case 1:
		return cw.LevelError
	default:
		return cw.LevelFatal
	}
}

// TestRenderUsesMessageProfile checks each message is keyed at its own speed:
// a fatal queued behind a warning must not inherit the warning's timing.
func TestRenderUsesMessageProfile(t *testing.T) {
	sink := &fakeSink{}
	p := New(Config{Sink: sink, StopWhenEmpty: true})

	text := strings.Repeat("E ", 5)
	p.Enqueue(warning(text)) // 13 WPM, slowest
	p.Enqueue(fatal(text))   // 28 WPM, fastest

	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v", err)
	}

	got := sink.played()
	if len(got) != 2 {
		t.Fatalf("played %d messages, want 2", len(got))
	}
	if got[0] <= got[1] {
		t.Errorf("warning produced %d samples and fatal %d; the warning must be longer",
			got[0], got[1])
	}
}

// recorder collects events for assertions.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, string(e.Kind))
	}
	return out
}

func TestEventsOnEnqueue(t *testing.T) {
	rec := &recorder{}
	p := New(Config{Sink: &fakeSink{}, MaxDepth: 1, OnEvent: rec.record})

	p.Enqueue(warning("first"))
	p.Enqueue(warning("second")) // pushes the queue over its cap of 1

	if got, want := rec.kinds(), []string{"queued", "queued", "dropped"}; !equalStrings(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if got := rec.events[2].Message.Text; got != "first" {
		t.Errorf("dropped event carried %q, want the message actually dropped", got)
	}
}

func TestEventsOnPlayback(t *testing.T) {
	rec := &recorder{}
	p := New(Config{Sink: &fakeSink{}, StopWhenEmpty: true, OnEvent: rec.record})

	p.Enqueue(errorMsg("E"))
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v", err)
	}

	if got, want := rec.kinds(), []string{"queued", "started", "finished"}; !equalStrings(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// TestFinishedEventFiresAfterASinkError checks a display is told the
// transmission ended even when it failed, so it does not sit showing "on air"
// for ever.
func TestFinishedEventFiresAfterASinkError(t *testing.T) {
	rec := &recorder{}
	p := New(Config{
		Sink:          &fakeSink{err: errors.New("device busy")},
		StopWhenEmpty: true,
		OnEvent:       rec.record,
	})

	p.Enqueue(errorMsg("E"))
	p.Run(context.Background())

	if got, want := rec.kinds(), []string{"queued", "started", "finished"}; !equalStrings(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// TestObserverMayCallBack is the deadlock guard: events are delivered without
// the queue lock held, so an observer can read the player from inside the
// callback. Written the other way round, this test hangs instead of failing.
func TestObserverMayCallBack(t *testing.T) {
	var p *Player
	done := make(chan struct{})

	p = New(Config{
		Sink:          &fakeSink{},
		StopWhenEmpty: true,
		OnEvent: func(e Event) {
			p.Depth()
			p.Snapshot()
			p.Dropped()
		},
	})

	go func() {
		defer close(done)
		p.Enqueue(errorMsg("E"))
		p.Run(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlocked: an event was delivered while holding the queue lock")
	}
}

func TestSnapshotWhileIdle(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}})
	p.Enqueue(warning("waiting"))

	s := p.Snapshot()
	if s.Playing != nil {
		t.Errorf("Playing = %+v, want nil while idle", s.Playing)
	}
	if s.Depth != 1 || len(s.Queue) != 1 {
		t.Errorf("Depth = %d, Queue = %d, want 1 and 1", s.Depth, len(s.Queue))
	}
	if s.Queue[0].Text != "waiting" {
		t.Errorf("Queue[0] = %q, want the queued message", s.Queue[0].Text)
	}
}

// TestSnapshotWhilePlaying is what a display connecting mid-transmission
// depends on: it must be able to see what is on the air and when it started.
func TestSnapshotWhilePlaying(t *testing.T) {
	gate := make(chan struct{})
	p := New(Config{Sink: &fakeSink{gate: gate}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	before := time.Now()
	p.Enqueue(fatal("on the air"))
	p.Enqueue(warning("waiting"))
	go p.Run(ctx)

	// Wait for playback to start.
	deadline := time.After(5 * time.Second)
	for p.Snapshot().Playing == nil {
		select {
		case <-deadline:
			t.Fatal("playback never started")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	s := p.Snapshot()
	if s.Playing.Text != "on the air" {
		t.Errorf("Playing = %q, want the first message", s.Playing.Text)
	}
	if s.StartedAt.Before(before) {
		t.Errorf("StartedAt = %v, before the message was even queued", s.StartedAt)
	}
	if s.Depth != 1 {
		t.Errorf("Depth = %d, want 1 — the playing message is no longer queued", s.Depth)
	}

	close(gate)
	cancel()
}

// TestSnapshotIsACopy checks a caller cannot reach into the live queue.
func TestSnapshotIsACopy(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}})
	p.Enqueue(warning("original"))

	s := p.Snapshot()
	s.Queue[0].Text = "mutated"

	if got := texts(p)[0]; got != "original" {
		t.Errorf("queue holds %q; Snapshot handed out the live slice", got)
	}
}

// TestFlush covers the demo escape hatch: too much queued, and the only other
// way out is restarting the station.
func TestFlush(t *testing.T) {
	p := New(Config{Sink: &fakeSink{}})

	for i := 0; i < 5; i++ {
		p.Enqueue(warning("noise"))
	}
	p.Enqueue(fatal("also gone"))

	if got := p.Flush(); got != 6 {
		t.Errorf("Flush() = %d, want 6", got)
	}
	if got := p.Depth(); got != 0 {
		t.Errorf("Depth() = %d after flush, want 0", got)
	}
	// Flushed messages are dropped messages, and the counter should say so.
	if got := p.Dropped(); got != 6 {
		t.Errorf("Dropped() = %d, want 6", got)
	}
	if got := p.Flush(); got != 0 {
		t.Errorf("flushing an empty queue reported %d", got)
	}
}

// TestFlushLeavesTheAirAlone: a sink cannot be interrupted, so the message
// already playing has to finish.
func TestFlushLeavesTheAirAlone(t *testing.T) {
	gate := make(chan struct{})
	p := New(Config{Sink: &fakeSink{gate: gate}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.Enqueue(fatal("on the air"))
	p.Enqueue(warning("queued"))
	go p.Run(ctx)

	deadline := time.After(5 * time.Second)
	for p.Snapshot().Playing == nil {
		select {
		case <-deadline:
			t.Fatal("playback never started")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	p.Flush()
	if s := p.Snapshot(); s.Playing == nil || s.Playing.Text != "on the air" {
		t.Error("flush interrupted the transmission on the air")
	}
	if got := p.Depth(); got != 0 {
		t.Errorf("Depth() = %d, want the queue behind it emptied", got)
	}

	close(gate)
	cancel()
}

func equalStrings(a, b []string) bool {
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
