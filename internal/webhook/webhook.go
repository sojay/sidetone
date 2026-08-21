// Package webhook receives Sentry alerts over HTTP and hands them to the queue.
//
// The handler's job is to answer Sentry quickly and get out of the way: verify,
// parse, compose, enqueue, respond. It never waits for audio — a message
// already on the air can have another minute to run, and Sentry is not going to
// hold the connection open for it.
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sojay/sidetone/internal/composer"
	"github.com/sojay/sidetone/internal/cw"
	"github.com/sojay/sidetone/internal/player"
)

// MaxBodyBytes caps a request body. Sentry events carry stack traces and can be
// large, but not this large, and we read the whole body into memory to compute
// its HMAC.
const MaxBodyBytes = 4 << 20 // 4 MiB

// SignatureHeader is the hex-encoded HMAC-SHA256 of the raw body, keyed with
// the integration's client secret.
const SignatureHeader = "sentry-hook-signature"

// ResourceHeader says what kind of thing the webhook is about: installation,
// event_alert, issue, metric_alert, error, comment, and so on.
const ResourceHeader = "sentry-hook-resource"

// Resources Sentry can label a webhook with. Only AlertResource is keyed by
// default; everything else sent to the same URL — the installation ping,
// comments, the raw error firehose — is acknowledged and dropped. Without that
// the integration would beep the moment it was installed.
const (
	// AlertResource is an issue alert firing, the normal path.
	AlertResource = "event_alert"

	// ErrorResource is every error event, delivered without waiting for alert
	// rule evaluation. Keying it is opt-in because in a real project it is a
	// firehose — but it skips a stage of Sentry's pipeline, so on a throwaway
	// project it is worth measuring against the alert path.
	ErrorResource = "error"
)

// An Admin is the part of the player the local-only routes need.
type Admin interface {
	Flush() int
	Depth() int
}

// A Queue is the part of the player this package needs. Keeping it narrow means
// the handler tests do not need audio, a player, or a goroutine.
type Queue interface {
	// Enqueue must not block: it is called from the request path.
	Enqueue(player.Message) (dropped player.Message, ok bool)
	Depth() int
}

// Config configures a Server.
type Config struct {
	Queue Queue

	// Secret is the Sentry client secret. If it is empty, /webhook/sentry
	// refuses every request: accepting unsigned alerts from the open internet
	// is not a trade worth making, and /test is there for local demos.
	Secret string

	// Admin, if set, enables the local-only routes registered by RegisterAdmin.
	Admin Admin

	// Resources are the Sentry-Hook-Resource values that get keyed. Empty means
	// just AlertResource. Anything not listed is acknowledged and dropped.
	Resources []string

	// OnReceived, if set, is called for every alert that makes it as far as
	// being composed. It exists so a display can show the payload beside the
	// message it turned into; the handler does not wait for it to do anything.
	OnReceived func(Received)

	Logger *log.Logger // nil discards
}

// A Received is an accepted alert, reported for display.
type Received struct {
	Endpoint string         // which route it arrived on
	Raw      []byte         // the request body, verbatim
	Message  string         // the composed CW message
	Alert    composer.Alert // what we understood the payload to mean
	Resource string         // the Sentry-Hook-Resource it arrived as, if any

	// SentryLag is how long the event sat inside Sentry before this webhook
	// arrived — ingest, issue creation, rule evaluation and dispatch. It dwarfs
	// everything sidetone does by three orders of magnitude, so it is worth
	// showing rather than leaving people to assume the audio is slow.
	//
	// Zero when the payload carried no timing (for example /test).
	SentryLag time.Duration
}

// A Server routes alert requests into the queue.
type Server struct {
	queue      Queue
	admin      Admin
	secret     string
	logger     *log.Logger
	onReceived func(Received)
	resources  map[string]bool

	// instance identifies this process. A 200 from /healthz only proves that
	// something answered; comparing this value through a tunnel proves the
	// public URL reaches *this* station and not a stale one.
	instance string
}

// New returns a Server.
func New(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	onReceived := cfg.OnReceived
	if onReceived == nil {
		onReceived = func(Received) {}
	}

	resources := map[string]bool{}
	for _, r := range cfg.Resources {
		if r = strings.ToLower(strings.TrimSpace(r)); r != "" {
			resources[r] = true
		}
	}
	if len(resources) == 0 {
		resources[AlertResource] = true
	}

	return &Server{
		instance:   newInstanceID(),
		admin:      cfg.Admin,
		queue:      cfg.Queue,
		secret:     cfg.Secret,
		logger:     logger,
		onReceived: onReceived,
		resources:  resources,
	}
}

// Keys reports whether a Sentry-Hook-Resource value will be keyed.
func (s *Server) Keys(resource string) bool {
	return s.resources[strings.ToLower(strings.TrimSpace(resource))]
}

// Register adds the alert routes to mux, so they can share a mux with other
// handlers such as the band scope.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhook/sentry", s.handleSentry)
	mux.HandleFunc("POST /test", s.handleTest)
	mux.HandleFunc("GET /healthz", s.handleHealth)
}

// Handler returns the routes on a mux of their own. Patterns carry their
// method, so anything else gets a 405 from the mux itself.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

// response is what we send back. It echoes the composed message so the demo can
// show what was keyed without digging through logs.
type response struct {
	Queued  bool   `json:"queued"`
	Message string `json:"message,omitempty"`
	Depth   int    `json:"depth"`
	Dropped string `json:"dropped,omitempty"`
	Note    string `json:"note,omitempty"`
}

func (s *Server) handleSentry(w http.ResponseWriter, r *http.Request) {
	if s.secret == "" {
		s.logger.Print("webhook: rejecting alert, SENTRY_CLIENT_SECRET is not set")
		http.Error(w, "signature verification is not configured", http.StatusServiceUnavailable)
		return
	}

	body, ok := readBody(w, r)
	if !ok {
		return
	}

	// Constant-time comparison: a signature check that leaks how far it got is
	// not a signature check.
	if !validSignature(s.secret, body, r.Header.Get(SignatureHeader)) {
		s.logger.Printf("webhook: rejecting alert from %s, bad signature", r.RemoteAddr)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	// Acknowledge anything we do not key. It has to be a 200: Sentry treats
	// repeated failures as a broken integration and will eventually disable it.
	resource := r.Header.Get(ResourceHeader)
	if resource != "" && !s.Keys(resource) {
		s.logger.Printf("webhook: acknowledged %q webhook, not keyed", resource)
		writeJSON(w, http.StatusOK, response{
			Queued: false,
			Note:   "acknowledged: " + resource + " is not keyed, nothing played",
			Depth:  s.queue.Depth(),
		})
		return
	}

	var p sentryPayload
	if err := json.Unmarshal(body, &p); err != nil {
		// A type mismatch on one field still leaves the rest populated, so it
		// is only worth giving up when the body is not JSON at all.
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			http.Error(w, "malformed JSON", http.StatusBadRequest)
			return
		}
		s.logger.Printf("webhook: ignoring unexpected type for %q", typeErr.Field)
	}

	// How long the event spent inside Sentry, if the payload said.
	var lag time.Duration
	if at := p.event().ingestedAt(); !at.IsZero() {
		lag = time.Since(at)
	}

	s.enqueue(w, "/webhook/sentry", body, p.alert(), resource, lag)
}

// handleTest is the demo entry point: same pipeline, no signature.
func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}

	var in struct {
		Project string `json:"project"`
		Level   string `json:"level"`
		Title   string `json:"title"`
		Culprit string `json:"culprit"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "malformed JSON", http.StatusBadRequest)
		return
	}

	s.enqueue(w, "/test", body, composer.Alert{
		Project: in.Project,
		Level:   in.Level,
		Title:   in.Title,
		Culprit: in.Culprit,
	}, "", 0)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"depth":    s.queue.Depth(),
		"instance": s.instance,
	})
}

// Instance is this process's id, as reported by /healthz.
func (s *Server) Instance() string { return s.instance }

// RegisterAdmin adds routes that must never be reachable from the internet.
// They belong on a listener bound to the loopback interface: requests arriving
// through a tunnel appear to come from 127.0.0.1 too, because the tunnel client
// connects locally, so checking the remote address cannot tell them apart.
func (s *Server) RegisterAdmin(mux *http.ServeMux) {
	mux.HandleFunc("POST /flush", s.handleFlush)
}

func (s *Server) handleFlush(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		http.Error(w, "flush is not enabled", http.StatusNotFound)
		return
	}

	n := s.admin.Flush()
	s.logger.Printf("webhook: flushed %d queued messages", n)
	writeJSON(w, http.StatusOK, map[string]any{
		"flushed": n,
		"depth":   s.admin.Depth(),
		"note":    "the message already on the air finishes; everything behind it is gone",
	})
}

// newInstanceID is eight hex characters — enough to tell two runs apart.
func newInstanceID() string {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(raw[:])
}

// enqueue composes the alert, queues it and answers. This is the only place
// that writes a success response, so the 200-after-enqueue contract holds for
// both endpoints.
func (s *Server) enqueue(w http.ResponseWriter, endpoint string, raw []byte, a composer.Alert, resource string, lag time.Duration) {
	msg := composer.Compose(a)

	s.onReceived(Received{
		Endpoint: endpoint, Raw: raw, Message: msg, Alert: a,
		Resource: resource, SentryLag: lag,
	})

	dropped, wasDropped := s.queue.Enqueue(player.Message{
		Text:    msg,
		Profile: cw.ProfileFor(a.Level),
	})

	resp := response{Queued: true, Message: msg, Depth: s.queue.Depth()}
	if wasDropped {
		resp.Dropped = dropped.Text
		if dropped.Text == msg {
			// The queue was full of higher-priority alerts and this one lost.
			resp.Queued = false
		}
		s.logger.Printf("webhook: queue full, dropped %q", dropped.Text)
	}

	if lag > 0 {
		// The interesting number: sidetone's own share of this is single-digit
		// milliseconds, so anything large here happened inside Sentry.
		s.logger.Printf("webhook: queued %q (depth %d, %v inside Sentry)",
			msg, resp.Depth, lag.Round(time.Millisecond))
	} else {
		s.logger.Printf("webhook: queued %q (depth %d)", msg, resp.Depth)
	}
	writeJSON(w, http.StatusOK, resp)
}

// readBody reads the whole body, which the HMAC needs, and writes the error
// response itself if it cannot.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return nil, false
		}
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// validSignature reports whether sig is the hex HMAC-SHA256 of body under
// secret.
func validSignature(secret string, body []byte, sig string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(want), []byte(sig))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already out; nothing useful left to do.
		return
	}
}
