#!/usr/bin/env bash
# Start the CI test services after the job has installed its locked daemon.
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
bash "$root/scripts/ci/require-docker-engine.sh"

docker run -d --name concord-ci-postgres \
  -e POSTGRES_USER=concord -e POSTGRES_PASSWORD=test_password -e POSTGRES_DB=concord_test \
  -p 127.0.0.1:5432:5432 \
  --health-cmd 'pg_isready -U concord -d concord_test' \
  --health-interval 5s --health-timeout 5s --health-retries 5 postgres:16-alpine
docker run -d --name concord-ci-redis -p 127.0.0.1:6379:6379 \
  --health-cmd 'redis-cli ping' \
  --health-interval 5s --health-timeout 5s --health-retries 5 redis:8-alpine
# NATS makes the voice-enforcement integration tests execute rather than skip.
docker run -d --name concord-ci-nats -p 127.0.0.1:4222:4222 \
  --health-cmd 'nc -z 127.0.0.1 4222 || exit 1' \
  --health-interval 5s --health-timeout 5s --health-retries 5 nats:2-alpine

for service in postgres redis nats; do
  healthy=false
  for _ in {1..60}; do
    health=$(docker inspect --format '{{.State.Health.Status}}' "concord-ci-$service")
    case "$health" in
      healthy) healthy=true; break ;;
      starting) sleep 1 ;;
      *) printf 'error: CI service did not become healthy\n' >&2; exit 1 ;;
    esac
  done
  [[ $healthy == true ]] || { printf 'error: CI service readiness timed out\n' >&2; exit 1; }
done
