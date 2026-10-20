// main starts the HTTP service of the CBL platform mock-gateway:
// the sandbox provider emulation and signed webhook delivery.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/ZMichael3012/cbl-kit/ginmw"

	"github.com/cbl/cbl-mock-gateway/internal/domain"
	"github.com/cbl/cbl-mock-gateway/internal/transport"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
	"github.com/cbl/cbl-mock-gateway/internal/webhook"
)

// main starts the mock provider HTTP server and the metrics server;
// it shuts down on SIGINT/SIGTERM.
func main() {
	log, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer func() { _ = log.Sync() }()

	switcher, err := buildSenderSwitch(log)
	if err != nil {
		log.Error("build webhook senders failed", zap.Error(err))
		return
	}
	service := usecase.NewService(switcher, log, usecase.WithTargets(switcher))
	server := transport.NewServer(service, log)

	httpAddr := getenv("HTTP_ADDR", ":8120")
	metricsAddr := getenv("METRICS_ADDR", ":9320")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	router := gin.New()
	router.Use(ginmw.RequestID())
	router.Use(ginmw.AccessLog(log))
	router.Use(ginmw.Recovery(log))
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/readyz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	server.Register(router)

	httpSrv := &http.Server{
		Addr:              httpAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// The processing budget covers the worst simulate scenario:
		// latency 5s + delay 5s + 10 deliveries at WEBHOOK_TIMEOUT (5s).
		WriteTimeout: 75 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	metricsSrv := &http.Server{Addr: metricsAddr, Handler: metricsHandler(), ReadHeaderTimeout: 5 * time.Second}

	go func() {
		log.Info("http server started", zap.String("addr", httpAddr))
		if serveErr := httpSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("http server failed", zap.Error(serveErr))
			stop()
		}
	}()
	go func() {
		log.Info("metrics server started", zap.String("addr", metricsAddr))
		if serveErr := metricsSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("metrics server failed", zap.Error(serveErr))
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutdown started")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if shutdownErr := httpSrv.Shutdown(shutdownCtx); shutdownErr != nil {
		log.Error("http shutdown failed", zap.Error(shutdownErr))
	}
	if shutdownErr := metricsSrv.Shutdown(shutdownCtx); shutdownErr != nil {
		log.Error("metrics shutdown failed", zap.Error(shutdownErr))
	}
	log.Info("shutdown finished")
}

// buildSenderSwitch wires the legacy mock sender and the per-profile senders:
// a profile without a derivable target and without an explicit env target has
// no sender, so simulate answers 400 for it (spec FR-7).
func buildSenderSwitch(log *zap.Logger) (*webhook.Switch, error) {
	timeout := durationEnv(log, "WEBHOOK_TIMEOUT", 5*time.Second)
	eventIDHeader := getenv("WEBHOOK_EVENT_ID_HEADER", "X-Event-Id")
	baseTarget := getenv("WEBHOOK_TARGET_URL", "http://localhost:8060/hooks/mock/ep1")

	legacy, err := webhook.NewHTTPSender(
		baseTarget,
		getenv("WEBHOOK_SECRET", "mock-secret"),
		getenv("WEBHOOK_SIGNATURE_HEADER", "X-Billing-Signature"),
		eventIDHeader,
		timeout,
	)
	if err != nil {
		return nil, err
	}
	senders := map[domain.ProfileID]usecase.WebhookSender{domain.ProfileMock: legacy}

	secretDefaults := map[domain.ProfileID]string{
		domain.ProfileNOWPayments: "mock-nowpayments-secret",
		domain.ProfileCoinGate:    "mock-coingate-token",
		domain.ProfileBitPay:      "mock-bitpay-secret",
		domain.ProfileBTCPay:      "mock-btcpay-secret",
	}
	for _, profile := range domain.AllProfiles() {
		if profile.ID == domain.ProfileMock {
			continue
		}
		envSuffix := strings.ToUpper(string(profile.ID))
		target := os.Getenv("WEBHOOK_TARGET_URL_" + envSuffix)
		if target == "" {
			derived, ok := webhook.DeriveTarget(baseTarget, profile.ID)
			if !ok {
				log.Warn("profile webhook target is not configured",
					zap.String("provider", string(profile.ID)),
					zap.String("env", "WEBHOOK_TARGET_URL_"+envSuffix),
				)
				continue
			}
			target = derived
		}
		secret := getenv("WEBHOOK_SECRET_"+envSuffix, secretDefaults[profile.ID])
		sender, senderErr := webhook.NewProfileSender(profile, target, []byte(secret), eventIDHeader, timeout)
		if senderErr != nil {
			return nil, senderErr
		}
		senders[profile.ID] = sender
	}
	return webhook.NewSwitch(senders)
}

// metricsHandler serves a metrics stub until task 0.7.
func metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintln(w, "# metrics endpoint placeholder (task 0.7)")
	})
	return mux
}

// getenv returns the environment variable value or the default value.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// durationEnv reads a duration from an environment variable;
// an invalid value falls back to the default with a warning.
func durationEnv(log *zap.Logger, key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		log.Warn("invalid duration env, using default",
			zap.String("key", key),
			zap.String("value", raw),
			zap.Duration("default", fallback),
		)
		return fallback
	}
	return parsed
}
