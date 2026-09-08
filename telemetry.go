package main

import (
	"context"
	"fmt"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv/v1.26.0"
)

// telemetry bundles the process-wide OpenTelemetry providers. All exports go
// to the OTLP endpoint configured via the standard OTEL_* environment
// variables (e.g. Logfire: OTEL_EXPORTER_OTLP_ENDPOINT +
// OTEL_EXPORTER_OTLP_HEADERS='Authorization=...'). When no endpoint is set,
// the providers stay no-op so existing deployments are unaffected.
type telemetry struct {
	tp *sdktrace.TracerProvider
	mp *metric.MeterProvider
	lp *sdklog.LoggerProvider
}

// otlpEndpoint returns the configured OTLP endpoint or "" when telemetry is
// disabled.
func otlpEndpoint() string {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
}

// initTelemetry builds tracer/metric/log providers backed by OTLP exporters
// and installs them as the global providers. It returns nil when OTLP is not
// configured (or setup fails), so callers can skip shutdown safely.
func initTelemetry() *telemetry {
	ep := otlpEndpoint()
	if ep == "" {
		log.Info("otel: OTEL_EXPORTER_OTLP_ENDPOINT not set; telemetry disabled")
		return nil
	}
	log.Infof("otel: exporting traces/metrics/logs to %s", ep)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName("wrapper-manager"),
			semconv.ServiceVersion("v2"),
		),
	)
	if err != nil {
		log.Warnf("otel: failed to build resource: %v; telemetry disabled", err)
		return nil
	}

	t := &telemetry{}

	// Tracer provider.
	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		log.Warnf("otel: failed to create trace exporter: %v; telemetry disabled", err)
		return nil
	}
	t.tp = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(t.tp)

	// Meter provider.
	if metricExporter, err := otlpmetrichttp.New(ctx); err == nil {
		t.mp = metric.NewMeterProvider(metric.WithReader(metric.NewPeriodicReader(metricExporter, metric.WithInterval(30*time.Second))))
		otel.SetMeterProvider(t.mp)
	} else {
		log.Warnf("otel: failed to create metric exporter: %v", err)
	}

	// Logger provider.
	if logExporter, err := otlploghttp.New(ctx); err == nil {
		t.lp = sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
			sdklog.WithResource(res),
		)
		global.SetLoggerProvider(t.lp)
	} else {
		log.Warnf("otel: failed to create log exporter: %v", err)
	}

	return t
}

// Shutdown flushes and stops all providers. Safe to call with nil receiver.
func (t *telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	var errs []error
	if t.tp != nil {
		if err := t.tp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("tracer provider: %w", err))
		}
	}
	if t.mp != nil {
		if err := t.mp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("meter provider: %w", err))
		}
	}
	if t.lp != nil {
		if err := t.lp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("logger provider: %w", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("telemetry shutdown: %v", errs)
	}
	return nil
}
