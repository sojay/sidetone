// Package player owns the alert queue and is the only package that touches
// audio hardware.
//
// One speaker means one transmission at a time, so alerts queue and play
// sequentially. Enqueue never blocks: the webhook handler has to answer Sentry
// in milliseconds while a message already on the air may have another minute to
// run.
//
// The queue is a slice behind a mutex rather than a buffered channel. A channel
// gives FIFO and non-blocking sends for free, but the drop policy has to remove
// a message from the *middle* of the queue — choosing the oldest warning while
// leaving fatals alone — and a channel cannot be inspected that way.
package player

import (
	"context"
	"io"
	"log"
	"sync"
	"time"

	"github.com/sojay/sidetone/internal/cw"
)

// DefaultMaxDepth is the queue limit from the spec. Past this, alerts are
// dropped by severity rather than blocking or growing without bound.
const DefaultMaxDepth = 20

// DefaultSampleRate is CD quality — plenty for a sub-kilohertz sidetone.
const DefaultSampleRate = 44100

// A Sink is somewhere PCM samples can be played. This is the seam that keeps
// the rest of the project testable: everything above it deals in samples, and
// only a Sink implementation talks to a device.
type Sink interface {
	// Play sends mono samples in the range [-1, 1] and returns when they have
	// finished playing.
	Play(samples []float64) error
}

// A Message is a composed alert ready to be keyed.
type Message struct {
	Text    string     // already composed and sanitized
	Profile cw.Profile // speed and pitch, and the level the drop policy reads
}

// An EventKind says what happened to a message.
type EventKind string

const (
	EventQueued   EventKind = "queued"
	EventDropped  EventKind = "dropped"
	EventStarted  EventKind = "started"
	EventFinished EventKind = "finished"
)

// An Event is a change worth telling a display about.
type Event struct {
	Kind    EventKind
	Message Message
	Depth   int       // queue depth after the change
	At      time.Time // when it happened
}

// Config configures a Player. Zero values get sensible defaults, so
// player.New(player.Config{Sink: s}) is a working player.
type Config struct {
	Sink       Sink
	Volume     float64 // 0.0-1.0; zero means DefaultVolume, not silence
	SampleRate int
	MaxDepth   int

	// OnEvent, if set, is called as messages are queued, dropped, started and
	// finished. It is called synchronously and never while the queue lock is
	// held, so an observer is free to call back into the Player — but a slow
	// observer delays the queue, so it should not block.
	OnEvent func(Event)

	// StopWhenEmpty makes Run return once the queue drains instead of waiting
	// for more work. The one-shot CLI wants this; the webhook server does not.
	StopWhenEmpty bool

	Logger *log.Logger // nil discards
}

// DefaultVolume leaves headroom below clipping.
const DefaultVolume = 0.8

// A Player owns the queue and plays from it, one message at a time.
type Player struct {
	sink       Sink
	volume     float64
	sampleRate int
	maxDepth   int
	stopEmpty  bool
	logger     *log.Logger
	onEvent    func(Event)

	mu      sync.Mutex
	queue   []Message
	dropped int
	played  int

	// playing is the message on the air, and startedAt when it began. A display
	// needs both to know how far through the transmission it is.
	playing   *Message
	startedAt time.Time

	// wake carries a single pending notification: capacity 1 with a
	// non-blocking send, so a burst of enqueues cannot block a producer and
	// cannot lose the wakeup either.
	wake chan struct{}
}

// New returns a Player. It does not start playing; call Run.
func New(cfg Config) *Player {
	p := &Player{
		sink:       cfg.Sink,
		volume:     cfg.Volume,
		sampleRate: cfg.SampleRate,
		maxDepth:   cfg.MaxDepth,
		stopEmpty:  cfg.StopWhenEmpty,
		logger:     cfg.Logger,
		onEvent:    cfg.OnEvent,
		wake:       make(chan struct{}, 1),
	}

	if p.onEvent == nil {
		p.onEvent = func(Event) {}
	}

	if p.volume == 0 {
		p.volume = DefaultVolume
	}
	if p.sampleRate <= 0 {
		p.sampleRate = DefaultSampleRate
	}
	if p.maxDepth <= 0 {
		p.maxDepth = DefaultMaxDepth
	}
	if p.logger == nil {
		p.logger = log.New(io.Discard, "", 0)
	}
	return p
}

// Enqueue adds m to the queue and returns immediately — it never blocks and
// never fails.
//
// If the queue is over capacity, one message is dropped to make room and
// returned with ok true. That may be m itself: an incoming warning behind a
// queue of fatals is the message with the least claim on the speaker.
func (p *Player) Enqueue(m Message) (dropped Message, ok bool) {
	p.mu.Lock()

	p.queue = append(p.queue, m)

	if len(p.queue) > p.maxDepth {
		if i, found := p.victim(); found {
			dropped, ok = p.queue[i], true
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			p.dropped++
		}
		// If nothing was droppable the queue is all fatals, and we let it grow
		// past the limit. A queued Message is a string and two numbers, so the
		// memory cost is nothing next to losing a fatal.
	}

	depth := len(p.queue)

	// Non-blocking: if a notification is already pending the run loop has not
	// yet gone back to sleep, so it will see this message anyway.
	select {
	case p.wake <- struct{}{}:
	default:
	}

	p.mu.Unlock()

	// Observers run unlocked so they can call Depth or Snapshot without
	// deadlocking against the call that notified them.
	now := time.Now()
	p.onEvent(Event{Kind: EventQueued, Message: m, Depth: depth, At: now})
	if ok {
		p.onEvent(Event{Kind: EventDropped, Message: dropped, Depth: depth, At: now})
	}

	return dropped, ok
}

// victim returns the index of the message to drop: the oldest warning, else the
// oldest error, else the oldest of any other non-fatal level. Fatals are never
// chosen, so a queue of nothing but fatals reports no victim.
func (p *Player) victim() (int, bool) {
	for _, level := range []string{cw.LevelWarning, cw.LevelError} {
		for i, m := range p.queue {
			if m.Profile.Level == level {
				return i, true
			}
		}
	}

	// Belt and braces: a Message built without cw.ProfileFor could carry a
	// level we do not rank. Only fatal is actually protected.
	for i, m := range p.queue {
		if m.Profile.Level != cw.LevelFatal {
			return i, true
		}
	}
	return 0, false
}

// Run plays queued messages until ctx is cancelled, or until the queue drains
// if StopWhenEmpty is set. It returns nil on either — a cancelled context is a
// graceful shutdown, not a failure.
//
// Cancellation is only observed between messages: a Sink is playing to a device
// and cannot be cut off mid-transmission. Worst case that is one message of
// delay, which at the warning speed can be about a minute.
func (p *Player) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		m, ok := p.pop()
		if !ok {
			if p.stopEmpty {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-p.wake:
			}
			continue
		}

		p.play(m)
	}
}

// play renders and sends one message. A sink failure is logged and swallowed:
// losing one alert to a glitching device is not a reason to stop listening for
// the rest.
func (p *Player) play(m Message) {
	elems := cw.Encode(m.Text)
	if len(elems) == 0 {
		p.logger.Printf("player: nothing keyable in %q, skipping", m.Text)
		return
	}

	samples := cw.Render(elems, m.Profile, p.volume, p.sampleRate)

	// The start is timestamped as close to the first sample as we can manage,
	// because a display uses it to follow the keying character by character.
	p.mu.Lock()
	started := time.Now()
	p.playing, p.startedAt = &m, started
	depth := len(p.queue)
	p.mu.Unlock()

	p.onEvent(Event{Kind: EventStarted, Message: m, Depth: depth, At: started})

	err := p.sink.Play(samples)

	p.mu.Lock()
	p.playing = nil
	if err == nil {
		p.played++
	}
	depth = len(p.queue)
	p.mu.Unlock()

	if err != nil {
		p.logger.Printf("player: playing %q: %v", m.Text, err)
	}
	p.onEvent(Event{Kind: EventFinished, Message: m, Depth: depth, At: time.Now()})
}

// pop takes the oldest message, reporting false when the queue is empty.
func (p *Player) pop() (Message, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.queue) == 0 {
		return Message{}, false
	}
	m := p.queue[0]
	p.queue = p.queue[1:]
	return m, true
}

// A Snapshot is the whole visible state of the player at one instant. A display
// that connects mid-transmission needs all of it at once, and reading it under
// a single lock means the parts cannot disagree with each other.
type Snapshot struct {
	Playing   *Message  // nil when nothing is on the air
	StartedAt time.Time // when Playing began; zero if nothing is playing
	Queue     []Message // waiting, oldest first
	Depth     int
	Dropped   int
	Played    int
}

// Snapshot returns the current state.
func (p *Player) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := Snapshot{
		StartedAt: p.startedAt,
		Queue:     append([]Message(nil), p.queue...),
		Depth:     len(p.queue),
		Dropped:   p.dropped,
		Played:    p.played,
	}
	if p.playing != nil {
		m := *p.playing
		s.Playing = &m
	}
	return s
}

// Flush empties the queue and reports how many messages were discarded.
//
// It cannot stop the transmission already on the air — a sink is playing to a
// device and has no way to be cut short — so the current message finishes and
// everything behind it is dropped. That is the escape hatch for a demo where
// too much got queued: the alternative is restarting the station.
func (p *Player) Flush() int {
	p.mu.Lock()
	n := len(p.queue)
	p.queue = nil
	p.dropped += n
	p.mu.Unlock()

	if n > 0 {
		p.logger.Printf("player: flushed %d queued messages", n)
	}
	return n
}

// Depth is the number of messages waiting, excluding any now playing.
func (p *Player) Depth() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

// Dropped is the running count of messages discarded by the drop policy.
func (p *Player) Dropped() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

// Played is the running count of messages sent successfully.
func (p *Player) Played() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.played
}
