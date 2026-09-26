package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
)

// logrusHook forwards logrus entries to the OpenTelemetry logger provider so
// application logs appear alongside traces/metrics in the OTLP backend
// (e.g. Logfire). It is a no-op when no logger provider is configured.
type logrusHook struct {
	ctx context.Context
}

var _ logrus.Hook = (*logrusHook)(nil)

// Levels returns all logrus levels so every log line is captured.
func (h *logrusHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

// otelSeverity maps a logrus level to the OTel log severity.
func otelSeverity(lv logrus.Level) log.Severity {
	switch lv {
	case logrus.PanicLevel, logrus.FatalLevel:
		return log.SeverityFatal
	case logrus.ErrorLevel:
		return log.SeverityError
	case logrus.WarnLevel:
		return log.SeverityWarn
	case logrus.InfoLevel:
		return log.SeverityInfo
	case logrus.DebugLevel, logrus.TraceLevel:
		return log.SeverityDebug
	default:
		return log.SeverityInfo
	}
}

// Fire converts one logrus entry into an OTel log record and emits it.
// Lines that merely relay wrapper-lite process output ("[wrapper ...]") are
// skipped: they are high-volume debug relays, not manager diagnostics, and
// would drown the OTLP backend.
func (h *logrusHook) Fire(entry *logrus.Entry) error {
	if isWrapperRelayNoise(entry) {
		return nil
	}
	lp := global.GetLoggerProvider()
	if lp == nil {
		return nil
	}
	logger := lp.Logger("wrapper-manager")

	// Carry message text and the entry-level fields as log attributes.
	attrs := make([]attribute.KeyValue, 0, len(entry.Data)+2)
	attrs = append(attrs, attribute.String("message", entry.Message))
	for k, v := range entry.Data {
		attrs = append(attrs, logrusFieldAttribute(k, v))
	}

	rec := log.Record{}
	rec.SetTimestamp(entry.Time)
	rec.SetSeverity(otelSeverity(entry.Level))
	rec.SetSeverityText(entry.Level.String())
	rec.SetBody(attribute.StringValue(entry.Message))
	rec.AddAttributes(attrs...)

	logger.Emit(h.ctx, rec)
	return nil
}

// isWrapperRelayNoise reports whether a log entry is high-volume relay of
// wrapper-lite process output that should not be exported to OTLP.
//
// Two things are always kept because they are the actionable signals:
//   - any entry logged at WARN or above (manager health events such as
//     circuit-breaker trips and account state changes), regardless of its
//     message shape;
//   - lite relay lines that carry lite's own [ERROR]/[WARN] severity
//     (FairPlay failures, subscription problems, handler exceptions).
func isWrapperRelayNoise(entry *logrus.Entry) bool {
	// logrus levels run from Panic(0) to Trace(6): lower means more severe, so
	// anything more severe than Info is always exported.
	if entry.Level < logrus.InfoLevel {
		return false
	}
	msg := entry.Message
	if !strings.HasPrefix(msg, "[wrapper ") {
		return false
	}
	return !strings.Contains(msg, "[ERROR]") && !strings.Contains(msg, "[WARN")
}

// logrusFieldAttribute converts a logrus data field value into an OTel
// attribute with a sensible type mapping.
func logrusFieldAttribute(k string, v any) attribute.KeyValue {
	switch t := v.(type) {
	case string:
		return attribute.String(k, t)
	case int:
		return attribute.Int(k, t)
	case int64:
		return attribute.Int64(k, t)
	case bool:
		return attribute.Bool(k, t)
	case float64:
		return attribute.Float64(k, t)
	case error:
		return attribute.String(k, t.Error())
	default:
		return attribute.String(k, fmt.Sprintf("%v", t))
	}
}

// attachLogrusHook installs the logrus -> OTel bridge. Safe to call always:
// when no OTLP logger provider is configured, Emit is a no-op.
func attachLogrusHook() {
	logrus.AddHook(&logrusHook{ctx: context.Background()})
}
