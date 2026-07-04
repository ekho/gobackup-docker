#!/usr/bin/env bash
# e2e: supervisor mounts an app's sqlite DB volume READ-WRITE into the recreated
# gobackup engine and rewrites databases.main.path, so `sqlite3 <path> .dump`
# reads the file and the backup archive contains the seeded rows.
set -euo pipefail
cd "$(dirname "$0")"
CF="docker-compose.yml"
dc() { docker compose -f "$CF" "$@"; }

# Current gobackup container (recreated ones are found by the component label).
gb() { docker ps -q --filter "label=gobackup-docker.component=gobackup" | head -1; }

echo "== 0. clean slate =="
dc down -v --remove-orphans >/dev/null 2>&1 || true

echo "== 1. up (app seeds sqlite DB + gobackup + supervisor) =="
dc up -d

echo "== 2. wait for supervisor to recreate gobackup WITH the sqlite volume mounted at /volumes/shop/app/data =="
mounted=""
for i in $(seq 1 60); do
  id=$(gb)
  if [ -n "$id" ]; then
    if docker inspect "$id" --format '{{range .Mounts}}{{.Destination}} {{end}}' 2>/dev/null | grep -q "/volumes/shop/app/data"; then
      mounted="$id"; break
    fi
  fi
  sleep 1
done
if [ -z "$mounted" ]; then
  echo "   ❌ sqlite volume never mounted into gobackup"; dc logs gobackup-docker | tail -30; exit 1
fi

echo "== 3. verify the sqlite mount is READ-WRITE (sqlite3 .dump opens the DB + WAL sidecars) =="
rw=$(docker inspect "$mounted" --format '{{range .Mounts}}{{if eq .Destination "/volumes/shop/app/data"}}{{.RW}}{{end}}{{end}}')
docker inspect "$mounted" --format '{{range .Mounts}}   - {{.Source}} -> {{.Destination}} (rw={{.RW}}){{"\n"}}{{end}}'
if [ "$rw" != "true" ]; then
  echo "   ❌ sqlite volume mounted read-only (rw=$rw); .dump would fail on WAL DBs"; exit 1
fi
echo "   ✅ mounted read-write"

echo "== 4. verify the generated config rewrote databases.main.path to the mounted location =="
path=""
for i in $(seq 1 20); do
  path=$(docker exec "$mounted" sh -c "cat /etc/gobackup/gobackup.yml 2>/dev/null" | grep -A6 'main:' | grep 'path:' | head -1 | awk '{print $2}' || true)
  [ -n "$path" ] && break
  sleep 1
done
echo "   databases.main.path = $path"
if [ "$path" != "/volumes/shop/app/data/bot_database.sqlite3" ]; then
  echo "   ❌ path not rewritten to the mounted location"; docker exec "$mounted" cat /etc/gobackup/gobackup.yml; exit 1
fi
echo "   ✅ path rewritten"

echo "== 5. perform the sqlite backup and verify the archive contains the seeded row =="
docker exec "$mounted" /usr/local/bin/gobackup perform -m shop -c /etc/gobackup/gobackup.yml 2>&1 \
  | grep -iE "Dump|sqlite|Compress|Store succeeded|Perform" | sed 's/^/   /' || true

docker exec "$mounted" sh -c '
  set -e
  ls -la /backups/shop/
  arc=$(ls /backups/shop/*.tar.gz | head -1)
  echo "--- scanning $arc ---"
  tar -xzOf "$arc" | grep -a "sqlite-e2e-ok" && echo "SEEDED ROW FOUND IN DUMP"
' | sed 's/^/   /'

echo "== DONE (tear down: docker compose -f $CF down -v; docker rm -f \$(docker ps -aq --filter label=gobackup-docker.component=gobackup)) =="
