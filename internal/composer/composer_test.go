package composer

import (
	"strings"
	"testing"

	"github.com/sojay/sidetone/internal/cw"
)

func TestCompose(t *testing.T) {
	tests := []struct {
		name  string
		alert Alert
		want  string
	}{
		{
			"typical error",
			Alert{Project: "checkout-api", Level: "error", Title: "TypeError: cannot read x"},
			"VVV DE = CHECKOUT-API ERROR = TYPEERROR: CANNOT READ X = <AR>",
		},
		{
			"fatal",
			Alert{Project: "payments", Level: "fatal", Title: "DB connection lost"},
			"VVV DE = PAYMENTS FATAL = DB CONNECTION LOST = <AR>",
		},
		{
			"warning",
			Alert{Project: "web", Level: "warning", Title: "Slow query"},
			"VVV DE = WEB WARNING = SLOW QUERY = <AR>",
		},
		{
			"already uppercase",
			Alert{Project: "WEB", Level: "ERROR", Title: "BOOM"},
			"VVV DE = WEB ERROR = BOOM = <AR>",
		},
		{
			"level Sentry sends that we have no profile for is still announced",
			Alert{Project: "web", Level: "info", Title: "Deploy finished"},
			"VVV DE = WEB INFO = DEPLOY FINISHED = <AR>",
		},
		{
			"culprit stands in for a missing title",
			Alert{Project: "web", Level: "error", Culprit: "app/handlers.checkout"},
			"VVV DE = WEB ERROR = APP/HANDLERS.CHECKOUT = <AR>",
		},
		{
			"title wins over culprit",
			Alert{Project: "web", Level: "error", Title: "Boom", Culprit: "app/handlers.go"},
			"VVV DE = WEB ERROR = BOOM = <AR>",
		},
		{
			"empty alert falls back all round",
			Alert{},
			"VVV DE = UNKNOWN ERROR = NO TITLE = <AR>",
		},
		{
			"fields that sanitize to nothing use the defaults",
			Alert{Project: "🔥", Level: "  ", Title: "→→→"},
			"VVV DE = UNKNOWN ERROR = NO TITLE = <AR>",
		},
		{
			"unmappable characters are stripped from the title",
			Alert{Project: "web", Level: "error", Title: "Café crash 🔥 now"},
			"VVV DE = WEB ERROR = CAF CRASH NOW = <AR>",
		},
		{
			"newlines and runs of spaces collapse",
			Alert{Project: "web", Level: "error", Title: "line one\n\nline  two"},
			"VVV DE = WEB ERROR = LINE ONE LINE TWO = <AR>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Compose(tt.alert); got != tt.want {
				t.Errorf("Compose() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestComposeTruncation(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{
			"exactly at the limit is kept whole",
			"0123456789 0123456789 0123456789 012345", // 39 chars
			"0123456789 0123456789 0123456789 012345",
		},
		{
			"cuts at a word boundary",
			"the quick brown fox jumps over the lazy dog and keeps going",
			"THE QUICK BROWN FOX JUMPS OVER THE LAZY",
		},
		{
			// The 40th character is a space, so the cut needs no backtracking.
			"cut lands exactly on a space",
			"aaaaaaaaaa bbbbbbbbbb cccccccccc ddddddd eeeeee",
			"AAAAAAAAAA BBBBBBBBBB CCCCCCCCCC DDDDDDD",
		},
		{
			"a single long word is cut mid-word",
			"AAAAAAAAAABBBBBBBBBBCCCCCCCCCCDDDDDDDDDDEEEEEEEEEE",
			"AAAAAAAAAABBBBBBBBBBCCCCCCCCCCDDDDDDDDDD",
		},
		{
			"stack-frame style culprit",
			"src/components/checkout/PaymentForm.tsx in handleSubmit",
			"SRC/COMPONENTS/CHECKOUT/PAYMENTFORM.TSX",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := title(t, Compose(Alert{Project: "P", Level: "error", Title: tt.title}))
			if got != tt.want {
				t.Errorf("title = %q, want %q", got, tt.want)
			}
			if len(got) > MaxTitleChars {
				t.Errorf("title is %d chars, over the %d limit", len(got), MaxTitleChars)
			}
		})
	}
}

// TestComposeTitleNeverExceedsLimit is the property version of the table
// above: whatever comes in, the keyed title stays inside the limit.
func TestComposeTitleNeverExceedsLimit(t *testing.T) {
	inputs := []string{
		"", "a", strings.Repeat("x", 500),
		strings.Repeat("word ", 100),
		strings.Repeat("🔥", 100),
		"a " + strings.Repeat("b", 200),
		strings.Repeat("ab ", 60),
	}

	for _, in := range inputs {
		got := title(t, Compose(Alert{Project: "P", Level: "error", Title: in}))
		if len(got) > MaxTitleChars {
			t.Errorf("title for %d-char input is %d chars: %q", len(in), len(got), got)
		}
		if got == "" {
			t.Errorf("title for %q is empty; want a fallback", in)
		}
	}
}

// TestComposeCannotInjectProsign is the reason clean() strips angle brackets:
// a Sentry title is attacker-influenced text, and <SK> means "end of contact".
func TestComposeCannotInjectProsign(t *testing.T) {
	titles := []string{
		"unhandled <SK> in worker",
		"<AR>",
		"Foo<Bar> generic failed",
		"error in <anonymous>",
		"a < b comparison",
	}

	for _, tt := range titles {
		t.Run(tt, func(t *testing.T) {
			msg := Compose(Alert{Project: "web", Level: "error", Title: tt})

			// Exactly one prosign may appear, and it must be the terminator.
			if n := strings.Count(msg, "<"); n != 1 {
				t.Errorf("message has %d prosigns, want 1: %q", n, msg)
			}
			if !strings.HasSuffix(msg, " "+end) {
				t.Errorf("message does not end with %s: %q", end, msg)
			}
			if strings.Contains(title(t, msg), "<") {
				t.Errorf("title still contains a bracket: %q", msg)
			}
		})
	}
}

// TestComposeIsKeyable is the contract with the cw package: whatever Compose
// returns must survive sanitizing untouched and encode to real audio.
func TestComposeIsKeyable(t *testing.T) {
	alerts := []Alert{
		{Project: "checkout-api", Level: "fatal", Title: "DB connection pool exhausted"},
		{Project: "web", Level: "warning", Title: "Café 🔥 <SK> \\weird\\ ~input~"},
		{},
		{Project: strings.Repeat("z", 100), Level: "error", Title: strings.Repeat("y ", 100)},
	}

	for _, a := range alerts {
		msg := Compose(a)

		if got := cw.Sanitize(msg); got != msg {
			t.Errorf("Compose output is not stable under Sanitize:\n  got  %q\n  want %q", got, msg)
		}

		elems := cw.Encode(msg)
		if len(elems) == 0 {
			t.Fatalf("Compose(%+v) = %q encodes to nothing", a, msg)
		}
		if !elems[0].KeyDown || !elems[len(elems)-1].KeyDown {
			t.Errorf("encoded message starts or ends with silence: %q", msg)
		}
	}
}

// TestComposeEndsWithProsign checks the terminator is the run-together <AR>
// (13 units) and not the two letters A and R (15 units).
func TestComposeEndsWithProsign(t *testing.T) {
	msg := Compose(Alert{Project: "web", Level: "error", Title: "boom"})

	withProsign := cw.TotalUnits(cw.Encode(msg))
	asLetters := cw.TotalUnits(cw.Encode(strings.TrimSuffix(msg, end) + "AR"))

	if withProsign >= asLetters {
		t.Errorf("message is %d units, letters version is %d; the prosign should be shorter",
			withProsign, asLetters)
	}
}

// TestComposeShape pins the frame itself, so a change to the preamble or the
// separators has to be deliberate.
func TestComposeShape(t *testing.T) {
	msg := Compose(Alert{Project: "web", Level: "error", Title: "boom"})

	if !strings.HasPrefix(msg, preamble+" "+sep+" ") {
		t.Errorf("missing preamble: %q", msg)
	}
	if n := strings.Count(msg, " "+sep+" "); n != 3 {
		t.Errorf("found %d section breaks, want 3: %q", n, msg)
	}
}

// TestComposeLengthIsBounded is the assertion that actually matters for
// airtime: sending time is linear in characters, so a bound on message length
// is a bound on how long one alert can hold the queue.
func TestComposeLengthIsBounded(t *testing.T) {
	worst := Alert{
		Project: strings.Repeat("z", 500),
		Level:   strings.Repeat("y", 500),
		Title:   strings.Repeat("x", 500),
	}

	for _, a := range []Alert{worst, {}, {Project: "web", Level: "error", Title: "boom"}} {
		msg := Compose(a)
		if len(msg) > MaxMessageChars {
			t.Errorf("message is %d chars, over the %d bound: %q", len(msg), MaxMessageChars, msg)
		}
	}

	// And the bound must be reachable, or it is not really a bound on anything.
	if got := len(Compose(worst)); got != MaxMessageChars {
		t.Errorf("worst case is %d chars, want the stated bound of %d", got, MaxMessageChars)
	}
}

// TestComposeDurations reports how long a realistic alert actually takes at
// each severity. The ceiling here is a runaway canary, not a target — see the
// airtime note in the package docs.
func TestComposeDurations(t *testing.T) {
	msg := Compose(Alert{
		Project: "checkout-api",
		Level:   "fatal",
		Title:   "Timeout connecting to primary database",
	})

	for _, level := range []string{"fatal", "error", "warning"} {
		p := cw.ProfileFor(level)
		d := cw.Duration(cw.Encode(msg), p.WPM)

		t.Logf("%-8s %2.0f WPM: %6v for %d chars", level, p.WPM, d.Round(1e8), len(msg))

		// The slowest possible case: the longest legal message at 13 WPM.
		if d.Seconds() > 150 {
			t.Errorf("%s takes %v — something has gone wrong with the timing", level, d)
		}
	}
}

// title pulls the title field back out of a composed message: it is the third
// of the four sections.
func title(t *testing.T, msg string) string {
	t.Helper()

	parts := strings.Split(msg, " "+sep+" ")
	if len(parts) != 4 {
		t.Fatalf("message has %d sections, want 4: %q", len(parts), msg)
	}
	return parts[2]
}
