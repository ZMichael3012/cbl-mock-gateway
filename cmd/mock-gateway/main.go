// main запускает HTTP-сервис mock-gateway платформы CBL:
// эмуляцию sandbox-провайдера и отправку подписанных вебхуков.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-mock-gateway/internal/transport"
	"github.com/cbl/cbl-mock-gateway/internal/usecase"
	"github.com/cbl/cbl-mock-gateway/internal/webhook"
)

// main поднимает HTTP-сервер mock-провайдера и сервер метрик;
// завершает работу по SIGINT/SIGTERM.
func main() {
	log, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer func() { _ = log.Sync() }()

	sender, err := webhook.NewHTTPSender(
		getenv("WEBHOOK_TARGET_URL", "http://localhost:8060/hooks/mock/ep1"),
		getenv("WEBHOOK_SECRET", "mock-secret"),
		getenv("WEBHOOK_SIGNATURE_HEADER", "X-Billing-Signature"),
		getenv("WEBHOOK_EVENT_ID_HEADER", "X-Event-Id"),
		durationEnv(log, "WEBHOOK_TIMEOUT", 5*time.Second),
	)
	if err != nil {
		log.Error("build webhook sender failed", zap.Error(err))
		return
	}
	service := usecase.NewService(sender, log)
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
		// Бюджет обработки покрывает худший сценарий simulate:
		// latency 5с + delay 5с + 10 доставок по WEBHOOK_TIMEOUT (5с).
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

// metricsHandler отдает заглушку метрик до задачи 0.7.
func metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintln(w, "# metrics endpoint placeholder (task 0.7)")
	})
	return mux
}

// getenv возвращает значение переменной окружения или значение по умолчанию.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// durationEnv читает длительность из переменной окружения;
// невалидное значение заменяется значением по умолчанию с предупреждением.
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
