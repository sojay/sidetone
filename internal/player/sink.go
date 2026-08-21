package player

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ebitengine/oto/v3"
)

// OtoSink plays through the soundcard via oto. This is the primary output.
type OtoSink struct {
	ctx        *oto.Context
	sampleRate int
}

// NewOtoSink opens the audio device.
//
// oto allows exactly one context per process, so this must be called once and
// the sink shared. It also initialises asynchronously, and we wait for it here:
// playing before the device is ready drops the head of the message.
func NewOtoSink(sampleRate int) (*OtoSink, error) {
	ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:   sampleRate,
		ChannelCount: 1,
		Format:       oto.FormatSignedInt16LE,
	})
	if err != nil {
		return nil, fmt.Errorf("open audio device: %w", err)
	}
	<-ready

	return &OtoSink{ctx: ctx, sampleRate: sampleRate}, nil
}

// Play blocks until the samples have finished playing, which is what makes the
// queue sequential.
func (s *OtoSink) Play(samples []float64) error {
	pl := s.ctx.NewPlayer(bytes.NewReader(pcm16(samples)))
	defer pl.Close()

	pl.Play()
	for pl.IsPlaying() {
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// SystemPlayerSink writes a WAV and shells out to the system audio player. It
// is the fallback for when oto cannot open a device.
type SystemPlayerSink struct {
	sampleRate int
	path       string
	players    []string
}

// NewSystemPlayerSink returns a sink that plays through an external binary,
// or an error if no known player is installed.
func NewSystemPlayerSink(sampleRate int) (*SystemPlayerSink, error) {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{"afplay"}
	default:
		candidates = []string{"aplay", "paplay", "play"}
	}

	var found []string
	for _, name := range candidates {
		if bin, err := exec.LookPath(name); err == nil {
			found = append(found, bin)
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no system audio player found (tried %v)", candidates)
	}

	return &SystemPlayerSink{
		sampleRate: sampleRate,
		// One reused path: the queue is sequential, so the previous file is
		// never still in use.
		path:    filepath.Join(os.TempDir(), "sidetone.wav"),
		players: found,
	}, nil
}

func (s *SystemPlayerSink) Play(samples []float64) error {
	if err := os.WriteFile(s.path, wav(pcm16(samples), s.sampleRate), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}

	var err error
	for _, bin := range s.players {
		cmd := exec.Command(bin, s.path)
		cmd.Stderr = os.Stderr
		if err = cmd.Run(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("play %s: %w", s.path, err)
}

// Path is where the last message was written, for the WAV-export stretch goal.
func (s *SystemPlayerSink) Path() string { return s.path }

// pcm16 converts float samples to little-endian signed 16-bit, the format both
// oto and WAV want here.
func pcm16(samples []float64) []byte {
	buf := make([]byte, 0, len(samples)*2)
	for _, s := range samples {
		v := int16(math.Round(math.Min(math.Max(s, -1), 1) * math.MaxInt16))
		buf = append(buf, byte(v), byte(v>>8))
	}
	return buf
}

// wav wraps raw PCM in a canonical 44-byte RIFF/WAVE header: mono, 16-bit.
func wav(pcm []byte, sampleRate int) []byte {
	const (
		headerRest = 36 // bytes after the RIFF size field, excluding data
		fmtChunk   = 16
		pcmFormat  = 1
		channels   = 1
		bitDepth   = 16
	)
	byteRate := sampleRate * channels * bitDepth / 8

	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(headerRest+len(pcm)))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(fmtChunk))
	binary.Write(&b, binary.LittleEndian, uint16(pcmFormat))
	binary.Write(&b, binary.LittleEndian, uint16(channels))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&b, binary.LittleEndian, uint32(byteRate))
	binary.Write(&b, binary.LittleEndian, uint16(channels*bitDepth/8)) // block align
	binary.Write(&b, binary.LittleEndian, uint16(bitDepth))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}
