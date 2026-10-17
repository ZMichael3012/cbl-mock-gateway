package transport

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
)

// profileRoutes describes the provider API routes of a profile.
type profileRoutes struct {
	createPath string
	getPath    string
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
