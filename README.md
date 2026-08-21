# sidetone

Sentry alerts, played as Morse code.

A single Go binary receives Sentry issue-alert webhooks and keys them as CW out
of the laptop speakers. The point is ambient telemetry: you hear that something
is wrong — and how wrong — without looking at a dashboard. Severity is carried
by keying speed and sidetone pitch, so after a few alerts you stop decoding and
start recognising, the way you know a doorbell from a smoke alarm.

## Hear it

```sh
go build -o sidetone ./cmd/sidetone

# Play one alert locally. No credentials needed.
./sidetone -once -level fatal -project checkout-api -title "DB pool exhausted"

# Or run the station and send it something.
./sidetone
curl -X POST localhost:8080/test \
  -d '{"project":"checkout-api","level":"fatal","title":"DB pool exhausted"}'
```

Then open <http://localhost:8080/> to watch it.

## What an alert sounds like

Every message uses the same frame, so your ear can skip the preamble and wait
for the project name:

```
VVV DE = CHECKOUT-API FATAL = DB POOL EXHAUSTED = <AR>
```

`VVV` is the traditional attention call, `DE` means "from", `=` is the BT
section break, and `<AR>` is the end-of-message prosign — five symbols run
together as one character, not the letters A and R.

Severity sets the speed and the pitch. Faster and higher means worse:

| Level | Speed | Pitch | Airtime |
| --- | --- | --- | --- |
| fatal | 28 wpm | 800 Hz | ~14s |
| error | 20 wpm | 700 Hz | ~17s |
| warning | 13 wpm | 600 Hz | ~30s |

The timing is real: the ITU standard, where one unit is `1.2 / wpm` seconds, a
dah is three units, and the gap between words is seven. Keying uses a
raised-cosine envelope, because a hard-switched sine clicks.

Titles are uppercased, stripped of anything with no Morse mapping, and cut to 40
characters at a word boundary. Sending time is linear in characters, so length
is a queue-latency decision rather than a cosmetic one.

## The band scope

The station serves a read-only display from the same binary — no build step, no
separate process. It shows the message being decoded character by character in
step with the tone, a keying trace, a waterfall of recent traffic in pitch
lanes, the queue with live countdowns, and the raw Sentry payload each message
came from. Click a row in the traffic log to pin it and inspect its payload.

It stays in sync without streaming any audio: CW timing is exact, so the page is
told the message and the speed once and animates the keying itself.

## Configuration

Environment variables only, all optional.

| Variable | Default | Notes |
| --- | --- | --- |
| `PORT` | `8080` | Listen port for the webhook and the display. |
| `SENTRY_CLIENT_SECRET` | unset | Verifies alerts arriving **from** Sentry. While unset, `/webhook/sentry` refuses everything with 503; `/test` still works. |
| `SENTRY_DSN` | unset | Sends errors **to** Sentry. Used only by `-fire`. |
| `VOLUME` | `0.8` | 0.0–1.0. |
| `KEYED_RESOURCES` | `event_alert` | Which Sentry webhook resources to key. |

Nothing reads a `.env` file automatically — export it yourself:

```sh
set -a; source .env; set +a   # set -a is what exports to child processes
./sidetone
```

## Endpoints

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| `POST` | `/webhook/sentry` | HMAC signature | Real Sentry alerts. |
| `POST` | `/test` | none | Local demos without Sentry. |
| `GET` | `/healthz` | none | Liveness and queue depth. |
| `GET` | `/` | none | The band scope. |

Signatures are verified over the raw request body, hashed with the integration's
client secret and compared in constant time.

## Connecting Sentry

1. **Create a throwaway project.** The DSN comes from here, and a loose alert
   rule on a real project would play every genuine error at you. Keep the slug
   short — it gets keyed on every alert and is capped at 20 characters.
2. **Expose the port**, e.g. `cloudflared tunnel --url http://localhost:8080`.
   Check the public URL answers: `curl https://<host>/healthz`.
3. **Create an internal integration** (Settings → Developer Settings → Custom
   Integrations). Set the webhook URL to `https://<host>/webhook/sentry` and turn
   **Alert Rule Action** on — without it the integration never appears as an
   alert action and nothing will ever arrive. Leave the issue/error webhook
   subscriptions off; they are a firehose and are dropped anyway.
4. **Copy the client secret** into `SENTRY_CLIENT_SECRET` and restart. A signed
   request should now get 401 rather than 503.
5. **Add an issue alert rule** whose action notifies that integration.

Then send a real error and listen:

```sh
./sidetone -fire "connection pool exhausted" -level fatal
```

`-fire` posts a genuine event to Sentry's ingest API and exits; if a rule
matches, the alert comes back through the webhook and the station keys it. Each
error gets a unique fingerprint by default, so a rule on "a new issue is
created" fires every time rather than only the first.

**Expect about 25 seconds.** Nearly all of it is inside Sentry — ingest, issue
creation, rule evaluation, dispatch. Composing, encoding and synthesising the
whole message takes about 5ms. Every alert reports its own figure in the log and
on the display, so it is clear which is which.

## Before a demo

```sh
./demo.sh preflight
```

It checks each link separately — the station, whether the webhook secret is
loaded, whether cloudflared is actually connected rather than merely running,
whether the hostname it claims matches what Sentry is configured with, and
whether the public URL reaches *this* process (compared by the instance id
`/healthz` reports). It exits non-zero if anything is down, and names the link
that is, because "nothing plays" has several possible causes.

Set `SENTRY_WEBHOOK_HOST` in `.env` to the hostname you put in Sentry's webhook
URL. Without it, preflight cannot catch the failure that hides best: a healthy
tunnel on a new hostname while Sentry still posts to the old one.

`./demo.sh` also drives the demo itself — `fatal`, `error`, `warning`, `queue`,
and `flush` if too much gets queued. Flush empties everything behind the message
on the air; that one has to finish, since a sink cannot be interrupted. It lives
on a loopback-only port, because a request arriving through a tunnel comes from
127.0.0.1 as well and cannot be told apart by address.

## Development

```sh
go test ./... -race
go vet ./...
```

The `cw` package holds messages as integer units and returns PCM samples, so the
timing engine is exact and testable without a soundcard; `player` is the only
package that touches audio hardware. Layout:

```
cmd/sidetone/       wiring, config, graceful shutdown
internal/webhook/   HTTP handlers, signature check, payload parsing
internal/composer/  alert -> CW message string
internal/cw/        Morse table, timing engine, synthesis
internal/player/    queue, drop policy, audio sinks
internal/bandscope/ the browser display
internal/trigger/   sends a real error to Sentry
```

## Notes and limits

- **One speaker means one queue.** Alerts play in order. Past 20 deep the queue
  sheds the oldest warning first and never a fatal, so the cap bounds the
  droppable part of the queue rather than its depth.
- **A warning holds the speaker for about half a minute.** That is what 13 wpm
  costs. Prefer short titles.
- **No persistence.** Restart and the queue is empty, which is right for
  something that only reports the present.
- Go standard library throughout, plus one dependency for the soundcard. If no
  audio device is available it writes a WAV and shells out to the system player.
