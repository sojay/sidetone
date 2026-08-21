// Package bandscope is a read-only display of what the station is sending.
//
// It exists for the demo: an alert is half a minute of beeping, and a room
// cannot copy Morse by ear. The page shows the keying as it happens, the
// message decoded character by character in step with the sound, the queue
// behind it, and the Sentry payload each message came from.
//
// Nothing here can affect the station. There are no controls, and the handlers
// only read.
//
// # Staying in sync without streaming audio
//
// The page is told the message and the speed once, when a transmission starts,
// and animates the keying itself. That works because CW timing is exact: a unit
// is 1.2/WPM seconds and every character's offset is known up front, so the
// browser can compute what is sounding at any instant from a single timestamp.
// The alternative — streaming playback progress — would put the display at the
// mercy of network jitter for no benefit.
package bandscope

import (
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/sojay/sidetone/internal/composer"
	"github.com/sojay/sidetone/internal/cw"
	"github.com/sojay/sidetone/internal/player"
)

//go:embed page.html
var files embed.FS

// backlog is how many past events a newly connected page is caught up with. A
// projector showing the last few alerts is useful; a full history is not.
const backlog = 12

// subscriberBuffer is how far behind a page may fall before we give up on it.
// A stalled browser must never slow the queue down.
const subscriberBuffer = 64

// A Station is the part of the player the display reads.
type Station interface {
	Snapshot() player.Snapshot
}

// A Transmission describes one message completely enough for a page to draw and
// animate it without asking anything further.
type Transmission struct {
	Text    string      `json:"text"`
	Level   string      `json:"level"`
	WPM     float64     `json:"wpm"`
	Freq    float64     `json:"freq"`
	Symbols []cw.Symbol `json:"symbols"` // characters with their unit offsets
	Units   int         `json:"units"`   // total length in units
	Seconds float64     `json:"seconds"` // total airtime
	UnitMS  float64     `json:"unitMs"`  // milliseconds per unit
	Elapsed float64     `json:"elapsed"` // ms already sent, for late joiners
	Project string      `json:"project"` // parsed back out for the header
}

// An Event is one thing that happened, as the page sees it.
type Event struct {
	Kind string `json:"kind"` // received, queued, started, finished, dropped
	At   string `json:"at"`   // wall clock, formatted for display
	AtMS int64  `json:"atMs"` // the same instant in epoch milliseconds
	Seq  int64  `json:"seq"`

	Transmission *Transmission `json:"transmission,omitempty"`
	Depth        int           `json:"depth"`

	// Payload is the raw Sentry JSON, pretty-printed, on received events.
	Payload  string `json:"payload,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Note     string `json:"note,omitempty"`
	Resource string `json:"resource,omitempty"`

	// SentryLagMS is how long the event spent inside Sentry before reaching us.
	// Shown because it is three orders of magnitude larger than anything
	// sidetone does, and people otherwise assume the audio is what is slow.
	SentryLagMS float64 `json:"sentryLagMs,omitempty"`
}

// A state is the whole picture, sent to a page when it connects.
type state struct {
	Playing *Transmission  `json:"playing"`
	Queue   []Transmission `json:"queue"`
	Depth   int            `json:"depth"`
	Dropped int            `json:"dropped"`
	Played  int            `json:"played"`
	Recent  []Event        `json:"recent"`
}

// A Hub fans events out to connected pages.
type Hub struct {
	station Station
	logger  *log.Logger
	page    []byte

	mu          sync.Mutex
	subscribers map[chan Event]struct{}
	recent      []Event
	seq         int64
}

// New returns a Hub reading from station. The page is static — every value on
// it arrives over the event stream — so it is served as bytes rather than
// rendered from a template.
func New(station Station, logger *log.Logger) (*Hub, error) {
	page, err := files.ReadFile("page.html")
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.New(nopWriter{}, "", 0)
	}

	return &Hub{
		station:     station,
		logger:      logger,
		page:        page,
		subscribers: make(map[chan Event]struct{}),
	}, nil
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// Register adds the display's routes to mux.
func (h *Hub) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /", h.handlePage)
	mux.HandleFunc("GET /events", h.handleEvents)
	mux.HandleFunc("GET /state", h.handleState)
}

// PlayerEvent adapts a player event for the display. Wire it to
// player.Config.OnEvent.
func (h *Hub) PlayerEvent(e player.Event) {
	t := transmissionOf(e.Message, 0)

	h.publish(Event{
		Kind:         string(e.Kind),
		Depth:        e.Depth,
		Transmission: &t,
	})
}

// Received records an alert arriving, with the payload that produced it. This
// is the demo's "here is what was encoded" moment, so the raw JSON is kept
// verbatim apart from being indented.
func (h *Hub) Received(endpoint string, raw []byte, msg string, alert composer.Alert, resource string, sentryLag time.Duration) {
	t := transmissionOf(player.Message{Text: msg, Profile: cw.ProfileFor(alert.Level)}, 0)

	h.publish(Event{
		Kind:         "received",
		Endpoint:     endpoint,
		Payload:      indentJSON(raw),
		Transmission: &t,
		Resource:     resource,
		SentryLagMS:  float64(sentryLag.Milliseconds()),
	})
}

// Note publishes a line of commentary, such as a rejected request.
func (h *Hub) Note(note string) {
	h.publish(Event{Kind: "note", Note: note})
}

func (h *Hub) publish(e Event) {
	h.mu.Lock()

	h.seq++
	e.Seq = h.seq

	now := time.Now()
	e.At = now.Format("15:04:05")
	e.AtMS = now.UnixMilli()

	h.recent = append(h.recent, e)
	if len(h.recent) > backlog {
		h.recent = h.recent[len(h.recent)-backlog:]
	}

	// Copy the subscriber list so the sends below happen unlocked.
	subs := make([]chan Event, 0, len(h.subscribers))
	for ch := range h.subscribers {
		subs = append(subs, ch)
	}
	h.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- e:
		default:
			// A page that cannot keep up is skipped rather than allowed to
			// block the queue. It re-syncs from the next event it does get.
		}
	}
}

func (h *Hub) subscribe() chan Event {
	ch := make(chan Event, subscriberBuffer)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.subscribers[ch] = struct{}{}
	return ch
}

func (h *Hub) unsubscribe(ch chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subscribers, ch)
}

// Clients reports how many pages are connected.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}

func (h *Hub) handlePage(w http.ResponseWriter, r *http.Request) {
	// "GET /" matches everything the other patterns do not, so anything
	// unrecognised has to 404 here rather than serve the page.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// No caching. The page is a few KB served from memory, and a browser holding
	// a stale copy of a live dashboard is a genuinely confusing failure: the
	// station is running, the data is flowing, and the screen is simply an old
	// build. Rebuild-and-reload has to be enough.
	w.Header().Set("Cache-Control", "no-store, must-revalidate")

	if _, err := w.Write(h.page); err != nil {
		h.logger.Printf("bandscope: writing page: %v", err)
	}
}

func (h *Hub) handleState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.state())
}

func (h *Hub) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := h.subscribe()
	defer h.unsubscribe(ch)

	// Catch the page up before streaming, so one that connects mid-alert draws
	// the transmission already in progress at the right offset.
	if err := writeEvent(w, "state", h.state()); err != nil {
		return
	}
	flusher.Flush()

	// Keep-alives stop idle proxies closing a connection that may legitimately
	// see nothing for minutes.
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case e := <-ch:
			if err := writeEvent(w, "event", e); err != nil {
				return
			}
			flusher.Flush()

		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// state builds the full picture from the station.
func (h *Hub) state() state {
	snap := h.station.Snapshot()

	s := state{
		Depth:   snap.Depth,
		Dropped: snap.Dropped,
		Played:  snap.Played,
		Queue:   make([]Transmission, 0, len(snap.Queue)),
	}

	if snap.Playing != nil {
		elapsed := float64(time.Since(snap.StartedAt).Milliseconds())
		t := transmissionOf(*snap.Playing, elapsed)
		s.Playing = &t
	}
	for _, m := range snap.Queue {
		s.Queue = append(s.Queue, transmissionOf(m, 0))
	}

	h.mu.Lock()
	s.Recent = append([]Event(nil), h.recent...)
	h.mu.Unlock()

	return s
}

// transmissionOf works out everything the page needs to draw a message.
func transmissionOf(m player.Message, elapsed float64) Transmission {
	syms := cw.Symbols(m.Text)
	units := cw.TotalUnits(cw.Encode(m.Text))
	unit := cw.Unit(m.Profile.WPM)

	return Transmission{
		Text:    cw.Sanitize(m.Text),
		Level:   m.Profile.Level,
		WPM:     m.Profile.WPM,
		Freq:    m.Profile.Freq,
		Symbols: syms,
		Units:   units,
		Seconds: (time.Duration(units) * unit).Seconds(),
		UnitMS:  float64(unit.Microseconds()) / 1000,
		Elapsed: elapsed,
		Project: projectOf(syms),
	}
}

// projectOf recovers the project name from a composed message. The format is
// fixed — the project is the first word after the opening section break — so
// this is cheaper and less coupled than threading the alert through the player.
func projectOf(syms []cw.Symbol) string {
	var text string
	for _, s := range syms {
		text += s.Text
	}

	const sep = " = "
	i := indexOf(text, sep)
	if i < 0 {
		return ""
	}

	rest := text[i+len(sep):]
	if j := indexOf(rest, " "); j >= 0 {
		return rest[:j]
	}
	return rest
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// writeEvent frames one server-sent event.
func writeEvent(w http.ResponseWriter, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	// SSE is line-based; the JSON encoder never emits a bare newline, so a
	// single data: line is always enough.
	_, err = w.Write([]byte("event: " + name + "\ndata: " + string(data) + "\n\n"))
	return err
}

// indentJSON pretty-prints a payload for display, leaving it alone if it is not
// valid JSON.
func indentJSON(raw []byte) string {
	var out json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw)
	}

	pretty, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(pretty)
}
