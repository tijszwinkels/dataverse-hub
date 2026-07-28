#!/usr/bin/env bash
#
# fake-publish.sh — stand-in for scripts/publish-v2.sh in the Go tests.
#
# Same contract as the real publisher: takes one argument, the path to a
# signed envelope JSON file, and reports success with exit 0. Everything
# else is controlled by the environment so a test can drive the full
# behaviour matrix without a Freenet node anywhere in sight.
#
#   FAKE_PUBLISH_LOG    (required) append one line per invocation: the
#                       envelope contents, so tests can assert what was
#                       published and how often.
#   FAKE_PUBLISH_EXIT   exit with this code (default 0).
#   FAKE_PUBLISH_FAIL_TIMES
#                       fail the first N invocations with exit 3, then
#                       succeed — drives the retry/backoff tests.
#   FAKE_PUBLISH_SLEEP  sleep this many seconds before exiting — drives
#                       the timeout tests.
#   FAKE_PUBLISH_CHILD  spawn a background child that writes to this path
#                       after 2s. A timeout that only kills the direct
#                       child leaves it running and the file appears;
#                       a process-group kill means it never does.
#
set -uo pipefail

ENV_FILE="${1:-}"
LOG="${FAKE_PUBLISH_LOG:?FAKE_PUBLISH_LOG must be set}"

[ -n "$ENV_FILE" ] || { echo "usage: fake-publish.sh <envelope.json>" >&2; exit 64; }
[ -f "$ENV_FILE" ] || { echo "envelope not found: $ENV_FILE" >&2; exit 65; }

# Record the invocation (one compact line per call).
tr -d '\n' < "$ENV_FILE" >> "$LOG"
printf '\n' >> "$LOG"

echo "fake-publish: received $ENV_FILE"
echo "fake-publish: stderr line" >&2

if [ -n "${FAKE_PUBLISH_CHILD:-}" ]; then
    ( sleep 2; echo "orphan survived" > "$FAKE_PUBLISH_CHILD" ) &
fi

if [ -n "${FAKE_PUBLISH_SLEEP:-}" ]; then
    sleep "$FAKE_PUBLISH_SLEEP"
fi

if [ -n "${FAKE_PUBLISH_FAIL_TIMES:-}" ]; then
    calls=$(wc -l < "$LOG")
    if [ "$calls" -le "$FAKE_PUBLISH_FAIL_TIMES" ]; then
        echo "fake-publish: simulated failure (call $calls)" >&2
        exit 3
    fi
fi

exit "${FAKE_PUBLISH_EXIT:-0}"
