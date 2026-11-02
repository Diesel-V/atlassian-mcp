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

## Конфигурация

Все параметры задаются переменными окружения, полный пример лежит в `deploy/.env.example`. Обязательные:

| Переменная | Описание |
|---|---|
| `PUBLIC_URL` | Внешний HTTPS-адрес сервиса (на него возвращаются OAuth-редиректы) |
| `DATABASE_URL` | PostgreSQL |
| `ENCRYPTION_KEY` | 32 байта в base64, шифрует токены и PAT в БД (`openssl rand -base64 32`). Если его потерять, все гранты станут недействительны |
| `SESSION_KEY` | 32 байта в base64, защищает cookie страницы «Мои доступы» |
| `KEYCLOAK_ISSUER` | Например `https://auth.example.com/realms/<realm>` |
| `KEYCLOAK_WEB_CLIENT_ID` / `_SECRET` | Конфиденциальный клиент для страницы «Мои доступы» |
| `CONFLUENCE_URL` / `_CLIENT_ID` / `_CLIENT_SECRET` | Входящая ссылка на приложение в Confluence (можно не задавать, если Confluence не нужен) |
| `JIRA_URL` / `_CLIENT_ID` / `_CLIENT_SECRET` | То же для Jira |

Должен быть задан хотя бы один из продуктов.

Необязательные параметры с умолчаниями (сроки ключей, лимиты, таймауты, `AUDIT_RETENTION=8760h` — срок хранения аудита, `0` хранит вечно) перечислены в `deploy/.env.example`.

## Что нужно настроить в смежных системах

**Confluence и Jira** (администраторы, отдельно в каждом продукте): *Администрирование → Ссылки на приложения → Создать ссылку → Внешнее приложение → Входящая*.
- Redirect URL: `https://mcp-atlassian.example.com/oauth/confluence/callback` (для Jira — `/oauth/jira/callback`)
- Права: **Read** (запись в Jira появится в отдельной фазе)
- Результат: Client ID и Client Secret передаются в `CONFLUENCE_*` / `JIRA_*`

**Keycloak** (realm, которому доверяет Open WebUI):
- Конфиденциальный клиент `atlassian-mcp`, Standard flow, redirect URI `https://mcp-atlassian.example.com/login/callback`, PKCE S256.
- Для каждого автономного агента — отдельный клиент со Service accounts enabled. Его `client_id` регистрируется командой `agent add`.

**DNS/TLS**: имя `mcp-atlassian.example.com` с TLS через reverse proxy на порт 8082 сервера. Адрес должен открываться из браузеров сотрудников.

## Развёртывание (один хост, Docker Compose)

```bash
cd deploy
cp .env.example .env && chmod 600 .env     # заполнить секреты
echo "POSTGRES_PASSWORD=..." >> .env        # тот же пароль, что в DATABASE_URL
docker compose up -d --build
curl -s localhost:8082/readyz               # 200 — готово
```

При старте сервис сам применяет миграции, отдельно их запускать не нужно (`atlassian-mcp migrate` есть для CI и ручного запуска).

### Подключение в Open WebUI

*Настройки → Admin → Внешние инструменты → Добавить подключение*:
- Тип: **MCP (Streamable HTTP)**
- URL: `http://atlassian-mcp:8080/mcp` (контейнеры в общей сети `llm`)
- Авторизация: **OAuth** (Open WebUI передаёт SSO-токен пользователя)

Затем создать модель «Atlassian» на self-hosted модели с поддержкой function calling, включить в ней этот инструмент, режим вызова функций — Native, остальные встроенные инструменты выключить.

### OpenCode

На странице «Мои доступы» выпустить ключ, затем в `opencode.json`:

```json
{
  "mcp": {
    "atlassian": {
      "type": "remote",
      "url": "https://mcp-atlassian.example.com/mcp",
      "headers": { "Authorization": "Bearer {env:ATLASSIAN_MCP_KEY}" }
    }
  }
}
```

### Автономные агенты

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
internal/audit         аудит и срок его хранения
internal/offboard      отзыв доступа человека
internal/metrics       метрики Prometheus
internal/ratelimit     rate limit на субъекта
internal/web           страница «Мои доступы»
internal/server        сборка HTTP-сервера
internal/store         контракты хранилища; memory/ (тесты), postgres/ (прод), storetest/ (общий набор тестов)
```

`go.sum` создаётся при первой сборке (в Dockerfile стоит `GOFLAGS=-mod=mod`). После первой успешной сборки выполните `go mod tidy` и закоммитьте `go.sum`.
