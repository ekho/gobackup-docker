# gobackup-docker — архитектурный бриф

**Статус:** research + Phase-0 спайк done → **Вариант B подтверждён** · **Дата:** 2026-07-01

Цель: обёртка вокруг [gobackup](https://github.com/gobackup/gobackup), которая
следит за Docker-контейнерами, читает лейблы `gobackup.*`, генерирует конфиг gobackup и заставляет
gobackup применить его — без ручного редактирования `gobackup.yml`.

---

## 1. Ключевые факты о gobackup (проверено по исходникам `main`)

Это факты, которые ограничивают дизайн. Все подтверждены чтением реального кода репозитория.

### 1.1 Режимы запуска (`main.go`, `urfave/cli/v2`)
- **`perform`** — выполняет пайплайн **один раз** для указанных моделей (или всех) и выходит. Без шедулера.
- **`run`** — долгоживущий foreground-процесс: `scheduler.Start()` (кронер на `go-co-op/gocron`) + блокировка
  `select{}`; если `web.enabled` — поднимает Gin HTTP API. **Именно это дружелюбно к контейнеру** (официальный образ:
  `CMD ["gobackup", "run"]`).
- **`start`** — тот же `run`, но демонизированный через `sevlyar/go-daemon` (double-fork + PID-файл).
  **Не использовать как PID 1 контейнера.**

### 1.2 Конфиг: один файл, first-match-wins, БЕЗ merge ⚠️
Порядок поиска, если не передан `--config/-c`:
`./gobackup.yml` → `$HOME/.gobackup/gobackup.yml` → `/etc/gobackup/gobackup.yml`.
**Побеждает первый найденный файл; файлы не мёржатся, каталога сниппетов нет.**
→ Следствие: обёртка обязана **рендерить весь многомодельный конфиг целиком в один YAML**. Нельзя раскладывать
куски по контейнерам.

Форма (двухуровневое дерево):
```yaml
web: {host, port, username, password, enabled}   # по умолчанию 0.0.0.0:2703, enabled=true
models:
  <model-name>:
    schedule: {cron | every + at}
    compress_with: {type, filename_format}
    encrypt_with: {type, password, ...}
    split_with: {chunk_size, ...}
    archive: {includes, excludes}
    databases: {<id>: {type, host, port, ...}}   # type: — дискриминатор фабрики
    storages:  {<id>: {type, keep, path, ...}}    # type: — дискриминатор фабрики
    default_storage
    notifiers: {<id>: {type, on_success, on_failure}}
    before_script / after_script
```
Валидация: у модели должно быть `databases` ИЛИ `archive`, И минимум один `storage`. У каждого элемента
`databases/storages/notifiers` обязателен `type:`.

Поддерживается подстановка **`${VAR}` / `$VAR`** (`os.ExpandEnv` по сырым байтам файла) + автозагрузка соседнего
`.env` (`godotenv`). → Секреты отдаём как `${VAR}`-плейсхолдеры и прокидываем через env контейнера, не зашивая в YAML.

### 1.3 Hot-reload — gobackup сам перечитывает конфиг 🔑 (проверено спайком, см. `spike/reload_test/RESULTS.md`)
- **fsnotify**: `viper.WatchConfig()` + `OnConfigChange` → `loadConfig()` → `scheduler.Restart()`.
  Переписали файл → модели и расписание перечитались, процесс не трогали. **Спайк (gobackup 3.1.0, OrbStack):**
  работает cross-container через общий named-volume, и для in-place, и для **атомарного rename** (16/16 подряд, мгновенно).
  Устойчиво потому, что viper watch'ит **каталог** конфига, а не инод файла — rename не теряет watch.
- **SIGHUP**: ⚠️ **только в daemon-режиме `start`**. Спайк показал: в режиме `run` SIGHUP **убивает** процесс (exit 2),
  reload'а нет. → Для нас (и в A, и в B) reload = **только запись файла**, не сигнал.
- **Битый/частично записанный конфиг:** процесс не падает, но модели обнуляются в `{}` (**last-good НЕ сохраняется**);
  невалидная модель (без storage) — пропускается с логом. → **apply обязан быть атомарным + пре-валидированным**.

**Главный вывод: "apply" = атомарно записать корректный файл; gobackup сам подхватит через fsnotify.**

### 1.4 Web API (`:2703`, включён по умолчанию)
Gin-сервер. Маршруты: `GET /status`, `GET /metrics` (Prometheus), `GET /api/config` (**список моделей**),
`GET /api/list`, `GET /api/download`, `GET /api/log` (SSE), **`POST /api/perform {model}` → запуск бэкапа по требованию**.
Basic-auth только если заданы и `web.username`, и `web.password`.

### 1.5 Факты, влияющие на Docker-образ
- Пайплайн `model.Perform()`: `before_script → dump БД → archive → compress (ВСЕГДА, tar по умолчанию) →
  encrypt (openssl) → split → upload во ВСЕ storages`; `after_script` + очистка temp через defer.
- gobackup **шеллит нативные утилиты**: `pg_dump`/`pg_dumpall`, `mysqldump`, `mongodump`, redis, mssql, influxd,
  etcdctl + `tar`/`split`/`openssl`. **Образ должен содержать клиентские бинарники нужных БД** — главная забота сборки.
- БД: mysql, mariadb, redis, postgresql, mongodb, sqlite, mssql, influxdb2, etcd, firebird.
  Storages: local, webdav, ftp, scp, sftp, gcs, azure, S3-семейство (s3, oss, minio, b2, cos, r2, spaces, obs, tos, ...).
- **Retention (`keep`)**: состояние в `~/.gobackup/cycler/<model>_<storage>.json` + зеркалится на remote в
  `.gobackup-state/<name>.json` (специально чтобы `keep` пережил эфемерные контейнеры). `keep=0` = хранить всё.
  → Либо монтировать volume на `GOBACKUP_DIR`, либо полагаться на remote-зеркало.

### 1.6 Остаётся проверить (см. риски)
- ~~Срабатывает ли viper `WatchConfig` на атомарный `rename` поверх volume?~~ ✅ **РЕШЕНО спайком** (§1.3): да, надёжно
  cross-container через named-volume. Bind-mount тоже сработал на OrbStack (но зависит от file-sharing слоя Docker-хоста).
- ~~Поведение при mid-write / битом YAML.~~ ✅ **РЕШЕНО:** не падает, но обнуляет модели (last-good не держит) → пишем атомарно + валидируем.
- Точный набор внешних утилит на каждый тип БД (подтверждён только postgresql) — снимается стоковым образом в B.

---

## 2. Паттерн label-discovery (discover → parse → generate → apply)

Цикл **discover → parse-labels → generate-config → apply** в одной watcher-горутине.

| Стадия | Как делаем |
|---|---|
| **Discover** | стартовый `ContainerList(All)` + подписка на поток `/events` (не поллинг) |
| **React** | реагируем на `start`/`die`, **полностью пересобираем конфиг**; события только регенерируют конфиг, **бэкапы остаются по расписанию** |
| **Reconnect** | connect→list→subscribe в exponential backoff (SDK сам не переподключается): `select` по `Messages`/`Err`/`ctx.Done()`, ре-`Events` |
| **Channel** | горутина рендерит весь конфиг → канал → dedup vs last-applied (`reflect.DeepEqual`) → атомарная запись |
| **Debounce** | ring-buffer-of-1 (keep-latest) + таймер: `docker compose up` со шквалом `start` схлопывается в одну регенерацию |
| **Opt-in** | `gobackup.enable=true` + глобальный `exposedByDefault` default + `gobackup.instance=<id>` scope |
| **Label decode** | dotted keys → `map[string]any` → `yaml.v3` (свой декодер) |

**Docker SDK:** классический `github.com/docker/docker/client` (пин, напр. `v28.x`, с `WithAPIVersionNegotiation()`;
**не** мешать с новым source-incompatible `github.com/moby/moby/client`).
`ContainerList(ctx, container.ListOptions{Filters: label=gobackup.enable=true})`;
`Events(ctx, {type=container, event=start|die})` → `msg.Actor.Attributes` уже содержит лейблы (часто без inspect);
`ContainerInspect` даёт `Mounts[]` для discovery volume'ов.

---

## 3. Варианты архитектуры (ось решения — механизм "apply")

### Вариант A — супервизор + gobackup как ДОЧЕРНИЙ процесс в одном контейнере
- Рендерим `gobackup.yml` атомарно (`CreateTemp`→`Sync`→`Rename`); запускаем `gobackup run` через `os/exec`
  (`Setpgid`); на изменение — полагаемся на fsnotify gobackup, fallback `Process.Signal(SIGHUP)`.
- **Один образ** (супервизор + gobackup + утилиты БД), один контейнер, монтируем `docker.sock:ro`, volume на `GOBACKUP_DIR`.
- ➕ единый деплой, SIGHUP тривиален (общий PID namespace). ➖ супервизор должен быть корректным PID 1
  (нужен `tini`/reaping); большой образ; падение супервизора уносит gobackup.

### Вариант B — супервизор в своём контейнере + общий config-volume (рекомендуется)
- Рендерим `gobackup.yml` на **общий named-volume**, смонтированный в оба контейнера.
- Apply: **ничего не делаем** — fsnotify gobackup подхватит с volume. Fallback: `ContainerRestart` через Docker API.
- **Два контейнера**: gobackup может быть **стоковым `huacnlee/gobackup`** (уже с утилитами БД и `gobackup run`);
  супервизор — крошечный отдельный образ. `docker.sock:ro` только у супервизора.
- ➕ чистое разделение, стоковый образ решает проблему §1.5, минимальная привилегированная поверхность.
  ➖ зависит от fsnotify-поверх-volume (риск §1.6); fallback-restart возвращает связность через Docker API.

### Вариант C — управление через Web API / как библиотека
- API `POST /api/perform` только **триггерит уже сконфигурированную** модель, **не определяет** новые.
  Встраивание как библиотеки хрупко (глобальные синглтоны конфига/шедулера).
- → C не самостоятелен: это **control-plane надстройка** над A/B (кнопка "Backup now", `/status`, `/metrics`).

**Ключевое:** всё выше границы "apply" (discovery, debounce, dedup, label-декодер, рендер YAML) **идентично** в A и B.
Поэтому выбор A/B откладывается на маленький phase-0 спайк.

---

## 4. Рекомендация

**Вариант B** (супервизор отдельным контейнером + общий config-volume, полагаемся на нативный hot-reload gobackup),
**Web API из C** как опциональный control-plane, **A** — запасной вариант упаковки, если спайк покажет ненадёжность
fsnotify-поверх-volume.

**Почему B:** опровергнутый тезис "нет hot-reload" делает apply тривиальным; single-file конфиг всё равно требует
рендерить документ целиком (volume — естественный носитель); стоковый образ gobackup снимает вопрос утилит БД;
`docker.sock` трогает только маленький супервизор.

**Phase-0 спайк ВЫПОЛНЕН → выбран Вариант B.** gobackup надёжно перечитывает конфиг с общего named-volume без сигнала
(§1.3, `spike/reload_test/RESULTS.md`, 16/16 rename). SIGHUP в режиме `run` убивает процесс, поэтому apply = запись файла
в обоих вариантах; даже при упаковке как A дочерний процесс перечитывает файл сам, а не по сигналу.

**Сквозные требования (для любого A/B):**
- discovery/debounce/dedup + атомарная запись файла;
- volume на `GOBACKUP_DIR` (или remote-storage для `.gobackup-state`);
- секреты через `${VAR}` + env/`.env`/`_FILE`;
- расписание декларативно в лейблах; события Docker только регенерируют конфиг, не запускают бэкап.

---

## 5. Схема лейблов + DRY-механизм (Global Defaults Profile)

Двухуровневая модель: static-конфиг супервизора + dynamic-лейблы контейнеров. **DRY решается не якорями в лейблах** (лейблы плоские, якорей между
ними нет), а **наследованием от общего профиля**, который живёт в static-конфиге супервизора. Итог DRY-выигрыша сильнее
YAML-anchors: контейнеру нужны **только `gobackup.enable` + его `databases.*`**, всё остальное (storages, notifiers,
schedule, compress, retention, path) наследуется и не повторяется ни на одном контейнере.

### 5.1 Static-конфиг супервизора — `defaults.yml`
Файл `GOBACKUP_DOCKER_DEFAULTS` (по умолчанию `/etc/gobackup-docker/defaults.yml`), монтируется **только в супервизор**,
gobackup его не читает. Содержит именованные **профили** — «тело модели по умолчанию». Внутри файла **можно использовать
YAML-anchors/`<<:`** (человеку удобно, `yaml.v3` разворачивает их при парсинге — миграция ваших `x-*` почти copy-paste).
Секреты остаются литеральным `${VAR}` — супервизор копирует их байт-в-байт, `os.ExpandEnv` **не вызывает**; разворачивает
уже gobackup при загрузке.

```yaml
# defaults.yml — написано ОДИН раз, наследуется всеми контейнерами
default:                              # имя профиля (профиль по умолчанию)
  schedule: { cron: "0 1 * * *" }
  compress_with: { type: tgz }
  default_storage: yc-object-storage
  storages:
    local_disk:
      type: local
      keep: 10
      path: /backups/{{ .Model }}     # плейсхолдер супервизора (НЕ ${...}!)
    yc-object-storage:
      type: s3
      bucket: media.kd
      region: ru-central1
      endpoint: https://storage.yandexcloud.net
      access_key_id: ${YC_OS_ACCESS_KEY}     # ${VAR} пройдёт насквозь в gobackup
      secret_access_key: ${YC_OS_SECRET_KEY}
      keep: 10
      path: /backups/{{ .Model }}
  notifiers:
    telegram:
      type: telegram
      chat_id: "66097481"
      token: ${TELEGRAM_BOT_TOKEN}
# можно добавить второй профиль, напр. `heavy:` с другим расписанием — контейнер выберет его через gobackup.profile
```

### 5.2 Лейблы контейнера — минимальная поверхность
**Одна модель на контейнер**, грамматика плоская: `gobackup.<config.path>` кладётся прямо в тело модели (без уровня
`.model.<M>.`). Контейнер nextcloud (эквивалент вашего `models.mynextcloud`) целиком:
```yaml
labels:
  gobackup.enable: "true"
  gobackup.name: "mynextcloud"                                   # имя модели (опц.; иначе <container>-<host>)
  gobackup.databases.nextcloud.type: "postgresql"
  gobackup.databases.nextcloud.host: "${NEXTCLOUD_POSTGRESQL_HOST}"
  gobackup.databases.nextcloud.database: "${NEXTCLOUD_POSTGRESQL_DATABASE}"
  gobackup.databases.nextcloud.password: "${NEXTCLOUD_POSTGRESQL_PASSWORD}"
  gobackup.databases.nextcloud.args: "--if-exists --clean --no-owner"
  gobackup.archive.includes: "/var/www/html/data"               # БД и файлы могут жить в одной модели
```
Супервизор deep-merge'ит профиль `default` под модель, разворачивает `{{ .Model }}` → `mynextcloud`, и эмитит
`models.mynextcloud` (schedule, compress_with, default_storage, оба storage с `path: /backups/mynextcloud`, notifiers) +
`databases`/`archive` из лейблов. Без `gobackup.name` имя = `<имя_контейнера>-<docker_host>` (напр. `gitea-orbstack`).
**Все значения лейблов — в кавычках** (Compose коэрсит `true/no/on`), булевы парсим мягко.

### 5.3 Грамматика лейблов
Зарезервированы четыре мета-ключа `gobackup.<key>`; всё остальное `gobackup.<путь>` идёт в тело модели.
| Лейбл | Значение |
|---|---|
| `gobackup.enable` | гейт включения (мягкий bool) + глобальный `exposedByDefault` |
| `gobackup.name` | имя модели (ключ в `models:` и `{{ .Model }}`); по умолчанию `<container>-<host>` |
| `gobackup.instance` | scope: контейнером управляет супервизор с совпадающим `GOBACKUP_DOCKER_INSTANCE` |
| `gobackup.profile` | какой профиль из `defaults.yml` взять базой (по умолчанию `default`) |
| `gobackup.<config.path>` | тело модели: `databases.<id>.<key>`, `archive.includes`, `storages.<id>.<key>`, `schedule.cron`, … (deep-merge поверх профиля) |
| `gobackup.<subtree>: "!none"` | opt-out от унаследованного поддерева: `notifiers: "!none"`, `storages.local_disk: "!none"` |

Хост-суффикс для авто-имени = hostname демона (`docker info` → `Name`), override — env `GOBACKUP_DOCKER_HOST_ID`.

Пример с авто-именем, оверрайдом и opt-out (gitea → модель `gitea-<host>`, 90 копий, без уведомлений):
```yaml
  gobackup.enable: "true"
  gobackup.databases.gitea.type: "postgresql"
  gobackup.databases.gitea.host: "${GITEA_DB_HOST}"
  gobackup.storages.yc-object-storage.keep: "90"   # override
  gobackup.notifiers: "!none"                       # opt-out
```

### 5.4 Порядок слияния (низ → верх)
1. встроенные дефолты gobackup (tar, web enabled, keep=0…) — только где ниже ничего не задано;
2. тело профиля из `defaults.yml` (`default` или указанный `gobackup.profile`);
3. лейблы `gobackup.<config.path>` — **deep-merge, выигрывают по ключу**;
4. sentinel `"!none"` — применяется последним, **удаляет** названное поддерево (что бы профиль/сосед ни задал);
5. разворот шаблонов `{{ .Model }}` — рендер-проход по уже слитому дереву (не источник значений).

### 5.5 Плейсхолдеры (почему НЕ `${...}`)
Токены Go `text/template` (`{{ .Model }}`, также `{{ .Container }}`, `{{ .Host }}`, `{{ .Instance }}`) разворачивает **супервизор до
записи файла**. Нельзя использовать `${MODEL}`: gobackup гоняет `os.ExpandEnv` по файлу и превратит `${MODEL}` в пустоту
(сочтёт незаданной env). Разделение по **синтаксису И времени**: `{{...}}` съедает супервизор, `${VAR}` (секреты) доходят
до gobackup нетронутыми. `path_template` настраивается в профиле, если нужен не `/backups/<model>`.

### 5.6 Крайние случаи и правила
- Неизвестный `gobackup.profile` → модель **пропускается с явным логом** (fail-closed, без тихого фолбэка).
- Модель, у которой opt-out убрал все storages → **отбрасывается с логом** (gobackup требует ≥1 storage).
- Супервизор **fsnotify-watch'ит и `defaults.yml`** (не только Docker-события); битый/непарсящийся файл = no-op, держим last-good.
- Мульти-инстанс: префиксование имени модели (`<container|instance>-<model>`) — **до** разворота `{{ .Model }}`.
- `.type` у `databases/storages/notifiers` обязателен (дискриминатор фабрики gobackup).

### 5.7 Доступ к БД и файловые бэкапы (без изменений)
**Два способа достучаться до БД:** (1) сеть+DSN (по умолчанию, `docker.sock` не нужен для чистых dump'ов БД, `host` = имя
сервиса в общей сети); (2) `docker exec` в контейнер БД (модель offen; нужен socket) — опция на потом.

**Файловые бэкапы (auto-mount, реализовано):** `archive` работает по путям **внутри контейнера gobackup**. Docker не
умеет hot-add mount, поэтому супервизор **пересоздаёт** контейнер gobackup: находит его по лейблу
`gobackup-docker.component=gobackup`, инспектит source-контейнеры с `gobackup.archive.includes`, находит их volume'ы,
переписывает includes в `/volumes/<model>/...` и пересоздаёт gobackup с этими volume'ами (ro) + сохранёнными базовыми
mount'ами (config/`/backups`/state — всё, что не под `/volumes/`, через `mergeMounts`). Spec пересоздания берётся из
`gobackup_container.*` лейблов на самом супервизоре (см. §5.8). Проверено e2e (`spike/e2e-mount/`), пересоздание
one-shot без thrash. DB-dump по сети этого не требует.

### 5.8 Spec пересоздаваемого контейнера (`gobackup_container.*`)
Лейблы на **супервизоре** (читаются один раз при старте через self-inspect по `os.Hostname` → `ContainerInspect` →
`container.Parse`): `image`, `command` (**полный argv** — у образа нет entrypoint), `networks` (CSV), `env.<VAR>`,
`labels.<key>`. По каждому полю: лейбл выигрывает, иначе fallback к настройкам заменяемого контейнера. Component-label
форсится (иначе следующий reconcile не найдёт контейнер). `docker.sock:ro` достаточно для create/stop/remove/start.

---

## 6. Риски и открытые вопросы
- ~~fsnotify на rename поверх volume/overlay~~ ✅ **снят спайком** (§1.3): reload надёжен через named-volume; apply = атомарная запись + пре-валидация (last-good gobackup не держит).
- **Безопасность `docker.sock`** — даже `:ro` = root-эквивалент на хосте. Митигация: путь сеть+DSN (без socket);
  socket-proxy (`tecnativa/docker-socket-proxy`) с доступом только list/inspect(+restart); уважать `DOCKER_HOST`/rootless.
- **Матрица утилит БД** под образ (в B решается стоковым образом).
- **Пин версии Docker SDK**; поведение gobackup на битом конфиге; сериализация задач (gobackup гоняет джобы под одним мьютексом).

---

## 7. Источники
- gobackup: `main.go`, `config/config.go`, `scheduler/scheduler.go`, `model/model.go`, `web/api.go`, `storage/cycler.go`
  — github.com/gobackup/gobackup
- Docker SDK: pkg.go.dev/github.com/docker/docker/client (events stream, container list/inspect)
- Prior art: offen/docker-volume-backup
