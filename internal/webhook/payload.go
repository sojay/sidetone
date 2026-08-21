package webhook

import (
	"net/url"
	"strings"
	"time"

	"github.com/sojay/sidetone/internal/composer"
)

// Sentry has more than one webhook shape, and we want to key alerts from any of
// them rather than insisting on one.
//
// The legacy webhook plugin posts the interesting fields at the top level, with
// "project" holding the slug. The Integration Platform posts an envelope —
// {"action": ..., "data": {"event": {...}}} — where the event's "project" is a
// numeric id instead.
//
// So every field is looked up in several places and the first usable value
// wins. Fields we do not recognise are ignored, which is what lets this survive
// Sentry adding to the payload.
type sentryPayload struct {
	// "project" is a slug in one payload and a numeric id in another, so it is
	// decoded loosely and only used when it turns out to be a string.
	Project     any    `json:"project"`
	ProjectSlug string `json:"project_slug"`
	ProjectName string `json:"project_name"`
	Level       string `json:"level"`
	Culprit     string `json:"culprit"`
	Message     string `json:"message"`
	Title       string `json:"title"`

	Event *sentryEvent `json:"event"`

	Data *struct {
		// Issue alerts put the event under "event"; the error resource puts the
		// same shape under "error". Both are accepted.
		Event *sentryEvent `json:"event"`
		Error *sentryEvent `json:"error"`
	} `json:"data"`
}

type sentryEvent struct {
	Project     any    `json:"project"`
	ProjectSlug string `json:"project_slug"`
	Level       string `json:"level"`
	Title       string `json:"title"`
	Culprit     string `json:"culprit"`
	Message     string `json:"message"`
	Transaction string `json:"transaction"`

	// URL is the event's API address. On Integration Platform payloads it is
	// the only place the project's name appears at all — "project" there is a
	// numeric id, and there is no project_slug field.
	URL string `json:"url"`

	// Received is when Sentry ingested the event, in epoch seconds, and
	// Timestamp is when it happened. Comparing Received against the clock on
	// arrival measures how long the alert spent inside Sentry — which is the
	// overwhelming majority of the delay between an error and a sound.
	Received  float64 `json:"received"`
	Timestamp float64 `json:"timestamp"`
	Datetime  string  `json:"datetime"`

	Metadata *struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"metadata"`
}

// alert flattens the payload into what the composer needs. A missing field is
// left empty for the composer to fill in: an alert we can only half understand
// is still worth hearing.
func (p sentryPayload) alert() composer.Alert {
	event := p.event()

	return composer.Alert{
		Project: firstNonEmpty(
			p.ProjectSlug,
			asString(p.Project),
			p.ProjectName,
			event.ProjectSlug,
			projectFromURL(event.URL),
			asString(event.Project),
		),
		Level: firstNonEmpty(p.Level, event.Level),
		Title: firstNonEmpty(
			event.Title,
			p.Title,
			event.metadataTitle(),
			event.Message,
			p.Message,
		),
		Culprit: firstNonEmpty(event.Culprit, p.Culprit, event.Transaction),
	}
}

// event returns whichever event the payload carries, or an empty one, so
// callers never have to nil-check.
func (p sentryPayload) event() sentryEvent {
	if p.Data != nil {
		if p.Data.Event != nil {
			return *p.Data.Event
		}
		if p.Data.Error != nil {
			return *p.Data.Error
		}
	}
	if p.Event != nil {
		return *p.Event
	}
	return sentryEvent{}
}

// ingestedAt is when Sentry took delivery of the event. It prefers "received"
// over "timestamp": the first is when Sentry got it, the second is when the
// error happened, and only the first isolates Sentry's own processing time from
// however long the reporting client sat on the event.
//
// The zero time means the payload said nothing about when any of this happened.
func (e sentryEvent) ingestedAt() time.Time {
	switch {
	case e.Received > 0:
		return epoch(e.Received)
	case e.Timestamp > 0:
		return epoch(e.Timestamp)
	case e.Datetime != "":
		if t, err := time.Parse(time.RFC3339, e.Datetime); err == nil {
			return t
		}
	}
	return time.Time{}
}

// epoch converts fractional epoch seconds, as Sentry sends them, to a time.
func epoch(seconds float64) time.Time {
	sec := int64(seconds)
	nsec := int64((seconds - float64(sec)) * float64(time.Second))
	return time.Unix(sec, nsec)
}

// metadataTitle builds a title from the event metadata, which is where the
// useful summary lives when there is no title — typically an exception type and
// its value.
func (e sentryEvent) metadataTitle() string {
	if e.Metadata == nil {
		return ""
	}

	switch {
	case e.Metadata.Type != "" && e.Metadata.Value != "":
		return e.Metadata.Type + ": " + e.Metadata.Value
	case e.Metadata.Type != "":
		return e.Metadata.Type
	default:
		return e.Metadata.Value
	}
}

// projectFromURL digs the project slug out of an event's API address, which
// looks like:
//
//	https://sentry.io/api/0/projects/{organization}/{project}/events/{id}/
//
// This is the only route to a human-readable project name on an Integration
// Platform issue alert, so without it every alert would announce itself as
// UNKNOWN.
func projectFromURL(raw string) string {
	if raw == "" {
		return ""
	}

	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, part := range parts {
		// The slug is the segment after the organization, which is itself the
		// segment after "projects".
		if part == "projects" && i+2 < len(parts) {
			return parts[i+2]
		}
	}
	return ""
}

// asString returns v if it is a string, and "" otherwise. A numeric project id
// is not a name anyone wants to hear keyed out.
func asString(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// firstNonEmpty returns the first value that is not blank.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
