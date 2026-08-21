package cw

import (
	"strings"
	"testing"
	"time"
)

// on and off are shorthand for building expected element sequences.
func on(units int) Element  { return Element{KeyDown: true, Units: units} }
func off(units int) Element { return Element{Units: units} }

func TestEncode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Element
	}{
		{"empty", "", nil},
		{"only unmappable", "%%%", nil},
		{
			// The simplest possible message: one dit, no gaps at all.
			"single dit", "E", []Element{on(1)},
		},
		{
			"single dah", "T", []Element{on(3)},
		},
		{
			// One symbol gap inside the character, and nothing on the ends.
			"letter A", "A", []Element{on(1), off(1), on(3)},
		},
		{
			"letter S", "S", []Element{on(1), off(1), on(1), off(1), on(1)},
		},
		{
			// Between characters the gap is 3 units total, not 1 + 3.
			"two letters", "EE", []Element{on(1), off(3), on(1)},
		},
		{
			// Between words it is 7 units total.
			"two words", "E E", []Element{on(1), off(7), on(1)},
		},
		{
			"lowercase is keyed the same", "e", []Element{on(1)},
		},
		{
			"unmappable characters leave no stranded gap", "E%E", []Element{on(1), off(3), on(1)},
		},
		{
			"BT separator", "=", []Element{
				on(3), off(1), on(1), off(1), on(1), off(1), on(1), off(1), on(3),
			},
		},
		{
			// The whole point of the prosign: .-.-. run together, with only
			// one-unit symbol gaps and no three-unit letter gap inside it.
			"AR prosign", "<AR>", []Element{
				on(1), off(1), on(3), off(1), on(1), off(1), on(3), off(1), on(1),
			},
		},
		{
			// The same letters unbracketed are two characters, so a letter gap
			// separates them.
			"AR as two letters", "AR", []Element{
				on(1), off(1), on(3), // A
				off(3),
				on(1), off(1), on(3), off(1), on(1), // R
			},
		},
		{
			"prosign takes a letter gap from its neighbour", "K<AR>", []Element{
				on(3), off(1), on(1), off(1), on(3), // K
				off(3),
				on(1), off(1), on(3), off(1), on(1), off(1), on(3), off(1), on(1), // <AR>
			},
		},
		{
			"unknown prosign is keyed as letters", "<XQ>", Encode("XQ"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Encode(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("Encode(%q) = %v (%d elements), want %v (%d)",
					tt.in, got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Encode(%q)[%d] = %+v, want %+v", tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestEncodeUnitCounts(t *testing.T) {
	tests := []struct {
		in    string
		units int
	}{
		{"E", 1},           // one dit
		{"T", 3},           // one dah
		{"A", 1 + 1 + 3},   // dit, gap, dah
		{"EE", 1 + 3 + 1},  // letter gap is 3
		{"E E", 1 + 7 + 1}, // word gap is 7
		{"EEE", 1 + 3 + 1 + 3 + 1},
		{"PARIS", 43},       // the standard word, without its trailing space
		{"PARIS PARIS", 93}, // 43 + 7 + 43
		{"<AR>", 13},        // .-.-. is 9 keyed units plus 4 symbol gaps
		{"AR", 15},          // two letters: 12 keyed units, 2 symbol gaps, 1 letter gap
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := TotalUnits(Encode(tt.in)); got != tt.units {
				t.Errorf("TotalUnits(Encode(%q)) = %d, want %d", tt.in, got, tt.units)
			}
		})
	}
}

// TestParisStandard pins the definition of WPM itself: the word "PARIS "
// — the five characters plus the following word gap — is exactly 50 units, so
// at W words per minute a message of 50 units must take 60/W seconds.
func TestParisStandard(t *testing.T) {
	paris := TotalUnits(Encode("PARIS"))
	if want := 43; paris != want {
		t.Fatalf(`TotalUnits(Encode("PARIS")) = %d, want %d`, paris, want)
	}
	if got, want := paris+WordGapUnits, 50; got != want {
		t.Fatalf(`"PARIS " = %d units, want %d`, got, want)
	}

	for _, wpm := range []float64{5, 12, 20, 28, 60} {
		want := time.Duration(60 / wpm * float64(time.Second))
		got := 50 * Unit(wpm)
		if diff := got - want; diff < -time.Microsecond || diff > time.Microsecond {
			t.Errorf("at %g WPM, 50 units = %v, want %v", wpm, got, want)
		}
	}
}

func TestUnit(t *testing.T) {
	tests := []struct {
		wpm  float64
		want time.Duration
	}{
		{5, 240 * time.Millisecond},
		{12, 100 * time.Millisecond},
		{20, 60 * time.Millisecond},
		{28, 42857142 * time.Nanosecond}, // 1.2/28 s
		{60, 20 * time.Millisecond},
	}

	for _, tt := range tests {
		got := Unit(tt.wpm)
		if diff := got - tt.want; diff < -time.Microsecond || diff > time.Microsecond {
			t.Errorf("Unit(%g) = %v, want %v", tt.wpm, got, tt.want)
		}
	}
}

func TestUnitNonPositiveWPM(t *testing.T) {
	for _, wpm := range []float64{0, -1} {
		if got := Unit(wpm); got != 0 {
			t.Errorf("Unit(%g) = %v, want 0", wpm, got)
		}
	}
}

func TestDuration(t *testing.T) {
	// "PARIS" is 43 units; at 20 WPM a unit is 60ms.
	got := Duration(Encode("PARIS"), 20)
	want := 43 * 60 * time.Millisecond
	if diff := got - want; diff < -time.Millisecond || diff > time.Millisecond {
		t.Errorf("Duration = %v, want %v", got, want)
	}

	if got := Duration(nil, 20); got != 0 {
		t.Errorf("Duration(nil) = %v, want 0", got)
	}
}

// TestEncodeStructure checks invariants that hold for any input: silence never
// sits on either end, two silences never abut (which would mean a gap was
// emitted twice), and every element has a positive length.
func TestEncodeStructure(t *testing.T) {
	inputs := []string{
		"E", "A", "PARIS", "E E", "VVV DE SENTRY = <AR>",
		"VVV DE SENTRY = CHECKOUT-API FATAL = DB TIMEOUT = <AR>",
		"E%E", "  A  B  ",
		"<AR>", "<BT> <AR>", "K<AR>", "<XQ>", "<AR", "A <> B",
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			elems := Encode(in)
			if len(elems) == 0 {
				t.Fatalf("Encode(%q) returned nothing", in)
			}
			if !elems[0].KeyDown {
				t.Errorf("starts with silence: %+v", elems[0])
			}
			if !elems[len(elems)-1].KeyDown {
				t.Errorf("ends with silence: %+v", elems[len(elems)-1])
			}
			for i, e := range elems {
				if e.Units <= 0 {
					t.Errorf("element %d has %d units", i, e.Units)
				}
				if i > 0 && !e.KeyDown && !elems[i-1].KeyDown {
					t.Errorf("adjacent silences at %d: %+v then %+v", i, elems[i-1], e)
				}
			}
		})
	}
}

// TestEncodeProsignEqualsPunctuation checks the two spellings of the same
// signal key identically, so the composer can use whichever reads better.
func TestEncodeProsignEqualsPunctuation(t *testing.T) {
	pairs := []struct{ prosign, punct string }{
		{"<AR>", "+"},
		{"<BT>", "="},
		{"<KN>", "("},
		{"<AS>", "&"},
	}

	for _, p := range pairs {
		t.Run(p.prosign, func(t *testing.T) {
			got, want := Encode(p.prosign), Encode(p.punct)
			if len(got) != len(want) {
				t.Fatalf("Encode(%q) has %d elements, Encode(%q) has %d",
					p.prosign, len(got), p.punct, len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("element %d: %+v vs %+v", i, got[i], want[i])
				}
			}
		})
	}
}

// TestEncodeProsignIsOneCharacter is the regression guard for the bug this
// feature exists to fix: <AR> must be shorter than "AR", because it drops the
// three-unit letter gap between them.
func TestEncodeProsignIsOneCharacter(t *testing.T) {
	prosign := TotalUnits(Encode("<AR>"))
	letters := TotalUnits(Encode("AR"))

	if prosign >= letters {
		t.Errorf("<AR> = %d units, AR = %d; the prosign must be shorter", prosign, letters)
	}
	if diff := letters - prosign; diff != LetterGapUnits-SymbolGapUnits {
		t.Errorf("difference is %d units, want %d (a letter gap replaced by a symbol gap)",
			diff, LetterGapUnits-SymbolGapUnits)
	}

	// And no gap inside a prosign may be longer than a symbol gap.
	for i, e := range Encode("<AR>") {
		if !e.KeyDown && e.Units != SymbolGapUnits {
			t.Errorf("gap at %d is %d units, want %d", i, e.Units, SymbolGapUnits)
		}
	}
}

func TestSymbols(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Symbol
	}{
		{"empty", "", nil},
		{"one letter", "E", []Symbol{{Text: "E", Code: ".", Start: 0, Units: 1}}},
		{
			// A follows E after a three-unit letter gap: 1 + 3 = 4.
			"letter gap shows up in the offsets", "EA", []Symbol{
				{Text: "E", Code: ".", Start: 0, Units: 1},
				{Text: "A", Code: ".-", Start: 4, Units: 5},
			},
		},
		{
			"word gap is its own symbol", "E E", []Symbol{
				{Text: "E", Code: ".", Start: 0, Units: 1},
				{Text: " ", Code: "", Start: 1, Units: 7},
				{Text: "E", Code: ".", Start: 8, Units: 1},
			},
		},
		{
			"prosign is one symbol", "<AR>", []Symbol{
				{Text: "<AR>", Code: ".-.-.", Start: 0, Units: 13},
			},
		},
		{
			// K is dah-dit-dah: 3 + 1 + 1 + 1 + 3 = 9 units, then a 3-unit
			// letter gap before the prosign starts.
			"prosign after a letter", "K<AR>", []Symbol{
				{Text: "K", Code: "-.-", Start: 0, Units: 9},
				{Text: "<AR>", Code: ".-.-.", Start: 12, Units: 13},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Symbols(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("Symbols(%q) = %+v (%d), want %+v (%d)",
					tt.in, got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("symbol %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestSymbolsAgreeWithEncode is the important one: a display highlights
// characters from Symbols while the speaker plays Elements from Encode, so if
// these two ever disagree the screen drifts out of sync with the sound.
func TestSymbolsAgreeWithEncode(t *testing.T) {
	inputs := []string{
		"E", "A", "PARIS", "E E", "<AR>", "K<AR>",
		"VVV DE = CHECKOUT-API FATAL = DB POOL EXHAUSTED = <AR>",
		"SOS SOS", "0123456789",
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			syms := Symbols(in)
			if len(syms) == 0 {
				t.Fatal("no symbols")
			}

			// The last symbol must end exactly when the message does.
			last := syms[len(syms)-1]
			if got, want := last.Start+last.Units, TotalUnits(Encode(in)); got != want {
				t.Errorf("symbols end at unit %d, message is %d units long", got, want)
			}

			// Rebuilding the text from the symbols must give back the message.
			var rebuilt strings.Builder
			for _, s := range syms {
				rebuilt.WriteString(s.Text)
			}
			if got, want := rebuilt.String(), Sanitize(in); got != want {
				t.Errorf("symbols spell %q, want %q", got, want)
			}

			// Every symbol must sit inside the message and after the one before.
			prevEnd := 0
			for i, s := range syms {
				if s.Start < prevEnd {
					t.Errorf("symbol %d (%q) starts at %d, before the previous ended at %d",
						i, s.Text, s.Start, prevEnd)
				}
				if s.Units <= 0 {
					t.Errorf("symbol %d (%q) has %d units", i, s.Text, s.Units)
				}
				prevEnd = s.Start + s.Units
			}
		})
	}
}

// TestSymbolsKeyedTimeMatchesElements checks the keyed portions line up: the
// total time the key is down, summed over symbols, equals the elements'.
func TestSymbolsKeyedTimeMatchesElements(t *testing.T) {
	const msg = "VVV DE = WEB ERROR = BOOM = <AR>"

	var fromSymbols int
	for _, s := range Symbols(msg) {
		if s.Code == "" {
			continue
		}
		for _, sym := range s.Code {
			if sym == dah {
				fromSymbols += DahUnits
			} else {
				fromSymbols += DitUnits
			}
		}
	}

	var fromElements int
	for _, e := range Encode(msg) {
		if e.KeyDown {
			fromElements += e.Units
		}
	}

	if fromSymbols != fromElements {
		t.Errorf("symbols key for %d units, elements for %d", fromSymbols, fromElements)
	}
}

func TestProfileFor(t *testing.T) {
	tests := []struct {
		level string
		wpm   float64
		freq  float64
	}{
		{"fatal", 28, 800},
		{"error", 20, 700},
		{"warning", 13, 600},
		{"FATAL", 28, 800},     // case-insensitive
		{" warning ", 13, 600}, // trimmed
		{"info", 20, 700},      // unknown falls back to error
		{"", 20, 700},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			got := ProfileFor(tt.level)
			if got.WPM != tt.wpm || got.Freq != tt.freq {
				t.Errorf("ProfileFor(%q) = %g WPM / %g Hz, want %g / %g",
					tt.level, got.WPM, got.Freq, tt.wpm, tt.freq)
			}
		})
	}
}

// TestProfileOrdering encodes the design intent: more severe must sound both
// faster and higher.
func TestProfileOrdering(t *testing.T) {
	warning, err, fatal := ProfileFor("warning"), ProfileFor("error"), ProfileFor("fatal")

	if !(warning.WPM < err.WPM && err.WPM < fatal.WPM) {
		t.Errorf("WPM not ascending with severity: %g, %g, %g", warning.WPM, err.WPM, fatal.WPM)
	}
	if !(warning.Freq < err.Freq && err.Freq < fatal.Freq) {
		t.Errorf("pitch not ascending with severity: %g, %g, %g", warning.Freq, err.Freq, fatal.Freq)
	}
}
