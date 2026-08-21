package cw

import (
	"strings"
	"time"
)

// CW timing is defined in dimensionless "units"; only WPM converts a unit to
// real time. Holding the whole message as integer units keeps the engine exact
// and lets tests assert unit counts instead of comparing floats.
//
// These are the PARIS standard durations (the word "PARIS " is exactly 50
// units, which is what defines words-per-minute).
const (
	DitUnits = 1 // a dit, key down
	DahUnits = 3 // a dah, key down

	SymbolGapUnits = 1 // between dits/dahs inside one character
	LetterGapUnits = 3 // between characters of a word
	WordGapUnits   = 7 // between words
)

// An Element is one continuous stretch of key-down (tone) or key-up (silence),
// measured in units.
type Element struct {
	KeyDown bool
	Units   int
}

// A token is one keyable character: either a single rune or a bracketed
// prosign. A prosign is a single token precisely because its letters run
// together without the letter gap that would separate them as ordinary text.
type token struct {
	text string // "A" or "<AR>"
	code string // dit/dah spelling
}

// tokenize splits one sanitized word into keyable characters. It assumes
// Sanitize has already run, so every bracket pair encloses a known prosign.
func tokenize(word string) []token {
	var toks []token

	runes := []rune(word)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '<' {
			end := closingBracket(runes, i+1)
			if end < 0 {
				continue
			}
			name := string(runes[i+1 : end])
			if code, ok := Prosign(name); ok {
				toks = append(toks, token{text: "<" + name + ">", code: code})
			}
			i = end
			continue
		}

		if code, ok := Code(runes[i]); ok {
			toks = append(toks, token{text: string(runes[i]), code: code})
		}
	}
	return toks
}

// A Symbol is one character of a message together with where it falls in time,
// measured in units from the start. Word gaps appear as symbols too, with a
// space for text and no code, so the symbols joined back together reproduce the
// sanitized message exactly.
//
// This is what lets a display follow along by ear: given the elapsed time and
// the speed, the character currently sounding is the last one whose Start has
// passed.
type Symbol struct {
	Text  string `json:"text"`  // "A", "<AR>", or " " between words
	Code  string `json:"code"`  // dit/dah spelling, empty for a word gap
	Start int    `json:"start"` // units from the start of the message
	Units int    `json:"units"` // how long this character sounds for
}

// Symbols breaks text into its keyed characters. It is the primitive the rest
// of the timing engine is built on: Encode flattens these into elements, so the
// two can never disagree about where a character falls.
func Symbols(text string) []Symbol {
	var syms []Symbol

	unit := 0
	for wi, word := range strings.Fields(Sanitize(text)) {
		if wi > 0 {
			syms = append(syms, Symbol{Text: " ", Start: unit, Units: WordGapUnits})
			unit += WordGapUnits
		}

		for ci, tok := range tokenize(word) {
			if ci > 0 {
				unit += LetterGapUnits
			}

			n := codeUnits(tok.code)
			syms = append(syms, Symbol{Text: tok.text, Code: tok.code, Start: unit, Units: n})
			unit += n
		}
	}
	return syms
}

// codeUnits is how long a dit/dah spelling takes, including the gaps inside it.
func codeUnits(code string) int {
	var n int
	for i, sym := range code {
		if i > 0 {
			n += SymbolGapUnits
		}
		if sym == dah {
			n += DahUnits
		} else {
			n += DitUnits
		}
	}
	return n
}

// Encode turns text into the sequence of keyed and silent elements needed to
// send it. Input is sanitized first, so unmappable characters are dropped and
// bracketed prosigns such as <AR> are keyed as one run-together character.
//
// Gaps only ever appear *between* things: the returned sequence never starts or
// ends with silence, which leaves the caller free to decide on lead-in and
// tail padding.
func Encode(text string) []Element {
	var elems []Element

	prevEnd := -1
	for _, s := range Symbols(text) {
		if s.Code == "" {
			continue // a word gap; the silence falls out of the next Start
		}

		if prevEnd >= 0 {
			if gap := s.Start - prevEnd; gap > 0 {
				elems = append(elems, Element{Units: gap})
			}
		}

		for i, sym := range s.Code {
			if i > 0 {
				elems = append(elems, Element{Units: SymbolGapUnits})
			}
			units := DitUnits
			if sym == dah {
				units = DahUnits
			}
			elems = append(elems, Element{KeyDown: true, Units: units})
		}

		prevEnd = s.Start + s.Units
	}
	return elems
}

// TotalUnits sums the length of every element, keyed and silent alike.
func TotalUnits(elems []Element) int {
	var n int
	for _, e := range elems {
		n += e.Units
	}
	return n
}

// Unit is the duration of a single CW unit at the given keying speed. This is
// the one place WPM becomes wall-clock time: 1.2 / WPM seconds, which falls out
// of PARIS being 50 units long.
func Unit(wpm float64) time.Duration {
	if wpm <= 0 {
		return 0
	}
	return time.Duration(1.2 / wpm * float64(time.Second))
}

// Duration reports how long elems takes to send at the given speed.
func Duration(elems []Element, wpm float64) time.Duration {
	return time.Duration(TotalUnits(elems)) * Unit(wpm)
}

// The Sentry levels we have a sound for. Other packages match on these rather
// than on bare strings — the queue's drop policy in particular depends on
// telling a fatal from a warning.
const (
	LevelFatal   = "fatal"
	LevelError   = "error"
	LevelWarning = "warning"
)

// A Profile is how a severity sounds: faster and higher means more urgent.
type Profile struct {
	Level string
	WPM   float64
	Freq  float64 // sidetone pitch in Hz
}

// Profiles maps Sentry levels to their sound. Unknown levels fall back to
// error, on the grounds that an alert we can't classify is at least worth
// hearing at a normal speed.
var profiles = map[string]Profile{
	LevelFatal:   {Level: LevelFatal, WPM: 28, Freq: 800},
	LevelError:   {Level: LevelError, WPM: 20, Freq: 700},
	LevelWarning: {Level: LevelWarning, WPM: 13, Freq: 600},
}

// ProfileFor returns the keying speed and pitch for a Sentry level. The level
// is matched case-insensitively; anything unrecognised gets the error profile.
func ProfileFor(level string) Profile {
	if p, ok := profiles[strings.ToLower(strings.TrimSpace(level))]; ok {
		return p
	}
	return profiles[LevelError]
}
