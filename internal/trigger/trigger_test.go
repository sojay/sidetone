package trigger

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The placeholder from Sentry's own documentation. No real DSN appears in this
// repository, and none should.
const placeholderDSN = "https://examplePublicKey@o0.ingest.sentry.io/0"

func TestParseDSN(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantURL   string
		wantID    string
		wantKeyIn string // what the auth header must contain
	}{
		{
			"documentation placeholder",
			placeholderDSN,
			"https://o0.ingest.sentry.io/api/0/envelope/",
			"0",
			"sentry_key=examplePublicKey",
		},
		{
			"real-looking hosted DSN",
			"https://abc123def456@o447951.ingest.sentry.io/5428537",
			"https://o447951.ingest.sentry.io/api/5428537/envelope/",
			"5428537",
			"sentry_key=abc123def456",
		},
		{
			"deprecated secret key is ignored",
			"https://public:secret@sentry.example.com/42",
			"https://sentry.example.com/api/42/envelope/",
			"42",
			"sentry_key=public",
		},
		{
			"self-hosted with a path prefix",
			"https://public@sentry.example.com/sentry/42",
			"https://sentry.example.com/sentry/api/42/envelope/",
			"42",
			"sentry_key=public",
		},
		{
			"http and a port",
			"http://public@localhost:9000/3",
			"http://localhost:9000/api/3/envelope/",
			"3",
			"sentry_key=public",
		},
		{
			"surrounding whitespace",
			"  " + placeholderDSN + "\n",
			"https://o0.ingest.sentry.io/api/0/envelope/",
			"0",
			"sentry_key=examplePublicKey",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn, err := ParseDSN(tt.raw)
			if err != nil {
				t.Fatalf("ParseDSN: %v", err)
			}
			if got := dsn.EnvelopeURL(); got != tt.wantURL {
				t.Errorf("EnvelopeURL = %q, want %q", got, tt.wantURL)
			}
			if got := dsn.ProjectID(); got != tt.wantID {
				t.Errorf("ProjectID = %q, want %q", got, tt.wantID)
			}
			if got := dsn.authHeader(); !strings.Contains(got, tt.wantKeyIn) {
				t.Errorf("authHeader = %q, want it to contain %q", got, tt.wantKeyIn)
			}
		})
	}
}

func TestParseDSNRejects(t *testing.T) {
	tests := []struct{ name, raw string }{
		{"empty", ""},
		{"whitespace", "   "},
		{"no key", "https://o0.ingest.sentry.io/0"},
		{"no project id", "https://public@o0.ingest.sentry.io/"},
		{"no host", "https://public@/0"},
		{"wrong scheme", "ftp://public@example.com/1"},
		{"not a url", "://nonsense"},
		{"just a word", "hunter2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseDSN(tt.raw); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// TestParseDSNErrorsNeverLeakTheDSN is the reason this package does not wrap
// url.Parse: its error text quotes the whole input, and the input is a
// credential that must not reach a log line or a terminal.
func TestParseDSNErrorsNeverLeakTheDSN(t *testing.T) {
	secrets := []string{
		"https://SUPERSECRETKEY@o1.ingest.sentry.io/2",
		"https://SUPERSECRETKEY:ALSOSECRET@example.com/2",
		"ftp://SUPERSECRETKEY@example.com/2",
		"://SUPERSECRETKEY",
		"SUPERSECRETKEY",
	}

	for _, raw := range secrets {
		_, err := ParseDSN(raw)
		if err == nil {
			continue // valid ones are covered elsewhere
		}
		if strings.Contains(err.Error(), "SUPERSECRETKEY") {
			t.Errorf("error text leaks the DSN: %q", err)
		}
	}
}

// TestStringRedacts checks the loggable form keeps the host and project — the
// useful parts — and drops the key.
func TestStringRedacts(t *testing.T) {
	dsn, err := ParseDSN("https://SUPERSECRETKEY@o1.ingest.sentry.io/2")
	if err != nil {
		t.Fatal(err)
	}

	got := dsn.String()
	if strings.Contains(got, "SUPERSECRETKEY") {
		t.Errorf("String() leaks the key: %q", got)
	}
	for _, want := range []string{"o1.ingest.sentry.io", "/2"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}

// TestEnvelopeURLCarriesNoCredential guards the one URL that gets logged.
func TestEnvelopeURLCarriesNoCredential(t *testing.T) {
	dsn, _ := ParseDSN("https://SUPERSECRETKEY@o1.ingest.sentry.io/2")
	if strings.Contains(dsn.EnvelopeURL(), "SUPERSECRETKEY") {
		t.Errorf("EnvelopeURL leaks the key: %q", dsn.EnvelopeURL())
	}
}

func TestEnvelopeStructure(t *testing.T) {
	dsn, _ := ParseDSN(placeholderDSN)
	at := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	raw, err := envelope(dsn, Event{
		Message: "connection pool exhausted",
		Level:   "fatal",
		Kind:    "DemoError",
		Env:     "demo",
	}, "0123456789abcdef0123456789abcdef", at)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("envelope has %d lines, want 3 (headers, item headers, payload):\n%s", len(lines), raw)
	}

	var head struct {
		EventID string `json:"event_id"`
		SentAt  string `json:"sent_at"`
		DSN     string `json:"dsn"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &head); err != nil {
		t.Fatalf("envelope header is not JSON: %v", err)
	}
	if head.EventID != "0123456789abcdef0123456789abcdef" {
		t.Errorf("event_id = %q", head.EventID)
	}
	if head.DSN != "" {
		t.Errorf("envelope header carries the DSN (%q); the auth header is enough", head.DSN)
	}

	var itemHead struct {
		Type   string `json:"type"`
		Length int    `json:"length"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &itemHead); err != nil {
		t.Fatalf("item header is not JSON: %v", err)
	}
	if itemHead.Type != "event" {
		t.Errorf("item type = %q, want event", itemHead.Type)
	}
	// A wrong length makes the ingest read the wrong number of bytes.
	if itemHead.Length != len(lines[2]) {
		t.Errorf("item length = %d, payload is %d bytes", itemHead.Length, len(lines[2]))
	}

	var payload struct {
		Level     string `json:"level"`
		Platform  string `json:"platform"`
		Env       string `json:"environment"`
		Exception struct {
			Values []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"values"`
		} `json:"exception"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload.Level != "fatal" {
		t.Errorf("level = %q, want fatal", payload.Level)
	}
	if len(payload.Exception.Values) != 1 {
		t.Fatalf("want one exception value, got %d", len(payload.Exception.Values))
	}
	if got := payload.Exception.Values[0].Type; got != "DemoError" {
		t.Errorf("exception type = %q", got)
	}
	if got := payload.Exception.Values[0].Value; got != "connection pool exhausted" {
		t.Errorf("exception value = %q", got)
	}
}

// TestEnvelopeFingerprint covers the difference between a demo that works
// twice and one that works once: with Unique set, each send must carry its own
// fingerprint so Sentry files a new issue rather than grouping.
func TestEnvelopeFingerprint(t *testing.T) {
	dsn, _ := ParseDSN(placeholderDSN)

	grouped, err := envelope(dsn, Event{Message: "boom"}, "id-one", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(grouped), "fingerprint") {
		t.Error("a non-unique event should carry no fingerprint and group normally")
	}

	first, err := envelope(dsn, Event{Message: "boom", Unique: true}, "id-one", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	second, err := envelope(dsn, Event{Message: "boom", Unique: true}, "id-two", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	var a, b struct {
		Fingerprint []string `json:"fingerprint"`
	}
	json.Unmarshal(payloadLine(t, first), &a)
	json.Unmarshal(payloadLine(t, second), &b)

	if len(a.Fingerprint) != 1 || len(b.Fingerprint) != 1 {
		t.Fatalf("want one fingerprint each, got %v and %v", a.Fingerprint, b.Fingerprint)
	}
	if a.Fingerprint[0] == b.Fingerprint[0] {
		t.Errorf("both sends fingerprinted as %q; identical messages would group into one issue",
			a.Fingerprint[0])
	}
}

// payloadLine returns the item payload — the third line of an envelope.
func payloadLine(t *testing.T, raw []byte) []byte {
	t.Helper()

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("envelope has %d lines, want 3", len(lines))
	}
	return []byte(lines[2])
}

func TestEnvelopeDefaults(t *testing.T) {
	dsn, _ := ParseDSN(placeholderDSN)
	raw, err := envelope(dsn, Event{Message: "boom"}, "id", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	body := string(raw)
	if !strings.Contains(body, `"level":"error"`) {
		t.Error("an unset level should default to error")
	}
	if !strings.Contains(body, `"type":"DemoError"`) {
		t.Error("an unset kind should default to DemoError")
	}
}

func TestTitle(t *testing.T) {
	tests := []struct {
		ev   Event
		want string
	}{
		{Event{Kind: "DemoError", Message: "pool exhausted"}, "DemoError: pool exhausted"},
		{Event{Message: "pool exhausted"}, "pool exhausted"},
	}

	for _, tt := range tests {
		if got := tt.ev.Title(); got != tt.want {
			t.Errorf("Title() = %q, want %q", got, tt.want)
		}
	}
}

func TestSend(t *testing.T) {
	var (
		gotPath, gotAuth, gotType, gotMethod string
		gotBody                              []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("X-Sentry-Auth")
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"server-side-id"}`))
	}))
	defer srv.Close()

	dsn, err := ParseDSN(strings.Replace(srv.URL, "http://", "http://testkey@", 1) + "/7")
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}

	id, err := Send(context.Background(), srv.Client(), dsn, Event{
		Message: "pool exhausted", Level: "fatal", Kind: "DemoError",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(id) != 32 {
		t.Errorf("event id %q is %d chars, want 32 hex", id, len(id))
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api/7/envelope/" {
		t.Errorf("path = %q, want /api/7/envelope/", gotPath)
	}
	if gotType != envelopeContentType {
		t.Errorf("Content-Type = %q, want %q", gotType, envelopeContentType)
	}
	for _, want := range []string{"sentry_version=7", "sentry_key=testkey", "sentry_client=" + client} {
		if !strings.Contains(gotAuth, want) {
			t.Errorf("X-Sentry-Auth = %q, want it to contain %q", gotAuth, want)
		}
	}
	// The id we return must be the one we actually sent.
	if !strings.Contains(string(gotBody), id) {
		t.Errorf("returned id %q is not the one in the envelope", id)
	}
}

func TestSendRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sentry-Error", "invalid api key")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	dsn, _ := ParseDSN(strings.Replace(srv.URL, "http://", "http://testkey@", 1) + "/7")

	_, err := Send(context.Background(), srv.Client(), dsn, Event{Message: "boom"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error = %q, want Sentry's reason in it", err)
	}
}

func TestSendRespectsContext(t *testing.T) {
	// The handler stalls until the test releases it. A handler that waited
	// only on the request context would hang Close, because the server does
	// not notice the client giving up until it tries to write.
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release) // runs first, so Close is not left waiting

	dsn, _ := ParseDSN(strings.Replace(srv.URL, "http://", "http://testkey@", 1) + "/7")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := Send(ctx, srv.Client(), dsn, Event{Message: "boom"}); err == nil {
		t.Fatal("expected a timeout error")
	}
}

// TestSendErrorsKeepTheKeyOut: a failing send is exactly when someone pastes
// the output into a chat window.
func TestSendErrorsKeepTheKeyOut(t *testing.T) {
	dsn, _ := ParseDSN("https://SUPERSECRETKEY@127.0.0.1:1/9")

	_, err := Send(context.Background(), &http.Client{Timeout: 100 * time.Millisecond}, dsn, Event{Message: "boom"})
	if err == nil {
		t.Skip("expected the connection to fail")
	}
	if strings.Contains(err.Error(), "SUPERSECRETKEY") {
		t.Errorf("error leaks the key: %q", err)
	}
}
