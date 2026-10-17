package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cbl/cbl-kit/webhooksig"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
)

// ProfileWebhook builds the provider-shaped webhook body and the signature
// header of a profile; it fails closed: a signing problem returns an error and
// never an unsigned body.
func ProfileWebhook(profile domain.Profile, secret []byte, event usecase.WebhookEvent) ([]byte, string, string, error) {
	if len(secret) == 0 {
		return nil, "", "", fmt.Errorf("webhook: empty profile secret")
	}
	body, err := profileBody(profile, secret, event)
	if err != nil {
		return nil, "", "", err
	}
	header, value, err := profileSignature(profile, secret, body, event.Mode)
	if err != nil {
		return nil, "", "", err
	}
	return body, header, value, nil
}

// profileBody renders the provider-shaped webhook body of a profile.
func profileBody(profile domain.Profile, secret []byte, event usecase.WebhookEvent) ([]byte, error) {
	var payload any
	switch profile.ID {
	case domain.ProfileNOWPayments:
		payload = map[string]any{
			"payment_id":     event.GatewayID,
			"order_id":       event.PaymentID,
			"payment_status": event.RawStatus,
			"price_amount":   json.RawMessage(event.Amount.String()),
			"actually_paid":  json.RawMessage(event.ReceivedAmount.String()),
			"pay_currency":   event.Network,
			"updated_at":     event.EventTS.UTC().Format(time.RFC3339),
		}
	case domain.ProfileCoinGate:
		body := map[string]any{
			"id":             numericOrString(event.GatewayID),
			"order_id":       event.PaymentID,
			"status":         event.RawStatus,
			"price_amount":   event.Amount.String(),
			"price_currency": event.Amount.Asset,
			"pay_amount":     event.ReceivedAmount.String(),
			"pay_currency":   event.Network,
			"created_at":     event.EventTS.UTC().Format(time.RFC3339),
		}
		if event.Mode != domain.SignatureMissing {
			token := string(secret)
			if event.Mode == domain.SignatureInvalid {
				token = invalidToken(secret)
			}
			body["token"] = token
		}
		payload = body
	case domain.ProfileBitPay:
		payload = map[string]any{
			"event": map[string]any{
				"code": bitPayEventCode(event.RawStatus),
				"name": bitPayEventName(event.RawStatus),
			},
			"data": map[string]any{
				"id":            event.GatewayID,
				"orderId":       event.PaymentID,
				"status":        event.RawStatus,
				"price":         event.Amount.MinorUnits,
				"currency":      event.Amount.Asset,
				"amountPaid":    event.ReceivedAmount.MinorUnits,
				"confirmations": event.Confirmations,
				"invoiceTime":   event.EventTS.UnixMilli(),
			},
		}
	case domain.ProfileBTCPay:
		payload = map[string]any{
			"deliveryId": event.EventID,
			"webhookId":  "00000000-0000-7000-8000-0000000000b7",
			"type":       btcPayEventType(event.RawStatus),
			"invoiceId":  event.GatewayID,
			"storeId":    "mock-store",
			"metadata": map[string]string{
				"orderId": event.PaymentID,
			},
		}
	default:
		return nil, fmt.Errorf("webhook: provider %q has no profile signing", profile.ID)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("webhook: marshal profile body: %w", err)
	}
	return body, nil
}

// profileSignature computes the signature header of a profile body; an unknown
// scheme fails closed so a misconfigured profile never sends unsigned webhooks.
func profileSignature(profile domain.Profile, secret, body []byte, mode domain.SignatureMode) (string, string, error) {
	if profile.Signature.Kind == domain.SignatureTokenEcho {
		return "", "", nil
	}
	if mode == domain.SignatureMissing {
		return "", "", nil
	}
	header := profile.Signature.Header
	switch profile.Signature.Kind {
	case domain.SignatureCanonicalJSONHMAC:
		canonical, err := webhooksig.CanonicalJSON(body)
		if err != nil {
			return "", "", fmt.Errorf("webhook: canonicalize body: %w", err)
		}
		mac := hmac.New(sha512.New, secret)
		mac.Write(canonical)
		digest := hex.EncodeToString(mac.Sum(nil))
		if mode == domain.SignatureInvalid {
			digest = corruptDigest(digest)
		}
		return header, digest, nil
	case domain.SignatureRawHMAC:
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		sum := mac.Sum(nil)
		var digest string
		if profile.Signature.Encoding == "base64" {
			digest = base64.StdEncoding.EncodeToString(sum)
		} else {
			digest = hex.EncodeToString(sum)
		}
		if mode == domain.SignatureInvalid {
			digest = corruptDigest(digest)
		}
		return header, profile.Signature.Prefix + digest, nil
	default:
		return "", "", fmt.Errorf("webhook: provider %q has unknown signature kind %q", profile.ID, profile.Signature.Kind)
	}
}

// invalidToken derives the invalid token echo value from the secret by flipping
// the first byte: it can never equal the secret. Token echo embeds the secret
// in valid bodies by scheme design, so the invalid variant adds no exposure.
func invalidToken(secret []byte) string {
	if len(secret) == 0 {
		return "invalid"
	}
	corrupted := append([]byte(nil), secret...)
	if corrupted[0] == 'a' {
		corrupted[0] = 'b'
	} else {
		corrupted[0] = 'a'
	}
	return string(corrupted)
}

// numericOrString renders a decimal identifier as a JSON number when it is a
// valid number literal and as a string otherwise (CoinGate order ids).
func numericOrString(value string) any {
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return json.RawMessage(value)
	}
	return value
}

// corruptDigest breaks a hex or base64 digest deterministically for the
// invalid signature scenario while keeping the wire format valid.
func corruptDigest(digest string) string {
	if digest == "" {
		return digest
	}
	replacement := byte('0')
	if digest[0] == '0' {
		replacement = '1'
	}
	return string(replacement) + digest[1:]
}

// bitPayEventName maps a BitPay raw status to the IPN event name.
func bitPayEventName(raw string) string {
	switch raw {
	case "new":
		return "invoice_created"
	case "paid":
		return "invoice_paidInFull"
	case "confirmed":
		return "invoice_confirmed"
	case "complete":
		return "invoice_completed"
	case "expired":
		return "invoice_expired"
	case "invalid":
		return "invoice_failedToConfirm"
	default:
		return "invoice_created"
	}
}

// bitPayEventCode maps a BitPay raw status to the IPN event code.
func bitPayEventCode(raw string) int {
	switch raw {
	case "paid":
		return 1001
	case "confirmed":
		return 1002
	case "complete":
		return 1003
	case "expired":
		return 1004
	case "invalid":
		return 1005
	default:
		return 1000
	}
}

// btcPayEventType maps a BTCPay raw status to the webhook type.
func btcPayEventType(raw string) string {
	switch raw {
	case "Processing":
		return "InvoiceProcessing"
	case "Settled":
		return "InvoiceSettled"
	case "Expired":
		return "InvoiceExpired"
	case "Invalid":
		return "InvoiceInvalid"
	default:
		return "InvoiceCreated"
	}
}

// DeriveTarget derives the webhook target of a profile from the base target by
// replacing the /hooks/mock/ path segment; the mock profile keeps the base.
func DeriveTarget(base string, profile domain.ProfileID) (string, bool) {
	if strings.TrimSpace(base) == "" {
		return "", false
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	marker := "/hooks/" + string(domain.ProfileMock) + "/"
	if !strings.Contains(parsed.Path, marker) {
		return "", false
	}
	if profile == domain.ProfileMock {
		return parsed.String(), true
	}
	parsed.Path = strings.Replace(parsed.Path, marker, "/hooks/"+string(profile)+"/", 1)
	return parsed.String(), true
}

// ProfileSender sends provider-profile webhooks: the body and the signature
// follow the profile wire contract of the CBL-17.5 spec.
type ProfileSender struct {
	profile       domain.Profile
	client        *http.Client
	targetURL     string
	secret        []byte
	eventIDHeader string
}

// NewProfileSender creates a sender; the default timeout is 5 seconds.
func NewProfileSender(profile domain.Profile, targetURL string, secret []byte, eventIDHeader string, timeout time.Duration) (*ProfileSender, error) {
	if profile.Signature.Kind == domain.SignatureLegacyHMAC {
		return nil, fmt.Errorf("%w: profile %q uses the legacy sender", domain.ErrInvalidRequest, profile.ID)
	}
	switch profile.Signature.Kind {
	case domain.SignatureRawHMAC, domain.SignatureCanonicalJSONHMAC:
		if strings.TrimSpace(profile.Signature.Header) == "" {
			return nil, fmt.Errorf("%w: profile %q misses the signature header", domain.ErrInvalidRequest, profile.ID)
		}
	case domain.SignatureTokenEcho:
		if strings.TrimSpace(profile.Signature.Field) == "" {
			return nil, fmt.Errorf("%w: profile %q misses the token echo field", domain.ErrInvalidRequest, profile.ID)
		}
	default:
		return nil, fmt.Errorf("%w: profile %q has unknown signature kind %q", domain.ErrInvalidRequest, profile.ID, profile.Signature.Kind)
	}
	parsed, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("%w: invalid profile target URL", domain.ErrInvalidRequest)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: unsupported scheme %q", domain.ErrInvalidRequest, parsed.Scheme)
	}
	if len(secret) == 0 {
		return nil, fmt.Errorf("%w: empty profile secret", domain.ErrInvalidRequest)
	}
	if strings.TrimSpace(eventIDHeader) == "" {
		return nil, fmt.Errorf("%w: empty event id header", domain.ErrInvalidRequest)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &ProfileSender{
		profile: profile,
		client: &http.Client{
			Timeout: timeout,
			// Redirects are not followed: the target must not forward the
			// body and the signature to another host (mirror of HTTPSender).
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		targetURL:     parsed.String(),
		secret:        secret,
		eventIDHeader: strings.TrimSpace(eventIDHeader),
	}, nil
}

// Send signs and sends the profile event; network errors and 4xx/5xx go into
// the delivery result, mirroring the legacy sender.
func (s *ProfileSender) Send(ctx context.Context, event usecase.WebhookEvent) usecase.Delivery {
	started := time.Now().UTC()
	body, headerName, headerValue, err := ProfileWebhook(s.profile, s.secret, event)
	if err != nil {
		return usecase.Delivery{SentAt: started, Provider: event.Provider, Error: "build provider webhook failed"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.targetURL, bytes.NewReader(body))
	if err != nil {
		return usecase.Delivery{SentAt: started, Provider: event.Provider, Error: "build webhook request failed"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(s.eventIDHeader, event.EventID)
	if headerName != "" && headerValue != "" {
		request.Header.Set(headerName, headerValue)
	}

	response, err := s.client.Do(request)
	if err != nil {
		return usecase.Delivery{SentAt: time.Now().UTC(), Provider: event.Provider, Error: "webhook request failed"}
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
	delivery := usecase.Delivery{SentAt: time.Now().UTC(), Provider: event.Provider, HTTPStatus: response.StatusCode}
	if response.StatusCode >= http.StatusMultipleChoices {
		delivery.Error = fmt.Sprintf("webhook rejected with status %d", response.StatusCode)
	}
	return delivery
}

// Switch routes webhook events to the sender of the provider profile and
// answers target presence queries of the service.
type Switch struct {
	senders map[domain.ProfileID]usecase.WebhookSender
}

// NewSwitch creates a Switch over the per-profile senders.
func NewSwitch(senders map[domain.ProfileID]usecase.WebhookSender) (*Switch, error) {
	if len(senders) == 0 {
		return nil, fmt.Errorf("%w: no profile senders", domain.ErrInvalidRequest)
	}
	copied := make(map[domain.ProfileID]usecase.WebhookSender, len(senders))
	for id, sender := range senders {
		if _, ok := domain.ProfileByID(string(id)); !ok {
			return nil, fmt.Errorf("%w: unknown provider %q", domain.ErrInvalidRequest, id)
		}
		if sender == nil {
			return nil, fmt.Errorf("%w: nil sender for provider %q", domain.ErrInvalidRequest, id)
		}
		copied[id] = sender
	}
	return &Switch{senders: copied}, nil
}

// Send routes the event to the profile sender.
func (s *Switch) Send(ctx context.Context, event usecase.WebhookEvent) usecase.Delivery {
	sender, ok := s.senders[event.Provider]
	if !ok {
		return usecase.Delivery{SentAt: time.Now().UTC(), Provider: event.Provider, Error: "no sender for provider"}
	}
	return sender.Send(ctx, event)
}

// HasTarget reports whether the profile has a configured sender and target.
func (s *Switch) HasTarget(provider domain.ProfileID) bool {
	_, ok := s.senders[provider]
	return ok
}
