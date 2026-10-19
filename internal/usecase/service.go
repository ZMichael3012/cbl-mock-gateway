package usecase

import (
	"context"
	"fmt"
	"strconv"
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

// maxPullPayments and maxPayouts bound the payout emulation stores the same
// way (CBL-17.27).
const (
	maxPullPayments = 1000
	maxPayouts      = 1000
)

// simulateSendBudget sets the budget of simulate background deliveries: the webhook must
// be sent even when the client disconnects, but it must not hang forever.
const simulateSendBudget = 90 * time.Second

// WebhookEvent describes a mock provider webhook to be sent to Webhook Ingress.
type WebhookEvent struct {
	// EventID sets the event identifier; repeated deliveries use the same id.
	EventID string
	// Provider sets the provider profile of the event.
	Provider domain.ProfileID
	// PaymentID sets the platform payment.
	PaymentID string
	// GatewayID sets the provider invoice.
	GatewayID string
	// Status sets the canonical event status.
	Status domain.Status
	// RawStatus sets the provider raw status carried in the body.
	RawStatus string
	// Amount sets the invoice amount.
	Amount money.Money
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
	// Provider sets the provider profile of the delivery.
	Provider domain.ProfileID
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

// TargetResolver reports whether a provider profile has a configured webhook
// target; simulate answers 400 when the target of the requested provider is missing.
type TargetResolver interface {
	// HasTarget reports whether the profile can receive webhook deliveries.
	HasTarget(provider domain.ProfileID) bool
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
	mu           sync.Mutex
	sender       WebhookSender
	targets      TargetResolver
	invoices     map[string]domain.Invoice
	byKey        map[string]string
	pullPayments map[string]domain.PullPayment
	pullByName   map[string]string
	payouts      map[string]domain.Payout
	deliveries   []domain.WebhookDelivery
	scenario     domain.Scenario
	nextOrderID  int64
	clock        func() time.Time
	log          *zap.Logger
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

// WithTargets sets the webhook target resolver of the profiles.
func WithTargets(targets TargetResolver) Option {
	return func(s *Service) {
		if targets != nil {
			s.targets = targets
		}
	}
}

// NewService creates a Service on top of the webhook sender.
func NewService(sender WebhookSender, log *zap.Logger, opts ...Option) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	service := &Service{
		sender:       sender,
		invoices:     make(map[string]domain.Invoice),
		byKey:        make(map[string]string),
		pullPayments: make(map[string]domain.PullPayment),
		pullByName:   make(map[string]string),
		payouts:      make(map[string]domain.Payout),
		clock:        time.Now,
		log:          log,
	}
	for _, opt := range opts {
		opt(service)
	}
	return service
}

// CreateInvoice creates an invoice; a repeat with the same Idempotency-Key returns the original invoice.
func (s *Service) CreateInvoice(ctx context.Context, req domain.CreateInvoiceRequest) (domain.Invoice, bool, error) {
	provider := req.Provider
	if provider == "" {
		provider = domain.ProfileMock
	}
	profile, ok := domain.ProfileByID(string(provider))
	if !ok {
		return domain.Invoice{}, false, fmt.Errorf("%w: unknown provider %q", domain.ErrInvalidRequest, provider)
	}
	req.Provider = provider
	if err := req.Validate(); err != nil {
		return domain.Invoice{}, false, err
	}
	if err := s.applyScenario(ctx); err != nil {
		return domain.Invoice{}, false, err
	}
	rawStatus, err := profile.RawStatus(domain.StatusPending)
	if err != nil {
		return domain.Invoice{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	key := replayKey(provider, req.IdempotencyKey)
	if trimmed := strings.TrimSpace(req.IdempotencyKey); trimmed != "" {
		if gatewayID, ok := s.byKey[key]; ok {
			return s.invoices[gatewayID], true, nil
		}
	}
	if len(s.invoices) >= maxInvoices {
		return domain.Invoice{}, false, fmt.Errorf("%w: limit %d", domain.ErrStoreFull, maxInvoices)
	}
	gatewayID, err := s.newGatewayID(profile)
	if err != nil {
		return domain.Invoice{}, false, err
	}
	now := s.clock().UTC()
	invoice := domain.Invoice{
		GatewayID: gatewayID,
		Provider:  provider,
		PaymentID: strings.TrimSpace(req.PaymentID),
		Address:   mockAddress(gatewayID),
		Network:   strings.TrimSpace(req.Network),
		Amount:    req.Amount,
		Status:    domain.StatusPending,
		RawStatus: rawStatus,
		ReceivedAmount: money.Money{
			Asset:    req.Amount.Asset,
			Decimals: req.Amount.Decimals,
		},
		ExpiresAt: req.ExpiresAt.UTC(),
		CreatedAt: now,
	}
	s.invoices[invoice.GatewayID] = invoice
	if trimmed := strings.TrimSpace(req.IdempotencyKey); trimmed != "" {
		s.byKey[key] = invoice.GatewayID
	}
	s.log.Info("mock invoice created",
		zap.String("gateway_id", invoice.GatewayID),
		zap.String("payment_id", invoice.PaymentID),
		zap.String("provider", string(provider)),
	)
	return invoice, false, nil
}

// newGatewayID generates the profile-shaped invoice identifier: UUIDv7 for the
// mock and NOWPayments, a monotonic integer for CoinGate, 22 alphanumeric
// characters for BitPay and 32 hex characters for BTCPay.
func (s *Service) newGatewayID(profile domain.Profile) (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("usecase: generate gateway id: %w", err)
	}
	compact := strings.ReplaceAll(id.String(), "-", "")
	switch profile.ID {
	case domain.ProfileCoinGate:
		s.nextOrderID++
		return strconv.FormatInt(s.nextOrderID, 10), nil
	case domain.ProfileBitPay:
		return compact[:22], nil
	case domain.ProfileBTCPay:
		return compact, nil
	default:
		return id.String(), nil
	}
}

// replayKey scopes the idempotency map by provider so order ids cannot collide
// across profiles (spec decision T-MOCK-07).
func replayKey(provider domain.ProfileID, key string) string {
	return string(provider) + ":" + strings.TrimSpace(key)
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

// Simulate updates the invoice state and sends a webhook (or several duplicates);
// a silent simulate updates the state without any delivery (it models a lost
// webhook and injects provider-side divergence for reconciliation, T-MOCK-10).
func (s *Service) Simulate(ctx context.Context, req domain.SimulateRequest) (SimulateResult, error) {
	if err := req.Validate(); err != nil {
		return SimulateResult{}, err
	}
	status, err := domain.CanonicalStatus(req.Status)
	if err != nil {
		return SimulateResult{}, err
	}
	provider := req.Provider
	if provider == "" {
		provider = domain.ProfileMock
	}
	profile, ok := domain.ProfileByID(string(provider))
	if !ok {
		return SimulateResult{}, fmt.Errorf("%w: unknown provider %q", domain.ErrInvalidRequest, provider)
	}
	rawStatus := req.RawStatus
	if rawStatus == "" {
		rawStatus, err = profile.RawStatus(status)
		if err != nil {
			return SimulateResult{}, err
		}
	}
	if !req.Silent && s.targets != nil && !s.targets.HasTarget(provider) {
		return SimulateResult{}, fmt.Errorf("%w: missing_webhook_target for provider %q", domain.ErrInvalidRequest, provider)
	}

	s.mu.Lock()
	gatewayID := strings.TrimSpace(req.GatewayID)
	invoice, ok := s.invoices[gatewayID]
	if !ok {
		s.mu.Unlock()
		return SimulateResult{}, fmt.Errorf("%w: %s", domain.ErrInvoiceNotFound, gatewayID)
	}
	if invoice.Provider != provider {
		s.mu.Unlock()
		// The provider of an invoice is fixed at creation (spec E-11): a
		// mismatched simulate would sign a body with the wrong scheme.
		return SimulateResult{}, fmt.Errorf("%w: provider_mismatch: invoice belongs to %q", domain.ErrInvalidRequest, invoice.Provider)
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
	invoice.RawStatus = rawStatus
	invoice.ReceivedAmount = received
	invoice.Confirmations = req.Confirmations
	invoice.TXID = strings.TrimSpace(req.TXID)
	invoice.EventTS = eventTS
	s.invoices[gatewayID] = invoice
	s.mu.Unlock()

	if req.Silent {
		return SimulateResult{}, nil
	}

	eventID, err := uuid.NewV7()
	if err != nil {
		return SimulateResult{}, fmt.Errorf("usecase: generate event id: %w", err)
	}
	event := WebhookEvent{
		EventID:        eventID.String(),
		Provider:       provider,
		PaymentID:      invoice.PaymentID,
		GatewayID:      invoice.GatewayID,
		Status:         status,
		RawStatus:      rawStatus,
		Amount:         invoice.Amount,
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
		delivery.Provider = provider
		result.Deliveries = append(result.Deliveries, delivery)
		s.appendDelivery(domain.WebhookDelivery{
			EventID:    event.EventID,
			GatewayID:  event.GatewayID,
			Provider:   event.Provider,
			Status:     event.Status,
			Mode:       event.Mode,
			SentAt:     delivery.SentAt,
			HTTPStatus: delivery.HTTPStatus,
			Error:      delivery.Error,
		})
	}
	return result, nil
}

// CreatePullPayment creates a pull payment; a repeat with the same name returns
// the original one (T-MOCK-09: the connector sends the platform payout id or
// refund-<id> as the name, so a retry cannot orphan a second pull payment).
func (s *Service) CreatePullPayment(ctx context.Context, req domain.PullPaymentRequest) (domain.PullPayment, bool, error) {
	if err := req.Validate(); err != nil {
		return domain.PullPayment{}, false, err
	}
	if err := s.applyScenario(ctx); err != nil {
		return domain.PullPayment{}, false, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return domain.PullPayment{}, false, fmt.Errorf("%w: pull payment name is required", domain.ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if gatewayID, ok := s.pullByName[name]; ok {
		return s.pullPayments[gatewayID], true, nil
	}
	if len(s.pullPayments) >= maxPullPayments {
		return domain.PullPayment{}, false, fmt.Errorf("%w: limit %d", domain.ErrStoreFull, maxPullPayments)
	}
	gatewayID, err := s.newBTCPayID()
	if err != nil {
		return domain.PullPayment{}, false, err
	}
	pull := domain.PullPayment{
		ID:            gatewayID,
		Name:          name,
		Amount:        req.Amount,
		PayoutMethods: append([]string(nil), req.PayoutMethods...),
		CreatedAt:     s.clock().UTC(),
	}
	s.pullPayments[pull.ID] = pull
	s.pullByName[name] = pull.ID
	s.log.Info("mock pull payment created",
		zap.String("pull_payment_id", pull.ID),
		zap.String("name", pull.Name),
	)
	return pull, false, nil
}

// CreatePayout creates a payout against an existing pull payment; the stand
// starts it in AwaitingApproval and the operator advances it with SimulatePayout
// (T-MOCK-08: the stand never moves funds). The payout must be consistent with
// the pull payment (the offered method and the held amount).
func (s *Service) CreatePayout(ctx context.Context, pullPaymentID string, req domain.PayoutRequest) (domain.Payout, error) {
	if err := req.Validate(); err != nil {
		return domain.Payout{}, err
	}
	if err := s.applyScenario(ctx); err != nil {
		return domain.Payout{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pullPaymentID = strings.TrimSpace(pullPaymentID)
	pull, ok := s.pullPayments[pullPaymentID]
	if !ok {
		return domain.Payout{}, fmt.Errorf("%w: %s", domain.ErrPullPaymentNotFound, pullPaymentID)
	}
	if err := pull.AllowsPayout(req); err != nil {
		return domain.Payout{}, err
	}
	if len(s.payouts) >= maxPayouts {
		return domain.Payout{}, fmt.Errorf("%w: limit %d", domain.ErrStoreFull, maxPayouts)
	}
	gatewayID, err := s.newBTCPayID()
	if err != nil {
		return domain.Payout{}, err
	}
	payout := domain.Payout{
		ID:            gatewayID,
		PullPaymentID: pullPaymentID,
		Destination:   strings.TrimSpace(req.Destination),
		Amount:        req.Amount,
		MethodID:      strings.TrimSpace(req.MethodID),
		State:         domain.PayoutStateAwaitingApproval,
		CreatedAt:     s.clock().UTC(),
	}
	s.payouts[payout.ID] = payout
	s.log.Info("mock payout created",
		zap.String("payout_id", payout.ID),
		zap.String("pull_payment_id", payout.PullPaymentID),
	)
	return payout, nil
}

// GetPayout returns a payout by its provider identifier.
func (s *Service) GetPayout(ctx context.Context, payoutID string) (domain.Payout, error) {
	if err := s.applyScenario(ctx); err != nil {
		return domain.Payout{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	payout, ok := s.payouts[strings.TrimSpace(payoutID)]
	if !ok {
		return domain.Payout{}, fmt.Errorf("%w: %s", domain.ErrPayoutNotFound, payoutID)
	}
	return payout, nil
}

// SimulatePayout sets the provider state of a payout (the stand control that
// replaces the real operator approval and on-chain execution). It deliberately
// does not apply the failure scenario: it is not a provider API call, and the
// connector never invokes it (the operator does).
func (s *Service) SimulatePayout(_ context.Context, payoutID, state string) (domain.Payout, error) {
	state = strings.TrimSpace(state)
	if !domain.ValidPayoutState(state) {
		return domain.Payout{}, fmt.Errorf("%w: unknown payout state %q", domain.ErrInvalidRequest, state)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	payout, ok := s.payouts[strings.TrimSpace(payoutID)]
	if !ok {
		return domain.Payout{}, fmt.Errorf("%w: %s", domain.ErrPayoutNotFound, payoutID)
	}
	payout.State = state
	s.payouts[payout.ID] = payout
	s.log.Info("mock payout state simulated",
		zap.String("payout_id", payout.ID),
		zap.String("state", payout.State),
	)
	return payout, nil
}

// ExportInvoices returns the store invoices of one provider profile inside the
// inclusive window, sorted and paged for the connector export walk (T-MOCK-12:
// the stand store belongs to the BTCPay profile, the caller passes it so a
// future profile cannot silently reuse the BTCPay wire).
func (s *Service) ExportInvoices(ctx context.Context, provider domain.ProfileID, from, to time.Time, skip, take int) ([]domain.Invoice, error) {
	if err := s.applyScenario(ctx); err != nil {
		return nil, err
	}
	return domain.FilterInvoices(s.providerInvoices(provider), from, to, skip, take), nil
}

// LookupInvoicesByOrder returns the store invoices of one provider profile with
// the given order reference: the real API supports the orderId filter on the
// store list, and the connector lookup recovery (LookupByOrderID) relies on it;
// an empty answer keeps the connector not-found classification.
func (s *Service) LookupInvoicesByOrder(ctx context.Context, provider domain.ProfileID, orderID string) ([]domain.Invoice, error) {
	if err := s.applyScenario(ctx); err != nil {
		return nil, err
	}
	needle := strings.TrimSpace(orderID)
	matches := make([]domain.Invoice, 0)
	for _, invoice := range s.providerInvoices(provider) {
		if invoice.PaymentID == needle {
			matches = append(matches, invoice)
		}
	}
	domain.SortInvoicesByCreated(matches)
	return matches, nil
}

// providerInvoices returns a copy of the invoices of one provider profile.
func (s *Service) providerInvoices(provider domain.ProfileID) []domain.Invoice {
	s.mu.Lock()
	defer s.mu.Unlock()
	invoices := make([]domain.Invoice, 0, len(s.invoices))
	for _, invoice := range s.invoices {
		if invoice.Provider != provider {
			continue
		}
		invoices = append(invoices, invoice)
	}
	return invoices
}

// newBTCPayID generates a provider-shaped identifier of the BTCPay profile
// (32 hex characters), shared by pull payments and payouts.
func (s *Service) newBTCPayID() (string, error) {
	profile, ok := domain.ProfileByID(string(domain.ProfileBTCPay))
	if !ok {
		return "", fmt.Errorf("usecase: btcpay profile is not registered")
	}
	return s.newGatewayID(profile)
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
		status := scenario.FailStatus
		if status == 0 {
			status = 503
		}
		return domain.InjectedFailureError{Status: status, RetryAfterS: scenario.RetryAfterS}
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
