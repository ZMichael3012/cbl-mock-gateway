package transport

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/cbl/cbl-kit/ginmw"
	"github.com/cbl/cbl-kit/money"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
)

// maxBodyBytes sets the request body size limit (CBL-3.6 NFR).
const maxBodyBytes int64 = 64 << 10

// Server serves the mock provider API.
type Server struct {
	svc *usecase.Service
	log *zap.Logger
}

// NewServer creates a Server.
func NewServer(svc *usecase.Service, log *zap.Logger) *Server {
	if log == nil {
		log = zap.NewNop()
	}
	return &Server{svc: svc, log: log}
}

// Register registers the API routes with the body size limit.
func (s *Server) Register(router gin.IRouter) {
	router.Use(limitBody())
	router.POST("/api/v1/invoices", s.createInvoice)
	router.GET("/api/v1/invoices/:gateway_id", s.getInvoice)
	router.POST("/api/v1/simulate", s.simulate)
	router.GET("/api/v1/webhooks", s.listWebhooks)
	router.GET("/api/v1/scenarios", s.getScenario)
	router.POST("/api/v1/scenarios", s.setScenario)
	router.POST("/api/v1/scenarios/reset", s.resetScenario)
}

// limitBody limits request body reads; an overflow yields 413 on read.
func limitBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		}
		c.Next()
	}
}

// bindJSON strictly parses the body: unknown fields and trailing data are rejected.
func bindJSON(c *gin.Context, dst any) error {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("transport: unexpected trailing json")
	}
	return nil
}

// moneyDTO describes an amount in JSON.
type moneyDTO struct {
	MinorUnits int64  `json:"minor_units"`
	Asset      string `json:"asset"`
	Decimals   uint32 `json:"decimals"`
}

// createInvoiceRequest describes the invoice creation request body.
type createInvoiceRequest struct {
	PaymentID string   `json:"payment_id"`
	Amount    moneyDTO `json:"amount"`
	Network   string   `json:"network"`
	ExpiresAt string   `json:"expires_at"`
}

// invoiceResponse describes the invoice response.
type invoiceResponse struct {
	GatewayID      string   `json:"gateway_id"`
	PaymentID      string   `json:"payment_id"`
	Address        string   `json:"address"`
	Network        string   `json:"network"`
	Amount         moneyDTO `json:"amount"`
	Status         string   `json:"status"`
	ReceivedAmount moneyDTO `json:"received_amount"`
	Confirmations  uint32   `json:"confirmations"`
	TXID           string   `json:"txid"`
	EventTS        string   `json:"event_ts,omitempty"`
	ExpiresAt      string   `json:"expires_at"`
	CreatedAt      string   `json:"created_at"`
	Replayed       bool     `json:"replayed"`
}

// simulateRequest describes the webhook generation request body.
type simulateRequest struct {
	GatewayID      string    `json:"gateway_id"`
	Status         string    `json:"status"`
	ReceivedAmount *moneyDTO `json:"received_amount"`
	Confirmations  uint32    `json:"confirmations"`
	TXID           string    `json:"txid"`
	EventTS        string    `json:"event_ts"`
	DelayMS        int       `json:"delay_ms"`
	Duplicate      int       `json:"duplicate"`
	Signature      string    `json:"signature"`
}

// deliveryDTO describes the result of a single delivery.
type deliveryDTO struct {
	SentAt     string `json:"sent_at"`
	HTTPStatus int    `json:"http_status"`
	Mode       string `json:"mode"`
	Error      string `json:"error,omitempty"`
}

// simulateResponse describes the simulate result.
type simulateResponse struct {
	EventID    string        `json:"event_id"`
	Deliveries []deliveryDTO `json:"deliveries"`
}

// deliveryLogDTO describes a delivery log entry.
type deliveryLogDTO struct {
	EventID    string `json:"event_id"`
	GatewayID  string `json:"gateway_id"`
	Status     string `json:"status"`
	Mode       string `json:"mode"`
	SentAt     string `json:"sent_at"`
	HTTPStatus int    `json:"http_status"`
	Error      string `json:"error,omitempty"`
}

// scenarioRequest describes the failure scenario request body.
type scenarioRequest struct {
	FailNext  int `json:"fail_next"`
	LatencyMS int `json:"latency_ms"`
}

// scenarioResponse describes the current scenario.
type scenarioResponse struct {
	FailNext  int `json:"fail_next"`
	LatencyMS int `json:"latency_ms"`
}

// createInvoice handles provider invoice creation.
func (s *Server) createInvoice(c *gin.Context) {
	var request createInvoiceRequest
	if err := bindJSON(c, &request); err != nil {
		s.abortBind(c, err)
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, request.ExpiresAt)
	if err != nil {
		s.abort(c, http.StatusBadRequest, "invalid_expires_at", "Bad Request", "expires_at в формате RFC 3339")
		return
	}
	invoice, replayed, err := s.svc.CreateInvoice(c.Request.Context(), domain.CreateInvoiceRequest{
		IdempotencyKey: c.GetHeader("Idempotency-Key"),
		PaymentID:      request.PaymentID,
		Amount: money.Money{
			MinorUnits: request.Amount.MinorUnits,
			Asset:      request.Amount.Asset,
			Decimals:   request.Amount.Decimals,
		},
		Network:   request.Network,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		s.mapError(c, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	c.JSON(status, toInvoiceResponse(invoice, replayed))
}

// getInvoice returns an invoice by the provider identifier.
func (s *Server) getInvoice(c *gin.Context) {
	invoice, err := s.svc.GetInvoice(c.Request.Context(), c.Param("gateway_id"))
	if err != nil {
		s.mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, toInvoiceResponse(invoice, false))
}

// simulate updates the invoice and sends webhooks.
func (s *Server) simulate(c *gin.Context) {
	var request simulateRequest
	if err := bindJSON(c, &request); err != nil {
		s.abortBind(c, err)
		return
	}
	signature, err := domain.NormalizeSignature(request.Signature)
	if err != nil {
		s.mapError(c, err)
		return
	}
	var eventTS *time.Time
	if request.EventTS != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, request.EventTS)
		if parseErr != nil {
			s.abort(c, http.StatusBadRequest, "invalid_event_ts", "Bad Request", "event_ts в формате RFC 3339")
			return
		}
		eventTS = &parsed
	}
	var received *money.Money
	if request.ReceivedAmount != nil {
		amount := money.Money{
			MinorUnits: request.ReceivedAmount.MinorUnits,
			Asset:      request.ReceivedAmount.Asset,
			Decimals:   request.ReceivedAmount.Decimals,
		}
		received = &amount
	}
	result, err := s.svc.Simulate(c.Request.Context(), domain.SimulateRequest{
		GatewayID:      request.GatewayID,
		Status:         request.Status,
		ReceivedAmount: received,
		Confirmations:  request.Confirmations,
		TXID:           request.TXID,
		EventTS:        eventTS,
		DelayMS:        request.DelayMS,
		Duplicate:      request.Duplicate,
		Signature:      signature,
	})
	if err != nil {
		s.mapError(c, err)
		return
	}
	deliveries := make([]deliveryDTO, 0, len(result.Deliveries))
	for _, delivery := range result.Deliveries {
		deliveries = append(deliveries, deliveryDTO{
			SentAt:     delivery.SentAt.UTC().Format(time.RFC3339Nano),
			HTTPStatus: delivery.HTTPStatus,
			Mode:       string(signature),
			Error:      delivery.Error,
		})
	}
	c.JSON(http.StatusOK, simulateResponse{EventID: result.EventID, Deliveries: deliveries})
}

// listWebhooks returns the delivery log.
func (s *Server) listWebhooks(c *gin.Context) {
	deliveries := s.svc.ListDeliveries()
	response := make([]deliveryLogDTO, 0, len(deliveries))
	for _, delivery := range deliveries {
		response = append(response, deliveryLogDTO{
			EventID:    delivery.EventID,
			GatewayID:  delivery.GatewayID,
			Status:     string(delivery.Status),
			Mode:       string(delivery.Mode),
			SentAt:     delivery.SentAt.UTC().Format(time.RFC3339Nano),
			HTTPStatus: delivery.HTTPStatus,
			Error:      delivery.Error,
		})
	}
	c.JSON(http.StatusOK, gin.H{"webhooks": response})
}

// getScenario returns the current failure scenario.
func (s *Server) getScenario(c *gin.Context) {
	scenario := s.svc.GetScenario()
	c.JSON(http.StatusOK, scenarioResponse{FailNext: scenario.FailNext, LatencyMS: scenario.LatencyMS})
}

// setScenario sets the provider API failure scenario.
func (s *Server) setScenario(c *gin.Context) {
	var request scenarioRequest
	if err := bindJSON(c, &request); err != nil {
		s.abortBind(c, err)
		return
	}
	scenario := domain.Scenario{FailNext: request.FailNext, LatencyMS: request.LatencyMS}
	if err := s.svc.SetScenario(scenario); err != nil {
		s.mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, scenarioResponse{FailNext: scenario.FailNext, LatencyMS: scenario.LatencyMS})
}

// resetScenario resets the failure scenario.
func (s *Server) resetScenario(c *gin.Context) {
	s.svc.ResetScenario()
	c.JSON(http.StatusOK, scenarioResponse{})
}

// mapError converts domain errors into problem+json.
func (s *Server) mapError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidRequest):
		s.abort(c, http.StatusBadRequest, "invalid_request", "Bad Request", err.Error())
	case errors.Is(err, domain.ErrInvoiceNotFound):
		s.abort(c, http.StatusNotFound, "invoice_not_found", "Not Found", "счет не найден")
	case errors.Is(err, domain.ErrInjectedFailure):
		s.abort(c, http.StatusServiceUnavailable, "provider_unavailable", "Service Unavailable", "сценарий сбоя провайдера")
	case errors.Is(err, domain.ErrStoreFull):
		s.abort(c, http.StatusServiceUnavailable, "store_full", "Service Unavailable", "хранилище счетов заполнено")
	default:
		s.log.Error("mock-gateway request failed", zap.Error(err))
		s.abort(c, http.StatusInternalServerError, "internal_error", "Internal Server Error", "")
	}
}

// abortBind answers a body parse error: 413 when the limit is exceeded,
// otherwise 400 with problem+json.
func (s *Server) abortBind(c *gin.Context, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		s.abort(c, http.StatusRequestEntityTooLarge, "body_too_large", "Payload Too Large", "")
		return
	}
	s.abort(c, http.StatusBadRequest, "invalid_json", "Bad Request", "тело запроса не разобрано")
}

// abort answers with problem+json using the given status.
func (s *Server) abort(c *gin.Context, status int, code, title, detail string) {
	ginmw.AbortWithProblem(c, status, code, title, detail)
}

// toInvoiceResponse converts a domain invoice into the API response.
func toInvoiceResponse(invoice domain.Invoice, replayed bool) invoiceResponse {
	response := invoiceResponse{
		GatewayID: invoice.GatewayID,
		PaymentID: invoice.PaymentID,
		Address:   invoice.Address,
		Network:   invoice.Network,
		Amount: moneyDTO{
			MinorUnits: invoice.Amount.MinorUnits,
			Asset:      invoice.Amount.Asset,
			Decimals:   invoice.Amount.Decimals,
		},
		Status: string(invoice.Status),
		ReceivedAmount: moneyDTO{
			MinorUnits: invoice.ReceivedAmount.MinorUnits,
			Asset:      invoice.ReceivedAmount.Asset,
			Decimals:   invoice.ReceivedAmount.Decimals,
		},
		Confirmations: invoice.Confirmations,
		TXID:          invoice.TXID,
		ExpiresAt:     invoice.ExpiresAt.UTC().Format(time.RFC3339),
		CreatedAt:     invoice.CreatedAt.UTC().Format(time.RFC3339Nano),
		Replayed:      replayed,
	}
	if !invoice.EventTS.IsZero() {
		response.EventTS = invoice.EventTS.UTC().Format(time.RFC3339Nano)
	}
	return response
}
