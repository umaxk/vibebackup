# vbackuper

Сервис резервного копирования каталогов и баз данных в одном Docker-контейнере:
веб-панель, планировщик по cron, история бэкапов, скачивание и удаление.

## Возможности

- **Каталоги** — `tar` + `zstd`, с масками исключений и сохранением прав/владельцев/симлинков.
- **PostgreSQL** — `pg_dump --format=custom` (нативно сжатый дамп, восстанавливается `pg_restore`).
- **MySQL / MariaDB** — `mysqldump --single-transaction` + `zstd`.
- **Проекты** — каталог и/или база данных в одном прогоне: общий статус, история, retention и архив.
- **Расписание** — cron (5 полей), пресеты в UI, ручной запуск, включение/выключение проекта.
- **История** — статус, размер, длительность, sha256, лог ошибки, скачивание, удаление.
- **Retention** — хранить N последних успешных бэкапов на проект.
- **Надёжность** — один прогон на проект, лимит параллельных задач, таймаут, проверка свободного места,
  корректная остановка по SIGTERM, пометка прерванных прогонов после рестарта.
- **Интерфейсы** — веб-панель и REST API с авторизацией (cookie-сессия, HTTP Basic или API-ключ).
- **Режим только-API** — `VBC_UI_ENABLED=false` полностью отключает веб-интерфейс.

## Быстрый старт

```bash
cp .env.example .env      # задайте VBC_ADMIN_PASSWORD и SOURCE_DIR
docker compose up -d --build
```

Сгенерировать ключ API (для скриптов и режима только-API):

```bash
openssl rand -hex 32
```

```powershell
# Windows PowerShell, если нет openssl
-join ((1..32) | ForEach-Object { '{0:x2}' -f (Get-Random -Maximum 256) })
```

> **Linux:** контейнер работает от uid `10001`. Если каталог `./backups` создаётся на хосте,
> выдайте права: `mkdir -p backups && sudo chown -R 10001:10001 backups`.
> На Windows/macOS (Docker Desktop) это не требуется.

Панель: <http://localhost:8787>, логин `admin`, пароль из `VBC_ADMIN_PASSWORD`.

## Каталоги для бэкапа

Контейнер видит только то, что в него смонтировано. Смонтируйте нужные каталоги
**только для чтения** в `docker-compose.yml`:

```yaml
    volumes:
      - ./backups:/backups                       # сюда складываются бэкапы
      - D:/Photo:/sources/photo:ro               # Windows
      - /home/user/projects:/sources/projects:ro # Linux/macOS
```

Затем в UI укажите путь **внутри контейнера**: `/sources/photo`.
Разрешены только пути внутри `VBC_SOURCE_ROOTS` (по умолчанию `/sources`).

> Docker Desktop на Windows: включите доступ к диску в Settings → Resources → File sharing.

## Подключение к базам данных

| Где база | Host в форме |
|---|---|
| В том же `docker-compose.yml`, что и vbackuper | имя сервиса, например `postgres` или `mariadb` |
| На хосте, Windows/macOS | `host.docker.internal` |
| На хосте, Linux | `host.docker.internal` + раскомментировать `extra_hosts: ["host.docker.internal:host-gateway"]` |

Права пользователя:

- **PostgreSQL**: `CONNECT` на базу + чтение всех таблиц (`GRANT pg_read_all_data TO backup;` для PG14+).
- **MySQL/MariaDB**: `SELECT, SHOW VIEW, TRIGGER, LOCK TABLES, EVENT` (или `RELOAD`), для MySQL 8 — при необходимости `PROCESS`.

## Расписание

Стандартный cron из 5 полей: `минута час день месяц день-недели`.

| Выражение | Значение |
|---|---|
| `0 * * * *` | каждый час |
| `0 3 * * *` | ежедневно в 03:00 |
| `30 2 * * 1-5` | по будням в 02:30 |
| `0 4 1 * *` | 1-го числа в 04:00 |

Пустое поле — только ручной запуск. В UI показываются ближайшие 5 запусков.
После рестарта пропущенный запуск выполняется один раз, если попал в окно `VBC_GRACE_MINUTES`.
Хранить `0` бэкапов — использовать `default_retention` из настроек; `0` там — не удалять автоматически.

## Проекты: каталог и база вместе

Проект — отдельная сущность для случая «файлы проекта + его база». У проекта одно расписание,
один retention, одна история прогонов и один набор файлов на прогон.

Состав (нужна хотя бы одна часть):

- **каталог** — путь внутри контейнера и маски исключений;
- **база данных** — PostgreSQL **или** MySQL/MariaDB: хост, порт, база, пользователь, пароль,
  `sslmode` (только PostgreSQL).

В форме проекта блоки включаются галочками. Порт по умолчанию подставляется по типу базы
(`5432` или `3306`), поле `sslmode` показывается только для PostgreSQL.

### Проверка подключения

Кнопка «Проверить подключение» проверяет части раздельно:

- каталог — существует, читается, доступен внутри `VBC_SOURCE_ROOTS`;
- база — клиент подключается **именно к указанной базе** и выполняет `SELECT 1`,
  поэтому различаются три случая: сервер недоступен, неверные учётные данные,
  база не найдена или нет прав.

Тот же результат доступен в API: `POST /api/projects/{id}/test` (без тела — сохранённый проект,
с телом — значения из формы) и `POST /api/projects/test` для несохранённого проекта.
Ответ содержит независимые блоки `dir` и `db` с `ok`, `info` или `error`.

### Прогон

1. Сначала снимается дамп базы, затем архивируется каталог: база фиксируется в начале окна,
   файлы читаются после, поэтому восстановленные файлы не окажутся старше базы.
2. Части независимы: если упала база, архив каталога всё равно создаётся и остаётся доступным.
   Итоговый статус прогона — `failed`, а в ошибке перечислены упавшие части.
3. `manifest.json` пишется всегда: список файлов с ролями, размерами и `sha256`, а также ошибки частей.
4. Один прогон на проект, общий лимит `VBC_MAX_CONCURRENT`, таймаут `VBC_JOB_TIMEOUT` на весь прогон.
5. Retention хранит N последних **успешных** прогонов проекта; `0` — значение по умолчанию из настроек.

Полностью согласованной пары «файлы + база» без снапшота ФС или остановки приложения не бывает:
`tar` читает живой каталог, а `pg_dump`/`mysqldump` фиксируют состояние на момент старта.
Для строгих случаев используйте окно обслуживания или снапшот ФС.

## Обновление с прежних версий

При старте сервис выполняет две одноразовые миграции:

1. Таблицы прежней сущности «сайты» переименовываются: `sites` → `projects`,
   `site_backups` → `project_backups`, `site_backup_files` → `project_backup_files`.
   Данные сохраняются полностью.
2. Сущность «цели» удаляется безвозвратно: строки бэкапов, файлы и каталоги целей внутри
   `VBC_BACKUP_DIR`, затем таблицы `backups` и `targets`. В настройки записывается маркер
   `legacy_targets_removed`. Удаляются только пути строго внутри `VBC_BACKUP_DIR`.

Прежние прогоны сайтов остаются на диске в `/backups/sites/...` и доступны в истории;
новые прогоны пишутся в `/backups/projects/...`.

## Где лежат бэкапы

Один прогон проекта — одна папка:

```
/backups/projects/<slug>-<id>/<год>/<месяц>/<YYYYMMDDTHHMMSS>-b<backupID>/
├── manifest.json
├── files.tar.zst        (если у проекта есть каталог)
├── db.dump              (PostgreSQL)
└── db.sql.zst           (MySQL/MariaDB)
```

`manifest.json`:

```json
{
  "project_id": 3,
  "project_name": "example.com",
  "backup_id": 12,
  "started_at": "2026-11-14T03:00:00Z",
  "finished_at": "2026-11-14T03:00:41Z",
  "files": [
    {"role": "dir", "name": "files.tar.zst", "size_bytes": 1048576, "sha256": "..."},
    {"role": "db",  "name": "db.dump",       "size_bytes": 262144,  "sha256": "..."}
  ],
  "errors": {}
}
```

Скачивание «всё одним архивом» отдаёт `tar` с `manifest.json` и всеми файлами прогона;
отдельный файл доступен по роли: `?file=dir`, `?file=db`, `?file=manifest`.

Файлы читаются без сервиса. Проверка целостности: `sha256sum -c <файл>.sha256`.

## Восстановление

**Каталог**

```bash
zstd -d backup.tar.zst -c | tar -x -C /restore
```

**PostgreSQL**

```bash
pg_restore -h host -p 5432 -U user -d db --clean --if-exists backup.dump
# или в отдельную базу
createdb restore_target && pg_restore -d restore_target backup.dump
```

**MySQL / MariaDB**

```bash
zstd -d backup.sql.zst -c | mysql -h host -u user -p db
```

**Проект (каталог + база)**

Скачайте архив прогона из UI («Скачать всё») или через API и распакуйте:

```bash
curl -OJ -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/project-backups/12/download"

tar -xf project-3-backup-12.tar
# manifest.json, files.tar.zst, db.dump (или db.sql.zst)
zstd -d files.tar.zst -c | tar -x -C /restore
pg_restore -h host -p 5432 -U user -d db --clean --if-exists db.dump
# для MySQL: zstd -dc db.sql.zst | mysql -h host -u user -p db
```

Сверьте `sha256` файлов с `manifest.json`, если прогон завершился с ошибкой одной из частей:
успешные файлы всё равно пригодны для восстановления.

В контейнере уже есть `pg_restore`, `psql`, `mysql` и `zstd`, поэтому восстановление можно
делать прямо из него, не устанавливая клиенты на хост:

```bash
docker compose exec vbackuper sh -c \
  'zstd -dc "/backups/<slug>-<id>/2026/11/backup.sql.zst" | mysql -h mariadb -u demo -p demo'
```

## REST API

Авторизация — API-ключ (рекомендуется), cookie-сессия или HTTP Basic.
Ключ передаётся в заголовке `X-API-Key` или как `Authorization: Bearer <ключ>`;
в query-строке ключ не принимается, чтобы не попадать в логи.

```bash
export VBC_URL=http://localhost:8787
export VBC_API_KEY=...        # значение из .env
```

### Режимы доступа

| `VBC_UI_ENABLED` | `VBC_API_KEY` | Веб-интерфейс | Авторизация API |
|---|---|---|---|
| `true` | не задан | работает | cookie-сессия или Basic |
| `true` | задан | работает | API-ключ, cookie-сессия или Basic |
| `false` | задан | **отключён** (все UI-адреса → `404`) | **только API-ключ** |

В режиме только-API `/login`, `/api/login`, `/logout`, `/projects/*`, `/backups`, `/settings`
и `/static/*` не регистрируются вовсе. Открытыми остаются `/healthz` и `/api/*`;
Basic и cookie-сессия в этом режиме не действуют. Запуск с `VBC_UI_ENABLED=false`
без `VBC_API_KEY` завершается ошибкой — иначе сервис был бы недоступен.

### Примеры

```bash
# список проектов
curl -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/projects"

# проект: каталог + PostgreSQL в одном прогоне
curl -X POST -H "X-API-Key: $VBC_API_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"example.com","schedule":"0 3 * * *","retention_keep":7,
       "dir":{"path":"/sources/example.com","exclude":["node_modules","*.log"]},
       "db_type":"postgres",
       "database":{"host":"postgres","port":5432,"database":"example","user":"backup","password":"secret"}}' \
  "$VBC_URL/api/projects"

# проект только с MariaDB (каталог не нужен)
curl -X POST -H "X-API-Key: $VBC_API_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"shop-db","schedule":"0 4 * * *","db_type":"mysql",
       "database":{"host":"mariadb","port":3306,"database":"shop","user":"backup","password":"secret"}}' \
  "$VBC_URL/api/projects"

# проект только с каталогом
curl -X POST -H "X-API-Key: $VBC_API_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"Фото","schedule":"0 3 * * *","retention_keep":7,
       "dir":{"path":"/sources/photo","exclude":["*.tmp"]}}' \
  "$VBC_URL/api/projects"

# проверка частей: без тела — сохранённый проект, с телом — значения из формы
curl -X POST -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/projects/1/test"

# запуск прогона: 202, 409 если прогон уже идёт
curl -X POST -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/projects/1/run"

# история прогонов проекта и всех проектов
curl -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/projects/1/backups?limit=50"
curl -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/project-backups?project_id=1&limit=50"

# детали прогона: файлы, manifest_sha256, лог
curl -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/project-backups/12"

# скачать всё одним tar (manifest.json + files.tar.zst + db.dump/db.sql.zst)
curl -OJ -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/project-backups/12/download"

# отдельный файл по роли: dir, db или manifest
curl -OJ -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/project-backups/12/download?file=db"

# удалить проект с файлами / один прогон
curl -X DELETE -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/projects/1?remove_files=true"
curl -X DELETE -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/project-backups/12"

# настройки, предпросмотр cron, состояние системы, доступные каталоги
curl -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/settings"
curl -X PUT -H "X-API-Key: $VBC_API_KEY" -H 'Content-Type: application/json' \
  -d '{"default_retention":"7","grace_minutes":"60","min_free_mb":"512"}' "$VBC_URL/api/settings"
curl -X POST -H "X-API-Key: $VBC_API_KEY" -H 'Content-Type: application/json' \
  -d '{"schedule":"0 3 * * *"}' "$VBC_URL/api/cron/preview"
curl -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/system"
curl -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/options/sources"
```

Без ключа API отвечает `401` и JSON `{"error":"требуется авторизация"}`.
`POST /api/projects/{id}/run` возвращает `202` и `{"id":<backup_id>,"status":"queued"}`,
а `409`, если для проекта уже идёт прогон.

### Методы

| Метод | Путь | Описание |
|---|---|---|
| POST | `/api/login`, `/api/logout` | сессия (только при `VBC_UI_ENABLED=true`) |
| GET/POST | `/api/projects` | список / создание проекта |
| GET/PUT/DELETE | `/api/projects/{id}` | чтение / изменение / удаление (`?remove_files=false` — оставить файлы) |
| POST | `/api/projects/test` | проверка несохранённого проекта: `dir` и `db` раздельно |
| POST | `/api/projects/{id}/test` | проверка частей (тело необязательно) |
| POST | `/api/projects/{id}/run` | запуск прогона, `202` + `id` |
| GET | `/api/projects/{id}/backups` | история прогонов проекта |
| GET | `/api/project-backups?project_id=&limit=` | прогоны всех проектов |
| GET/DELETE | `/api/project-backups/{id}` | детали с файлами / удаление (`?remove_files=false`) |
| GET | `/api/project-backups/{id}/download?file=` | всё одним tar или один файл (`dir`, `db`, `manifest`) |
| POST | `/api/cron/preview` | ближайшие запуски `{"schedule":"..."}` |
| GET/PUT | `/api/settings` | политика хранения |
| GET | `/api/options/sources` | доступные каталоги |
| GET | `/api/system` | версия, диск, лимиты |
| GET | `/healthz` | проверка живости, без авторизации |

Алиасы `/api/sites*` и `/api/site-backups*` работают как `/api/projects*` и
`/api/project-backups*` один релиз; старые адреса `/sites*` и `/site-backups*`
отвечают редиректом `307` на новые.

## Переменные окружения

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `VBC_ADDR` | `:8787` | адрес HTTP |
| `VBC_ADMIN_USER` | `admin` | логин администратора |
| `VBC_ADMIN_PASSWORD` | `changeme` (с предупреждением) | пароль администратора |
| `VBC_API_KEY` | пусто | ключ API; несколько значений — через запятую (для ротации) |
| `VBC_UI_ENABLED` | `true` | `false` — только API, веб-интерфейс и сессии отключены |
| `VBC_SECRET` | генерируется в `/data/secret.key` | ключ подписи cookie |
| `VBC_DATA_DIR` | `/data` | SQLite и ключ |
| `VBC_BACKUP_DIR` | `/backups` | каталог бэкапов |
| `VBC_SOURCE_ROOTS` | `/sources` | разрешённые корни источников (через запятую) |
| `VBC_DEFAULT_RETENTION` | `7` | хранить последних бэкапов |
| `VBC_MAX_CONCURRENT` | `2` | параллельных задач |
| `VBC_JOB_TIMEOUT` | `2h` | таймаут задачи |
| `VBC_MIN_FREE_MB` | `512` | минимум свободного места |
| `VBC_GRACE_MINUTES` | `60` | окно догоняющих запусков |
| `VBC_TZ` / `TZ` | системный | часовой пояс расписаний |

Проверка состояния: `GET /healthz` (используется Docker healthcheck).

## Безопасность

- Один администратор, пароль из окружения; cookie подписана HMAC, `HttpOnly`, `SameSite=Lax`.
- API-ключ сравнивается в постоянном времени, принимается только из заголовка и не пишется в логи.
- `VBC_UI_ENABLED=false` убирает UI-маршруты и сессии: остаётся единственный вход по ключу.
- Ограничение попыток входа (10 за 10 минут на IP), проверка `Origin` для изменяющих запросов.
- Пароли БД не отдаются API (только `password_set: true/false`).
- TLS — снаружи, reverse proxy (Caddy/Traefik/nginx).
- Секреты БД хранятся в SQLite на volume без шифрования — защищайте каталог `/data`.
- Ключ и пароль задаются через окружение: они видны в `docker inspect`, учитывайте это на общем хосте.

## Ограничения

- Каталоги — только из смонтированных путей. Произвольный путь хоста без `docker.sock` недоступен
  (проброс сокета сознательно не делаем: это root-доступ к Docker).
- Бэкап каталога — «живой» `tar`, без снапшота ФС. Для строгой консистентности нужен LVM/btrfs/restic.
- `pg_dump` должен быть не старше мажорной версии сервера. В образе клиент PostgreSQL 17,
  версия задаётся `--build-arg PG_VERSION=16`.
- MySQL: `--single-transaction` корректен для InnoDB; таблицы MyISAM могут быть неконсистентны.
- Проект не делает каталог и базу атомарными: `tar` читает живой каталог, дамп фиксирует базу
  на момент старта. Для строгой согласованности нужно окно обслуживания или снапшот ФС.
- Восстановления из UI нет (разрушительная операция) — только скачивание и команды выше.

## Разработка

Go 1.26, SQLite (pure Go), статика и шаблоны встроены в бинарь.

```bash
make test     # go vet + go test в контейнере golang
make build    # сборка в контейнере
make image    # docker build
make up       # docker compose up -d --build
```

Структура:

```
cmd/vbackuper          точка входа
internal/config        переменные окружения
internal/store         SQLite: проекты, прогоны, файлы прогонов, настройки
internal/backup        движки: dir, postgres, mysql + хэш/логи/проверки
internal/service       оркестрация проектов, retention, проверки
internal/scheduler     cron, догоняющие запуски
internal/web           UI, REST API, авторизация
internal/web/templates html-шаблоны (embed)
internal/web/static    css/js (embed)
```

## Дымовой тест

Поднимает PostgreSQL и MariaDB с демо-данными и сам сервис:

```bash
docker compose -f docker-compose.test.yml up -d --build
```

- UI: <http://localhost:8787>, `admin` / `test123`
- каталог: `/sources/fixtures`
- PostgreSQL: `postgres:5432`, база `demo`, пользователь `demo`, пароль `demo`
- MariaDB: `mariadb:3306`, база `demo`, пользователь `demo`, пароль `demo`

Сценарий: добавить проекты → «Запустить прогон» → проверить статусы, размеры и скачивание →
удалить один прогон → убедиться, что retention оставляет 3.

Проверка проектов:

1. Создать проект «каталог + PostgreSQL»: `/sources/fixtures` + `postgres:5432/demo`, и проект
   «каталог + MariaDB»: `/sources/fixtures` + `mariadb:3306/demo`.
2. «Проверить подключение» — обе части должны быть зелёными; при неверном пароле базы
   каталог всё равно проверяется успешно, а база показывает ошибку учётных данных.
3. Запустить прогон → статус `ok`; скачать всё одним tar → внутри `manifest.json`,
   `files.tar.zst`, `db.dump` или `db.sql.zst`; sha256 совпадает с манифестом.
4. Retention: оставить 2, выполнить три прогона → остаются два последних успешных.
5. Частичный отказ: испортить пароль базы и запустить → статус `failed`,
   но `files.tar.zst` присутствует и скачивается.
6. Проект только с каталогом и проект только с базой — оба запускаются и дают `manifest.json`
   с одной частью.

Остановка: `docker compose -f docker-compose.test.yml down -v`.
