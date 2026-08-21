#!/bin/sh
# Drive the demo through POST /test — sub-second, no Sentry round trip.
#
# /test takes the same path as a real alert (compose -> queue -> keyer) but
# skips the ~25 seconds an alert spends inside Sentry, so it is what you want
# for the beats you drive on stage. Use one real `-fire` early to prove the
# Sentry round trip is genuine, then pace the rest from here.
#
#   ./demo.sh fatal      14s  fast and high — 28 wpm, 800 Hz
#   ./demo.sh error      17s  the middle    — 20 wpm, 700 Hz
#   ./demo.sh warning    30s  slow and low  — 13 wpm, 600 Hz
#   ./demo.sh queue           three at once, to show the queue filling
#   ./demo.sh storm           overflow the queue, to show the drop policy
#   ./demo.sh health          is the station up?
#   ./demo.sh preflight       check the whole chain before you present
#   ./demo.sh flush           drop everything queued (the message on air finishes)
#
# Airtimes above are measured, not estimated. Set DRY=1 to print the requests
# without sending them, and SIDETONE_HOST to point somewhere other than
# localhost:8080.

set -eu

HOST="${SIDETONE_HOST:-localhost:8080}"

send() {
	if [ "${DRY:-0}" = "1" ]; then
		printf '  would POST %s  %s\n' "$HOST/test" "$1"
		return
	fi
	# -s keeps curl quiet; the response is the composed message, which is worth
	# seeing on stage.
	printf '  '
	curl -s -X POST "$HOST/test" -d "$1"
	printf '\n'
}

alert() { # project level title
	send "{\"project\":\"$1\",\"level\":\"$2\",\"title\":\"$3\"}"
}

case "${1:-}" in
fatal)
	echo "fatal — payments — 28 wpm / 800 Hz — about 14s"
	alert payments fatal "DB down"
	;;

error)
	echo "error — api — 20 wpm / 700 Hz — about 17s"
	alert api error "Timeout"
	;;

warning)
	echo "warning — web — 13 wpm / 600 Hz — about 30s"
	echo "  (the slow one: this is the contrast, not a beat to wait through)"
	alert web warning "Cache miss"
	;;

queue)
	echo "three alerts at once — the first plays, two queue behind it"
	alert payments fatal "DB down"
	alert api error "Timeout"
	alert web warning "Cache miss"
	echo "  watch the queue panel: each row shows when it will be heard"
	;;

storm)
	echo "flooding the queue past its limit of 20 to show the drop policy."
	echo "NOTE: this queues many minutes of audio. Restart the server to clear it."
	printf "continue? [y/N] "
	read -r reply
	case "$reply" in
	y | Y) ;;
	*)
		echo "  cancelled"
		exit 0
		;;
	esac

	# One fatal first, then enough warnings to overflow. The fatal must survive:
	# the policy drops the oldest warning and never a fatal.
	alert payments fatal "DB down"
	i=1
	while [ "$i" -le 25 ]; do
		alert web warning "noise $i"
		i=$((i + 1))
	done
	echo "  the DROPPED counter should climb while the fatal stays in the queue"
	;;

health)
	curl -s "$HOST/healthz"
	printf '\n'
	;;

flush)
	# Local controls live on their own loopback port: a tunnelled request comes
	# from 127.0.0.1 too, so the route simply must not exist on the public one.
	curl -s -X POST "http://127.0.0.1:${ADMIN_PORT:-8081}/flush"
	printf '\n'
	;;

preflight)
	# Every link in the chain, named separately, because "nothing plays" has
	# four different causes and they need telling apart. Non-zero exit if any
	# link is down.
	ok=0
	say() { printf '  %-12s %s\n' "$1" "$2"; }
	bad() { ok=1; printf '  %-12s %s\n' "$1" "$2"; }

	# 1. the station itself
	local_json=$(curl -s -m 5 "$HOST/healthz" || true)
	case "$local_json" in
	*'"ok":true'*) say station "up on $HOST" ;;
	*) bad station "NOT RESPONDING on $HOST — start it with ./sidetone" ;;
	esac

	instance=$(printf '%s' "$local_json" | sed -n 's/.*"instance":"\([^"]*\)".*/\1/p')
	[ -n "$instance" ] && say instance "$instance"

	# 2. is the webhook armed? 401 means the secret is loaded, 503 means not.
	code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST "$HOST/webhook/sentry" \
		-H 'sentry-hook-signature: preflight' -d '{}' || true)
	case "$code" in
	401) say webhook "401 — signature checking live" ;;
	503) bad webhook "503 — SENTRY_CLIENT_SECRET not set in this process" ;;
	*) bad webhook "unexpected $code" ;;
	esac

	# 3. cloudflared: is it connected, and to which name?
	cf=$(pgrep -f 'cloudflared tunnel' 2>/dev/null | head -1 || true)
	live=""
	if [ -z "$cf" ]; then
		bad tunnel "cloudflared is NOT RUNNING"
	else
		for port in $(lsof -nP -p "$cf" -iTCP -sTCP:LISTEN 2>/dev/null |
			awk 'NR>1{print $9}' | sed 's/.*://' | sort -u); do
			ready=$(curl -s -m 3 "http://127.0.0.1:$port/ready" 2>/dev/null || true)
			case "$ready" in *readyConnections*) ;; *) continue ;; esac

			conns=$(printf '%s' "$ready" | sed -n 's/.*"readyConnections":\([0-9]*\).*/\1/p')
			live=$(curl -s -m 3 "http://127.0.0.1:$port/quicktunnel" 2>/dev/null |
				sed -n 's/.*"hostname":"\([^"]*\)".*/\1/p')

			if [ "${conns:-0}" -ge 1 ]; then
				say tunnel "connected, ${conns} connection(s), pid $cf"
			else
				bad tunnel "cloudflared is running but has ${conns:-0} connections — restart it"
			fi
			break
		done
		[ -n "$live" ] && say hostname "$live"
	fi

	# 4. does Sentry point at that same name? This is the failure that hides.
	if [ -n "${SENTRY_WEBHOOK_HOST:-}" ]; then
		if [ "$SENTRY_WEBHOOK_HOST" = "$live" ]; then
			say expected "matches Sentry"
		else
			bad expected "$SENTRY_WEBHOOK_HOST — MISMATCH, update the webhook URL in Sentry"
		fi
	else
		say expected "SENTRY_WEBHOOK_HOST unset — cannot check what Sentry points at"
	fi

	# 5. does the public URL actually reach *this* process?
	if [ -n "$live" ]; then
		pub=$(curl -s -m 20 "https://$live/healthz" || true)
		pub_instance=$(printf '%s' "$pub" | sed -n 's/.*"instance":"\([^"]*\)".*/\1/p')
		if [ -z "$pub_instance" ]; then
			bad public "https://$live/healthz did not answer — tunnel is up but nothing is behind it"
		elif [ "$pub_instance" = "$instance" ]; then
			say public "reaches this station ($pub_instance)"
		else
			bad public "answered as $pub_instance, not $instance — that is a different process"
		fi
	fi

	if [ "$ok" -eq 0 ]; then
		echo "  READY"
	else
		echo "  NOT READY — fix the lines above"
	fi
	exit "$ok"
	;;

*)
	sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
	exit 1
	;;
esac
