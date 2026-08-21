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

*)
	sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
	exit 1
	;;
esac
