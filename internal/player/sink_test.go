package player

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestPCM16(t *testing.T) {
	tests := []struct {
		name    string
		samples []float64
		want    []int16
	}{
		{"silence", []float64{0, 0}, []int16{0, 0}},
		{"full scale", []float64{1, -1}, []int16{math.MaxInt16, -math.MaxInt16}},
		{"half scale", []float64{0.5}, []int16{16384}}, // 32767 * 0.5, rounded
		{"clips above", []float64{1.5, 99}, []int16{math.MaxInt16, math.MaxInt16}},
		{"clips below", []float64{-1.5, -99}, []int16{-math.MaxInt16, -math.MaxInt16}},
		{"empty", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pcm16(tt.samples)

			if len(got) != len(tt.want)*2 {
				t.Fatalf("got %d bytes, want %d", len(got), len(tt.want)*2)
			}
			for i, want := range tt.want {
				// Little-endian, as declared to oto and in the WAV header.
				if v := int16(binary.LittleEndian.Uint16(got[i*2:])); v != want {
					t.Errorf("sample %d = %d, want %d", i, v, want)
				}
			}
		})
	}
}

// TestWAVHeader pins the 44-byte canonical header. afplay is forgiving, but a
// wrong sample rate or block align is the kind of bug that shows up as
// chipmunk audio on a different machine.
func TestWAVHeader(t *testing.T) {
	const rate = 44100
	pcm := pcm16(make([]float64, 100)) // 200 bytes
	got := wav(pcm, rate)

	if len(got) != 44+len(pcm) {
		t.Fatalf("file is %d bytes, want %d", len(got), 44+len(pcm))
	}

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"RIFF marker", string(got[0:4]), "RIFF"},
		{"riff size", binary.LittleEndian.Uint32(got[4:]), uint32(36 + len(pcm))},
		{"WAVE marker", string(got[8:12]), "WAVE"},
		{"fmt marker", string(got[12:16]), "fmt "},
		{"fmt chunk size", binary.LittleEndian.Uint32(got[16:]), uint32(16)},
		{"PCM format", binary.LittleEndian.Uint16(got[20:]), uint16(1)},
		{"mono", binary.LittleEndian.Uint16(got[22:]), uint16(1)},
		{"sample rate", binary.LittleEndian.Uint32(got[24:]), uint32(rate)},
		{"byte rate", binary.LittleEndian.Uint32(got[28:]), uint32(rate * 2)},
		{"block align", binary.LittleEndian.Uint16(got[32:]), uint16(2)},
		{"bit depth", binary.LittleEndian.Uint16(got[34:]), uint16(16)},
		{"data marker", string(got[36:40]), "data"},
		{"data size", binary.LittleEndian.Uint32(got[40:]), uint32(len(pcm))},
	}

	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

func TestWAVCarriesTheSamples(t *testing.T) {
	pcm := pcm16([]float64{1, -1, 0})
	got := wav(pcm, 8000)

	for i, b := range pcm {
		if got[44+i] != b {
			t.Fatalf("payload byte %d = %d, want %d", i, got[44+i], b)
		}
	}
}

// TestWAVSampleRateIsNotHardcoded guards against the header drifting from the
// rate the samples were actually rendered at.
func TestWAVSampleRateIsNotHardcoded(t *testing.T) {
	for _, rate := range []int{8000, 22050, 44100, 48000} {
		got := wav(nil, rate)
		if v := binary.LittleEndian.Uint32(got[24:]); v != uint32(rate) {
			t.Errorf("header says %d Hz, want %d", v, rate)
		}
		if v := binary.LittleEndian.Uint32(got[28:]); v != uint32(rate*2) {
			t.Errorf("byte rate = %d, want %d", v, rate*2)
		}
	}
}
