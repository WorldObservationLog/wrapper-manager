package main

import (
	"context"
	"fmt"

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
	msg := entry.Message
	if len(msg) >= 10 && msg[:10] == "[wrapper " {
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
