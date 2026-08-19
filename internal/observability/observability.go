// Package observability configures structured logging and OpenTelemetry tracing.
package observability

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/SarnautCore/server/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/credentials"
)

// NewLogger creates the process JSON logger.
func NewLogger(levelName string) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToLower(levelName))); err != nil {
		level = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

// Setup installs the global OpenTelemetry provider. An empty endpoint records no spans.
func Setup(
	ctx context.Context,
	settings config.OTelConfig,
	serviceName string,
	buildID string,
) (func(context.Context) error, error) {
	options := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", serviceName),
			attribute.String("service.version", buildID),
		)),
	}

	if settings.Endpoint == "" {
		options = append(options, sdktrace.WithSampler(sdktrace.NeverSample()))
	} else {
		exporterOptions := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(settings.Endpoint)}
		if settings.Insecure {
			exporterOptions = append(exporterOptions, otlptracegrpc.WithInsecure())
		} else {
			exporterOptions = append(exporterOptions, otlptracegrpc.WithTLSCredentials(
				credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13}),
			))
		}

		exporter, err := otlptracegrpc.New(ctx, exporterOptions...)
		if err != nil {
			return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
		}
		options = append(options, sdktrace.WithBatcher(exporter))
	}

	provider := sdktrace.NewTracerProvider(options...)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return provider.Shutdown, nil
}
