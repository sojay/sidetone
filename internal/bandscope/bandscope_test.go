package bandscope

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sojay/sidetone/internal/composer"
	"github.com/sojay/sidetone/internal/cw"
	"github.com/sojay/sidetone/internal/player"
)

// fakeStation stands in for the player.
type fakeStation struct {
	mu   sync.Mutex
	snap player.Snapshot
}

func (f *fakeStation) Snapshot() player.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeStation) set(s player.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = s
}

func msg(level, text string) player.Message {
	return player.Message{Text: text, Profile: cw.ProfileFor(level)}
}

func newHub(t *testing.T, st Station) *Hub {
	t.Helper()

	h, err := New(st, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestPageIsServed(t *testing.T) {
	h := newHub(t, &fakeStation{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.Handler(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	// A cached copy of a live dashboard is worse than a slow one: the station
	// runs, the data flows, and the screen shows an old build.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	body := rec.Body.String()
	for _, want := range []string{"<canvas id=\"scope\">", "EventSource(\"/events\")", "sidetone"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

// TestUnknownPathIsNotThePage guards the catch-all: "GET /" matches everything,
// so a typo must 404 rather than silently serve the display.
func TestUnknownPathIsNotThePage(t *testing.T) {
	h := newHub(t, &fakeStation{})

	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	h.Handler(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestStateReportsTheStation(t *testing.T) {
	playing := msg("fatal", "VVV DE = WEB FATAL = BOOM = <AR>")
	st := &fakeStation{}
	st.set(player.Snapshot{
		Playing:   &playing,
		StartedAt: time.Now().Add(-2 * time.Second),
		Queue:     []player.Message{msg("warning", "VVV DE = WEB WARNING = SLOW = <AR>")},
		Depth:     1,
		Played:    3,
		Dropped:   2,
	})

	h := newHub(t, st)
	req := httptest.NewRequest(http.MethodGet, "/state", nil)
	rec := httptest.NewRecorder()
	h.Handler(t).ServeHTTP(rec, req)

	var got state
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if got.Playing == nil {
		t.Fatal("Playing is nil, want the message on the air")
	}
	if got.Playing.Level != "fatal" || got.Playing.WPM != 28 {
		t.Errorf("playing = %s at %g WPM, want fatal at 28", got.Playing.Level, got.Playing.WPM)
	}
	if got.Playing.Project != "WEB" {
		t.Errorf("Project = %q, want WEB", got.Playing.Project)
	}
	if len(got.Queue) != 1 || got.Queue[0].Level != "warning" {
		t.Errorf("Queue = %+v, want one warning", got.Queue)
	}
	if got.Played != 3 || got.Dropped != 2 || got.Depth != 1 {
		t.Errorf("counters = %d played, %d dropped, %d deep", got.Played, got.Dropped, got.Depth)
	}
}

// TestElapsedLetsALateJoinerCatchUp is what keeps a page opened mid-alert in
// sync: it must be told how far in the transmission already is.
func TestElapsedLetsALateJoinerCatchUp(t *testing.T) {
	playing := msg("error", "VVV DE = WEB ERROR = BOOM = <AR>")
	st := &fakeStation{}
	st.set(player.Snapshot{Playing: &playing, StartedAt: time.Now().Add(-3 * time.Second)})

	h := newHub(t, st)
	got := h.state()

	if got.Playing == nil {
		t.Fatal("nothing playing")
	}
	if got.Playing.Elapsed < 2900 || got.Playing.Elapsed > 3200 {
		t.Errorf("Elapsed = %gms, want about 3000", got.Playing.Elapsed)
	}
}

// TestTransmissionCarriesEnoughToAnimate is the contract with the page: it gets
// the characters, their offsets and the unit length, and needs nothing else to
// draw the keying in step with the sound.
func TestTransmissionCarriesEnoughToAnimate(t *testing.T) {
	tr := transmissionOf(msg("warning", "E E"), 0)

	if len(tr.Symbols) != 3 {
		t.Fatalf("got %d symbols, want 3 (E, gap, E)", len(tr.Symbols))
	}
	if tr.Symbols[2].Start != 8 {
		t.Errorf("second E starts at unit %d, want 8", tr.Symbols[2].Start)
	}
	if tr.Units != 9 {
		t.Errorf("Units = %d, want 9", tr.Units)
	}

	// 13 WPM: a unit is 1.2/13 s ≈ 92.3ms, so the message is about 831ms.
	if tr.UnitMS < 92 || tr.UnitMS > 93 {
		t.Errorf("UnitMS = %g, want about 92.3", tr.UnitMS)
	}
	if got, want := tr.Seconds, float64(tr.Units)*tr.UnitMS/1000; got < want-0.01 || got > want+0.01 {
		t.Errorf("Seconds = %g, does not match %d units of %gms", got, tr.Units, tr.UnitMS)
	}
}

func TestProjectOf(t *testing.T) {
	tests := []struct{ text, want string }{
		{"VVV DE = CHECKOUT-API FATAL = BOOM = <AR>", "CHECKOUT-API"},
		{"VVV DE = WEB ERROR = BOOM = <AR>", "WEB"},
		{"VVV DE = UNKNOWN ERROR = NO TITLE = <AR>", "UNKNOWN"},
		{"NO SEPARATORS HERE", ""},
	}

	for _, tt := range tests {
		if got := projectOf(cw.Symbols(tt.text)); got != tt.want {
			t.Errorf("projectOf(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestPlayerEventIsPublished(t *testing.T) {
	h := newHub(t, &fakeStation{})
	ch := h.subscribe()

	h.PlayerEvent(player.Event{
		Kind:    player.EventStarted,
		Message: msg("fatal", "VVV DE = WEB FATAL = BOOM = <AR>"),
		Depth:   2,
	})

	select {
	case e := <-ch:
		if e.Kind != "started" {
			t.Errorf("Kind = %q, want started", e.Kind)
		}
		if e.Depth != 2 {
			t.Errorf("Depth = %d, want 2", e.Depth)
		}
		if e.Transmission == nil || e.Transmission.Level != "fatal" {
			t.Errorf("Transmission = %+v, want the fatal message", e.Transmission)
		}
		if e.At == "" {
			t.Error("At is empty; the log needs a timestamp")
		}
	default:
		t.Fatal("no event published")
	}
}

func TestReceivedCarriesThePayload(t *testing.T) {
	h := newHub(t, &fakeStation{})
	ch := h.subscribe()

	raw := []byte(`{"project":"web","level":"error"}`)
	h.Received("/test", raw, "VVV DE = WEB ERROR = BOOM = <AR>",
		composer.Alert{Project: "web", Level: "error"}, "event_alert", 24*time.Second)

	e := <-ch
	if e.Kind != "received" {
		t.Errorf("Kind = %q, want received", e.Kind)
	}
	if e.Endpoint != "/test" {
		t.Errorf("Endpoint = %q, want /test", e.Endpoint)
	}
	// Pretty-printed, so it should have gained newlines but kept the content.
	if !strings.Contains(e.Payload, "\"project\": \"web\"") {
		t.Errorf("Payload = %q, want indented JSON", e.Payload)
	}
	// The lag is the number that explains why a demo waits; it has to reach the
	// page or the display cannot make the point.
	if e.SentryLagMS != 24000 {
		t.Errorf("SentryLagMS = %g, want 24000", e.SentryLagMS)
	}
	if e.Resource != "event_alert" {
		t.Errorf("Resource = %q, want event_alert", e.Resource)
	}
}

func TestReceivedKeepsNonJSONPayloadAsIs(t *testing.T) {
	h := newHub(t, &fakeStation{})
	ch := h.subscribe()

	h.Received("/test", []byte("not json at all"), "VVV DE = X = <AR>", composer.Alert{}, "", 0)

	if got := (<-ch).Payload; got != "not json at all" {
		t.Errorf("Payload = %q, want the raw text", got)
	}
}

// TestSlowSubscriberIsSkipped is the property that protects the queue: a page
// that stops reading must never block the player.
func TestSlowSubscriberIsSkipped(t *testing.T) {
	h := newHub(t, &fakeStation{})
	h.subscribe() // never drained

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < subscriberBuffer*3; i++ {
			h.Note("filling the buffer")
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on a subscriber that stopped reading")
	}
}

func TestRecentIsBounded(t *testing.T) {
	h := newHub(t, &fakeStation{})

	for i := 0; i < backlog*3; i++ {
		h.Note("event")
	}

	if got := len(h.state().Recent); got != backlog {
		t.Errorf("kept %d recent events, want the cap of %d", got, backlog)
	}
}

func TestUnsubscribe(t *testing.T) {
	h := newHub(t, &fakeStation{})

	ch := h.subscribe()
	if got := h.Clients(); got != 1 {
		t.Errorf("Clients() = %d, want 1", got)
	}

	h.unsubscribe(ch)
	if got := h.Clients(); got != 0 {
		t.Errorf("Clients() = %d, want 0 after unsubscribing", got)
	}
}

// TestEventStream drives the SSE endpoint over a real connection: the initial
// state must arrive, then live events, in the frame format the browser expects.
func TestEventStream(t *testing.T) {
	h := newHub(t, &fakeStation{})

	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	reader := bufio.NewReader(resp.Body)

	// The first frame is always the current state.
	if line, _ := reader.ReadString('\n'); line != "event: state\n" {
		t.Fatalf("first frame = %q, want event: state", line)
	}
	if line, _ := reader.ReadString('\n'); !strings.HasPrefix(line, "data: {") {
		t.Fatalf("state frame data = %q", line)
	}

	// Wait for the subscription to be registered, then publish.
	for h.Clients() == 0 {
		time.Sleep(time.Millisecond)
	}
	h.Note("hello")

	reader.ReadString('\n') // blank line closing the state frame
	if line, _ := reader.ReadString('\n'); line != "event: event\n" {
		t.Fatalf("second frame = %q, want event: event", line)
	}

	data, _ := reader.ReadString('\n')
	if !strings.Contains(data, `"note":"hello"`) {
		t.Errorf("event data = %q, want the published note", data)
	}
}

// Handler is a test helper: the real server shares one mux across packages.
func (h *Hub) Handler(t *testing.T) http.Handler {
	t.Helper()

	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}
