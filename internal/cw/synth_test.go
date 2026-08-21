package cw

import (
	"math"
	"testing"
)

const testRate = 44100

// TestRenderLength checks the sample count against the timing engine rather
// than against the audio: length is the one thing about the waveform that the
// unit math fully determines.
func TestRenderLength(t *testing.T) {
	for _, level := range []string{"warning", "error", "fatal"} {
		t.Run(level, func(t *testing.T) {
			p := ProfileFor(level)
			elems := Encode("PARIS")

			got := len(Render(elems, p, 0.8, testRate))
			want := int(Unit(p.WPM).Seconds() * float64(TotalUnits(elems)) * testRate)

			// Each element rounds its own sample count down, so allow a slack
			// of one sample per element.
			if diff := got - want; diff < -len(elems) || diff > len(elems) {
				t.Errorf("Render length = %d samples, want ~%d", got, want)
			}
		})
	}
}

func TestRenderEmpty(t *testing.T) {
	if got := Render(nil, ProfileFor("error"), 0.8, testRate); len(got) != 0 {
		t.Errorf("Render(nil) returned %d samples, want 0", len(got))
	}
	if got := Render(Encode("E"), ProfileFor("error"), 0.8, 0); got != nil {
		t.Errorf("Render with zero sample rate = %v, want nil", got)
	}
}

// TestRenderSilence verifies that key-up elements really are silent, and that
// they land where the timing engine says they do.
func TestRenderSilence(t *testing.T) {
	p := ProfileFor("error")
	elems := Encode("E E") // dit, 7-unit gap, dit
	samples := Render(elems, p, 0.8, testRate)

	perUnit := int(Unit(p.WPM).Seconds() * testRate)
	gapStart, gapEnd := perUnit, perUnit*8

	for i := gapStart + 1; i < gapEnd-1 && i < len(samples); i++ {
		if samples[i] != 0 {
			t.Fatalf("sample %d inside the word gap = %g, want 0", i, samples[i])
		}
	}

	// And the keyed parts must not be silent.
	var peak float64
	for _, s := range samples[:perUnit] {
		peak = math.Max(peak, math.Abs(s))
	}
	if peak < 0.1 {
		t.Errorf("first dit peaks at %g, want a real tone", peak)
	}
}

func TestRenderRespectsVolume(t *testing.T) {
	tests := []struct {
		name     string
		volume   float64
		wantPeak float64
	}{
		{"full", 1.0, 1.0},
		{"partial", 0.5, 0.5},
		{"silent", 0.0, 0.0},
		{"clamped above", 1.5, 1.0},
		{"clamped below", -1, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			samples := Render(Encode("T"), ProfileFor("error"), tt.volume, testRate)

			var peak float64
			for _, s := range samples {
				peak = math.Max(peak, math.Abs(s))
			}
			// A dah is long enough that the sine reaches near its peak.
			if peak > tt.wantPeak+0.01 || peak < tt.wantPeak-0.05 {
				t.Errorf("peak = %g, want ~%g", peak, tt.wantPeak)
			}
		})
	}
}

// TestToneEnvelope is the anti-click test: the waveform must start and end at
// zero and ramp smoothly, because a hard edge is an audible defect.
func TestToneEnvelope(t *testing.T) {
	samples := Tone(700, testRate/10, 1.0, testRate) // 100ms

	if got := math.Abs(samples[0]); got > 1e-9 {
		t.Errorf("first sample = %g, want 0", got)
	}
	if got := math.Abs(samples[len(samples)-1]); got > 1e-9 {
		t.Errorf("last sample = %g, want 0", got)
	}

	// Within the ramp, the envelope should still be well below full scale.
	// rate is a variable so this mirrors synth.go's truncation exactly; as a
	// constant expression, int(0.005 * 44100) would not compile.
	rate := testRate
	ramp := int(RampDuration * float64(rate))
	var earlyPeak float64
	for _, s := range samples[:ramp/2] {
		earlyPeak = math.Max(earlyPeak, math.Abs(s))
	}
	if earlyPeak > 0.5 {
		t.Errorf("first half of the ramp peaks at %g, want a gentle rise", earlyPeak)
	}
}

// TestToneShortElementEnvelope covers the case the ramp logic exists for: at
// high speed a dit can be shorter than two full ramps.
func TestToneShortElementEnvelope(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 10, 50} {
		samples := Tone(800, n, 1.0, testRate)
		if len(samples) != n {
			t.Errorf("Tone(n=%d) returned %d samples", n, len(samples))
		}
		for i, s := range samples {
			if math.Abs(s) > 1.0 {
				t.Errorf("n=%d: sample %d = %g, outside [-1, 1]", n, i, s)
			}
		}
	}
}

func TestRenderSamplesInRange(t *testing.T) {
	samples := Render(Encode("VVV DE SENTRY = AR"), ProfileFor("fatal"), 1.0, testRate)
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for i, s := range samples {
		if s < -1 || s > 1 || math.IsNaN(s) {
			t.Fatalf("sample %d = %g, outside [-1, 1]", i, s)
		}
	}
}

// The alert a demo actually sends, used by the benchmarks below.
const benchMessage = "VVV DE = SIDETONE-DEMO FATAL = DEMOERROR: CONNECTION POOL = <AR>"

// BenchmarkEncode and BenchmarkRender measure the only work that stands between
// a webhook arriving and the first sample being available. Worth knowing in
// absolute terms: if this were slow it would add to alert latency, and if it is
// fast then latency lives somewhere else entirely.
func BenchmarkEncode(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Encode(benchMessage)
	}
}

func BenchmarkRender(b *testing.B) {
	elems := Encode(benchMessage)
	p := ProfileFor(LevelFatal)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Render(elems, p, 0.8, testRate)
	}
}

// BenchmarkRenderWarning is the worst case: the slowest speed means the most
// samples for the same text.
func BenchmarkRenderWarning(b *testing.B) {
	elems := Encode(benchMessage)
	p := ProfileFor(LevelWarning)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Render(elems, p, 0.8, testRate)
	}
}

// TestToneFrequency counts zero crossings to confirm the sine is actually at
// the requested pitch — a cheap check that catches a factor-of-two or
// sample-rate mix-up.
func TestToneFrequency(t *testing.T) {
	const freq = 700
	samples := Tone(freq, testRate, 1.0, testRate) // exactly one second

	var crossings int
	for i := 1; i < len(samples); i++ {
		if (samples[i-1] < 0) != (samples[i] < 0) {
			crossings++
		}
	}

	// A sine crosses zero twice per cycle.
	if got, want := float64(crossings)/2, float64(freq); math.Abs(got-want) > 2 {
		t.Errorf("measured %g Hz, want %g Hz", got, want)
	}
}
