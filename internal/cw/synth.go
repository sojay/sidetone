package cw

import "math"

// RampDuration is the rise and fall time of the keying envelope, in seconds.
// Switching a sine on and off instantaneously produces a broadband click, so
// each keyed element is shaped with a raised cosine — the conventional CW fix.
const RampDuration = 0.005

// Render synthesises elems as mono PCM in the range [-1, 1].
//
// It deliberately returns samples rather than playing them: this is what lets
// the whole timing engine be tested without a soundcard.
func Render(elems []Element, p Profile, volume float64, sampleRate int) []float64 {
	if sampleRate <= 0 {
		return nil
	}
	volume = math.Min(math.Max(volume, 0), 1)

	unitSecs := Unit(p.WPM).Seconds()
	samplesPerUnit := unitSecs * float64(sampleRate)

	out := make([]float64, 0, int(samplesPerUnit*float64(TotalUnits(elems)))+1)
	for _, e := range elems {
		n := int(samplesPerUnit * float64(e.Units))
		if !e.KeyDown {
			out = append(out, make([]float64, n)...)
			continue
		}
		out = append(out, Tone(p.Freq, n, volume, sampleRate)...)
	}
	return out
}

// Tone generates n samples of an enveloped sine at freq.
//
// Each keyed element restarts the sine's phase. That is audibly fine because
// the envelope takes the amplitude to zero at both edges, so there is no
// discontinuity to hear.
func Tone(freq float64, n int, volume float64, sampleRate int) []float64 {
	if n <= 0 || sampleRate <= 0 {
		return nil
	}

	ramp := int(RampDuration * float64(sampleRate))
	// A dit at high speed can be shorter than two full ramps; split it evenly
	// rather than letting the rise and fall overlap.
	if ramp*2 > n {
		ramp = n / 2
	}

	samples := make([]float64, n)
	for i := range samples {
		s := volume * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate))

		if ramp > 0 {
			switch {
			case i < ramp:
				s *= raisedCosine(i, ramp)
			case i >= n-ramp:
				s *= raisedCosine(n-1-i, ramp)
			}
		}

		samples[i] = s
	}
	return samples
}

// raisedCosine rises smoothly from 0 to 1 as i goes from 0 to ramp.
func raisedCosine(i, ramp int) float64 {
	return 0.5 * (1 - math.Cos(math.Pi*float64(i)/float64(ramp)))
}
