// Package composer turns a parsed Sentry alert into the message that gets
// keyed on the air.
//
// The format is fixed:
//
//	VVV DE SENTRY = {PROJECT} {LEVEL} = {SHORT TITLE} = <AR>
//
// VVV is the traditional test/attention call, DE means "from", = is the BT
// section break, and <AR> is the end-of-message prosign. Keeping the frame
// constant is what makes the variable parts easy to copy by ear: after a few
// alerts your ear skips the preamble and waits for the project name.
//
// # Airtime
//
// One alert occupies the speaker for its whole transmission, and sending time
// is linear in characters — so message length is a queue-latency decision, not
// a cosmetic one. Every variable field is therefore capped (see MaxTitleChars
// and friends), which bounds a single alert at MaxMessageChars.
//
// Note that the caps bound the worst case but do not make a warning short: at
// the warning speed of 13 WPM a full-length message still runs about a minute.
// If that turns out to be too slow in rehearsal, the lever is the format or the
// WPM table, both of which live in CLAUDE.md.
package composer

import (
	"strings"

	"github.com/sojay/sidetone/internal/cw"
)

// Every variable part is length-capped, because sending time is linear in
// characters: at 20 WPM roughly five characters take a second, and at the
// warning speed of 13 WPM they take nearly two. An unbounded field would mean
// an unbounded transmission, during which every other alert sits in the queue.
const (
	// MaxTitleChars is roughly where a title stops being copyable by ear.
	MaxTitleChars = 40

	// MaxProjectChars fits real Sentry slugs ("checkout-api", "web-frontend")
	// while capping a pathological one.
	MaxProjectChars = 20

	// MaxLevelChars fits the longest level Sentry sends ("warning").
	MaxLevelChars = 12
)

// MaxMessageChars is the resulting worst case. It is derived from the frame
// rather than written out, so editing the preamble cannot leave a stale bound
// behind: the message is eight space-joined parts, three of which are the
// section break.
const MaxMessageChars = len(preamble) + 3*len(sep) + len(end) + 7 + // frame and its spaces
	MaxProjectChars + MaxLevelChars + MaxTitleChars

// Defaults used when Sentry gives us nothing usable for a field. An alert with
// a missing project is still worth hearing, so we never drop one.
const (
	defaultProject = "UNKNOWN"
	defaultLevel   = "ERROR"
	defaultTitle   = "NO TITLE"
)

const (
	// VVV DE is deliberately short. The preamble is pure overhead paid on
	// every single alert, and at the warning speed it is seconds of airtime;
	// the project name that follows the first break identifies the sender.
	preamble = "VVV DE"
	sep      = "=" // BT, the section break
	end      = "<AR>"
)

// An Alert is the subset of a Sentry payload that we actually key. The webhook
// package is responsible for digging these out of the JSON.
type Alert struct {
	Project string
	Level   string
	Title   string
	Culprit string // stands in for a missing title
}

// Compose renders a as a CW message. The result is already sanitized, so it
// can be handed straight to cw.Encode.
func Compose(a Alert) string {
	project := truncate(field(a.Project, defaultProject), MaxProjectChars)
	level := truncate(field(a.Level, defaultLevel), MaxLevelChars)

	// Culprit is usually something like "app/handlers.checkout" — less
	// readable than a title, but far better than announcing nothing.
	title := truncate(clean(a.Title), MaxTitleChars)
	if title == "" {
		title = truncate(clean(a.Culprit), MaxTitleChars)
	}
	if title == "" {
		title = defaultTitle
	}

	return strings.Join([]string{
		preamble, sep, project, level, sep, title, sep, end,
	}, " ")
}

// field cleans a value, falling back to def when nothing keyable survives.
func field(v, def string) string {
	if c := clean(v); c != "" {
		return c
	}
	return def
}

// clean prepares an untrusted Sentry string for keying.
//
// Angle brackets are removed *before* sanitizing: cw.Sanitize treats <AR> as a
// prosign, and a title like "unhandled <SK> in worker" must not be able to
// inject an end-of-contact signal into the middle of a message.
func clean(s string) string {
	s = strings.NewReplacer("<", " ", ">", " ").Replace(s)
	return cw.Sanitize(s)
}

// truncate shortens s to at most max characters, cutting at a word boundary
// when there is one. s must already be sanitized, which means it is ASCII and
// single-spaced, so byte offsets and character counts agree.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}

	cut := s[:max]
	// If the character we stopped before is a space, the cut is already clean.
	if s[max] == ' ' {
		return cut
	}
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		return cut[:i]
	}
	// A single word longer than the limit: cut it mid-word rather than key a
	// 60-character stack trace fragment.
	return cut
}
