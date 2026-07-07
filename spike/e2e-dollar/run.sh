#!/usr/bin/env bash
# e2e: a postgres password containing literal '$' survives gobackup's whole-file
# os.ExpandEnv because the supervisor escapes each bare '$' to ${GB_DOLLAR} and
# injects GB_DOLLAR=$ into the engine. Proof = pg_dump authenticates and the
# backup contains the seeded row (a mangled password would fail auth).
set -euo pipefail
cd "$(dirname "$0")"
CF="docker-compose.yml"
dc() { docker compose -f "$CF" "$@"; }
gb() { docker ps -q --filter "label=gobackup-docker.component=gobackup" | head -1; }

echo "== 0. clean slate =="
dc down -v --remove-orphans >/dev/null 2>&1 || true

echo "== 1. up (postgres w/ \$-password + gobackup + supervisor) =="
dc up -d

echo "== 2. wait for the engine to be recreated with the GB_DOLLAR sentinel =="
eng=""
for i in $(seq 1 60); do
  id=$(gb)
  if [ -n "$id" ] && docker inspect "$id" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null | grep -Fxq 'GB_DOLLAR=$'; then
    eng="$id"; break
  fi
  sleep 1
done
if [ -z "$eng" ]; then
  echo "   ❌ GB_DOLLAR sentinel never injected into the engine"; dc logs gobackup-docker | tail -30; exit 1
fi
echo "   ✅ engine $eng has the sentinel:"
docker inspect "$eng" --format '{{range .Config.Env}}{{if eq . "GB_DOLLAR=$"}}     {{.}}{{end}}{{end}}'

echo "== 3. verify the config escaped the literal \$ (no raw m9qq!\$7v) =="
docker exec "$eng" sh -c 'grep -n "password" /etc/gobackup/gobackup.yml' | sed 's/^/   /'
if docker exec "$eng" sh -c 'grep -q "m9qq!\${GB_DOLLAR}7v" /etc/gobackup/gobackup.yml'; then
  echo "   ✅ password rendered as m9qq!\${GB_DOLLAR}7v...  (sentinel escape)"
else
  echo "   ❌ password not escaped as expected"; docker exec "$eng" cat /etc/gobackup/gobackup.yml; exit 1
fi

echo "== 4. wait for postgres to accept connections =="
for i in $(seq 1 30); do
  docker exec gbde2edollar-db-1 pg_isready -U shopuser -d shopdb >/dev/null 2>&1 && break
  sleep 1
done

echo "== 5. perform the backup — pg_dump auth proves the \$-password decoded correctly =="
docker exec "$eng" /usr/local/bin/gobackup perform -m shop -c /etc/gobackup/gobackup.yml 2>&1 \
  | grep -iE "Dump|PostgreSQL|Compress|Store succeeded|Perform|FATAL|password authentication" | sed 's/^/   /' || true

echo "== 6. verify the dump archive contains the seeded row =="
docker exec "$eng" sh -c '
  set -e
  ls -la /backups/shop/
  arc=$(ls /backups/shop/*.tar.gz | head -1)
  echo "--- scanning $arc ---"
  tar -xzOf "$arc" | grep -a "dollar-e2e-ok" && echo "SEEDED ROW FOUND → literal-\$ password authenticated"
' | sed 's/^/   /'

echo "== DONE (tear down: docker compose -f $CF down -v; docker rm -f \$(docker ps -aq --filter label=gobackup-docker.component=gobackup)) =="
