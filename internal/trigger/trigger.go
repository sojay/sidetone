// Package trigger sends a real error event to Sentry.
//
// It exists to close the demo loop. Without it the demo starts by posting a
// payload to our own /test endpoint, which proves nothing about Sentry; with
// it the demo starts where a real incident starts — something breaks, Sentry
// records it, an alert rule fires, and the webhook comes back to be keyed.
//
// # About the DSN
//
// A DSN is write-only: it authorises submitting events to one project and
// grants no read access, which is why it cannot be used to receive alerts.
// Even so it is a credential, and this package treats it as one: it is never
// logged, never included in an error message, and never written into the
// event body. Note in particular that url.Parse puts its whole input into its
// error text, so parse failures here return their own messages rather than
// wrapping it.
package trigger

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	envelopeContentType = "application/x-sentry-envelope"

	// client identifies us to Sentry's ingest, and shows up on the event.
	client = "sidetone/1.0"

	// protocolVersion is the current Sentry ingest protocol.
	protocolVersion = "7"
)

// A DSN is a parsed Sentry ingest credential. The public key is unexported so
// it cannot be printed by accident.
type DSN struct {
	scheme    string
	host      string
	prefix    string // path in front of /api, used by self-hosted installs
	projectID string
	publicKey string
}

// ParseDSN reads a DSN of the form
//
//	{scheme}://{public_key}@{host}/{project_id}
//
// The deprecated `:{secret_key}` form is accepted and the secret ignored.
//
// No error returned here contains any part of raw.
func ParseDSN(raw string) (DSN, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DSN{}, errors.New("DSN is empty")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return DSN{}, errors.New("DSN is not a valid URL")
	}

	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return DSN{}, errors.New("DSN scheme must be http or https")
	case u.Host == "":
		return DSN{}, errors.New("DSN has no host")
	case u.User == nil || u.User.Username() == "":
		return DSN{}, errors.New("DSN has no public key")
	}

	// The project id is the last path segment. Anything before it is a prefix
	// that self-hosted installs put in front of the API.
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	projectID := segments[len(segments)-1]
	if projectID == "" {
		return DSN{}, errors.New("DSN has no project id")
	}

	var prefix string
	if len(segments) > 1 {
		prefix = "/" + strings.Join(segments[:len(segments)-1], "/")
	}

	return DSN{
		scheme:    u.Scheme,
		host:      u.Host,
		prefix:    prefix,
		projectID: projectID,
		publicKey: u.User.Username(),
	}, nil
}

// FromEnv reads SENTRY_DSN.
func FromEnv() (DSN, error) {
	raw := os.Getenv("SENTRY_DSN")
	if raw == "" {
		return DSN{}, errors.New("SENTRY_DSN is not set")
	}
	return ParseDSN(raw)
}

// EnvelopeURL is where events are submitted. It carries no credential.
func (d DSN) EnvelopeURL() string {
	return d.scheme + "://" + d.host + d.prefix + "/api/" + d.projectID + "/envelope/"
}

// ProjectID is the numeric project the DSN submits to.
func (d DSN) ProjectID() string { return d.projectID }

// String is safe to log: the public key is replaced.
func (d DSN) String() string {
	return d.scheme + "://[key]@" + d.host + d.prefix + "/" + d.projectID
}

// authHeader is the ingest credential in header form.
func (d DSN) authHeader() string {
	return fmt.Sprintf("Sentry sentry_version=%s, sentry_client=%s, sentry_key=%s",
		protocolVersion, client, d.publicKey)
}

// An Event is the error we are asking Sentry to record.
type Event struct {
	Message string // what went wrong
	Level   string // fatal, error or warning
	Kind    string // exception type; combines with Message to form the title
	Env     string

	// Unique gives the event its own fingerprint so Sentry files it as a brand
	// new issue instead of grouping it with identical earlier ones.
	//
	// This matters more than it sounds. The obvious alert rule is "a new issue
	// is created", and Sentry groups repeated identical errors into one issue —
	// so without a fresh fingerprint the second demo run of the same message
	// creates no new issue, fires no rule, and plays nothing.
	Unique bool
}

// Title is what Sentry will call this issue, and therefore what sidetone will
// key when the alert comes back. Worth showing before sending.
func (e Event) Title() string {
	if e.Kind == "" {
		return e.Message
	}
	return e.Kind + ": " + e.Message
}

// Send submits the event and returns the id Sentry recorded it under.
func Send(ctx context.Context, httpClient *http.Client, dsn DSN, ev Event) (string, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	id, err := eventID()
	if err != nil {
		return "", err
	}

	body, err := envelope(dsn, ev, id, time.Now().UTC())
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dsn.EnvelopeURL(), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", envelopeContentType)
	req.Header.Set("X-Sentry-Auth", dsn.authHeader())
	req.Header.Set("User-Agent", client)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("submit to %s: %w", dsn, err)
	}
	defer resp.Body.Close()

	// Drain so the connection can be reused, and so a rejection reason is
	// available below.
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode/100 != 2 {
		// Sentry explains rejections in this header; it holds no credential.
		if reason := resp.Header.Get("X-Sentry-Error"); reason != "" {
			return "", fmt.Errorf("sentry rejected the event: %s (%s)", reason, resp.Status)
		}
		return "", fmt.Errorf("sentry rejected the event: %s", resp.Status)
	}

	return id, nil
}

// envelope builds the newline-delimited envelope Sentry's ingest expects:
// envelope headers, then item headers, then the item payload.
func envelope(dsn DSN, ev Event, id string, sentAt time.Time) ([]byte, error) {
	level := strings.ToLower(strings.TrimSpace(ev.Level))
	if level == "" {
		level = "error"
	}
	kind := ev.Kind
	if kind == "" {
		kind = "DemoError"
	}

	item := map[string]any{
		"event_id":  id,
		"timestamp": sentAt.Format(time.RFC3339),
		"platform":  "other",
		"level":     level,
		"logger":    "sidetone",
		"exception": map[string]any{
			"values": []map[string]any{{
				"type":  kind,
				"value": ev.Message,
			}},
		},
		"tags": map[string]string{"sidetone": "demo"},
	}
	if ev.Env != "" {
		item["environment"] = ev.Env
	}
	if ev.Unique {
		// The event id is unique per send, so using it as the fingerprint
		// guarantees a new issue every time.
		item["fingerprint"] = []string{"sidetone-" + id}
	}

	payload, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("encode event: %w", err)
	}

	// The envelope header may carry the DSN, but we leave it out: the auth
	// header already identifies us, and there is no reason to copy a
	// credential into a request body.
	head, err := json.Marshal(map[string]any{
		"event_id": id,
		"sent_at":  sentAt.Format(time.RFC3339),
	})
	if err != nil {
		return nil, fmt.Errorf("encode envelope header: %w", err)
	}

	itemHead, err := json.Marshal(map[string]any{
		"type":         "event",
		"content_type": "application/json",
		"length":       len(payload),
	})
	if err != nil {
		return nil, fmt.Errorf("encode item header: %w", err)
	}

	var b bytes.Buffer
	b.Write(head)
	b.WriteByte('\n')
	b.Write(itemHead)
	b.WriteByte('\n')
	b.Write(payload)
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// eventID is 32 hex characters, no dashes, as the protocol requires.
func eventID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate event id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
