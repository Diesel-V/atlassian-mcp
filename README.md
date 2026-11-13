# atlassian-mcp

MCP-сервер, через который LLM-клиенты (Open WebUI, OpenCode и др.) и агенты получают доступ к **Confluence и Jira Data Center строго под правами того, кто спрашивает**.

- Общей служебной учётки нет. Пользователь видит через ассистента ровно то, что видит в Confluence и Jira сам.
- Доступ даётся через OAuth 2.0 самих Confluence и Jira (входящие ссылки на приложения). Вход через SSO, пароль не нужен.
- Агентам доступ выдаётся двумя способами: персональный ключ (агент действует от имени человека) или Keycloak client credentials (автономный агент со своей выделенной учёткой).
- Все вызовы пишутся в аудит. Содержимое документов в аудит не попадает.

Спецификация (OpenSpec): `docs/atlassian-mcp-spec.md`. Раздел 6 в ней связывает каждый сценарий с тестами.

## Как это работает

```
Пользователь ─► Open WebUI ──(SSO-токен Keycloak)──►┐
OpenCode / скрипт ─────────(персональный ключ)──────►├─► atlassian-mcp ──(OAuth-токен субъекта)──► Confluence / Jira
Прод-агент ────────────(client credentials Keycloak)─►┘      │
                                                             ├─► PostgreSQL: зашифрованные токены, ключи, агенты, аудит
                                                             └─► /metrics (Prometheus)
```

| Кто вызывает | Как аутентифицируется | Чьи права в Atlassian |
|---|---|---|
| Пользователь Open WebUI | Access-токен Keycloak, `azp` из `KEYCLOAK_USER_CLIENTS` | Его OAuth-грант, полученный на странице «Мои доступы» |
| Агент от имени человека | `Authorization: Bearer amcp_…` (персональный ключ) | OAuth-грант владельца ключа |
| Автономный агент | Access-токен Keycloak (client credentials), `azp` зарегистрирован в реестре агентов | PAT выделенной служебной учётки этого агента |

Валидный токен Keycloak от клиента, которого нет среди `KEYCLOAK_USER_CLIENTS` и в реестре агентов (или агент выключен), получает 403.
Если у пользователя нет гранта, инструмент не ходит в Atlassian, а возвращает ссылку `PUBLIC_URL/connect/{confluence|jira}`.
Если Atlassian отверг токен (отозван, сотрудник заблокирован), грант удаляется, и пользователя просят подключиться заново.

## Инструменты MCP

| Инструмент | Что делает |
|---|---|
| `confluence_search` | Поиск по Confluence: все слова или точная фраза, фильтры по пространствам, автору, дате изменения, только по заголовкам |
| `confluence_get_page` | Страница по ID или URL, тело в Markdown |
| `jira_search` | Поиск задач: текст, проекты, статусы, исполнитель (`me`), дата обновления или «сырой» JQL |
| `jira_get_issue` | Задача по ключу: поля, описание, последние 20 комментариев |

Все инструменты только читают данные (`readOnlyHint`). Пользовательский текст сводится к простым словам, а все значения экранируются при сборке CQL/JQL. Ключи пространств, проектов и логины проверяются по строгим шаблонам. «Сырой» JQL принимается как есть: он выполняется под правами самого субъекта и не может их расширить.

## HTTP-эндпоинты

| Путь | Назначение |
|---|---|
| `POST /mcp` | MCP Streamable HTTP (stateless, JSON), требует Bearer |
| `GET /` | «Мои доступы»: подключение Confluence/Jira, ключи агентов (вход через Keycloak) |
| `GET /connect/{provider}`, `GET /oauth/{provider}/callback` | OAuth-подключение продукта |
| `GET /healthz`, `GET /readyz` | liveness / readiness (проверка БД) |
| `:9090/` | Метрики Prometheus (отдельный порт, наружу не публиковать): `amcp_tool_calls_total`, `amcp_tool_duration_seconds`, `amcp_connected_users` |

## Режим пилота (требования ИБ)

Пилот согласован на условиях: только поиск и чтение, только согласованные пространства и проекты, только пилотная группа и только через Open WebUI. Сервер обеспечивает это сам, независимо от прав сотрудника в продукте:

| Требование | Как обеспечено |
|---|---|
| Только согласованные пространства и проекты | `CONFLUENCE_ALLOWED_SPACES`, `JIRA_ALLOWED_PROJECTS` — обязательны. Ограничение вставляется в каждый CQL/JQL; страница или задача, открытая по ID или ссылке, проверяется после получения (в том числе перенесённая задача); результаты вне списка отбрасываются. Свободный JQL при ограниченном списке проектов отключён |
| Только пилотная группа | `KEYCLOAK_REQUIRED_GROUP`: остальные получают 403 и не могут войти на страницу «Мои доступы» |
| Только через Open WebUI | `ENABLE_API_KEYS=false`, `ENABLE_AGENTS=false` (по умолчанию): ключи отклоняются (401), агенты — 403, раздел ключей скрыт |
| Только чтение | Инструментов записи нет; OAuth-гранты со scope READ |
| TLS до ВМ | `TLS_CERT_FILE` / `TLS_KEY_FILE`, минимум TLS 1.2 |
| Секреты в Vault | Любая переменная читается из файла `<ИМЯ>_FILE` (Vault Agent) |
| Минимальные права в БД | `deploy/postgres-roles.sql`: рабочая роль без DDL, миграции — отдельной ролью; `MIGRATE_ON_START=false` |
| Аудит | Пользователь, инструмент, параметры, ID найденных и открытых объектов, статус (`out_of_scope` для отказов по списку). Без токенов, кодов и текста документов |
| Поставка | Образ собирается и проверяется в CI (`.gitlab-ci.yml`: тесты с PostgreSQL, govulncheck, Trivy), запускается из внутреннего реестра без root, с read-only FS, без capabilities |

## Конфигурация

Все параметры задаются переменными окружения, полный пример для пилота — `deploy/.env.example` (его корректность проверяется тестом). Любую переменную можно передать файлом через `<ИМЯ>_FILE`. Обязательные:

| Переменная | Описание |
|---|---|
| `PUBLIC_URL` | Внешний HTTPS-адрес сервиса (на него возвращаются OAuth-редиректы) |
| `DATABASE_URL` | PostgreSQL, рабочая роль |
| `ENCRYPTION_KEY` | 32 байта в base64, шифрует токены и PAT в БД (`openssl rand -base64 32`). Если его потерять, все гранты станут недействительны |
| `SESSION_KEY` | 32 байта в base64, защищает cookie страницы «Мои доступы» |
| `KEYCLOAK_ISSUER` | Например `https://auth.example.com/realms/<realm>` |
| `KEYCLOAK_WEB_CLIENT_ID` / `_SECRET` | Конфиденциальный клиент для страницы «Мои доступы» |
| `CONFLUENCE_URL` / `_CLIENT_ID` / `_CLIENT_SECRET` / `_ALLOWED_SPACES` | Входящая ссылка на приложение в Confluence и разрешённые пространства (`*` — все) |
| `JIRA_URL` / `_CLIENT_ID` / `_CLIENT_SECRET` / `_ALLOWED_PROJECTS` | То же для Jira |

Должен быть задан хотя бы один из продуктов. Пилотные параметры (`KEYCLOAK_REQUIRED_GROUP`, `ENABLE_*`, `TLS_*`, `MIGRATE_*`) и необязательные с умолчаниями (сроки ключей, лимиты, таймауты, `AUDIT_RETENTION`) — в `deploy/.env.example`.

## Что нужно настроить в смежных системах

**Confluence и Jira** (администраторы, отдельно в каждом продукте): *Администрирование → Ссылки на приложения → Создать ссылку → Внешнее приложение → Входящая*.
- Redirect URL: `${PUBLIC_URL}/oauth/confluence/callback` (для Jira — `/oauth/jira/callback`)
- Права: **Read**
- Результат: Client ID и Client Secret (секрет — в Vault)

**Keycloak** (realm, которому доверяет Open WebUI):
- Конфиденциальный клиент `atlassian-mcp`, Standard flow, redirect URI `${PUBLIC_URL}/login/callback`, PKCE S256.
- Группа пилота и mapper «Group Membership» (claim `groups`) в access-токене клиента `open-webui` и в ID-токене клиента `atlassian-mcp`.
- Срок жизни access-токена — минуты, не сутки.

**Сеть** (отдельный VLAN, по умолчанию всё закрыто):
- Reverse proxy публикует `PUBLIC_URL` по HTTPS. Через VPN сотрудникам открыты только пути входа и подключения: `/`, `/login`, `/login/callback`, `/logout`, `/connect/*`, `/oauth/*/callback`, `/disconnect/*`. Путь `/mcp` — только с сервера Open WebUI. `/healthz`, `/readyz` — мониторинг.
- Порт 8082 (TLS) — только с reverse proxy; SSH — только с jump host; порт метрик 9090 — только система мониторинга.
- Исходящие с ВМ: Confluence, Jira, Keycloak, PostgreSQL, Vault, внутренний реестр. Публичные репозитории закрыты.

**Vault**: секреты `database_url`, `encryption_key`, `session_key`, `keycloak_web_client_secret`, `confluence_client_secret`, `jira_client_secret`. Vault Agent кладёт их в `/run/atlassian-mcp/secrets/` (файлы `0400`, владелец uid `65532`).

**PostgreSQL** (штатный кластер, штатные бэкапы): DBA выполняет `deploy/postgres-roles.sql` в БД сервиса и задаёт пароли ролям `amcp_migrator` и `amcp_app`.

## Развёртывание

Образ собирает CI и публикует во внутренний реестр. На ВМ ничего не собирается:

```bash
cd /opt/atlassian-mcp      # docker-compose.yml из deploy/
export REGISTRY_IMAGE=<реестр>/ai/atlassian-mcp VERSION=<тег>
docker compose --profile migrate run --rm migrate   # схема, роль amcp_migrator
docker compose up -d
curl -sk https://localhost:8082/readyz              # 200 — готово
```

Сервис работает под ролью `amcp_app` и при `MIGRATE_ON_START=false` только проверяет, что схема актуальна: если миграции не применены, он не стартует и пишет об этом в лог. После настройки временный sudo и членство в группе docker отзываются; кто выкатывает обновления после этого — открытый вопрос спецификации.

### Подключение в Open WebUI

*Настройки → Admin → Внешние инструменты → Добавить подключение*:
- Тип: **MCP (Streamable HTTP)**
- URL: `${PUBLIC_URL}/mcp`
- Авторизация: **OAuth** (Open WebUI передаёт SSO-токен пользователя)

Модель «Atlassian» — на self-hosted LLM, режим вызова функций Native, подключён только этот инструмент. Для пилота дополнительно:
- у модели выключены веб-поиск, загрузка URL, генерация изображений и встроенные инструменты отправки наружу (автоматизации, уведомления, создание инструментов);
- модель задач (заголовки, теги) — self-hosted;
- публикация и общий доступ к чатам выключены, `ENABLE_ADMIN_CHAT_ACCESS=false`;
- в шлюзе LiteLLM кешируются только эмбеддинги.

### Отключение MCP

От быстрого к полному:
1. Выключить инструмент в Open WebUI — модели перестают его вызывать.
2. `docker compose stop atlassian-mcp` — сервис недоступен.
3. Отключить входящую ссылку на приложение в Confluence/Jira — продукт перестаёт принимать все выданные токены.
4. `atlassian-mcp user offboard -username X` — для одного человека (ключи и гранты).

### OpenCode

Вне пилота (`ENABLE_API_KEYS=true`). На странице «Мои доступы» выпустить ключ, затем в `opencode.json`:

```json
{
  "mcp": {
    "atlassian": {
      "type": "remote",
      "url": "https://atlassian-mcp.example.com/mcp",
      "headers": { "Authorization": "Bearer {env:ATLASSIAN_MCP_KEY}" }
    }
  }
}
```

### Автономные агенты

Вне пилота (`ENABLE_AGENTS=true`).

```bash
docker compose exec atlassian-mcp /atlassian-mcp agent add -client-id ticket-bot -name "Разбор тикетов" -owner p.petrov
# PAT выделенной служебной учётки агента; читается из stdin и сохраняется в БД в зашифрованном виде
docker compose exec -T atlassian-mcp /atlassian-mcp agent set-token -client-id ticket-bot -provider jira < pat.txt
docker compose exec atlassian-mcp /atlassian-mcp agent list
docker compose exec atlassian-mcp /atlassian-mcp agent disable -client-id ticket-bot
```

### Офбординг

При увольнении или срочном отзыве доступа:

```bash
docker compose exec atlassian-mcp /atlassian-mcp user offboard -username i.ivanov -operator s.sidorov
```

Команда отзывает все ключи человека (следующий вызов с ними получает 401), удаляет его гранты Confluence и Jira и выводит список автономных агентов, владельцем которых он был, для переназначения. Действие пишется в аудит. Без команды доступ пропадает сам, когда Confluence/Jira отклонят обновление токена заблокированной учётки.

## Безопасность

- Токены и PAT в БД зашифрованы AES-256-GCM. Шифротекст привязан к владельцу и полю: если скопировать значение в чужую строку, оно не расшифруется.
- От персональных ключей хранится только SHA-256 секрета. Сам ключ показывается один раз.
- «Отключить» на странице «Мои доступы» удаляет токены у сервиса. У OAuth-провайдера Data Center нет API отзыва, поэтому само разрешение пользователь при желании отзывает в профиле Confluence/Jira.
- Аудит хранится `AUDIT_RETENTION` (по умолчанию 1 год) и очищается раз в сутки.
- Подключение OAuth защищено от подмены пользователя. Состояние и PKCE-verifier лежат в зашифрованной cookie, привязанной к сессии. После обмена кода логин в Confluence/Jira сверяется с текущим пользователем, и при несовпадении грант не сохраняется.
- Страница «Мои доступы»: CSRF-токен и проверка Origin на POST, cookie HttpOnly и SameSite=Lax, CSP, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`.
- Ошибки, которые видит модель, не содержат секретов и внутренних деталей. Access-лог пишется без query string, потому что в callback-ах есть коды авторизации.
- Refresh-токены Data Center одноразовые, поэтому параллельные обновления схлопываются (singleflight), а между репликами токен перечитывается перед обновлением.

## Разработка

```bash
make test               # unit + e2e (настоящий MCP-клиент против всего стека с фейковыми Confluence/Jira)
make test-integration   # плюс PostgreSQL (нужен одноразовый PostgreSQL, см. Makefile)
make lint build image
```

Структура:

```
cmd/atlassian-mcp      точка входа: serve | migrate | agent | user | version
internal/config        конфигурация из окружения
internal/identity      кто вызывает: Keycloak JWT, персональные ключи, агенты
internal/atlassian     HTTP, OAuth 2.0 с Data Center, учётные данные субъекта, экранирование
  confluence/ jira/    клиенты и сборка CQL/JQL
internal/tools         MCP-инструменты
internal/scope         белый список пространств и проектов
internal/audit         аудит и срок его хранения
internal/offboard      отзыв доступа человека
internal/metrics       метрики Prometheus
internal/ratelimit     rate limit на субъекта
internal/web           страница «Мои доступы»
internal/server        сборка HTTP-сервера
internal/store         контракты хранилища; memory/ (тесты), postgres/ (прод), storetest/ (общий набор тестов)
```

`go.sum` создаётся при первой сборке (в Dockerfile стоит `GOFLAGS=-mod=mod`). После первой успешной сборки выполните `go mod tidy` и закоммитьте `go.sum`.
