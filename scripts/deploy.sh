#!/bin/sh
set -eu
cd /opt/archive
test -f .env
test -n "${1:-}"
case "$1" in ghcr.io/*) ;; *) echo 'Expected a ghcr.io image' >&2; exit 1;; esac
set -a
. ./.env
set +a
export APP_IMAGE="$1"
docker stack deploy --with-registry-auth --prune -c deploy/stack.yaml archive
attempt=0
until curl --fail --silent http://127.0.0.1:8080/healthz >/dev/null; do
    attempt=$((attempt + 1))
    test "$attempt" -lt 30 || exit 1
    sleep 2
done
echo 'Stack submitted; checking application image convergence.'
for service in app worker; do
    expected=1
    test "$service" != app || expected=2
    attempt=0
    until docker service ps --no-trunc --filter desired-state=running --format '{{.Image}} {{.CurrentState}}' "archive_$service" | awk -v image="$APP_IMAGE" -v expected="$expected" '
        { count++; if (index($1,image) != 1 || $2 != "Running") bad=1 }
        END { exit !(count == expected && !bad) }'; do
        attempt=$((attempt + 1))
        test "$attempt" -lt 45 || exit 1
        sleep 2
    done
done
curl --fail --silent http://127.0.0.1:8080/healthz
