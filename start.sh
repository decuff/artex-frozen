#!/bin/sh
# ARTEX supervisor startup script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       Run in foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   Run persistently in background
#   ./start.sh -addr :9000           Extra args are passed through to artex as-is
#
# It does only one thing: get artex running, and after the process exits, decide based on the exit code whether to start it again.
#
#   0      User stopped normally   → exit the loop
#   75     Program requests restart        → rerun immediately (the page clicked 'one-click update' or 'rollback')
#   other  Crash                 → rerun after backoff (1→2→4… up to 60 seconds)
#
# Deliberately no download, SHA256 verification, or swapping here: that logic would have to be written twice for sh and bat,
# and it is exactly the part that must not go wrong—once a binary that won't run is swapped in, this script will faithfully
# keep restarting it, and the user can only rescue it by hand on the machine. So verification/swapping all stays in Go (selfupdate package),
# completed by artex itself at startup, keeping the script foolproof.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] executable not found: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward stop signals to the artex process itself.
#
# This is required under Docker: docker stop only sends SIGTERM to PID 1 (i.e. this script),
# not to the child process. Without forwarding, artex never receives the signal and can't shut down gracefully; 10 seconds later it gets SIGKILLed
# hard, and running tasks are cut off mid-way.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# The signal interrupts wait and makes it return >128. At this point the child process is actually still shutting down gracefully,
	# so you must wait once more to get its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			# Update/rollback is ready: after rerunning, artex will complete the swap at startup (see selfupdate.Bootstrap).
			echo "[artex] restart requested (applying new version)…"
			delay=1
			;;
		*)
			echo "[artex] abnormal exit (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
