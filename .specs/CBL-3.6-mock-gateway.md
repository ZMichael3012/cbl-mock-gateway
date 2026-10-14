# CBL-3.6. Mock-gateway: sandbox-провайдер с подписанными вебхуками

| Поле | Значение |
| :-- | :-- |
| Эпик | E3, Gateway Connector |
| Репозиторий | `cbl-mock-gateway` |
| Размер | M |
| Волна и параллельная группа | W1-CONNECTOR |
| Зависит от | 1.7 (CF-1), 1.6 (HMAC), 3.5 (формат вебхука) |
| Версия контрактов | `cbl-protos@cf-1` (события `billing/events/v1`) |
| Статус | done (ревью закрыто, коммит CBL-3.6) |

## 1. Контекст и цель

До фиксации реального провайдера (KI-01) платформе нужен sandbox-шлюз: адаптер 3.2, e2e
платежных потоков (CBL-002, раздел 7, GE-1) и проверки устойчивости 3.3 работают против
mock-провайдера. Сервис эмулирует API шлюза, отправляет подписанные HMAC вебхуки по
контракту sandbox-провайдера (CBL-3.5) в Webhook Ingress и умеет воспроизводить сбои:
недоступность, задержки, дубликаты, неверную подпись, нештатные статусы.

## 2. Границы

In scope: HTTP API провайдера (счета и статусы), отправка подписанных вебхуков, сценарии
сбоев (fail_next, latency, duplicate, delay, signature mode), журнал отправок, e2e проверка
контракта вебхука, compose и smoke.

Out of scope: реальные крипто-операции и адреса, адаптер 3.2 (клиент этого API), конвейер
нормализации 3.5, устойчивость клиента 3.3, prod-использование.

## 3. Функциональные требования

FR-1. API провайдера (без аутентификации, стенд доверенной сети):
- `POST /api/v1/invoices`: тело `{payment_id: uuid, amount: Money, network, expires_at}`;
  создает счет c `gateway_id` (UUIDv7), детерминированным mock-адресом и статусом
  `pending`; заголовок `Idempotency-Key` делает повтор возвратом того же счета
  (`replayed: true`), без дубля; ответ `201`/`200`;
- `GET /api/v1/invoices/{gateway_id}`: текущее состояние счета, включая последние
  `status`, `received_amount`, `confirmations`, `txid`, `event_ts`; `404` для неизвестного;
- валидация: `payment_id` - UUID, `amount.minor_units > 0`, asset 2 - 16, decimals до 18,
  `network` из charset кода, `expires_at` непустое; ошибки - `400 problem+json`.

FR-2. Вебхуки (контракт sandbox-провайдера из CBL-3.5):
- `POST /api/v1/simulate`: тело `{gateway_id, status, received_amount?, confirmations?,
  txid?, event_ts?, delay_ms?, duplicate?, signature?}`; статус из канонического набора
  3.1 (detected, confirmed, failed, expired, underpaid, overpaid; плюс псевдонимы
  pending/completed/error); simulate обновляет состояние счета и отправляет вебхук;
- тело вебхука - JSON sandbox-контракта (`payment_id`, `gateway_id`, `status`,
  `received_amount`, `confirmations`, `txid`, `network`, `event_ts`);
- подпись: kit `hmac.Sign` по сырому телу, заголовок `t=<unix>,v1=<hex>`
  (`hmac.BuildHeader`); имена заголовков подписи и идентификатора события
  конфигурируются (`X-Billing-Signature`, `X-Event-Id` по умолчанию) и совпадают с
  конфигурацией Webhook Ingress;
- `signature`: `valid` (default), `invalid` (порченная подпись), `missing` (заголовок
  отсутствует) - для проверки отказа Ingress;
- `duplicate` (default 1, до 10): повторная отправка того же события (тот же `event_id`
  и тело) - проверка дедупликации Ingress; `delay_ms` (до 5000) откладывает отправку
  (сценарии гонок, порядка, истечения), доставка отделена от контекста запроса и
  завершается даже при обрыве клиента; `event_ts` переопределяет время события (события
  не по порядку);
- результат: `200` со списком доставок `{http_status, error, mode}` (ошибки доставки не
  меняют код ответа simulate); каждая доставка пишется в журнал; редиректы получателя не
  выполняются (3xx считается недоставкой).

FR-3. Сценарии сбоев API провайдера:
- `POST /api/v1/scenarios {fail_next, latency_ms}`: следующие `fail_next` вызовов API
  провайдера отвечают `503 problem+json` (проверка ретраев и circuit breaker 3.3),
  `latency_ms` (до 5000) добавляет задержку к ответу (проверка таймаутов);
- `GET /api/v1/scenarios` - текущее состояние, `POST /api/v1/scenarios/reset` - сброс.

FR-4. Журнал отправок: `GET /api/v1/webhooks` отдает последние 100 доставок
(`event_id`, `gateway_id`, `status`, `signature mode`, `sent_at`, `http_status`, `error`);
журнал in-memory с ограничением размера.

FR-5. Хранилище - in-memory с мьютексом (счета до 1000 записей, сценарии, журнал
последних 100 отправок), состояние теряется при рестарте: mock-сервис детерминированно
пересоздается в тестах. Переполнение счетов - `503 store_full`. Деньги - int64 minimal
units плюс decimals, float запрещен.

FR-6. Конфигурация: `HTTP_ADDR` (:8120), `METRICS_ADDR` (:9320), `WEBHOOK_TARGET_URL`
(default `http://localhost:8060/hooks/mock/ep1`), `WEBHOOK_SECRET` (default `mock-secret`),
`WEBHOOK_SIGNATURE_HEADER`, `WEBHOOK_EVENT_ID_HEADER`, `WEBHOOK_TIMEOUT` (default 5s).
Секрет не логируется.

FR-7. E2E: Go-тест (tag `e2e`) поднимает mock-gateway и локальный sink: sink проверяет
подпись kit `hmac.Verify`, дедуплицирует по `event_id` и парсит тело как sandbox-контракт;
сценарии: create -> simulate confirmed с дубликатом -> две доставки одного event_id ->
подпись валидна -> повторная доставка отвергается дедупликацией sink; invalid/missing
подпись отвергается sink; delay/out-of-order и fail_next проверяются на API. Compose-смоук:
healthz, создание счета, simulate, журнал отправок; живая интеграция с Webhook Ingress -
глобальный GE-1.

## 4. Контракты

- REST (эмуляция провайдера): см. FR-1 - FR-4; ошибки `application/problem+json` через
  ginmw. Webhook-тело - контракт sandbox-провайдера CBL-3.5 (JSON), подпись - kit hmac
  (совместима с Webhook Ingress 5.1). `cbl-protos@cf-1` не меняется.

## 5. Модель данных и миграции

Миграций нет (in-memory). Структуры: `Invoice`, `WebhookDelivery`, `Scenario` в domain.

## 6. Конечные автоматы и правила

Статус счета меняется только через simulate (mock не проверяет монотонность - это зона
Payment 4.3); `pending` - начальный статус, simulate переводит в запрошенный канонический
статус. Повтор simulate допустим: он моделирует повторные/дублирующие вебхуки провайдера.

## 7. Edge cases

Повтор create с тем же `Idempotency-Key` (тот же счет), simulate неизвестного счета (404),
`received_amount` другого актива счета (принимается - сценарий underpaid/overpaid в другой
сумме), нулевые подтверждения, пустой txid, `delay_ms` больше таймаута клиента (доставка
завершается отделенным контекстом), недоступный `WEBHOOK_TARGET_URL` (ошибка в журнале,
simulate отвечает 200), редирект получателя (3xx - недоставка, тело не уходит на другой
хост), `fail_next` исчерпывается, конкурентные simulate, переполнение журнала (100),
переполнение счетов (503 store_full), тело больше 64 КБ (413), неизвестные поля JSON (400).

## 8. Нефункциональные требования

- Сервис тестовый: без аутентификации, не выкладывается в prod (KI-mock-gateway-02);
  слушает доверенную сеть compose; секреты только из env.
- Бюджет ответа API - локальные миллисекунды (за вычетом сценарной задержки);
  `CreateInvoice` для адаптера 3.2 укладывается в p99 < 1 с (CBL-002).
- Тела запросов ограничены 64 КБ (`MaxBytesReader`, 413), JSON строгий
  (`DisallowUnknownFields`, trailing data запрещены); `WriteTimeout` (75 с) покрывает
  худший сценарий latency 5 с + delay 5 с + 10 доставок по `WEBHOOK_TIMEOUT`.
- Доставка simulate отделена от контекста запроса (бюджет 90 с): обрыв клиента не
  отменяет вебхук и не теряет запись журнала.
- Параллелизм - мьютекс, гонки исключены; покрытие unit+e2e не ниже 80%;
  денежные значения int64/decimals.

## 9. План тестов

| Уровень | Сценарии |
| :-- | :-- |
| Smoke | `go build ./...`; compose: healthz, create, simulate, журнал (`test/e2e/smoke.ps1`) |
| Unit | domain (валидация, статусы), usecase (идемпотентность create, simulate, сценарии, журнал, подпись через fake sender + kit hmac.Verify), transport (маршруты, problem+json, fail_next 503) |
| Integration | не применимо: состояние in-memory, внешних БД нет |
| E2E | tag `e2e`: mock-gateway + sink с kit hmac.Verify и дедупликацией; duplicate, invalid/missing подпись, delay, fail_next |
| Load | 14.1 (всплеск вебхуков у Ingress) |

## 10. План работ и коммит

1. Спека, тесты (unit + e2e), затем реализация: domain, usecase, transport, webhook sender,
   main и env, compose, smoke.
2. Четыре ревью-прохода, AGENTS.md, линтеры, финальные прогоны.
3. Коммит `CBL-3.6: sandbox-шлюз с подписанными вебхуками и сценариями сбоев`; спека в том
   же коммите.

## 11. Открытые вопросы и решения

- ADR: формат вебхука повторяет контракт CBL-3.5 (sandbox-провайдер) - единый источник для
  коннектора и тестов; расхождение только через обновление обеих спек.
- ADR: simulate всегда отвечает `200` при валидном запросе: тесты различают недоставку по
  журналу и `http_status`, а не по коду simulate (ошибки валидации дают 400/404/413/503).
- ADR: лимиты сценариев (delay/latency до 5 с, duplicate до 10) выведены из бюджета
  обработчика (`WriteTimeout` 75 с); доставка ведется отделенным контекстом (90 с), чтобы
  сценарий "delay больше таймаута клиента" воспроизводился.
- ADR: редиректы получателя не выполняются (режим "не следовать"): цель из env не должна
  переадресовывать тело события и подпись на другой хост.
- Принято с ограничением: SSRF-поверхность ограничена env `WEBHOOK_TARGET_URL`
  (конфигурирует оператор стенда); in-memory счета ограничены 1000 записями; сервис не
  выкладывается в prod (KI-mock-gateway-02).
- Открытый вопрос: реальные коды/подписи провайдера номер 1 появятся в 3.2; mock останется
  sandbox-слоем для тестов.

## 12. Ревью (CBL-001, шаг 6)

Четыре независимых прохода (14.10.2026). Blocker безопасности (неограниченное тело запроса
и поля без лимитов у неаутентифицированного сервиса) закрыт лимитом 64 КБ и строгим JSON.
Все major и minor закрыты или зафиксированы.

| Роль | Ключевые замечания | Закрытие |
| :-- | :-- | :-- |
| Без контекста | в ответе simulate нет `mode`; delay/latency больше серверных таймаутов; лимит тела не реализован; нет unit-тестов webhook-пакета; concurrent-тест без assert; smoke не проверяет доставку | `mode` добавлен в доставки; лимиты выведены из `WriteTimeout` (75 с) и отделенного контекста; `MaxBytesReader` 64 КБ + строгий JSON; добавлены sender-тесты и assert-ы; smoke проверяет статусы доставок |
| Архитектуры | бюджет сценариев против таймаутов; `delay_ms` отменялся контекстом клиента; отсутствие лимита счетов; редиректы; compose-target localhost; global compose без связи с ingress; storage-doc вводит в заблуждение | отделенный контекст доставки (90 с); cap 1000 счетов (503 store_full); редиректы запрещены; target `host.docker.internal`; global compose: явные env + depends_on webhook-ingress; doc storage/comments исправлены |
| С контекстом | NFR 64 КБ/строгий JSON не выполнен; `mode` нет в ответе; trade-off T-MOCK-02 неточен; журнал опережал ревью; cover.out в дереве; latency/duplicate bounds | NFR реализован и покрыт тестами 413/400; `mode` добавлен; формулировки исправлены; артефакты чистятся, `cover.out` в .gitignore; границы обновлены в спеке |
| Безопасности (BLOCK снят) | тело без лимита (memory DoS); delay до 60 с против WriteTimeout; неограниченные счета; редиректы (SSRF); asset/txid без лимитов; default-секрет | 64 КБ + 413; сценарии <=5 с и бюджет 75/90 с; cap 1000; редиректы не следуются; charset/длины asset/txid в Validate; default-секрет и отсутствие аутентификации - принятый риск стенда (KI-mock-gateway-02) |
