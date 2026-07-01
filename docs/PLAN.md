# gobackup-docker — план реализации

**База:** [ARCHITECTURE.md](ARCHITECTURE.md) · **Дата:** 2026-07-01

**Зафиксированные решения:**
1. **Топология** — план топология-агностичен выше границы «apply»; выбор A (один контейнер) vs B (два контейнера)
   решает **phase-0 спайк** про надёжность fsnotify-поверх-volume.
2. **Scope v1** — полный: dump БД (по сети) + файловые бэкапы (archive) + notifiers + compress/encrypt/split/schedule.
3. **Доступ к Docker** — `client.FromEnv` + `WithAPIVersionNegotiation()`; поддержка rootless и удалённого `DOCKER_HOST`
   (путь сокета не хардкодим).

Язык — **Go** (совпадает с gobackup и Docker SDK). Обёртка **не** реализует пайплайн бэкапа — только генерирует конфиг.

---

## Целевой layout пакета

```
gobackup-docker/
  cmd/gobackup-docker/main.go     # entrypoint: env/flags → wire всех компонентов, graceful shutdown
  internal/
    docker/
      client.go        # NewClientWithOpts(FromEnv, WithAPIVersionNegotiation); Close
      watcher.go       # initial ContainerList + Events(ctx) loop, backoff-reconnect, Since-replay
    labels/
      parse.go         # gobackup.* → nested map[string]any; enable-gating; instance-scope; мягкий bool
      vocab.go         # константы префиксов, известные под-ключи, валидация
    render/
      profiles.go      # загрузка defaults.yml (yaml.v3, anchors резолвятся при парсинге), кэш, last-good
      model.go         # контейнер → []Model: deep-merge профиля + лейблов + opt-out "!none"; префикс имени
      render.go        # []Model → gobackup-config map → yaml.v3 байты; проход text/template ({{ .Model }})
    pipeline/
      debounce.go      # ring-buffer-of-1 (keep-latest) + таймер троттлинга
      reconcile.go     # events → debounce → render → dedup(DeepEqual) → apply
    apply/
      applier.go       # интерфейс Applier { Apply(cfg []byte) error }
      filewatch.go     # пре-валидация → атомарная запись (temp→fsync→rename) → gobackup сам reload (Вариант B)
      restart.go       # (опц.) ContainerRestart через SDK — только аварийный ручной триггер, НЕ штатный apply
    webapi/
      client.go        # (опц.) прокси к gobackup :2703 — /api/config, /api/perform (control-plane)
  spike/
    reload_test/       # phase-0 спайк ✅ (RESULTS.md): reload через volume надёжен → Вариант B
  Dockerfile           # supervisor (B) или combined (A)
  docker-compose.example.yml
  go.mod
```

---

## Фазы

### Phase 0 — Спайк: надёжность hot-reload ✅ ВЫПОЛНЕНО → Вариант B
Проверено на gobackup 3.1.0 / OrbStack (`spike/reload_test/RESULTS.md`):
- **fsnotify reload cross-container через общий named-volume работает** — и in-place, и atomic rename, 16/16 подряд, мгновенно.
  Bind-mount тоже сработал на OrbStack (но зависит от file-sharing слоя Docker-хоста → в проде предпочитаем named-volume).
- **Битый/частичный конфиг** не роняет gobackup, но **обнуляет модели** (last-good не держит) → apply обязан быть
  **атомарным + пре-валидированным**, супервизор держит свой last-good.
- **SIGHUP в режиме `run` убивает процесс** (reload только в daemon `start`) → apply = **только запись файла**, не сигнал, в обоих A/B.

**Итог:** apply = атомарная запись корректного `gobackup.yml` на общий volume → gobackup сам перечитывает.

### Phase 1 — Ядро discovery + render (топология-агностично, ~основной объём)
1. **`docker/client.go`** — клиент через `FromEnv` + negotiation; корректный `Close`.
2. **`docker/watcher.go`** — стартовый `ContainerList(All:true, filter label=gobackup.enable=true)` →
   поток `Events(ctx, {type=container, event=start|die})`; `select` по `Messages`/`Err`/`ctx.Done()`;
   exponential backoff + ре-`Events` при разрыве; `Since` для реплея пропущенных событий.
   Читаем лейблы из `msg.Actor.Attributes` (без inspect, где хватает); `ContainerInspect` для `Mounts[]` при файловых бэкапах.
3. **`labels/parse.go`** — из плоской `map[string]string` вытащить `gobackup.*` (одна модель на контейнер); мета-ключи
   `enable`/`name`/`instance`/`profile`; остальные `gobackup.<config.path>` → вложенный `map[string]any`; sentinel `"!none"`;
   мягкий bool. **Декодер stateless** — не знает про `defaults.yml`.
4. **`render/`** — DRY-механизм (см. ARCHITECTURE §5): `profiles.go` грузит `defaults.yml`; `model.go` deep-merge'ит
   профиль (низ) + лейблы (верх) + применяет opt-out `"!none"` как удаление поддерева; префиксование имён
   (`<container|instance>-<model>`); `render.go` — проход `text/template` (`{{ .Model }}` и т.п.) по слитому дереву,
   затем маршалинг `yaml.v3` (`SetIndent(2)`); `${VAR}` не трогаем (развернёт gobackup).
5. **`pipeline/`** — debounce (ring-buffer-of-1 + таймер) для схлопывания шквала `docker compose up`;
   dedup через `reflect.DeepEqual` vs last-applied; оркестрация `(события Docker ∪ изменение defaults.yml) → debounce →
   render → apply`. **Два источника fsnotify:** Docker events и `defaults.yml`; битый static-файл = no-op, last-good.

**Критерий:** юнит-тесты парсера/рендера зелёные; на живом Docker поднятие/остановка контейнеров даёт корректный
сгенерированный YAML; шквал событий схлопывается в одну регенерацию.

### Phase 2 — Слой apply (Вариант B, apply = запись файла)
- **`apply/applier.go`** — интерфейс `Applier { Apply(cfg []byte) error }`.
- **`filewatch.go`** — единственный apply: **пре-валидация** отрендеренного конфига → атомарная запись
  (`CreateTemp` в тот же каталог → `Sync` → `Rename`) на общий volume → дальше ничего (gobackup сам перечитает через fsnotify).
  Держим supervisor-side **last-good**: если рендер невалиден/пуст — не пишем, оставляем предыдущий и логируем.
- **SIGHUP не используем** (в `run` убивает процесс — см. Phase 0). Даже при упаковке как A дочерний `gobackup run` перечитывает файл сам.
- (опц.) `restart.go` — `ContainerRestart` через SDK как аварийный ручной триггер, не штатный путь.

**Критерий:** изменение лейбла на живом контейнере приводит к применённому бэкап-конфигу без ручных действий и без рестарта gobackup.

### Phase 3 — Полное покрытие типов (scope = всё)
- **databases:** все типы gobackup (postgresql/mysql/mariadb/mongodb/redis/mssql/sqlite/influxdb2/etcd/firebird) —
  прокинуть их специфичные ключи как есть; путь **сеть+DSN** по умолчанию (`host` = имя сервиса в общей сети).
- **storages:** local/s3-семейство/gcs/azure/webdav/ftp/scp/sftp + `keep`, `path`, `default_storage`.
- **notifiers:** mail/webhook/slack/discord/telegram/healthchecks + `on_success`/`on_failure`.
- **compress_with / encrypt_with / split_with / schedule (cron|every+at)** — маппинг 1:1.
- **Файловые бэкапы (`archive`)** ⚠️ — работают по путям **внутри контейнера gobackup**. v1: поддержать `archive.includes/excludes`
  с явными in-container путями + **валидация**: если путь недоступен — пропустить модель с внятным логом. Целевые volume'ы
  должны быть **предмонтированы** в контейнер gobackup (ro). Через `ContainerInspect.Mounts` можно транслировать имя volume →
  точку монтирования и предупреждать, если volume не проброшен. Авто-remount (рестарт для hot-add mount) — **вне v1**, задокументировать.

**Критерий:** сквозной прогон для Postgres (dump в S3) и для файлового archive в local — оба проходят реальный `Perform()`.

### Phase 4 — Упаковка и деплой
- **Dockerfile** — B: минимальный образ супервизора (только Go-бинарь + Docker SDK); A: combined (супервизор + gobackup + утилиты БД + `tini`).
- **docker-compose.example.yml** + **defaults.yml** — сеть `backup_net`, `DOCKER_HOST`/сокет по env, `GOBACKUP_DIR` на
  volume (cycler-стейт), общий config-volume (B), `GOBACKUP_DOCKER_DEFAULTS` → `defaults.yml`, минимальные лейблы из ARCHITECTURE §5.
- **Конфиг обёртки (env):** `GOBACKUP_DOCKER_INSTANCE`, `..._EXPOSED_BY_DEFAULT`, `..._DEBOUNCE`, `..._CONFIG_PATH`,
  `..._MODE`, `DOCKER_HOST`. Секреты — `${VAR}` + `.env` + `_FILE`-конвенция (Docker secrets).
- **Web API overlay (опц.):** пробросить `/status`, `/metrics`, «Backup now» через `POST /api/perform`.

**Критерий:** `docker compose up` в примере поднимает связку, бэкап отрабатывает по расписанию, retention переживает рестарт.

### Phase 5 — Тесты и наблюдаемость
- Юнит: label-parser (dotted→tree, enable/instance-гейты, мягкий bool), renderer (детерминированный YAML, коллизии имён),
  dedup, debounce (шквал → один apply).
- Интеграция: реальный Docker — `up`/`down`/label-change → корректный конфиг; спайк из Phase 0 как регресс-тест.
- Наблюдаемость: структурные логи, `--log-level`, healthcheck, graceful shutdown (ctx cancel → stop watcher/child),
  устойчивость к битым лейблам (skip + log, не падать); проксирование `/metrics` gobackup.

---

## Точки, где твой выбор формирует поведение (обсудим на реализации)
1. **Стратегия имён/коллизий моделей** — `<container>-<model>` vs `<instance>-<container>-<model>` vs хэш; что делать при
   дубликате имён между контейнерами.
2. **Семантика debounce** — длительность (150ms…2s) и триггеры (только `start`/`die` или ещё `health_status`).
3. **Гейтинг включения** — `exposedByDefault=false` (строгий opt-in, как Traefik по умолчанию рекомендует) vs `true`.
4. **Поведение при невалидном/битом конфиге** — не перезаписывать last-good, или писать и полагаться на last-good gobackup.

## Порядок работ
`Phase 0 (спайк) ∥ Phase 1 (ядро)` → `Phase 2 (apply)` → `Phase 3 (типы)` → `Phase 4 (упаковка)` → `Phase 5 (тесты/ops)`.
Первый вертикальный срез (walking skeleton): discovery одного Postgres-контейнера → render → apply → реальный dump в local storage.
