#!/usr/bin/env bash
# Reactive proof: no manual `perform`. Add a labeled container to a running
# stack; the supervisor renders it and gobackup's own cron backs it up.
set -euo pipefail
cd "$(dirname "$0")"
CF="docker-compose.yml"
dc() { docker compose -f "$CF" "$@"; }

echo "== 0. clean slate =="
dc --profile addlater down -v --remove-orphans >/dev/null 2>&1 || true

echo "== 1. start supervisor + gobackup (no db yet) =="
dc up -d gobackup-docker gobackup

echo "== 2. wait for supervisor to write config (empty models) =="
for i in $(seq 1 40); do
  dc exec -T gobackup-docker sh -c 'cat /etc/gobackup/gobackup.yml 2>/dev/null' | grep -q 'models' && break
  sleep 0.5
done
dc exec -T gobackup-docker sh -c 'cat /etc/gobackup/gobackup.yml' | sed 's/^/   /'

echo "== 3. ADD labeled postgres to the running stack (reactive discovery) =="
dc --profile addlater up -d postgres

echo "== 4. wait for supervisor to render model 'pgtest' reactively =="
for i in $(seq 1 40); do
  dc exec -T gobackup-docker sh -c 'cat /etc/gobackup/gobackup.yml' | grep -q 'pgtest' && break
  sleep 0.5
done
echo "   rendered config now contains pgtest:"
dc exec -T gobackup-docker sh -c 'cat /etc/gobackup/gobackup.yml' | sed 's/^/     /'

echo "== 5. wait for postgres ready + seed data =="
for i in $(seq 1 60); do dc exec -T postgres pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
dc exec -T postgres psql -U postgres -d appdb -c \
  "CREATE TABLE IF NOT EXISTS items(id serial primary key, name text); INSERT INTO items(name) VALUES ('alpha'),('beta'),('gamma');" >/dev/null
echo "   seeded 3 rows into appdb.items"

echo "== 6. confirm gobackup registered the cron for pgtest (fsnotify reload) =="
for i in $(seq 1 30); do dc logs gobackup 2>&1 | grep -q 'Register pgtest' && break; sleep 1; done
dc logs gobackup 2>&1 | grep -iE 'Register pgtest' | tail -1 | sed 's/^/   /'

echo "== 7. WAIT for an AUTOMATIC (scheduler-driven) backup — no manual perform =="
found=""
for i in $(seq 1 45); do
  if dc exec -T gobackup sh -c 'ls /backups/pgtest/*.tar.gz 2>/dev/null' | grep -q 'tar.gz'; then found=1; break; fi
  sleep 3
done
if [ -z "$found" ]; then
  echo "   ❌ no scheduled backup within timeout"; dc logs gobackup 2>&1 | tail -30; exit 1
fi
echo "   ✅ scheduled backup produced:"
dc exec -T gobackup sh -c 'ls -la /backups/pgtest/' | sed 's/^/     /'
echo "   -- gobackup log (scheduler ran the dump; we never called perform) --"
dc logs gobackup 2>&1 | grep -iE 'Dump succeeded|Store succeeded /backups' | tail -4 | sed 's/^/     /'

echo "== DONE (tear down: docker compose -f $CF --profile addlater down -v) =="
