package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cbl/cbl-kit/money"
)

// Ошибки домена mock-gateway.
var (
	// ErrInvalidRequest сообщает о некорректном запросе.
	ErrInvalidRequest = errors.New("mock-gateway: invalid request")
	// ErrInvoiceNotFound сообщает о неизвестном счете mock-провайдера.
	ErrInvoiceNotFound = errors.New("mock-gateway: invoice not found")
	// ErrInjectedFailure сообщает о сценарии сбоя провайдера.
	ErrInjectedFailure = errors.New("mock-gateway: injected provider failure")
	// ErrStoreFull сообщает о переполнении in-memory хранилища счетов.
	ErrStoreFull = errors.New("mock-gateway: invoice store is full")
)

// Status описывает статус счета и события mock-провайдера.
type Status string

// Статусы счетов и событий mock-провайдера.
const (
	// StatusPending описывает созданный, но не оплаченный счет.
	StatusPending Status = "pending"
	// StatusDetected описывает обнаруженную оплату.
	StatusDetected Status = "detected"
	// StatusConfirmed описывает подтвержденную оплату.
	StatusConfirmed Status = "confirmed"
	// StatusFailed описывает неуспешную оплату.
	StatusFailed Status = "failed"
	// StatusExpired описывает истечение счета.
	StatusExpired Status = "expired"
	// StatusUnderpaid описывает недоплату.
	StatusUnderpaid Status = "underpaid"
	// StatusOverpaid описывает переплату.
	StatusOverpaid Status = "overpaid"
)

// statusAliases сопоставляет строки simulate каноническим статусам события,
// повторяя таблицу псевдонимов коннектора (CBL-3.1), чтобы тесты могли
// оперировать провайдерскими формулировками.
var statusAliases = map[string]Status{
	"detected":  StatusDetected,
	"pending":   StatusDetected,
	"seen":      StatusDetected,
	"confirmed": StatusConfirmed,
	"completed": StatusConfirmed,
	"success":   StatusConfirmed,
	"failed":    StatusFailed,
	"error":     StatusFailed,
	"expired":   StatusExpired,
	"underpaid": StatusUnderpaid,
	"overpaid":  StatusOverpaid,
}

// CanonicalStatus переводит строку статуса simulate в канонический статус события.
func CanonicalStatus(raw string) (Status, error) {
	status, ok := statusAliases[strings.ToLower(strings.TrimSpace(raw))]
	if !ok {
		return "", fmt.Errorf("%w: status %q", ErrInvalidRequest, raw)
	}
	return status, nil
}

// SignatureMode описывает режим подписи вебхука для сценариев сбоев.
type SignatureMode string

// Режимы подписи вебхука.
const (
	// SignatureValid задает корректную подпись.
	SignatureValid SignatureMode = "valid"
	// SignatureInvalid задает испорченную подпись.
	SignatureInvalid SignatureMode = "invalid"
	// SignatureMissing задает отсутствие заголовка подписи.
	SignatureMissing SignatureMode = "missing"
)

// NormalizeSignature переводит строку режима подписи в значение; пусто - valid.
func NormalizeSignature(raw string) (SignatureMode, error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return SignatureValid, nil
	}
	switch SignatureMode(trimmed) {
	case SignatureValid, SignatureInvalid, SignatureMissing:
		return SignatureMode(trimmed), nil
	default:
		return "", fmt.Errorf("%w: signature mode %q", ErrInvalidRequest, raw)
	}
}

// Ограничения полей mock-провайдера: сценарии ограничены бюджетами обработчика
// (WriteTimeout) и лимитом тела запроса (NFR спеки).
const (
	maxAssetLen   = 16
	minAssetLen   = 2
	maxNetworkLen = 32
	maxTXIDLen    = 128
	maxLatencyMS  = 5_000
	maxFailNext   = 100
	maxDuplicates = 10
	maxDelayMS    = 5_000
)

// CreateInvoiceRequest описывает запрос создания счета.
type CreateInvoiceRequest struct {
	// IdempotencyKey задает ключ идемпотентности вызова.
	IdempotencyKey string
	// PaymentID задает платеж платформы.
	PaymentID string
	// Amount задает сумму счета.
	Amount money.Money
	// Network задает сеть оплаты.
	Network string
	// ExpiresAt задает срок действия счета.
	ExpiresAt time.Time
}

// Validate проверяет обязательные поля запроса создания счета.
func (r CreateInvoiceRequest) Validate() error {
	if _, err := uuid.Parse(strings.TrimSpace(r.PaymentID)); err != nil {
		return fmt.Errorf("%w: payment_id must be uuid", ErrInvalidRequest)
	}
	if _, err := money.New(r.Amount.MinorUnits, r.Amount.Asset, r.Amount.Decimals); err != nil {
		return fmt.Errorf("%w: amount: %w", ErrInvalidRequest, err)
	}
	if r.Amount.MinorUnits <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrInvalidRequest)
	}
	if !validCode(r.Amount.Asset, minAssetLen, maxAssetLen) {
		return fmt.Errorf("%w: invalid amount asset", ErrInvalidRequest)
	}
	if !validCode(r.Network, 1, maxNetworkLen) {
		return fmt.Errorf("%w: invalid network", ErrInvalidRequest)
	}
	if r.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: empty expires_at", ErrInvalidRequest)
	}
	return nil
}

// Invoice описывает счет mock-провайдера.
type Invoice struct {
	// GatewayID задает идентификатор счета у провайдера.
	GatewayID string
	// PaymentID задает платеж платформы.
	PaymentID string
	// Address задает mock-адрес оплаты.
	Address string
	// Network задает сеть оплаты.
	Network string
	// Amount задает ожидаемую сумму счета.
	Amount money.Money
	// Status задает текущий статус счета.
	Status Status
	// ReceivedAmount задает полученную сумму.
	ReceivedAmount money.Money
	// Confirmations задает глубину подтверждений.
	Confirmations uint32
	// TXID задает хеш последней транзакции.
	TXID string
	// EventTS задает время последнего события.
	EventTS time.Time
	// ExpiresAt задает срок действия счета.
	ExpiresAt time.Time
	// CreatedAt задает время создания счета.
	CreatedAt time.Time
}

// SimulateRequest описывает запрос генерации вебхука.
type SimulateRequest struct {
	// GatewayID задает счет, по которому отправляется событие.
	GatewayID string
	// Status задает статус события (канонический или псевдоним).
	Status string
	// ReceivedAmount задает полученную сумму; nil - сумма счета.
	ReceivedAmount *money.Money
	// Confirmations задает глубину подтверждений.
	Confirmations uint32
	// TXID задает хеш транзакции.
	TXID string
	// EventTS задает время события; nil - текущее время.
	EventTS *time.Time
	// DelayMS задает задержку отправки в миллисекундах.
	DelayMS int
	// Duplicate задает число отправок события; 0 - одна.
	Duplicate int
	// Signature задает режим подписи.
	Signature SignatureMode
}

// Validate проверяет поля запроса генерации вебхука.
func (r SimulateRequest) Validate() error {
	if strings.TrimSpace(r.GatewayID) == "" {
		return fmt.Errorf("%w: empty gateway_id", ErrInvalidRequest)
	}
	if _, err := CanonicalStatus(r.Status); err != nil {
		return err
	}
	if r.ReceivedAmount != nil {
		if _, err := money.New(r.ReceivedAmount.MinorUnits, r.ReceivedAmount.Asset, r.ReceivedAmount.Decimals); err != nil {
			return fmt.Errorf("%w: received_amount: %w", ErrInvalidRequest, err)
		}
		if r.ReceivedAmount.MinorUnits < 0 {
			return fmt.Errorf("%w: negative received_amount", ErrInvalidRequest)
		}
		if !validCode(r.ReceivedAmount.Asset, minAssetLen, maxAssetLen) {
			return fmt.Errorf("%w: invalid received_amount asset", ErrInvalidRequest)
		}
	}
	if len(r.TXID) > maxTXIDLen {
		return fmt.Errorf("%w: txid too long", ErrInvalidRequest)
	}
	if r.DelayMS < 0 || r.DelayMS > maxDelayMS {
		return fmt.Errorf("%w: delay_ms out of range", ErrInvalidRequest)
	}
	if r.Duplicate < 0 || r.Duplicate > maxDuplicates {
		return fmt.Errorf("%w: duplicate out of range", ErrInvalidRequest)
	}
	if r.Signature != SignatureValid && r.Signature != SignatureInvalid && r.Signature != SignatureMissing {
		return fmt.Errorf("%w: signature mode %q", ErrInvalidRequest, r.Signature)
	}
	return nil
}

// Scenario описывает сценарий сбоя API провайдера.
type Scenario struct {
	// FailNext задает число следующих вызовов, отвечающих ошибкой 503.
	FailNext int
	// LatencyMS задает добавляемую задержку ответа.
	LatencyMS int
}

// Validate проверяет параметры сценария.
func (s Scenario) Validate() error {
	if s.FailNext < 0 || s.FailNext > maxFailNext {
		return fmt.Errorf("%w: fail_next out of range", ErrInvalidRequest)
	}
	if s.LatencyMS < 0 || s.LatencyMS > maxLatencyMS {
		return fmt.Errorf("%w: latency_ms out of range", ErrInvalidRequest)
	}
	return nil
}

// WebhookDelivery описывает запись журнала отправки вебхука.
type WebhookDelivery struct {
	// EventID задает идентификатор события вебхука.
	EventID string
	// GatewayID задает счет провайдера.
	GatewayID string
	// Status задает статус события.
	Status Status
	// Mode задает режим подписи.
	Mode SignatureMode
	// SentAt задает время отправки.
	SentAt time.Time
	// HTTPStatus задает код ответа получателя; 0 - сетевой сбой.
	HTTPStatus int
	// Error задает текст ошибки доставки.
	Error string
}

// validCode проверяет код (актив, сеть): латиница, цифры, дефис и подчеркивание.
func validCode(value string, minLen, maxLen int) bool {
	if len(value) < minLen || len(value) > maxLen {
		return false
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9', b == '-', b == '_':
		default:
			return false
		}
	}
	return true
}
