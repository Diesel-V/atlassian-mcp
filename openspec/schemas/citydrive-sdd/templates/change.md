# <name>

> Тип: <feature | bugfix | refactor | tech-debt>. Источник: <Jira key / Confluence page id / explore>

<!-- One fact — one section: neighboring sections give a reference, not a repeat. Do not delete required sections — an empty one says «нет»; content-conditional ones (Ограничения, Проверка QA) — nothing to write, no section. -->

## Что меняем

<!-- 2–5 one-line effect items; mechanics and config-key/field/method names — in Scope/Impact, not here. Breaking ones — **BREAKING**. -->

## Зачем

<!-- The problem or business goal, why now; what the user or a downstream system will see after rollout. -->

## Scope

**Входит:** ...
**Не входит:** ...
**Capabilities:** `<capability>` — new / modified. For refactor / tech-debt — «не затрагиваются».

## Impact

<!-- Every row required; an untouched area — «не затрагивается». Code — at package level. -->

| Область | Что затронуто | Изменение |
|---|---|---|
| Код | `internal/features/<f>/...` | ... |
| gRPC-методы | ... | ... |
| Kafka-топики | ... | ... |
| БД (таблицы, миграции) | ... | ... |
| Конфиг | ... | ... |
| Зависимости и тулинг | ... | ... |

## Требования

**Функциональные:**

- FR-1: <observable behavior, one line> → delta `<capability>` ADDED «<Requirement title>»

**Технические:**

<!-- One line — a verifiable technical property of the result (RFC 2119). How to implement (code structure, types) — in design. -->

- TR-1: <код / тулинг> SHALL <verifiable technical property>.

## Ограничения

<!-- Conditional: only unique facts for what is touched in Impact; do not repeat «Что меняем» and the delta. None — no section. -->

- Совместимость API и данных для затронутого в Impact.
- БД: миграции, soft delete, именование — по `openspec/config.yaml`; план отката схемы и топиков.
- Безопасность, производительность.
- Допущения.

## Критерии приёмки

- AC-1 (FR-1) [позитивный]: «<Scenario>» — Проверка: <способ>
- AC-2 (FR-1) [позитивный]: «<Scenario-1>», «<Scenario-2>» — Проверка: <способ>
- AC-3 (TR-1) [регрессия]: **GIVEN** <состояние> **WHEN** <действие> **THEN** <наблюдаемый результат> — Проверка: <способ>

## Проверка QA

<!-- Conditional: for ACs with «QA на стенде». No such ACs — no section. -->

- Стенд: ...
- Предусловия и тестовые данные: ... (how to obtain; no secrets)
- Точки наблюдения: gRPC-метод и код ответа / топик и событие / таблица и поле / лог
- QA проверяет: AC-... ; закрыты автотестами: AC-...
