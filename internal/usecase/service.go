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

// deliveriesLimit sets the maximum size of the delivery log.
const deliveriesLimit = 100

// maxInvoices sets the invoice count limit of the in-memory store:
// the unauthenticated stand must not grow without bound (review 3.6).
const maxInvoices = 1000

// simulateSendBudget sets the budget of simulate background deliveries: the webhook must
// be sent even when the client disconnects, but it must not hang forever.
const simulateSendBudget = 90 * time.Second

// WebhookEvent describes a mock provider webhook to be sent to Webhook Ingress.
type WebhookEvent struct {
	// EventID sets the event identifier; repeated deliveries use the same id.
	EventID string
	// PaymentID sets the platform payment.
	PaymentID string
	// GatewayID sets the provider invoice.
	GatewayID string
	// Status sets the canonical event status.
	Status domain.Status
	// ReceivedAmount sets the received amount.
	ReceivedAmount money.Money
	// Confirmations sets the confirmation depth.
	Confirmations uint32
	// TXID sets the transaction hash.
	TXID string
	// Network sets the payment network.
	Network string
	// EventTS sets the event time at the provider.
	EventTS time.Time
	// Mode sets the signature mode for failure scenarios.
	Mode domain.SignatureMode
}

// Delivery describes the result of a webhook send.
type Delivery struct {
	// SentAt sets the send time.
	SentAt time.Time
	// HTTPStatus sets the receiver response code; 0 means a network failure.
	HTTPStatus int
	// Error sets the delivery error text.
	Error string
}

// WebhookSender sends a webhook to the receiver (Webhook Ingress).
type WebhookSender interface {
	// Send signs and sends the event, returning the delivery result.
	Send(ctx context.Context, event WebhookEvent) Delivery
}

// SimulateResult describes the simulate result: the event identifier and the deliveries.
type SimulateResult struct {
	// EventID sets the identifier of the generated event.
	EventID string
	// Deliveries sets the results of all sends.
	Deliveries []Delivery
}

// Service stores mock provider invoices, scenarios and the delivery log.
// The state is in-memory: the service is deterministically recreated in tests.
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

// Option configures Service.
type Option func(*Service)

// WithClock sets the time source; used by tests.
func WithClock(clock func() time.Time) Option {
	return func(s *Service) {
		if clock != nil {
			s.clock = clock
		}
	}
}

// NewService creates a Service on top of the webhook sender.
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

// CreateInvoice creates an invoice; a repeat with the same Idempotency-Key returns the original invoice.
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

// GetInvoice returns an invoice by the provider identifier.
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

// Simulate updates the invoice state and sends a webhook (or several duplicates).
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
	// Delivery is detached from the request context: the delay and the webhook send
	// must complete even when the client disconnects (delay > timeout scenario).
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

// ListDeliveries returns a copy of the delivery log in send order.
func (s *Service) ListDeliveries() []domain.WebhookDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.WebhookDelivery(nil), s.deliveries...)
}

// SetScenario sets the provider API failure scenario.
func (s *Service) SetScenario(scenario domain.Scenario) error {
	if err := scenario.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenario = scenario
	return nil
}

// GetScenario returns the current failure scenario.
func (s *Service) GetScenario() domain.Scenario {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scenario
}

// ResetScenario resets the failure scenario.
func (s *Service) ResetScenario() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenario = domain.Scenario{}
}

// applyScenario applies the latency and the injected failure to a provider API call.
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

// appendDelivery adds an entry to the log while keeping the size limit.
func (s *Service) appendDelivery(delivery domain.WebhookDelivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries = append(s.deliveries, delivery)
	if len(s.deliveries) > deliveriesLimit {
		s.deliveries = s.deliveries[len(s.deliveries)-deliveriesLimit:]
	}
}

// waitDelay waits for the given number of milliseconds respecting context cancellation.
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

// mockAddress builds a deterministic mock address from the invoice identifier.
func mockAddress(gatewayID string) string {
	compact := strings.ReplaceAll(gatewayID, "-", "")
	if len(compact) > 12 {
		compact = compact[:12]
	}
	return "MOCK" + strings.ToUpper(compact)
}
