package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cbl/cbl-kit/hmac"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
)

// maxResponseBytes задает предельный размер читаемого ответа получателя.
const maxResponseBytes = 1 << 10

// HTTPSender отправляет вебхуки HTTP POST с HMAC-подписью kit.
type HTTPSender struct {
	client          *http.Client
	targetURL       string
	secret          []byte
	signatureHeader string
	eventIDHeader   string
	clock           func() time.Time
}

// Option настраивает HTTPSender.
type Option func(*HTTPSender)

// WithClock задает источник времени для подписи; используется тестами.
func WithClock(clock func() time.Time) Option {
	return func(s *HTTPSender) {
		if clock != nil {
			s.clock = clock
		}
	}
}

// NewHTTPSender создает отправитель; timeout по умолчанию 5 секунд.
func NewHTTPSender(targetURL, secret, signatureHeader, eventIDHeader string, timeout time.Duration, opts ...Option) (*HTTPSender, error) {
	parsed, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("%w: invalid WEBHOOK_TARGET_URL", domain.ErrInvalidRequest)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: unsupported scheme %q", domain.ErrInvalidRequest, parsed.Scheme)
	}
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("%w: empty WEBHOOK_SECRET", domain.ErrInvalidRequest)
	}
	if strings.TrimSpace(signatureHeader) == "" || strings.TrimSpace(eventIDHeader) == "" {
		return nil, fmt.Errorf("%w: empty webhook header name", domain.ErrInvalidRequest)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	sender := &HTTPSender{
		client: &http.Client{
			Timeout: timeout,
			// Редиректы не выполняются: цель из env не должна переадресовывать
			// тело события и подпись на другой хост (review 3.6).
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		targetURL:       parsed.String(),
		secret:          []byte(secret),
		signatureHeader: strings.TrimSpace(signatureHeader),
		eventIDHeader:   strings.TrimSpace(eventIDHeader),
		clock:           time.Now,
	}
	for _, opt := range opts {
		opt(sender)
	}
	return sender, nil
}

// webhookBody описывает JSON-тело вебхука по контракту sandbox-провайдера.
type webhookBody struct {
	PaymentID      string     `json:"payment_id"`
	GatewayID      string     `json:"gateway_id"`
	Status         string     `json:"status"`
	ReceivedAmount amountBody `json:"received_amount"`
	Confirmations  uint32     `json:"confirmations"`
	TXID           string     `json:"txid"`
	Network        string     `json:"network"`
	EventTS        string     `json:"event_ts"`
}

// amountBody описывает сумму в минимальных единицах.
type amountBody struct {
	MinorUnits int64  `json:"minor_units"`
	Asset      string `json:"asset"`
	Decimals   uint32 `json:"decimals"`
}

// Send подписывает и отправляет событие; сетевые ошибки и 4xx/5xx попадают
// в результат доставки, а не возвращаются ошибкой: simulate всегда отвечает 200.
func (s *HTTPSender) Send(ctx context.Context, event usecase.WebhookEvent) usecase.Delivery {
	started := s.clock().UTC()
	body, err := marshalBody(event)
	if err != nil {
		return usecase.Delivery{SentAt: started, Error: "marshal webhook body failed"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.targetURL, bytes.NewReader(body))
	if err != nil {
		return usecase.Delivery{SentAt: started, Error: "build webhook request failed"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(s.eventIDHeader, event.EventID)
	if signature := s.buildSignature(event.Mode, body); signature != "" {
		request.Header.Set(s.signatureHeader, signature)
	}

	response, err := s.client.Do(request)
	if err != nil {
		return usecase.Delivery{SentAt: s.clock().UTC(), Error: "webhook request failed"}
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
	delivery := usecase.Delivery{SentAt: s.clock().UTC(), HTTPStatus: response.StatusCode}
	// 3xx считается недоставкой: редиректы не выполняются.
	if response.StatusCode >= http.StatusMultipleChoices {
		delivery.Error = fmt.Sprintf("webhook rejected with status %d", response.StatusCode)
	}
	return delivery
}

// buildSignature собирает заголовок подписи по режиму сценария.
func (s *HTTPSender) buildSignature(mode domain.SignatureMode, body []byte) string {
	if mode == domain.SignatureMissing {
		return ""
	}
	timestamp := s.clock().UTC()
	signature := hmac.Sign(s.secret, timestamp, body)
	if mode == domain.SignatureInvalid {
		signature = corruptHex(signature)
	}
	return hmac.BuildHeader(hmac.Signature{Timestamp: timestamp, Hex: signature})
}

// marshalBody сериализует событие в JSON-контракт sandbox-провайдера.
func marshalBody(event usecase.WebhookEvent) ([]byte, error) {
	body := webhookBody{
		PaymentID: event.PaymentID,
		GatewayID: event.GatewayID,
		Status:    string(event.Status),
		ReceivedAmount: amountBody{
			MinorUnits: event.ReceivedAmount.MinorUnits,
			Asset:      event.ReceivedAmount.Asset,
			Decimals:   event.ReceivedAmount.Decimals,
		},
		Confirmations: event.Confirmations,
		TXID:          event.TXID,
		Network:       event.Network,
		EventTS:       event.EventTS.UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("webhook: marshal body: %w", err)
	}
	return payload, nil
}

// corruptHex портит hex-подпись, сохраняя формат заголовка.
func corruptHex(signature string) string {
	if signature == "" {
		return signature
	}
	replacement := byte('f')
	if signature[0] == 'f' {
		replacement = 'e'
	}
	return string(replacement) + signature[1:]
}
