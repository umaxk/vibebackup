# vbackuper

Сервис резервного копирования каталогов и баз данных в одном Docker-контейнере:
веб-панель, планировщик по cron, история бэкапов, скачивание и удаление.

## Возможности

- **Каталоги** — `tar` + `zstd`, с масками исключений и сохранением прав/владельцев/симлинков.
- **PostgreSQL** — `pg_dump --format=custom` (нативно сжатый дамп, восстанавливается `pg_restore`).
- **MySQL / MariaDB** — `mysqldump --single-transaction` + `zstd`.
- **Расписание** — cron (5 полей), пресеты в UI, ручной запуск, включение/выключение цели.
- **История** — статус, размер, длительность, sha256, лог ошибки, скачивание, удаление.
- **Retention** — хранить N последних успешных бэкапов на цель.
- **Надёжность** — один прогон на цель, лимит параллельных задач, таймаут, проверка свободного места,
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

## Где лежат бэкапы

```
/backups/<slug>-<id>/<год>/<месяц>/<slug>-<id>-<YYYYMMDDTHHMMSS>-b<backupID>.tar.zst
/backups/<slug>-<id>/<год>/<месяц>/<slug>-<id>-<YYYYMMDDTHHMMSS>-b<backupID>.dump      (PostgreSQL)
/backups/<slug>-<id>/<год>/<месяц>/<slug>-<id>-<YYYYMMDDTHHMMSS>-b<backupID>.sql.zst   (MySQL)
+ рядом файл .sha256
```

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

В режиме только-API `/login`, `/api/login`, `/logout`, `/targets/*`, `/backups`, `/settings`
и `/static/*` не регистрируются вовсе. Открытыми остаются `/healthz` и `/api/*`;
Basic и cookie-сессия в этом режиме не действуют. Запуск с `VBC_UI_ENABLED=false`
без `VBC_API_KEY` завершается ошибкой — иначе сервис был бы недоступен.

### Примеры

```bash
# список целей
curl -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/targets"

# создать цель-каталог
curl -X POST -H "X-API-Key: $VBC_API_KEY" -H 'Content-Type: application/json' \
  -d '{"type":"dir","name":"Фото","schedule":"0 3 * * *","retention_keep":7,
       "dir":{"path":"/sources/photo","exclude":["*.tmp"]}}' \
  "$VBC_URL/api/targets"

# создать цель-PostgreSQL
curl -X POST -H "X-API-Key: $VBC_API_KEY" -H 'Content-Type: application/json' \
  -d '{"type":"postgres","name":"app-db","schedule":"0 * * * *",
       "database":{"host":"postgres","port":5432,"database":"app","user":"backup","password":"secret"}}' \
  "$VBC_URL/api/targets"

# создать цель-MySQL/MariaDB
curl -X POST -H "X-API-Key: $VBC_API_KEY" -H 'Content-Type: application/json' \
  -d '{"type":"mysql","name":"shop","schedule":"0 4 * * *",
       "database":{"host":"mariadb","port":3306,"database":"shop","user":"backup","password":"secret"}}' \
  "$VBC_URL/api/targets"

# проверить подключение (тело необязательно: без него проверяется сохранённая цель)
curl -X POST -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/targets/1/test"

# запустить бэкап: 202 и id прогона
curl -X POST -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/targets/1/run"

# список бэкапов цели / всех
curl -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/targets/1/backups?limit=50"
curl -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/backups?target_id=1&limit=50"

# детали одного бэкапа (со логом)
curl -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/backups/12"

# скачать файл; -J подставит имя, Range и докачка поддерживаются
curl -OJ -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/backups/12/download"

# удалить цель и её файлы (remove_files=false — оставить файлы)
curl -X DELETE -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/targets/1?remove_files=true"

# удалить один бэкап (remove_file=false — оставить файл)
curl -X DELETE -H "X-API-Key: $VBC_API_KEY" "$VBC_URL/api/backups/12"

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
`POST /api/targets/{id}/run` возвращает `202` и `{"id":<backup_id>,"status":"queued"}`,
а `409`, если для цели уже идёт прогон.

### Методы

| Метод | Путь | Описание |
|---|---|---|
| POST | `/api/login`, `/api/logout` | сессия (только при `VBC_UI_ENABLED=true`) |
| GET/POST | `/api/targets` | список / создание цели |
| GET/PUT/DELETE | `/api/targets/{id}` | чтение / изменение / удаление (`?remove_files=false` — оставить файлы) |
| POST | `/api/targets/{id}/test` | проверка подключения (тело необязательно) |
| POST | `/api/targets/{id}/run` | запуск бэкапа, `202` + `id` |
| GET | `/api/targets/{id}/backups` | история цели |
| GET | `/api/backups?target_id=&limit=` | все бэкапы |
| GET/DELETE | `/api/backups/{id}` | детали / удаление (`?remove_file=false`) |
| GET | `/api/backups/{id}/download` | скачивание файла |
| POST | `/api/test` | проверка не сохранённой конфигурации |
| POST | `/api/cron/preview` | ближайшие запуски `{"schedule":"..."}` |
| GET/PUT | `/api/settings` | политика хранения |
| GET | `/api/options/sources` | доступные каталоги |
| GET | `/api/system` | версия, диск, лимиты |
| GET | `/healthz` | проверка живости, без авторизации |

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
internal/store         SQLite: цели, бэкапы, настройки
internal/backup        движки: dir, postgres, mysql + хэш/логи
internal/service       оркестрация, retention, проверки
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

Сценарий: добавить все три цели → «Запустить бэкап» → проверить статусы, размеры и скачивание →
удалить один бэкап → убедиться, что retention оставляет 3.

Остановка: `docker compose -f docker-compose.test.yml down -v`.
