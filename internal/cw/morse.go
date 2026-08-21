// Package cw turns text into Morse code (CW) timing and audio samples.
//
// Nothing in this package touches audio hardware: Render returns PCM samples
// and internal/player is responsible for getting them to a soundcard. That
// split is what makes the timing math testable in CI.
package cw

import (
	"strings"
	"unicode"
)

// dit and dah are the two symbols a character's code is spelled with.
const (
	dit = '.'
	dah = '-'
)

// morse holds the ITU-R M.1677-1 mappings we support. Anything absent here is
// stripped by Sanitize rather than silently keyed as something else.
var morse = map[rune]string{
	'A': ".-",
	'B': "-...",
	'C': "-.-.",
	'D': "-..",
	'E': ".",
	'F': "..-.",
	'G': "--.",
	'H': "....",
	'I': "..",
	'J': ".---",
	'K': "-.-",
	'L': ".-..",
	'M': "--",
	'N': "-.",
	'O': "---",
	'P': ".--.",
	'Q': "--.-",
	'R': ".-.",
	'S': "...",
	'T': "-",
	'U': "..-",
	'V': "...-",
	'W': ".--",
	'X': "-..-",
	'Y': "-.--",
	'Z': "--..",

	'0': "-----",
	'1': ".----",
	'2': "..---",
	'3': "...--",
	'4': "....-",
	'5': ".....",
	'6': "-....",
	'7': "--...",
	'8': "---..",
	'9': "----.",

	'.':  ".-.-.-",
	',':  "--..--",
	'?':  "..--..",
	'\'': ".----.",
	'!':  "-.-.--",
	'/':  "-..-.",
	'(':  "-.--.",
	')':  "-.--.-",
	'&':  ".-...",
	':':  "---...",
	';':  "-.-.-.",
	'=':  "-...-", // BT — the section break used as a separator in our messages
	'+':  ".-.-.", // AR — end of message
	'-':  "-....-",
	'_':  "..--.-",
	'"':  ".-..-.",
	'$':  "...-..-",
	'@':  ".--.-.",
}

// prosigns are procedural signals: two or more letters run together as a
// single character, with no letter gap between them. They are written in angle
// brackets — "VVV DE SENTRY = <AR>" — because "AR" on its own is two ordinary
// letters and sounds different on the air.
//
// Several have the same code as a punctuation mark (<BT> is '=', <AR> is '+');
// both spellings are accepted and key identically.
var prosigns = map[string]string{
	"AR": ".-.-.",    // end of message
	"SK": "...-.-",   // end of contact
	"VA": "...-.-",   // = SK
	"BT": "-...-",    // break between sections
	"KN": "-.--.",    // go ahead, named station only
	"AS": ".-...",    // wait
	"CT": "-.-.-",    // attention / start of transmission
	"KA": "-.-.-",    // = CT
	"SN": "...-.",    // understood
	"VE": "...-.",    // = SN
	"HH": "........", // error, disregard the last word
}

// Supported reports whether r has an ITU Morse mapping. Case-insensitive.
func Supported(r rune) bool {
	_, ok := morse[unicode.ToUpper(r)]
	return ok
}

// Prosign returns the code for a prosign name such as "AR", and whether it is
// known. The name is matched case-insensitively and without its brackets.
func Prosign(name string) (string, bool) {
	code, ok := prosigns[strings.ToUpper(strings.TrimSpace(name))]
	return code, ok
}

// Sanitize prepares text for keying: uppercase, unmappable characters removed,
// and runs of whitespace collapsed to a single space (a space is meaningful in
// CW — it becomes a seven-unit word gap).
//
// Bracketed prosigns are preserved as "<AR>". A bracketed name that is not a
// known prosign keeps its letters and loses its brackets, so a typo is keyed
// as ordinary text rather than falling silent.
func Sanitize(text string) string {
	var b strings.Builder
	b.Grow(len(text))

	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]

		switch {
		case r == '<':
			end := closingBracket(runes, i+1)
			if end < 0 {
				continue // unterminated: drop the bracket, key what follows
			}
			name := keepKeyable(string(runes[i+1 : end]))
			if _, ok := Prosign(name); ok {
				b.WriteString("<" + name + ">")
			} else {
				b.WriteString(name)
			}
			i = end

		case unicode.IsSpace(r):
			b.WriteRune(' ')

		case Supported(r):
			b.WriteRune(unicode.ToUpper(r))
		}
	}

	// Fields+Join collapses the runs of spaces we may have just introduced by
	// dropping unmappable characters, and trims the ends.
	return strings.Join(strings.Fields(b.String()), " ")
}

// closingBracket returns the index of the '>' that closes a prosign opened
// before start, or -1. Brackets do not nest, so an intervening '<' means the
// first bracket was never closed.
func closingBracket(runes []rune, start int) int {
	for i := start; i < len(runes); i++ {
		switch runes[i] {
		case '>':
			return i
		case '<':
			return -1
		}
	}
	return -1
}

// keepKeyable uppercases s and drops anything with no Morse mapping.
func keepKeyable(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if Supported(r) {
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}

// Code returns the dit/dah spelling of r, and whether r is supported.
func Code(r rune) (string, bool) {
	c, ok := morse[unicode.ToUpper(r)]
	return c, ok
}
