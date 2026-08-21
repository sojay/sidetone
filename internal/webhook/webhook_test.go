package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sojay/sidetone/internal/player"
)

const testSecret = "s3cr3t"

// fakeQueue records what the handler enqueued without needing a player.
type fakeQueue struct {
	mu       sync.Mutex
	messages []player.Message

	// drop, when set, is returned as the dropped message from every Enqueue.
	drop *player.Message

	// block, when non-nil, holds Enqueue — used to prove the handler is not
	// waiting on anything slow.
	block chan struct{}
}

func (q *fakeQueue) Enqueue(m player.Message) (player.Message, bool) {
	if q.block != nil {
		<-q.block
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	q.messages = append(q.messages, m)

	if q.drop != nil {
		return *q.drop, true
	}
	return player.Message{}, false
}

func (q *fakeQueue) Depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.messages)
}

func (q *fakeQueue) all() []player.Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]player.Message(nil), q.messages...)
}

func (q *fakeQueue) only(t *testing.T) player.Message {
	t.Helper()

	got := q.all()
	if len(got) != 1 {
		t.Fatalf("queued %d messages, want 1", len(got))
	}
	return got[0]
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// post sends a signed request unless sig is explicitly given.
func post(t *testing.T, srv *Server, path, body string, sig string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if sig != "" {
		req.Header.Set(SignatureHeader, sig)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func newServer(q Queue) *Server {
	return New(Config{Queue: q, Secret: testSecret})
}

// The legacy webhook plugin payload: everything at the top level.
const legacyPayload = `{
	"id": "1234",
	"project": "checkout-api",
	"project_name": "Checkout API",
	"level": "fatal",
	"culprit": "app/handlers.checkout",
	"message": "Something broke",
	"url": "https://sentry.io/issues/1234/",
	"event": {
		"event_id": "abc",
		"title": "OperationalError: connection refused",
		"level": "fatal",
		"culprit": "app/handlers.checkout"
	}
}`

// The Integration Platform payload: an envelope around data.event.
const platformPayload = `{
	"action": "triggered",
	"installation": {"uuid": "xyz"},
	"data": {
		"event": {
			"event_id": "abc",
			"project": 42,
			"project_slug": "payments",
			"level": "warning",
			"title": "TimeoutError: upstream slow",
			"culprit": "billing/charge.py",
			"web_url": "https://sentry.io/issues/1/"
		},
		"triggered_rule": "Send a notification for new issues"
	},
	"actor": {"type": "application", "id": "sentry", "name": "Sentry"}
}`

func TestSentryWebhookAcceptsLegacyPayload(t *testing.T) {
	q := &fakeQueue{}
	rec := post(t, newServer(q), "/webhook/sentry", legacyPayload, sign(testSecret, legacyPayload))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	got := q.only(t)
	want := "VVV DE = CHECKOUT-API FATAL = OPERATIONALERROR: CONNECTION REFUSED = <AR>"
	if got.Text != want {
		t.Errorf("keyed\n  %q\nwant\n  %q", got.Text, want)
	}
	if got.Profile.Level != "fatal" {
		t.Errorf("profile level = %q, want fatal", got.Profile.Level)
	}
	if got.Profile.WPM != 28 {
		t.Errorf("WPM = %g, want the fatal speed of 28", got.Profile.WPM)
	}
}

func TestSentryWebhookAcceptsPlatformPayload(t *testing.T) {
	q := &fakeQueue{}
	rec := post(t, newServer(q), "/webhook/sentry", platformPayload, sign(testSecret, platformPayload))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	got := q.only(t)
	if !strings.Contains(got.Text, "PAYMENTS WARNING") {
		t.Errorf("keyed %q, want the project slug and level from data.event", got.Text)
	}
	if got.Profile.Level != "warning" {
		t.Errorf("profile level = %q, want warning", got.Profile.Level)
	}
}

// The payload Sentry actually sends for an issue alert: the project is a
// numeric id, there is no project_slug, and the only human-readable project
// name in the whole body is inside the event's API url.
const realIssueAlert = `{
	"action": "triggered",
	"installation": {"uuid": "a8dc71d4-9c9d-4ba9-9f8c-c2e0b6f8e0d1"},
	"data": {
		"event": {
			"event_id": "e4874d664c3540c1a32eab185f12c5ab",
			"project": 1,
			"level": "error",
			"culprit": "?(<anonymous>)",
			"title": "ReferenceError: heck is not defined",
			"url": "https://sentry.io/api/0/projects/test-org/front-end/events/e4874d664c3540c1a32eab185f12c5ab/",
			"web_url": "https://sentry.io/organizations/test-org/issues/1117540176/events/e4874d664c3540c1a32eab185f12c5ab/",
			"issue_url": "https://sentry.io/api/0/issues/1117540176/",
			"issue_id": "1117540176"
		},
		"triggered_rule": "Send a notification for new issues"
	}
}`

// TestSentryWebhookRecoversProjectFromEventURL is the difference between
// hearing which project broke and hearing "UNKNOWN" every time.
func TestSentryWebhookRecoversProjectFromEventURL(t *testing.T) {
	q := &fakeQueue{}
	req := httptest.NewRequest(http.MethodPost, "/webhook/sentry", strings.NewReader(realIssueAlert))
	req.Header.Set(SignatureHeader, sign(testSecret, realIssueAlert))
	req.Header.Set(ResourceHeader, AlertResource)

	rec := httptest.NewRecorder()
	newServer(q).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	got := q.only(t).Text
	want := "VVV DE = FRONT-END ERROR = REFERENCEERROR: HECK IS NOT DEFINED = <AR>"
	if got != want {
		t.Errorf("keyed\n  %q\nwant\n  %q", got, want)
	}
	if strings.Contains(got, "UNKNOWN") {
		t.Error("project fell back to UNKNOWN; the slug is in data.event.url")
	}
}

// TestSentryLagIsMeasured is the point of reporting lag at all: sidetone's own
// work is single-digit milliseconds, so a large number here is provably Sentry's
// pipeline and not the audio being slow.
func TestSentryLagIsMeasured(t *testing.T) {
	received := float64(time.Now().Add(-24*time.Second).UnixNano()) / 1e9
	body := fmt.Sprintf(`{"action":"triggered","data":{"event":{
		"level":"fatal","title":"pool exhausted","received":%.6f,
		"url":"https://sentry.io/api/0/projects/o/sidetone-demo/events/x/"}}}`, received)

	var got Received
	srv := New(Config{
		Queue:      &fakeQueue{},
		Secret:     testSecret,
		OnReceived: func(r Received) { got = r },
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook/sentry", strings.NewReader(body))
	req.Header.Set(SignatureHeader, sign(testSecret, body))
	req.Header.Set(ResourceHeader, AlertResource)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got.SentryLag < 23*time.Second || got.SentryLag > 26*time.Second {
		t.Errorf("SentryLag = %v, want about 24s", got.SentryLag)
	}
	if got.Resource != AlertResource {
		t.Errorf("Resource = %q, want %q", got.Resource, AlertResource)
	}
}

// A payload with no timing must report no lag rather than 56 years.
func TestNoTimingMeansNoLag(t *testing.T) {
	var got Received
	srv := New(Config{
		Queue:      &fakeQueue{},
		Secret:     testSecret,
		OnReceived: func(r Received) { got = r },
	})

	rec := post(t, srv, "/webhook/sentry", legacyPayload, sign(testSecret, legacyPayload))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got.SentryLag != 0 {
		t.Errorf("SentryLag = %v, want 0 when the payload carries no timestamps", got.SentryLag)
	}
}

func TestIngestedAtPrefersReceived(t *testing.T) {
	// received is when Sentry got it; timestamp is when the error happened.
	// Only the first isolates Sentry's own processing time.
	ev := sentryEvent{Received: 1786996345.5, Timestamp: 1786996000}
	if got, want := ev.ingestedAt().Unix(), int64(1786996345); got != want {
		t.Errorf("ingestedAt = %d, want %d (received, not timestamp)", got, want)
	}

	ev = sentryEvent{Timestamp: 1786996000}
	if got, want := ev.ingestedAt().Unix(), int64(1786996000); got != want {
		t.Errorf("ingestedAt = %d, want the timestamp fallback %d", got, want)
	}

	ev = sentryEvent{Datetime: "2026-08-17T19:52:25Z"}
	if ev.ingestedAt().IsZero() {
		t.Error("ingestedAt should fall back to parsing datetime")
	}

	if !(sentryEvent{}).ingestedAt().IsZero() {
		t.Error("an event with no timing should give the zero time")
	}
}

// TestErrorResourcePayload covers the other webhook shape: the error resource
// nests the event under data.error rather than data.event.
func TestErrorResourcePayload(t *testing.T) {
	body := `{"action":"created","data":{"error":{
		"level":"warning","title":"TimeoutError: upstream slow","received":1786996345.5,
		"url":"https://sentry.io/api/0/projects/o/sidetone-demo/events/x/"}}}`

	q := &fakeQueue{}
	srv := New(Config{
		Queue:     q,
		Secret:    testSecret,
		Resources: []string{AlertResource, ErrorResource},
	})

	req := httptest.NewRequest(http.MethodPost, "/webhook/sentry", strings.NewReader(body))
	req.Header.Set(SignatureHeader, sign(testSecret, body))
	req.Header.Set(ResourceHeader, ErrorResource)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	got := q.only(t).Text
	want := "VVV DE = SIDETONE-DEMO WARNING = TIMEOUTERROR: UPSTREAM SLOW = <AR>"
	if got != want {
		t.Errorf("keyed\n  %q\nwant\n  %q", got, want)
	}
}

// TestErrorResourceIsOptIn: keying every error is a firehose in a real project,
// so it must stay off unless asked for.
func TestErrorResourceIsOptIn(t *testing.T) {
	body := `{"action":"created","data":{"error":{"level":"error","title":"boom"}}}`

	q := &fakeQueue{}
	srv := newServer(q) // default resources

	req := httptest.NewRequest(http.MethodPost, "/webhook/sentry", strings.NewReader(body))
	req.Header.Set(SignatureHeader, sign(testSecret, body))
	req.Header.Set(ResourceHeader, ErrorResource)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if n := len(q.all()); n != 0 {
		t.Errorf("queued %d error webhooks by default, want none", n)
	}
	if srv.Keys(ErrorResource) {
		t.Error("Keys(error) is true by default")
	}
	if !srv.Keys(AlertResource) {
		t.Error("Keys(event_alert) should always be true by default")
	}
}

func TestResourcesAreCaseInsensitive(t *testing.T) {
	srv := New(Config{Queue: &fakeQueue{}, Secret: testSecret, Resources: []string{" Error ", "EVENT_ALERT"}})

	for _, r := range []string{"error", "ERROR", "event_alert", " error "} {
		if !srv.Keys(r) {
			t.Errorf("Keys(%q) = false, want true", r)
		}
	}
	if srv.Keys("comment") {
		t.Error("Keys(comment) = true, want false")
	}
}

func TestProjectFromURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://sentry.io/api/0/projects/test-org/front-end/events/abc/", "front-end"},
		{"https://sentry.io/api/0/projects/acme/checkout-api/events/1/", "checkout-api"},
		{"https://self-hosted.example.com/api/0/projects/org/proj/events/1/", "proj"},
		{"https://sentry.io/api/0/issues/1117540176/", ""}, // no project segment
		{"https://sentry.io/organizations/test-org/issues/1/", ""},
		{"https://sentry.io/api/0/projects/org/", ""}, // truncated, no slug
		{"", ""},
		{"://not a url", ""},
	}

	for _, tt := range tests {
		if got := projectFromURL(tt.in); got != tt.want {
			t.Errorf("projectFromURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSentryWebhookIgnoresNonAlertResources: Sentry posts installation pings,
// comments and the error firehose to the same URL. Keying those would mean the
// station beeps the moment the integration is installed.
func TestSentryWebhookIgnoresNonAlertResources(t *testing.T) {
	for _, resource := range []string{"installation", "comment", "issue", "metric_alert", "error", "seer"} {
		t.Run(resource, func(t *testing.T) {
			q := &fakeQueue{}
			body := `{"action":"created","data":{}}`

			req := httptest.NewRequest(http.MethodPost, "/webhook/sentry", strings.NewReader(body))
			req.Header.Set(SignatureHeader, sign(testSecret, body))
			req.Header.Set(ResourceHeader, resource)

			rec := httptest.NewRecorder()
			newServer(q).Handler().ServeHTTP(rec, req)

			// Must be a 200: Sentry disables integrations that keep failing.
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 so Sentry does not disable the integration", rec.Code)
			}
			if n := len(q.all()); n != 0 {
				t.Errorf("queued %d messages for a %s webhook, want none", n, resource)
			}

			var resp response
			json.NewDecoder(rec.Body).Decode(&resp)
			if resp.Queued {
				t.Error("response claims the alert was queued")
			}
			if !strings.Contains(resp.Note, resource) {
				t.Errorf("note = %q, want it to name the resource", resp.Note)
			}
		})
	}
}

// A webhook with no resource header is the legacy plugin or a hand-rolled
// request, and is still keyed.
func TestSentryWebhookWithoutResourceHeaderIsKeyed(t *testing.T) {
	q := &fakeQueue{}
	rec := post(t, newServer(q), "/webhook/sentry", legacyPayload, sign(testSecret, legacyPayload))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(q.all()) != 1 {
		t.Error("nothing queued for an unlabelled webhook")
	}
}

// A bad signature must be rejected before the resource is even considered.
func TestSentryWebhookChecksSignatureBeforeResource(t *testing.T) {
	q := &fakeQueue{}
	body := `{"action":"created"}`

	req := httptest.NewRequest(http.MethodPost, "/webhook/sentry", strings.NewReader(body))
	req.Header.Set(SignatureHeader, "wrong")
	req.Header.Set(ResourceHeader, "installation")

	rec := httptest.NewRecorder()
	newServer(q).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestSentryWebhookEchoesTheMessage(t *testing.T) {
	q := &fakeQueue{}
	rec := post(t, newServer(q), "/webhook/sentry", legacyPayload, sign(testSecret, legacyPayload))

	var resp response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if !resp.Queued {
		t.Error("response says the alert was not queued")
	}
	if resp.Message != q.only(t).Text {
		t.Errorf("response message %q does not match what was queued %q", resp.Message, q.only(t).Text)
	}
	if resp.Depth != 1 {
		t.Errorf("depth = %d, want 1", resp.Depth)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestSentryWebhookRejectsBadSignature(t *testing.T) {
	tests := []struct {
		name string
		sig  string
	}{
		{"wrong secret", sign("not-the-secret", legacyPayload)},
		{"signature of a different body", sign(testSecret, `{"project":"other"}`)},
		{"garbage", "not-hex-at-all"},
		{"empty header", ""},
		{"truncated", sign(testSecret, legacyPayload)[:32]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &fakeQueue{}
			rec := post(t, newServer(q), "/webhook/sentry", legacyPayload, tt.sig)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if n := len(q.all()); n != 0 {
				t.Errorf("queued %d messages, want none", n)
			}
		})
	}
}

// TestSentryWebhookSignatureCoversTheWholeBody is the reason the HMAC is
// computed over raw bytes: tampering with the body must invalidate it.
func TestSentryWebhookSignatureCoversTheWholeBody(t *testing.T) {
	q := &fakeQueue{}
	tampered := strings.Replace(legacyPayload, "fatal", "warning", 1)

	rec := post(t, newServer(q), "/webhook/sentry", tampered, sign(testSecret, legacyPayload))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a tampered body", rec.Code)
	}
}

// TestSentryWebhookWithoutSecretFailsClosed: an unconfigured server must not
// accept unsigned alerts from the internet.
func TestSentryWebhookWithoutSecretFailsClosed(t *testing.T) {
	q := &fakeQueue{}
	srv := New(Config{Queue: q})

	rec := post(t, srv, "/webhook/sentry", legacyPayload, sign("", legacyPayload))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if n := len(q.all()); n != 0 {
		t.Errorf("queued %d messages, want none", n)
	}
}

func TestSentryWebhookMalformedJSON(t *testing.T) {
	for _, body := range []string{"", "not json", "{", `{"project":`, "[1,2,3"} {
		q := &fakeQueue{}
		rec := post(t, newServer(q), "/webhook/sentry", body, sign(testSecret, body))

		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
		if n := len(q.all()); n != 0 {
			t.Errorf("body %q: queued %d messages, want none", body, n)
		}
	}
}

// TestSentryWebhookSurvivesUnexpectedTypes is the defensive-parsing case: one
// field of the wrong type must not lose the whole alert.
func TestSentryWebhookSurvivesUnexpectedTypes(t *testing.T) {
	body := `{"project": 42, "project_slug": "web", "level": "error",
	          "event": {"title": "Boom", "culprit": 99}}`

	q := &fakeQueue{}
	rec := post(t, newServer(q), "/webhook/sentry", body, sign(testSecret, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := q.only(t).Text; !strings.Contains(got, "WEB ERROR") {
		t.Errorf("keyed %q, want the fields that did parse", got)
	}
}

// TestSentryWebhookNumericProjectIsNotKeyed: a project id is not a name, and
// keying "42" would be worse than announcing it as unknown.
func TestSentryWebhookNumericProjectIsNotKeyed(t *testing.T) {
	body := `{"project": 42, "level": "error", "event": {"title": "Boom"}}`

	q := &fakeQueue{}
	rec := post(t, newServer(q), "/webhook/sentry", body, sign(testSecret, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := q.only(t).Text; !strings.Contains(got, "UNKNOWN") {
		t.Errorf("keyed %q, want the unknown-project fallback", got)
	}
}

// TestSentryWebhookEmptyObject: a payload we understand nothing of still gets
// keyed, on the grounds that silence is the one unacceptable outcome.
func TestSentryWebhookEmptyObject(t *testing.T) {
	q := &fakeQueue{}
	rec := post(t, newServer(q), "/webhook/sentry", "{}", sign(testSecret, "{}"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := q.only(t).Text; got != "VVV DE = UNKNOWN ERROR = NO TITLE = <AR>" {
		t.Errorf("keyed %q, want the all-defaults message", got)
	}
}

func TestSentryWebhookBodyTooLarge(t *testing.T) {
	body := `{"project":"web","level":"error","title":"` + strings.Repeat("x", MaxBodyBytes) + `"}`

	q := &fakeQueue{}
	rec := post(t, newServer(q), "/webhook/sentry", body, sign(testSecret, body))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if n := len(q.all()); n != 0 {
		t.Errorf("queued %d messages, want none", n)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	paths := []string{"/webhook/sentry", "/test"}

	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			req := httptest.NewRequest(method, path, nil)
			rec := httptest.NewRecorder()
			newServer(&fakeQueue{}).Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status = %d, want 405", method, path, rec.Code)
			}
		}
	}
}

func TestUnknownPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/nope", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	newServer(&fakeQueue{}).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestTestEndpoint(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"full",
			`{"project":"checkout-api","level":"fatal","title":"DB pool exhausted"}`,
			"VVV DE = CHECKOUT-API FATAL = DB POOL EXHAUSTED = <AR>",
		},
		{
			"culprit only",
			`{"project":"web","level":"warning","culprit":"app/handlers.go"}`,
			"VVV DE = WEB WARNING = APP/HANDLERS.GO = <AR>",
		},
		{
			"empty object gets the defaults",
			`{}`,
			"VVV DE = UNKNOWN ERROR = NO TITLE = <AR>",
		},
		{
			"unknown fields are ignored",
			`{"project":"web","level":"error","title":"Boom","nonsense":{"a":[1,2]}}`,
			"VVV DE = WEB ERROR = BOOM = <AR>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &fakeQueue{}
			rec := post(t, newServer(q), "/test", tt.body, "")

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
			}
			if got := q.only(t).Text; got != tt.want {
				t.Errorf("keyed\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// TestTestEndpointNeedsNoSignature is the point of /test: local demos without
// Sentry, and without a secret configured.
func TestTestEndpointNeedsNoSignature(t *testing.T) {
	q := &fakeQueue{}
	srv := New(Config{Queue: q}) // no secret at all

	rec := post(t, srv, "/test", `{"project":"web","level":"error","title":"Boom"}`, "")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if len(q.all()) != 1 {
		t.Error("nothing queued")
	}
}

func TestTestEndpointMalformedJSON(t *testing.T) {
	q := &fakeQueue{}
	rec := post(t, newServer(q), "/test", "{oops", "")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestReportsDroppedMessage checks the caller learns when the queue shed load,
// including the case where the alert it just sent was the one dropped.
func TestReportsDroppedMessage(t *testing.T) {
	t.Run("an older message was dropped", func(t *testing.T) {
		dropped := player.Message{Text: "VVV DE = OLD WARNING = SOMETHING = <AR>"}
		q := &fakeQueue{drop: &dropped}

		rec := post(t, newServer(q), "/test", `{"project":"web","level":"fatal","title":"Boom"}`, "")

		var resp response
		json.NewDecoder(rec.Body).Decode(&resp)

		if !resp.Queued {
			t.Error("queued = false, but this alert was accepted")
		}
		if resp.Dropped != dropped.Text {
			t.Errorf("dropped = %q, want %q", resp.Dropped, dropped.Text)
		}
	})

	t.Run("this message lost to a queue of fatals", func(t *testing.T) {
		q := &fakeQueue{}
		// The queue returns the incoming message as the dropped one.
		body := `{"project":"web","level":"warning","title":"Boom"}`
		q.drop = &player.Message{Text: "VVV DE = WEB WARNING = BOOM = <AR>"}

		rec := post(t, newServer(q), "/test", body, "")

		var resp response
		json.NewDecoder(rec.Body).Decode(&resp)

		if resp.Queued {
			t.Error("queued = true, but this alert was the one dropped")
		}
	})
}

// TestHandlerDoesNotWaitOnAudio: the handler must answer while the queue is
// still busy. A slow Enqueue is the closest stand-in for a blocked player.
func TestHandlerDoesNotWaitOnAudio(t *testing.T) {
	release := make(chan struct{})
	q := &fakeQueue{block: release}

	done := make(chan int, 1)
	go func() {
		rec := post(t, newServer(q), "/test", `{"project":"web","level":"error","title":"Boom"}`, "")
		done <- rec.Code
	}()

	// The handler is blocked in Enqueue; confirm it is not spinning or failing.
	select {
	case <-done:
		t.Fatal("handler returned before the queue accepted the message")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never returned after the queue unblocked")
	}
}

// fakeAdmin stands in for the player on the local-only routes.
type fakeAdmin struct{ flushed, depth int }

func (f *fakeAdmin) Flush() int { n := f.depth; f.depth = 0; f.flushed = n; return n }
func (f *fakeAdmin) Depth() int { return f.depth }

// TestHealthzReportsAnInstance is what lets a preflight check prove the public
// URL reaches *this* process rather than merely getting a 200 from something.
func TestHealthzReportsAnInstance(t *testing.T) {
	srv := newServer(&fakeQueue{})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	var body struct {
		OK       bool   `json:"ok"`
		Instance string `json:"instance"`
	}
	json.NewDecoder(rec.Body).Decode(&body)

	if !body.OK {
		t.Error("ok = false")
	}
	if len(body.Instance) != 8 {
		t.Errorf("instance = %q, want 8 hex characters", body.Instance)
	}
	if body.Instance != srv.Instance() {
		t.Errorf("/healthz says %q, Instance() says %q", body.Instance, srv.Instance())
	}

	// Two stations must be distinguishable, or the check proves nothing.
	if newServer(&fakeQueue{}).Instance() == srv.Instance() {
		t.Error("two servers share an instance id")
	}
}

// TestFlushIsNotOnThePublicMux is the point of a separate listener: a tunnelled
// request arrives from 127.0.0.1 like any local one, so the route simply must
// not exist on the port that is exposed.
func TestFlushIsNotOnThePublicMux(t *testing.T) {
	admin := &fakeAdmin{depth: 3}
	srv := New(Config{Queue: &fakeQueue{}, Secret: testSecret, Admin: admin})

	// Mirror the deployed mux: the band scope registers a "GET /" catch-all
	// alongside these routes, which makes an unregistered POST come back 405
	// rather than 404. The status is incidental — what must hold is that the
	// handler never runs.
	mux := http.NewServeMux()
	srv.Register(mux)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {})

	req := httptest.NewRequest(http.MethodPost, "/flush", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("public mux served /flush with 200")
	}
	if admin.flushed != 0 || admin.depth != 3 {
		t.Errorf("the public route flushed the queue: flushed=%d depth=%d", admin.flushed, admin.depth)
	}
}

func TestFlushOnTheAdminMux(t *testing.T) {
	admin := &fakeAdmin{depth: 3}
	srv := New(Config{Queue: &fakeQueue{}, Secret: testSecret, Admin: admin})

	mux := http.NewServeMux()
	srv.RegisterAdmin(mux)

	req := httptest.NewRequest(http.MethodPost, "/flush", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		Flushed int `json:"flushed"`
		Depth   int `json:"depth"`
	}
	json.NewDecoder(rec.Body).Decode(&body)
	if body.Flushed != 3 || body.Depth != 0 {
		t.Errorf("flushed %d leaving %d, want 3 and 0", body.Flushed, body.Depth)
	}
}

// Without an Admin the route reports itself absent rather than panicking.
func TestFlushWithoutAdmin(t *testing.T) {
	srv := newServer(&fakeQueue{})
	mux := http.NewServeMux()
	srv.RegisterAdmin(mux)

	req := httptest.NewRequest(http.MethodPost, "/flush", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	q := &fakeQueue{}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	newServer(q).Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestConcurrentRequests is the race-detector target: a real server handles
// each request in its own goroutine.
func TestConcurrentRequests(t *testing.T) {
	q := &fakeQueue{}
	srv := newServer(q)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, srv, "/test", `{"project":"web","level":"error","title":"Boom"}`, "")
		}()
	}
	wg.Wait()

	if got := len(q.all()); got != 32 {
		t.Errorf("queued %d messages, want 32", got)
	}
}
