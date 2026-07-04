#!/usr/bin/env bash
# e2e: supervisor recreates the gobackup container with an app's data volume
# auto-mounted, driven by gobackup.archive.includes + gobackup_container.* labels.
set -euo pipefail
cd "$(dirname "$0")"
CF="docker-compose.yml"
dc() { docker compose -f "$CF" "$@"; }

# Current gobackup container (recreated ones are found by the component label).
gb() { docker ps -q --filter "label=gobackup-docker.component=gobackup" | head -1; }

echo "== 0. clean slate =="
dc down -v --remove-orphans >/dev/null 2>&1 || true

echo "== 1. up (app + gobackup + supervisor) =="
dc up -d

echo "== 2. wait for supervisor to recreate gobackup WITH the app volume mounted at /volumes/app/data =="
mounted=""
for i in $(seq 1 60); do
  id=$(gb)
  if [ -n "$id" ]; then
    if docker inspect "$id" --format '{{range .Mounts}}{{.Destination}} {{end}}' 2>/dev/null | grep -q "/volumes/app/data"; then
      mounted="$id"; break
    fi
  fi
  sleep 1
done
if [ -z "$mounted" ]; then
  echo "   ❌ archive volume never mounted into gobackup"; dc logs gobackup-docker | tail -30; exit 1
fi
echo "   ✅ gobackup container $mounted has the archive mount:"
docker inspect "$mounted" --format '{{range .Mounts}}   - {{.Source}} -> {{.Destination}} (ro={{not .RW}}){{"\n"}}{{end}}'

echo "== 3. verify recreated container used the gobackup_container.* spec =="
echo "   image:   $(docker inspect "$mounted" --format '{{.Config.Image}}')"
echo "   command: $(docker inspect "$mounted" --format '{{json .Config.Cmd}}')"
echo "   base mounts preserved (cfg/backups):"
docker inspect "$mounted" --format '{{range .Mounts}}{{.Destination}} {{end}}' | tr ' ' '\n' | grep -E '/etc/gobackup|/backups' | sed 's/^/     /'

echo "== 4. wait for config to load in the recreated container, then trigger a backup =="
for i in $(seq 1 20); do
  docker exec "$mounted" sh -c 'test -f /etc/gobackup/gobackup.yml' 2>/dev/null && break
  sleep 1
done
docker exec "$mounted" /usr/local/bin/gobackup perform -m app -c /etc/gobackup/gobackup.yml 2>&1 | grep -iE "Dump|Archive|Compress|Store succeeded|Perform" | sed 's/^/   /' || true

echo "== 5. verify the archive exists and contains the seeded file =="
docker exec "$mounted" sh -c 'ls -la /backups/app/ && echo "---" && tar -xzOf $(ls /backups/app/*.tar.gz | head -1) | grep -a hello-e2e && echo "MARKER FOUND"' | sed 's/^/   /'

echo "== DONE (tear down: docker compose -f $CF down -v; docker rm -f \$(docker ps -aq --filter label=gobackup-docker.component=gobackup)) =="
