#!/bin/sh
# Ship a tagged release to the managed appliance and PROVE it is live.
#
# "Committed", "tagged", "CI-green" and "live" are four different facts, and
# only the last one is worth anyone's testing time. This waits for the release
# image, pulls it on the box, and reports the version the site actually serves.
#
#   deploy/ship.sh            # ship whatever tag HEAD carries
#   deploy/ship.sh v1.11.9
set -eu
TAG="${1:-$(git describe --tags --exact-match HEAD 2>/dev/null || echo '')}"
[ -n "$TAG" ] || { echo "no tag on HEAD - pass one explicitly" >&2; exit 1; }
HOST="${LS_HOST:-ubuntu@44.198.69.248}"
KEY="${LS_KEY:-$HOME/.ssh/Ron-Pinkas-AWS-1.pem}"
SITE="${LS_SITE:-https://connect.instantaiguru.com}"

# Wait for THIS tag's run, selected by headBranch. Taking "the most recent
# release run" instead was silently wrong: on a fresh tag the previous run is
# already complete, so the wait returned instantly, the pull got a stale
# `latest`, and compose reported the container merely "Running". That is
# exactly the tagged-but-not-live failure this script exists to prevent.
echo "waiting for the release build of ${TAG}"
run=""
i=0
while [ "$i" -lt 40 ]; do
  run=$(gh run list --workflow=release --limit 20 \
    --json databaseId,headBranch -q "[.[] | select(.headBranch == \"$TAG\")][0].databaseId")
  [ -n "$run" ] && break
  i=$((i + 1))
  sleep 5
done
[ -n "$run" ] || { echo "no release run found for $TAG - was the tag pushed?" >&2; exit 1; }
gh run watch "$run" --exit-status >/dev/null

echo "pulling the image on ${HOST}"
ssh -i "$KEY" "$HOST" \
  'cd /opt/appliance && sudo docker compose pull live-support && sudo docker compose up -d live-support' >/dev/null

# The served ?v= is substituted from the running binary, so it is the
# appliance's own statement of what it is serving, not what we hoped to ship.
live=""
i=0
while [ "$i" -lt 20 ]; do
  live=$(curl -fsS --max-time 15 "$SITE/" | /usr/bin/grep -o 'shared\.js?v=[^"]*' | head -1 | sed 's/.*v=//' || true)
  if [ "$live" = "$TAG" ]; then
    echo "live: $live"
    exit 0
  fi
  i=$((i + 1))
  sleep 3
done
echo "still serving ${live:-unknown}, expected ${TAG}" >&2
exit 1
