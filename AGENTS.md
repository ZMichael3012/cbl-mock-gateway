# AGENTS.md - cbl-mock-gateway

Mock-провайдер для тестов и стендов: эмуляция API шлюза и подписанных вебхуков.

Обязательные глобальные правила: `../../AGENTS.md`, `../../.specs/CBL-001-development-process.md`,
`../../.specs/CBL-002-repos-and-docker.md`, контракты `../../.specs/CBL-003-contracts.md`.
Длинное тире (U+2014) запрещено, используется только дефис.

## 1. Границы

In scope: создание счетов, статусы, подписанные вебхуки, сценарии сбоев.

Out of scope: другие сервисы платформы; взаимодействие только через контракты `cbl-protos`
(gRPC), Kafka и REST периметра (см. `CBL-000`, раздел 2).

## 2. Контракты

- REST: `POST /api/v1/invoices`, `GET /api/v1/invoices/{gateway_id}` - эмуляция API
  провайдера; `POST /api/v1/simulate` - отправка подписанных вебхуков; `GET /api/v1/webhooks`
  - журнал отправок; `POST|GET /api/v1/scenarios`, `POST /api/v1/scenarios/reset` - сценарии
  сбоев; вебхуки уходят на Webhook Ingress по контракту sandbox-провайдера (CBL-3.5)
- gRPC: нет
- Kafka продюсирует: нет
- Kafka потребляет: нет
- Хранилище: in-memory (счета, сценарии, журнал последних 100 отправок)

Контракты заморожены в `cbl-protos` (CF-1 для gRPC, CF-2/CF-3 для событий), см. `CBL-004`.

## 3. Архитектура

Слои: `internal/transport` -> `internal/usecase` -> `internal/domain`; отправка подписанных
вебхуков - адаптер `internal/webhook` через порт `usecase.WebhookSender`.
`internal/storage` не используется: состояние стенда in-memory (T-MOCK-01).
Библиотеки: Gin, zap, cbl-kit (hmac, ginmw, money), google/uuid; GORM, franz-go, go-redis,
goose и PostgreSQL mock-сервису не нужны - отклонение от набора CBL-002 зафиксировано
trade-off T-MOCK-01.

## 4. Команды

```powershell
make build        # go build ./...
make test         # go test ./...
make test-e2e     # сквозной сценарий вебхук-контракта (tag e2e)
make smoke        # compose-стенд: healthz, create/replay, simulate, журнал
make vet          # go vet ./...
make lint         # golangci-lint в Docker
make up           # docker compose -f deploy/docker-compose.yml --profile app up -d
make down         # docker compose -f deploy/docker-compose.yml down -v
```

## 5. Порты и окружение

HTTP `8120`, gRPC `-`, метрики `9320`.
Переменные (применимые к сервису): `HTTP_ADDR`, `METRICS_ADDR`, `WEBHOOK_TARGET_URL`
(default `http://localhost:8060/hooks/mock/ep1`), `WEBHOOK_SECRET` (default `mock-secret`),
`WEBHOOK_SIGNATURE_HEADER` (default `X-Billing-Signature`), `WEBHOOK_EVENT_ID_HEADER`
(default `X-Event-Id`), `WEBHOOK_TIMEOUT` (default 5s).

## 6. Правила разработки

- Конвейер: SDD-спека в `.specs/` -> архитектура -> контракты -> тесты вперед -> код ->
  ревью 4 агентами (без контекста, архитектура, контекст, безопасность - блокирующий) ->
  AGENTS.md -> финальные прогоны -> линтеры -> коммит.
- Покрытие unit+integration не ниже 80%; денежные сценарии только на реальных PostgreSQL и
  Kafka (testcontainers); локальный e2e в docker-compose обязателен.
- Деньги только int64 в минимальных единицах плюс decimals; float запрещен.
- Docstring на русском в стиле godoc (первая строка с имени сущности, точка в конце);
  комментарии объясняют почему; ошибки через `%w`; панки в бизнес-коде запрещены.
- Коммит: `CBL-<эпик>.<номер>: <краткое сообщение>`, спека с тем же префиксом в том же коммите.

## 7. SLO и метрики

SLO уточняется в задаче 14.4. Базовые метрики: latency p95/p99, доля ошибок, outbox lag,
consumer lag; дашборды после задачи 0.7.

## 8. Trade-offs

| ID | Решение | Плюсы | Минусы и компенсация |
| :-- | :-- | :-- | :-- |
| T-MOCK-01 | Состояние in-memory (счета, сценарии, журнал) | простой и детерминированный стенд, нет зависимостей | состояние теряется при рестарте, нет масштабируемости; набор библиотек CBL-002 не задействован | счета ограничены 1000 записями (503 store_full), журнал 100; тесты пересоздают сервис |
| T-MOCK-02 | `simulate` отвечает `200` при валидном запросе со списком доставок | тесты различают недоставку по `http_status`/журналу | недоставка не видна по коду ответа | журнал `GET /api/v1/webhooks`, поле `error` и `mode` фиксируют результат; ошибки валидации дают 400/404/413/503 |
| T-MOCK-03 | Формат вебхука повторяет контракт CBL-3.5 (sandbox-провайдер) | единый источник формата для Connector и тестов | mock не проверяет формат на живом шлюзе | расхождение только через обновление обеих спек; реальные коды - 3.2 |
| T-MOCK-04 | Редиректы получателя не выполняются; доставка simulate отделена от контекста клиента (90 с) | тело и подпись не уходят на редирект; обрыв клиента не теряет вебхук | сценарий `delay` занимает обработчик до завершения | лимиты сценариев (5 с/10 дублей) и `WriteTimeout` 75 с выведены совместно |

## 9. Known issues

| ID | Проблема | Влияние | План | Статус |
| :-- | :-- | :-- | :-- | :-- |
| KI-mock-gateway-01 | CI и pre-commit не настроены | линтеры запускаются вручную | задача 0.2 | open |
| KI-mock-gateway-02 | Нет аутентификации; simulate может сформировать любое событие; `WEBHOOK_TARGET_URL` настраивается оператором (SSRF-поверхность, редиректы не выполняются); default-секрет `mock-secret` | сервис пригоден только для стенда доверенной сети | не выкладывать в prod; реальные интеграции - через провайдера и mTLS (0.6) | open |

## 10. Журнал задач

| CBL | Дата | Статус | Спека | Коммит |
| :-- | :-- | :-- | :-- | :-- |
| CBL-3.6 | 2026-10-14 | done, ревью закрыто (API провайдера, подписанные вебхуки, сценарии сбоев, e2e контракта, покрытие 89.4%) | `.specs/CBL-3.6-mock-gateway.md` | этот коммит |

## 11. Runbook

Диагностика стенда mock-gateway:

1. `GET /api/v1/scenarios`: включен ли `fail_next`/`latency_ms`; сброс - `/scenarios/reset`.
2. `GET /api/v1/webhooks`: последние доставки с `http_status` и `error`; код 0 - сетевой
   сбой (проверить `WEBHOOK_TARGET_URL` и Webhook Ingress), 401 - подпись отвергнута
   (режим `invalid`/`missing` или секрет не совпадает с провайдером Ingress).
3. Счет не найден - состояние in-memory теряется при рестарте, пересоздать счет.
4. Секрет и заголовки подписи должны совпадать с конфигурацией Webhook Ingress.
