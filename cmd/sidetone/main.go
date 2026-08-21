// Command sidetone receives Sentry issue alerts and plays them as Morse code
// (CW) out of the laptop speakers.
//
// By default it runs the webhook server. Two flags do something else instead:
//
//   - -once composes and plays a single alert locally, for rehearsing how a
//     particular message sounds without involving Sentry at all.
//   - -fire sends a real error to Sentry and exits. If an alert rule matches,
//     Sentry posts it back to the running server, which keys it — so the whole
//     loop can be driven from one laptop.
//
// Configuration is by environment variable:
//
//	PORT                    listen port (default 8080)
//	SENTRY_CLIENT_SECRET    shared secret for webhook signatures
//	SENTRY_DSN              project DSN, used only by -fire
//	VOLUME                  0.0-1.0 (default 0.8)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sojay/sidetone/internal/bandscope"
	"github.com/sojay/sidetone/internal/composer"
	"github.com/sojay/sidetone/internal/cw"
	"github.com/sojay/sidetone/internal/player"
	"github.com/sojay/sidetone/internal/trigger"
	"github.com/sojay/sidetone/internal/webhook"
)

const sampleRate = player.DefaultSampleRate

// shutdownTimeout bounds how long we wait for in-flight requests. It does not
// bound audio: a message on the air finishes, because cutting a transmission in
// half sounds like a fault.
const shutdownTimeout = 5 * time.Second

func main() {
	once := flag.Bool("once", false, "compose and play a single alert, then exit")
	level := flag.String("level", "error", "severity for -once: fatal, error, or warning")
	project := flag.String("project", "sidetone", "project slug for -once")
	title := flag.String("title", "test alert", "issue title for -once")
	culprit := flag.String("culprit", "", "issue culprit for -once, used when the title is empty")
	text := flag.String("text", "", "key this raw message for -once instead of composing one")
	forceWAV := flag.Bool("wav", false, "skip oto and use the WAV + system player fallback")
	fire := flag.String("fire", "", "send a real error to Sentry with this message, then exit (needs SENTRY_DSN)")
	kind := flag.String("kind", "DemoError", "exception type for -fire; forms the issue title with the message")
	unique := flag.Bool("unique", true, "fingerprint each -fire as a new Sentry issue, so \"a new issue is created\" rules fire every time")
	flag.Parse()

	logger := log.Default()

	// -fire is a client, not a station: it sends an error to Sentry and stops.
	// The alert comes back through the webhook to whichever sidetone is
	// serving, so this path never opens the audio device.
	if *fire != "" {
		if err := fireError(*fire, *kind, *level, *unique, logger); err != nil {
			logger.Fatalf("fire: %v", err)
		}
		return
	}

	sink, err := openSink(*forceWAV)
	if err != nil {
		logger.Fatalf("audio: %v", err)
	}

	// Ctrl-C stops the server immediately and the player after the message in
	// progress.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The band scope watches the player, so it is built first and attached
	// through the player's event hook.
	var scope *bandscope.Hub

	p := player.New(player.Config{
		Sink:       sink,
		Volume:     volumeFromEnv(logger),
		SampleRate: sampleRate,
		Logger:     logger,

		OnEvent: func(e player.Event) {
			if scope != nil {
				scope.PlayerEvent(e)
			}
		},

		// One-shot plays what it queued and exits; the server waits for more.
		StopWhenEmpty: *once,
	})

	if *once {
		if err := playOnce(ctx, p, *text, composer.Alert{
			Project: *project,
			Level:   *level,
			Title:   *title,
			Culprit: *culprit,
		}); err != nil {
			logger.Fatal(err)
		}
		return
	}

	hub, err := bandscope.New(p, logger)
	if err != nil {
		logger.Fatalf("band scope: %v", err)
	}
	scope = hub

	if err := serve(ctx, p, scope, logger); err != nil {
		logger.Fatal(err)
	}
}

// fireError sends a real error to Sentry so the demo can start where an
// incident starts. Nothing is played here: if an alert rule matches, Sentry
// posts the alert back to the webhook and the running station keys it.
//
// The DSN is read from the environment and never printed — not on success, not
// in an error. dsn.String() redacts the key for the one line that logs it.
func fireError(message, kind, level string, unique bool, logger *log.Logger) error {
	dsn, err := trigger.FromEnv()
	if err != nil {
		return fmt.Errorf("%w — set it to the project's DSN to send errors", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	event := trigger.Event{Message: message, Level: level, Kind: kind, Env: "demo", Unique: unique}

	id, err := trigger.Send(ctx, nil, dsn, event)
	if err != nil {
		return err
	}

	logger.Printf("sent %s to %s as event %s", level, dsn, id)

	// What the room will hear, assuming an alert rule matches. The project name
	// is the one part we cannot know from here — it arrives with the webhook —
	// so it stands in as PROJECT and the log line says so.
	preview := composer.Compose(composer.Alert{
		Project: "PROJECT",
		Level:   level,
		Title:   event.Title(),
	})
	logger.Printf("if an alert rule matches, the station will key (PROJECT comes from the alert): %s", preview)
	return nil
}

// playOnce composes one alert and plays it to completion.
func playOnce(ctx context.Context, p *player.Player, raw string, a composer.Alert) error {
	msg := raw
	if msg == "" {
		msg = composer.Compose(a)
	}

	elems := cw.Encode(msg)
	if len(elems) == 0 {
		return fmt.Errorf("nothing to key: %q has no Morse-mappable characters", msg)
	}

	profile := cw.ProfileFor(a.Level)
	log.Printf("keying %q as %s: %g WPM, %g Hz, %v",
		msg, profile.Level, profile.WPM, profile.Freq,
		cw.Duration(elems, profile.WPM).Round(time.Millisecond))

	p.Enqueue(player.Message{Text: msg, Profile: profile})

	if err := p.Run(ctx); err != nil {
		return fmt.Errorf("player: %w", err)
	}
	if p.Played() == 0 {
		return errors.New("nothing was played")
	}
	return nil
}

// serve runs the player and the HTTP server together until ctx is cancelled.
func serve(ctx context.Context, p *player.Player, scope *bandscope.Hub, logger *log.Logger) error {
	secret := os.Getenv("SENTRY_CLIENT_SECRET")
	if secret == "" {
		logger.Print("SENTRY_CLIENT_SECRET is not set: /webhook/sentry will reject everything, use POST /test for local demos")
	}

	// The player owns the speaker for the process lifetime; the handler only
	// ever hands it work.
	played := make(chan struct{})
	go func() {
		defer close(played)
		if err := p.Run(ctx); err != nil {
			logger.Printf("player stopped: %v", err)
		}
	}()

	// Alerts and the display share one mux, so the demo needs only one port
	// and one tunnel.
	mux := http.NewServeMux()
	webhook.New(webhook.Config{
		Queue:     p,
		Secret:    secret,
		Logger:    logger,
		Resources: keyedResourcesFromEnv(logger),

		OnReceived: func(r webhook.Received) {
			scope.Received(r.Endpoint, r.Raw, r.Message, r.Alert, r.Resource, r.SentryLag)
		},
	}).Register(mux)
	scope.Register(mux)

	srv := &http.Server{
		Addr:    net.JoinHostPort("", portFromEnv()),
		Handler: mux,

		// Modest timeouts: the handler does no I/O beyond reading the body.
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  time.Minute,
	}

	errs := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s — band scope at http://localhost:%s/", srv.Addr, portFromEnv())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http server: %w", err)
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		logger.Print("shutting down; the alert on the air will finish first")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("http shutdown: %v", err)
	}

	<-played // the player returns once the current message ends
	return nil
}

// keyedResourcesFromEnv reads KEYED_RESOURCES, a comma-separated list of
// Sentry-Hook-Resource values to key. Default is issue alerts only; adding
// "error" keys every error event, which skips Sentry's alert-rule stage and so
// arrives sooner, at the cost of being a firehose in any real project.
func keyedResourcesFromEnv(logger *log.Logger) []string {
	raw := os.Getenv("KEYED_RESOURCES")
	if raw == "" {
		return nil
	}

	var out []string
	for _, r := range strings.Split(raw, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	logger.Printf("keying webhook resources: %v", out)
	return out
}

func portFromEnv() string {
	if port := os.Getenv("PORT"); port != "" {
		return port
	}
	return "8080"
}

func volumeFromEnv(logger *log.Logger) float64 {
	raw := os.Getenv("VOLUME")
	if raw == "" {
		return player.DefaultVolume
	}

	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || v > 1 {
		logger.Printf("ignoring VOLUME=%q: want a number between 0.0 and 1.0", raw)
		return player.DefaultVolume
	}
	return v
}

// openSink picks an audio output: the soundcard if we can have it, otherwise a
// WAV handed to the system player.
func openSink(forceWAV bool) (player.Sink, error) {
	if !forceWAV {
		sink, err := openOto()
		if err == nil {
			return sink, nil
		}
		log.Printf("soundcard unavailable, falling back to a WAV file: %v", err)
	}
	return player.NewSystemPlayerSink(sampleRate)
}

// openOto guards against oto panicking on a machine with no audio device at
// all, so the fallback still gets its turn. Losing the demo to a panic in a
// dependency is exactly the failure the WAV path exists to prevent.
func openOto() (sink player.Sink, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("oto panicked opening the device: %v", r)
		}
	}()
	return player.NewOtoSink(sampleRate)
}
