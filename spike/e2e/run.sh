#!/usr/bin/env bash
# End-to-end proof: a labeled Postgres container gets backed up by gobackup with
# a config generated entirely by the supervisor — no hand-written gobackup.yml.
set -euo pipefail
cd "$(dirname "$0")"
C="docker compose -f docker-compose.yml"

echo "== 0. clean slate =="
$C down -v --remove-orphans >/dev/null 2>&1 || true

echo "== 1. start postgres + supervisor (supervisor discovers the label & renders config) =="
$C up -d postgres gobackup-docker

echo "== 2. wait for supervisor to write /etc/gobackup/gobackup.yml with model 'pgtest' =="
for i in $(seq 1 40); do
  if $C exec -T gobackup-docker sh -c 'cat /etc/gobackup/gobackup.yml 2>/dev/null' | grep -q 'pgtest'; then
    echo "   config rendered:"; $C exec -T gobackup-docker sh -c 'cat /etc/gobackup/gobackup.yml' | sed 's/^/     /'
    break
  fi
  $C exec -T gobackup-docker sleep 0.5 >/dev/null 2>&1 || sleep 0.5
  [ "$i" = 40 ] && { echo "   TIMEOUT waiting for config"; $C logs gobackup-docker; exit 1; }
done

echo "== 3. start gobackup (loads the generated config) =="
$C up -d gobackup

echo "== 4. wait for postgres ready, seed data =="
for i in $(seq 1 40); do
  if $C exec -T postgres pg_isready -U postgres >/dev/null 2>&1; then break; fi
  $C exec -T postgres sleep 0.5 >/dev/null 2>&1 || true
done
$C exec -T postgres psql -U postgres -d appdb -c \
  "CREATE TABLE IF NOT EXISTS items(id serial primary key, name text); INSERT INTO items(name) VALUES ('alpha'),('beta'),('gamma');" >/dev/null
echo "   seeded 3 rows into appdb.items"

echo "== 5. trigger a one-shot backup via gobackup (deterministic, no waiting for cron) =="
$C exec -T gobackup /usr/local/bin/gobackup perform -m pgtest -c /etc/gobackup/gobackup.yml 2>&1 | sed 's/^/   /'

echo "== 6. verify a backup artifact landed in local storage =="
$C exec -T gobackup sh -c 'ls -la /backups/pgtest/ && echo "---" && find /backups -type f' | sed 's/^/   /'

echo "== DONE. Tear down with: $C down -v =="
