package usecase

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/cbl/cbl-kit/money"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
)

// deliveriesLimit задает предельный размер журнала отправок.
const deliveriesLimit = 100

// maxInvoices задает предел числа счетов в in-memory хранилище:
// неаутентифицированный стенд не должен расти без предела (review 3.6).
const maxInvoices = 1000

// simulateSendBudget задает бюджет фоновых доставок simulate: вебхук обязан
// уйти даже при обрыве клиента, но не может висеть бесконечно.
const simulateSendBudget = 90 * time.Second

// WebhookEvent описывает вебхук mock-провайдера для отправки в Webhook Ingress.
type WebhookEvent struct {
	// EventID задает идентификатор события; повторные доставки используют тот же id.
	EventID string
	// PaymentID задает платеж платформы.
	PaymentID string
	// GatewayID задает счет провайдера.
	GatewayID string
	// Status задает канонический статус события.
	Status domain.Status
	// ReceivedAmount задает полученную сумму.
	ReceivedAmount money.Money
	// Confirmations задает глубину подтверждений.
	Confirmations uint32
	// TXID задает хеш транзакции.
	TXID string
	// Network задает сеть оплаты.
	Network string
	// EventTS задает время события у провайдера.
	EventTS time.Time
	// Mode задает режим подписи для сценариев сбоев.
	Mode domain.SignatureMode
}

// Delivery описывает результат отправки вебхука.
type Delivery struct {
	// SentAt задает время отправки.
	SentAt time.Time
	// HTTPStatus задает код ответа получателя; 0 - сетевой сбой.
	HTTPStatus int
	// Error задает текст ошибки доставки.
	Error string
}

// WebhookSender отправляет вебхук получателю (Webhook Ingress).
type WebhookSender interface {
	// Send подписывает и отправляет событие, возвращая результат доставки.
	Send(ctx context.Context, event WebhookEvent) Delivery
}

// SimulateResult описывает результат simulate: идентификатор события и доставки.
type SimulateResult struct {
	// EventID задает идентификатор сгенерированного события.
	EventID string
	// Deliveries задает результаты всех отправок.
	Deliveries []Delivery
}

// Service хранит счета mock-провайдера, сценарии и журнал отправок.
// Состояние in-memory: сервис детерминированно пересоздается в тестах.
type Service struct {
	mu         sync.Mutex
	sender     WebhookSender
	invoices   map[string]domain.Invoice
	byKey      map[string]string
	deliveries []domain.WebhookDelivery
	scenario   domain.Scenario
	clock      func() time.Time
	log        *zap.Logger
}

// Option настраивает Service.
type Option func(*Service)

// WithClock задает источник времени; используется тестами.
func WithClock(clock func() time.Time) Option {
	return func(s *Service) {
		if clock != nil {
			s.clock = clock
		}
	}
}

// NewService создает Service поверх отправителя вебхуков.
func NewService(sender WebhookSender, log *zap.Logger, opts ...Option) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	service := &Service{
		sender:   sender,
		invoices: make(map[string]domain.Invoice),
		byKey:    make(map[string]string),
		clock:    time.Now,
		log:      log,
	}
	for _, opt := range opts {
		opt(service)
	}
	return service
}

// CreateInvoice создает счет; повтор с тем же Idempotency-Key возвращает исходный счет.
func (s *Service) CreateInvoice(ctx context.Context, req domain.CreateInvoiceRequest) (domain.Invoice, bool, error) {
	if err := req.Validate(); err != nil {
		return domain.Invoice{}, false, err
	}
	if err := s.applyScenario(ctx); err != nil {
		return domain.Invoice{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if key := strings.TrimSpace(req.IdempotencyKey); key != "" {
		if gatewayID, ok := s.byKey[key]; ok {
			return s.invoices[gatewayID], true, nil
		}
	}
	if len(s.invoices) >= maxInvoices {
		return domain.Invoice{}, false, fmt.Errorf("%w: limit %d", domain.ErrStoreFull, maxInvoices)
	}
	gatewayID, err := uuid.NewV7()
	if err != nil {
		return domain.Invoice{}, false, fmt.Errorf("usecase: generate gateway id: %w", err)
	}
	now := s.clock().UTC()
	invoice := domain.Invoice{
		GatewayID: gatewayID.String(),
		PaymentID: strings.TrimSpace(req.PaymentID),
		Address:   mockAddress(gatewayID.String()),
		Network:   strings.TrimSpace(req.Network),
		Amount:    req.Amount,
		Status:    domain.StatusPending,
		ReceivedAmount: money.Money{
			Asset:    req.Amount.Asset,
			Decimals: req.Amount.Decimals,
		},
		ExpiresAt: req.ExpiresAt.UTC(),
		CreatedAt: now,
	}
	s.invoices[invoice.GatewayID] = invoice
	if key := strings.TrimSpace(req.IdempotencyKey); key != "" {
		s.byKey[key] = invoice.GatewayID
	}
	s.log.Info("mock invoice created",
		zap.String("gateway_id", invoice.GatewayID),
		zap.String("payment_id", invoice.PaymentID),
	)
	return invoice, false, nil
}

// GetInvoice возвращает счет по идентификатору провайдера.
func (s *Service) GetInvoice(ctx context.Context, gatewayID string) (domain.Invoice, error) {
	if err := s.applyScenario(ctx); err != nil {
		return domain.Invoice{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	invoice, ok := s.invoices[strings.TrimSpace(gatewayID)]
	if !ok {
		return domain.Invoice{}, fmt.Errorf("%w: %s", domain.ErrInvoiceNotFound, gatewayID)
	}
	return invoice, nil
}

// Simulate обновляет состояние счета и отправляет вебхук (или несколько дубликатов).
func (s *Service) Simulate(ctx context.Context, req domain.SimulateRequest) (SimulateResult, error) {
	if err := req.Validate(); err != nil {
		return SimulateResult{}, err
	}
	status, err := domain.CanonicalStatus(req.Status)
	if err != nil {
		return SimulateResult{}, err
	}

	s.mu.Lock()
	gatewayID := strings.TrimSpace(req.GatewayID)
	invoice, ok := s.invoices[gatewayID]
	if !ok {
		s.mu.Unlock()
		return SimulateResult{}, fmt.Errorf("%w: %s", domain.ErrInvoiceNotFound, gatewayID)
	}
	eventTS := s.clock().UTC()
	if req.EventTS != nil {
		eventTS = req.EventTS.UTC()
	}
	received := invoice.Amount
	if req.ReceivedAmount != nil {
		received = *req.ReceivedAmount
	}
	invoice.Status = status
	invoice.ReceivedAmount = received
	invoice.Confirmations = req.Confirmations
	invoice.TXID = strings.TrimSpace(req.TXID)
	invoice.EventTS = eventTS
	s.invoices[gatewayID] = invoice
	s.mu.Unlock()

	eventID, err := uuid.NewV7()
	if err != nil {
		return SimulateResult{}, fmt.Errorf("usecase: generate event id: %w", err)
	}
	event := WebhookEvent{
		EventID:        eventID.String(),
		PaymentID:      invoice.PaymentID,
		GatewayID:      invoice.GatewayID,
		Status:         status,
		ReceivedAmount: received,
		Confirmations:  req.Confirmations,
		TXID:           invoice.TXID,
		Network:        invoice.Network,
		EventTS:        eventTS,
		Mode:           req.Signature,
	}
	// Доставка отделена от контекста запроса: задержка и отправка вебхука
	// должны завершиться даже при обрыве клиента (сценарий delay > timeout).
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), simulateSendBudget)
	defer cancel()
	if err := waitDelay(sendCtx, req.DelayMS); err != nil {
		return SimulateResult{}, err
	}

	attempts := req.Duplicate
	if attempts == 0 {
		attempts = 1
	}
	result := SimulateResult{EventID: event.EventID, Deliveries: make([]Delivery, 0, attempts)}
	for i := 0; i < attempts; i++ {
		delivery := s.sender.Send(sendCtx, event)
		result.Deliveries = append(result.Deliveries, delivery)
		s.appendDelivery(domain.WebhookDelivery{
			EventID:    event.EventID,
			GatewayID:  event.GatewayID,
			Status:     event.Status,
			Mode:       event.Mode,
			SentAt:     delivery.SentAt,
			HTTPStatus: delivery.HTTPStatus,
			Error:      delivery.Error,
		})
	}
	return result, nil
}

// ListDeliveries возвращает копию журнала отправок в порядке отправки.
func (s *Service) ListDeliveries() []domain.WebhookDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.WebhookDelivery(nil), s.deliveries...)
}

// SetScenario задает сценарий сбоя API провайдера.
func (s *Service) SetScenario(scenario domain.Scenario) error {
	if err := scenario.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenario = scenario
	return nil
}

// GetScenario возвращает текущий сценарий сбоя.
func (s *Service) GetScenario() domain.Scenario {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scenario
}

// ResetScenario сбрасывает сценарий сбоя.
func (s *Service) ResetScenario() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenario = domain.Scenario{}
}

// applyScenario применяет задержку и внедренный сбой к вызову API провайдера.
func (s *Service) applyScenario(ctx context.Context) error {
	s.mu.Lock()
	scenario := s.scenario
	if s.scenario.FailNext > 0 {
		s.scenario.FailNext--
	}
	s.mu.Unlock()
	if scenario.LatencyMS > 0 {
		if err := waitDelay(ctx, scenario.LatencyMS); err != nil {
			return err
		}
	}
	if scenario.FailNext > 0 {
		return fmt.Errorf("%w: fail_next", domain.ErrInjectedFailure)
	}
	return nil
}

// appendDelivery добавляет запись в журнал, поддерживая предельный размер.
func (s *Service) appendDelivery(delivery domain.WebhookDelivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries = append(s.deliveries, delivery)
	if len(s.deliveries) > deliveriesLimit {
		s.deliveries = s.deliveries[len(s.deliveries)-deliveriesLimit:]
	}
}

// waitDelay ждет заданное число миллисекунд с учетом отмены контекста.
func waitDelay(ctx context.Context, delayMS int) error {
	if delayMS <= 0 {
		return nil
	}
	timer := time.NewTimer(time.Duration(delayMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// mockAddress строит детерминированный mock-адрес по идентификатору счета.
func mockAddress(gatewayID string) string {
	compact := strings.ReplaceAll(gatewayID, "-", "")
	if len(compact) > 12 {
		compact = compact[:12]
	}
	return "MOCK" + strings.ToUpper(compact)
}
