# Design: <name>

> Источник: change.md (FR/TR, AC), delta-спеки.

## Context

- Change: `openspec/changes/<name>/`.
- Проблема: 1–2 предложения — что не работает или чего не хватает сейчас.
- Текущее состояние: пути пакетов и функций, таблицы, топики, секции конфига.
- Ограничения среды: только отличия от AGENTS.md.

## Goals / Non-Goals

**Goals:** 2–4 design-level goals — what the solution provides, not a retelling of FR/TR.

| Цель | Закрывает |
|---|---|
| <цель> | FR-N, TR-N |

**Non-Goals:** 2–5 technical temptations we are NOT doing. Do not repeat Scope from change.md.

- <не-цель>

## Decisions

Each decision — `### D<N>. <решение> (<архитектурное / БД / API / интеграционное>)`:

- Закрывает: FR-N, TR-N.
- Почему: 1–2 phrases — why it was chosen; the "what" contract (fields, DDL, payload) — in the conditional sections, not in D<N>.
- Проверяемость: <what to observe in a test> — only when the mechanism is not obviously verifiable or new test infrastructure is needed; otherwise no line.
- Отклонено: alternative — reason — only when a real alternative was considered; otherwise no line. Accepted decisions and rejected alternatives are recorded here; they are not in change.md.

## API

Only if the gRPC/HTTP contract changes per the change.md Impact; otherwise no section.

| Метод | Surface / путь | Изменение | BREAKING |
|---|---|---|---|
| <метод> | <surface, путь> | <что меняется> | Нет |

For new or changed methods:

| Поле | Тип | Обязательное | Constraints | Описание |
|---|---|---|---|---|
| <поле> | | | | |

- Валидация: rules QA verifies negatively.
- Идемпотентность: request key (repeat behavior — in the delta).
- Protobuf: changed messages, BREAKING and compatibility explicitly.

## Данные (БД)

Only if the schema or data changes per the change.md Impact; otherwise no section.

### Миграции

| Сущность | Тип миграции | Описание | Блокировки |
|---|---|---|---|
| <сущность> | | | |

### DDL-эскиз

```sql
-- PK, FK, UNIQUE, CHECK, indexes, NULL/DEFAULT — explicit
```

### Данные и откат

- Объём таблицы и блокировки при миграции.
- Backfill: what, volume, time.
- Влияние на существующие данные.
- Откат: DROP or leave nullable; what happens to the new version's data.

## Интеграции

Only the subsections touched per the change.md Impact; none — no section. What is touched and the level (additive / BREAKING) — in Impact; idempotency, retries, assumptions — in «Ограничениях» and the delta; here only the contract.

### Kafka

Per each new or changed topic / consumer group:

| Параметр | Значение |
|---|---|
| Топик / consumer group | |
| Ключ и значение | type, fields, schema reference |
| Эволюция схемы | version, change, BREAKING |

- Ключ дедупликации консьюмера, если есть.
- DLQ: topic, attempts.

### Внешние сервисы

Only a new call or a changed contract / call parameters. "No changes" rows are not written.

| Параметр | Значение |
|---|---|
| Endpoint / метод | |
| Request / Response | |
| Timeout / Retry | |
| Деградация | best-effort / fail-fast / fallback |
| Аутентификация | where credentials come from; never logged |
| Вызов | synchronous / asynchronous; inside or outside the transaction |

## Ошибки

Only new and changed errors. None — no section.

| Ситуация | Доменная ошибка | gRPC-код | Сообщение | Транзакция / ретрай |
|---|---|---|---|---|
| ERR-1: <exact condition> (→ AC-K, if the behavior is already a delta scenario) | <ошибка> | <код> | <текст> | <откат / ретрай> |

Unchanged errors held by regression: <list in one line> | нет.

## Риски / Trade-offs

Go through the categories: API / DB / Kafka-schema BREAKING; performance (N+1, locks, volume); data consistency (races, eventual consistency); external service (unavailability, contract); security (authorization, PII, tenants); result correctness (a shortcut in D<N>). Only real risks go in the table. None — no section.

| Риск | Категория | Вероятность | Влияние | Митигация | Владелец | Статус |
|---|---|---|---|---|---|---|
| <риск> | <категория> | | | | | открыт / в работе / принято / закрыто |

## Мониторинг / Алерты

Only when a metric or a log event with an alert is introduced; otherwise no section.

| Лог-эвент / метрика | Уровень | Когда | Алерт |
|---|---|---|---|
| <имя> | | | |

## Security / Compliance

Nothing changes — no section. Changes — a table only for the changed aspects:

| Аспект | Изменение | Описание |
|---|---|---|
| <аспект> | | |
