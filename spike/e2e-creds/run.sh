#!/usr/bin/env bash
# e2e: DB credential from a Docker secret file. The supervisor re-mounts the
# secret into the recreated gobackup engine and reads it via the command wrapper;
# the daemon's scheduled backup authenticates to postgres, and the secret value
# never appears in docker inspect.
set -euo pipefail
cd "$(dirname "$0")"
CF="docker-compose.yml"
dc() { docker compose -f "$CF" "$@"; }
gb() { docker ps -q --filter "label=gobackup-docker.component=gobackup" | head -1; }

echo "== 0. clean =="
dc down -v --remove-orphans >/dev/null 2>&1 || true

echo "== 1. up =="
dc up -d

echo "== 2. wait for supervisor to recreate gobackup with the secret bind-mounted under /gobackup-secrets/ =="
mounted=""
for i in $(seq 1 60); do
  id=$(gb)
  if [ -n "$id" ] && docker inspect "$id" --format '{{range .Mounts}}{{.Destination}} {{end}}' 2>/dev/null | grep -q "/gobackup-secrets/"; then
    mounted="$id"; break
  fi
  sleep 1
done
[ -z "$mounted" ] && { echo "   ❌ secret never mounted into gobackup"; dc logs gobackup-docker | tail -30; exit 1; }
echo "   ✅ gobackup $mounted has the secret mount:"
docker inspect "$mounted" --format '{{range .Mounts}}   - {{.Source}} -> {{.Destination}} (ro={{not .RW}}){{"\n"}}{{end}}' | grep gobackup-secrets
echo "   wrapped command:"; docker inspect "$mounted" --format '{{json .Config.Cmd}}' | sed 's/^/     /'

echo "== 3. CRITICAL: the secret VALUE must NOT appear in docker inspect =="
if docker inspect "$mounted" | grep -q "s3cr3t-from-file"; then
  echo "   ❌ secret value leaked into docker inspect!"; exit 1
fi
echo "   ✅ 0 occurrences of the secret value in inspect (only the path is present)"

echo "== 4. wait for postgres, seed data =="
for i in $(seq 1 60); do dc exec -T postgres pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
dc exec -T postgres psql -U postgres -d appdb -c \
  "CREATE TABLE IF NOT EXISTS t(id serial, v text); INSERT INTO t(v) VALUES ('cred-e2e-ok');" >/dev/null
echo "   seeded"

echo "== 5. wait for the DAEMON's scheduled backup (auth uses the wrapper-injected secret) =="
archive=""
for i in $(seq 1 50); do
  f=$(docker exec "$(gb)" sh -c 'ls /backups/pgcreds/*.tar.gz 2>/dev/null | head -1' || true)
  if [ -n "$f" ]; then archive="$f"; break; fi
  sleep 3
done
[ -z "$archive" ] && { echo "   ❌ no scheduled backup — auth likely failed"; docker logs "$(gb)" 2>&1 | grep -iE "postgres|error|fail|denied|password" | tail -15; exit 1; }
echo "   ✅ backup produced: $archive"

echo "== 6. verify the dump authenticated and captured the seeded row =="
docker exec "$(gb)" sh -c "tar -xzOf '$archive' | grep -a 'cred-e2e-ok' && echo '   ROW FOUND (postgres auth via secret succeeded)'" | sed 's/^/   /'

echo "== DONE (tear down: docker compose -f $CF down -v; docker rm -f \$(docker ps -aq --filter label=gobackup-docker.component=gobackup)) =="
