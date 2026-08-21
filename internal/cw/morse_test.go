package cw

import "testing"

// wantCodes is written out by hand rather than derived from the package's own
// table — a test that reads the map it is checking would pass no matter what
// the map said.
var wantCodes = map[rune]string{
	'A': ".-", 'B': "-...", 'C': "-.-.", 'D': "-..", 'E': ".",
	'F': "..-.", 'G': "--.", 'H': "....", 'I': "..", 'J': ".---",
	'K': "-.-", 'L': ".-..", 'M': "--", 'N': "-.", 'O': "---",
	'P': ".--.", 'Q': "--.-", 'R': ".-.", 'S': "...", 'T': "-",
	'U': "..-", 'V': "...-", 'W': ".--", 'X': "-..-", 'Y': "-.--",
	'Z': "--..",

	'0': "-----", '1': ".----", '2': "..---", '3': "...--", '4': "....-",
	'5': ".....", '6': "-....", '7': "--...", '8': "---..", '9': "----.",

	'.': ".-.-.-", ',': "--..--", '?': "..--..", '\'': ".----.",
	'!': "-.-.--", '/': "-..-.", '(': "-.--.", ')': "-.--.-",
	'&': ".-...", ':': "---...", ';': "-.-.-.", '=': "-...-",
	'+': ".-.-.", '-': "-....-", '_': "..--.-", '"': ".-..-.",
	'$': "...-..-", '@': ".--.-.",
}

func TestCode(t *testing.T) {
	for r, want := range wantCodes {
		t.Run(string(r), func(t *testing.T) {
			got, ok := Code(r)
			if !ok {
				t.Fatalf("Code(%q): not supported, want %q", r, want)
			}
			if got != want {
				t.Errorf("Code(%q) = %q, want %q", r, got, want)
			}
		})
	}
}

// TestCodeTableComplete guards the other direction: the package must not carry
// mappings this test does not know about.
func TestCodeTableComplete(t *testing.T) {
	for r := range morse {
		if _, ok := wantCodes[r]; !ok {
			t.Errorf("package maps %q but the test table does not", r)
		}
	}
	if len(morse) != len(wantCodes) {
		t.Errorf("table has %d entries, test expects %d", len(morse), len(wantCodes))
	}
}

func TestCodeLowercase(t *testing.T) {
	got, ok := Code('a')
	if !ok || got != ".-" {
		t.Errorf("Code('a') = %q, %v; want %q, true", got, ok, ".-")
	}
}

func TestCodeUnsupported(t *testing.T) {
	for _, r := range []rune{'%', '#', '~', 'é', '中', '\t'} {
		if code, ok := Code(r); ok {
			t.Errorf("Code(%q) = %q, true; want unsupported", r, code)
		}
		if Supported(r) {
			t.Errorf("Supported(%q) = true, want false", r)
		}
	}
}

// wantProsigns, like wantCodes, is written out independently of the package.
var wantProsigns = map[string]string{
	"AR": ".-.-.",
	"SK": "...-.-",
	"VA": "...-.-",
	"BT": "-...-",
	"KN": "-.--.",
	"AS": ".-...",
	"CT": "-.-.-",
	"KA": "-.-.-",
	"SN": "...-.",
	"VE": "...-.",
	"HH": "........",
}

func TestProsign(t *testing.T) {
	for name, want := range wantProsigns {
		t.Run(name, func(t *testing.T) {
			got, ok := Prosign(name)
			if !ok {
				t.Fatalf("Prosign(%q): not known, want %q", name, want)
			}
			if got != want {
				t.Errorf("Prosign(%q) = %q, want %q", name, got, want)
			}
		})
	}
}

func TestProsignTableComplete(t *testing.T) {
	for name := range prosigns {
		if _, ok := wantProsigns[name]; !ok {
			t.Errorf("package maps prosign %q but the test table does not", name)
		}
	}
	if len(prosigns) != len(wantProsigns) {
		t.Errorf("table has %d prosigns, test expects %d", len(prosigns), len(wantProsigns))
	}
}

func TestProsignLookupNormalises(t *testing.T) {
	for _, name := range []string{"ar", "Ar", " AR "} {
		if got, ok := Prosign(name); !ok || got != ".-.-." {
			t.Errorf("Prosign(%q) = %q, %v; want %q, true", name, got, ok, ".-.-.")
		}
	}
	for _, name := range []string{"", "XQ", "A"} {
		if got, ok := Prosign(name); ok {
			t.Errorf("Prosign(%q) = %q, true; want unknown", name, got)
		}
	}
}

// TestProsignPunctuationAliases documents that <AR> and '+' — and <BT> and '='
// — are the same signal written two ways.
func TestProsignPunctuationAliases(t *testing.T) {
	aliases := []struct{ prosign, punct string }{
		{"AR", "+"},
		{"BT", "="},
		{"KN", "("},
		{"AS", "&"},
	}

	for _, a := range aliases {
		t.Run(a.prosign, func(t *testing.T) {
			pro, ok := Prosign(a.prosign)
			if !ok {
				t.Fatalf("prosign %q unknown", a.prosign)
			}
			punct, ok := Code([]rune(a.punct)[0])
			if !ok {
				t.Fatalf("punctuation %q unknown", a.punct)
			}
			if pro != punct {
				t.Errorf("<%s> = %q but %q = %q; want identical", a.prosign, pro, a.punct, punct)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"uppercases", "sentry", "SENTRY"},
		{"keeps digits and separators", "V2 = AR", "V2 = AR"},
		{"strips unmappable", "café 100%", "CAF 100"},
		{"collapses whitespace", "VVV   DE\tSENTRY", "VVV DE SENTRY"},
		{"trims ends", "  AR  ", "AR"},
		{"newlines become gaps", "DE\nSENTRY", "DE SENTRY"},
		{"drops word that was entirely unmappable", "A %%% B", "A B"},
		{"empty", "", ""},
		{"only unmappable", "%%%", ""},
		{
			"representative message",
			"VVV DE SENTRY = checkout-api ERROR = Null pointer! = <AR>",
			"VVV DE SENTRY = CHECKOUT-API ERROR = NULL POINTER! = <AR>",
		},

		{"keeps a known prosign", "<AR>", "<AR>"},
		{"uppercases a prosign", "<ar>", "<AR>"},
		{"prosign in context", "= <sk>", "= <SK>"},
		{"unknown prosign keeps its letters", "<XQ>", "XQ"},
		{"empty brackets vanish", "A <> B", "A B"},
		{"unterminated bracket drops only the bracket", "<AR", "AR"},
		{"stray closing bracket", "AR>", "AR"},
		{"two prosigns", "<BT> <AR>", "<BT> <AR>"},
		{"prosign adjacent to text", "K<AR>", "K<AR>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sanitize(tt.in); got != tt.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
