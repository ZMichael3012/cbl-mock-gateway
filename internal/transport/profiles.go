package transport

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
)

// profileRoutes describes the provider API routes of a profile.
type profileRoutes struct {
	createPath string
	getPath    string
	// payout carries the optional payout, refund and export routes of a
	// profile (CBL-17.27); an empty pullPaymentPath skips them.
	payout payoutRoutes
}

// payoutRoutes describes the payout, refund and export routes of a profile.
type payoutRoutes struct {
	pullPaymentPath    string
	payoutCreatePath   string
	payoutReadPath     string
	payoutSimulatePath string
	invoiceListPath    string
}

// routesOf returns the provider routes of a profile.
func routesOf(profile domain.Profile) (profileRoutes, bool) {
	switch profile.ID {
	case domain.ProfileNOWPayments:
		return profileRoutes{createPath: "/v1/invoice", getPath: "/v1/payment/:gateway_id"}, true
	case domain.ProfileCoinGate:
		return profileRoutes{createPath: "/v2/orders", getPath: "/v2/orders/:gateway_id"}, true
	case domain.ProfileBitPay:
		return profileRoutes{createPath: "/invoices", getPath: "/invoices/:gateway_id"}, true
	case domain.ProfileBTCPay:
		return profileRoutes{
			createPath: "/api/v1/stores/:store_id/invoices",
			getPath:    "/api/v1/stores/:store_id/invoices/:gateway_id",
			payout: payoutRoutes{
				pullPaymentPath:    "/api/v1/stores/:store_id/pull-payments",
				payoutCreatePath:   "/api/v1/pull-payments/:pull_payment_id/payouts",
				payoutReadPath:     "/api/v1/stores/:store_id/payouts/:payout_id",
				payoutSimulatePath: "/api/v1/payouts/:payout_id/simulate",
				invoiceListPath:    "/api/v1/stores/:store_id/invoices",
			},
		}, true
	default:
		return profileRoutes{}, false
	}
}

// registerProfileRoutes mounts the provider API routes of all real profiles;
// the mock profile keeps its legacy sandbox routes.
func (s *Server) registerProfileRoutes(router gin.IRouter) {
	for _, profile := range domain.AllProfiles() {
		routes, ok := routesOf(profile)
		if !ok {
			continue
		}
		router.POST(routes.createPath, requireProfileAuth(profile), s.createProfileInvoice(profile))
		router.GET(routes.getPath, requireProfileAuth(profile), s.getProfileInvoice(profile))
		if routes.payout.pullPaymentPath == "" {
			continue
		}
		router.GET(routes.payout.invoiceListPath, requireProfileAuth(profile), s.listProfileInvoices(profile))
		router.POST(routes.payout.pullPaymentPath, requireProfileAuth(profile), s.createProfilePullPayment(profile))
		router.POST(routes.payout.payoutCreatePath, requireProfileAuth(profile), s.createProfilePayout(profile))
		router.GET(routes.payout.payoutReadPath, requireProfileAuth(profile), s.getProfilePayout(profile))
		router.POST(routes.payout.payoutSimulatePath, requireProfileAuth(profile), s.simulateProfilePayout(profile))
	}
}

// requireProfileAuth rejects requests failing the profile auth requirements
// with 401 problem+json before any state change.
func requireProfileAuth(profile domain.Profile) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := profile.ValidateAuth(c.Request.Header); err != nil {
			ginmw.AbortWithProblem(c, http.StatusUnauthorized, "unauthorized", "Unauthorized", "")
			c.Abort()
			return
		}
		c.Next()
	}
}

// createProfileInvoice handles provider-shaped invoice creation of a profile.
func (s *Server) createProfileInvoice(profile domain.Profile) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			s.abortBind(c, err)
			return
		}
		request, err := profile.ParseCreateInvoice(body, time.Now().UTC())
		if err != nil {
			s.mapError(c, err)
			return
		}
		request.Provider = profile.ID
		invoice, replayed, err := s.svc.CreateInvoice(c.Request.Context(), request)
		if err != nil {
			s.mapError(c, err)
			return
		}
		payload, err := profile.CreateInvoiceResponse(invoice)
		if err != nil {
			s.mapError(c, err)
			return
		}
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		}
		c.JSON(status, payload)
	}
}

// getProfileInvoice returns the provider-shaped payment state of a profile.
func (s *Server) getProfileInvoice(profile domain.Profile) gin.HandlerFunc {
	return func(c *gin.Context) {
		invoice, err := s.svc.GetInvoice(c.Request.Context(), c.Param("gateway_id"))
		if err != nil {
			s.mapError(c, err)
			return
		}
		if invoice.Provider != profile.ID {
			// A foreign invoice id must not render a provider-shaped body
			// (spec E-11): report it as missing for this profile.
			s.mapError(c, fmt.Errorf("%w: %s", domain.ErrInvoiceNotFound, invoice.GatewayID))
			return
		}
		payload, err := profile.PaymentResponse(invoice)
		if err != nil {
			s.mapError(c, err)
			return
		}
		c.JSON(http.StatusOK, payload)
	}
}

// listProfileInvoices returns the provider store invoice list used by the
// connector export walk (CBL-17.27): the window is inclusive at wire
// granularity and the page is paged by skip/take with the stand clamp. An
// orderId query filters by the order reference without a window (the real API
// supports it and the connector lookup recovery relies on it).
func (s *Server) listProfileInvoices(profile domain.Profile) gin.HandlerFunc {
	return func(c *gin.Context) {
		var invoices []domain.Invoice
		var err error
		if orderID := strings.TrimSpace(c.Query("orderId")); orderID != "" {
			invoices, err = s.svc.LookupInvoicesByOrder(c.Request.Context(), profile.ID, orderID)
		} else {
			var from, to time.Time
			var skip, take int
			from, to, skip, take, err = exportWindow(c)
			if err == nil {
				invoices, err = s.svc.ExportInvoices(c.Request.Context(), profile.ID, from, to, skip, take)
			}
		}
		if err != nil {
			s.mapError(c, err)
			return
		}
		rows := make([]any, 0, len(invoices))
		for _, invoice := range invoices {
			payload, err := profile.CreateInvoiceResponse(invoice)
			if err != nil {
				s.mapError(c, err)
				return
			}
			rows = append(rows, payload)
		}
		c.JSON(http.StatusOK, rows)
	}
}

// exportWindow parses the store list query parameters: startDate/endDate are
// required unix seconds, skip/take are optional non-negative integers.
func exportWindow(c *gin.Context) (time.Time, time.Time, int, int, error) {
	from, err := strconv.ParseInt(c.Query("startDate"), 10, 64)
	if err != nil {
		return time.Time{}, time.Time{}, 0, 0, fmt.Errorf("%w: startDate must be unix seconds", domain.ErrInvalidRequest)
	}
	to, err := strconv.ParseInt(c.Query("endDate"), 10, 64)
	if err != nil {
		return time.Time{}, time.Time{}, 0, 0, fmt.Errorf("%w: endDate must be unix seconds", domain.ErrInvalidRequest)
	}
	if to < from {
		return time.Time{}, time.Time{}, 0, 0, fmt.Errorf("%w: endDate precedes startDate", domain.ErrInvalidRequest)
	}
	skip, take := 0, 0
	if raw := c.Query("skip"); raw != "" {
		skip, err = strconv.Atoi(raw)
		if err != nil || skip < 0 {
			return time.Time{}, time.Time{}, 0, 0, fmt.Errorf("%w: skip must be a non-negative integer", domain.ErrInvalidRequest)
		}
	}
	if raw := c.Query("take"); raw != "" {
		take, err = strconv.Atoi(raw)
		if err != nil || take < 0 {
			return time.Time{}, time.Time{}, 0, 0, fmt.Errorf("%w: take must be a non-negative integer", domain.ErrInvalidRequest)
		}
	}
	return time.Unix(from, 0).UTC(), time.Unix(to, 0).UTC(), skip, take, nil
}

// createProfilePullPayment handles the provider-shaped pull payment creation;
// a same-name repeat replays the original (T-MOCK-09).
func (s *Server) createProfilePullPayment(profile domain.Profile) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			s.abortBind(c, err)
			return
		}
		request, err := profile.ParsePullPaymentCreate(body)
		if err != nil {
			s.mapError(c, err)
			return
		}
		pull, replayed, err := s.svc.CreatePullPayment(c.Request.Context(), request)
		if err != nil {
			s.mapError(c, err)
			return
		}
		payload, err := profile.PullPaymentResponse(pull)
		if err != nil {
			s.mapError(c, err)
			return
		}
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		}
		c.JSON(status, payload)
	}
}

// createProfilePayout handles the provider-shaped payout creation of a pull
// payment (the refund flow reuses the same route).
func (s *Server) createProfilePayout(profile domain.Profile) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			s.abortBind(c, err)
			return
		}
		request, err := profile.ParsePayoutCreate(body)
		if err != nil {
			s.mapError(c, err)
			return
		}
		payout, err := s.svc.CreatePayout(c.Request.Context(), c.Param("pull_payment_id"), request)
		if err != nil {
			s.mapError(c, err)
			return
		}
		payload, err := profile.PayoutResponse(payout)
		if err != nil {
			s.mapError(c, err)
			return
		}
		c.JSON(http.StatusOK, payload)
	}
}

// getProfilePayout returns the provider-shaped payout body; the refund read of
// the connector aliases this route.
func (s *Server) getProfilePayout(profile domain.Profile) gin.HandlerFunc {
	return func(c *gin.Context) {
		payout, err := s.svc.GetPayout(c.Request.Context(), c.Param("payout_id"))
		if err != nil {
			s.mapError(c, err)
			return
		}
		payload, err := profile.PayoutResponse(payout)
		if err != nil {
			s.mapError(c, err)
			return
		}
		c.JSON(http.StatusOK, payload)
	}
}

// simulateProfilePayout advances the payout state (the stand control that
// replaces the real operator approval and on-chain execution, T-MOCK-08).
func (s *Server) simulateProfilePayout(profile domain.Profile) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request payoutSimulateRequest
		if err := bindJSON(c, &request); err != nil {
			s.abortBind(c, err)
			return
		}
		payout, err := s.svc.SimulatePayout(c.Request.Context(), c.Param("payout_id"), request.State)
		if err != nil {
			s.mapError(c, err)
			return
		}
		payload, err := profile.PayoutResponse(payout)
		if err != nil {
			s.mapError(c, err)
			return
		}
		c.JSON(http.StatusOK, payload)
	}
}

// payoutSimulateRequest describes the stand payout state control body.
type payoutSimulateRequest struct {
	State string `json:"state"`
}

// mapInjectedFailure answers a scenario failure: the injected status plus the
// Retry-After header when the status is 429.
func (s *Server) mapInjectedFailure(c *gin.Context, failure domain.InjectedFailureError) {
	if failure.RetryAfterS > 0 && failure.Status == http.StatusTooManyRequests {
		c.Header("Retry-After", strconv.Itoa(failure.RetryAfterS))
	}
	s.abort(c, failure.Status, "provider_unavailable", http.StatusText(failure.Status), "provider failure scenario injected")
}

// isInjectedFailure extracts the injected failure of an error chain.
func isInjectedFailure(err error) (domain.InjectedFailureError, bool) {
	var failure domain.InjectedFailureError
	if errors.As(err, &failure) {
		return failure, true
	}
	return domain.InjectedFailureError{}, false
}
