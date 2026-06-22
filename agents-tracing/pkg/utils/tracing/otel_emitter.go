/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tracing

import (
	"context"
	"fmt"
	"sync"

	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// OTELConfig holds configuration for the OTEL trace emitter.
type OTELConfig struct {
	Enabled    bool
	Endpoint   string
	Insecure   bool
	SampleRate float64
}

// BindFlags registers OTEL tracing CLI flags on the provided flag set.
func BindFlags(fs *pflag.FlagSet, cfg *OTELConfig) {
	fs.BoolVar(&cfg.Enabled, "enable-tracing", false, "Enable OpenTelemetry tracing")
	fs.StringVar(&cfg.Endpoint, "otel-endpoint", "localhost:4317", "OTLP gRPC collector endpoint")
	fs.BoolVar(&cfg.Insecure, "otel-insecure", true, "Use insecure gRPC connection to OTLP collector")
	fs.Float64Var(&cfg.SampleRate, "otel-sample-rate", 1.0, "Trace sampling rate (0.0 to 1.0)")
}

// OTELEmitter implements TraceEmitter using the OpenTelemetry SDK.
type OTELEmitter struct {
	cfg      OTELConfig
	service  string
	tracer   trace.Tracer
	shutdown func(context.Context) error
	once     sync.Once
}

// NewOTELEmitter creates a new OTEL emitter with the given config and service name.
func NewOTELEmitter(cfg OTELConfig, serviceName string) *OTELEmitter {
	return &OTELEmitter{
		cfg:     cfg,
		service: serviceName,
	}
}

func (e *OTELEmitter) Enabled() bool {
	return e.cfg.Enabled
}

func (e *OTELEmitter) Init() {
	e.once.Do(func() {
		if !e.cfg.Enabled {
			return
		}
		ctx := context.Background()

		var dialOpts []grpc.DialOption
		if e.cfg.Insecure {
			dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		}

		exporter, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(e.cfg.Endpoint),
			otlptracegrpc.WithDialOption(dialOpts...),
		)
		if err != nil {
			fmt.Printf("[tracing] failed to create OTLP exporter: %v\n", err)
			return
		}

		res, err := resource.New(ctx,
			resource.WithAttributes(
				semconv.ServiceNameKey.String(e.service),
			),
		)
		if err != nil {
			fmt.Printf("[tracing] failed to create resource: %v\n", err)
			return
		}

		sampler := sdktrace.AlwaysSample()
		if e.cfg.SampleRate < 1.0 {
			sampler = sdktrace.TraceIDRatioBased(e.cfg.SampleRate)
		}

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sampler),
		)
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))

		e.tracer = tp.Tracer("agents.kruise.io/tracing")
		e.shutdown = tp.Shutdown
	})
}

func (e *OTELEmitter) Emit(ctx context.Context, entry *TraceLogEntry) {
	if e.tracer == nil {
		return
	}

	_, span := e.tracer.Start(ctx, entry.Name,
		trace.WithTimestamp(entry.StartTime),
		trace.WithAttributes(
			attribute.String("k8s.resource.uuid", entry.ResourceUID),
			attribute.String("k8s.lifecycle.operation", entry.Phase),
			attribute.String("module", entry.Module),
			attribute.String("trace.id", entry.TraceID),
		),
	)

	if !entry.CreationTime.IsZero() {
		span.SetAttributes(attribute.String("k8s.resource.creation_time", entry.CreationTime.UTC().String()))
	}

	for k, v := range entry.Extra {
		span.SetAttributes(attribute.String(k, v))
	}

	if !entry.Success {
		span.SetStatus(codes.Error, entry.ErrorCode)
		span.SetAttributes(attribute.String("error.code", entry.ErrorCode))
	}

	span.End(trace.WithTimestamp(entry.EndTime))
}

// Shutdown flushes pending spans and shuts down the TracerProvider.
func (e *OTELEmitter) Shutdown(ctx context.Context) error {
	if e.shutdown != nil {
		return e.shutdown(ctx)
	}
	return nil
}
