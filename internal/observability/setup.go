// Package observability — OTEL SDK 初始化。
// 本文件实现 Setup/Providers：按 Config 组装 Tracer/Meter（noop/stdout/otlp 三种
// Exporter），并提供 Shutdown 与 ForceFlush 钩子。
package observability

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Providers 持有已初始化的 OTEL tracer 和 meter，以及关闭函数和 flush 函数。
type Providers struct {
	Tracer trace.Tracer
	Meter  metric.Meter
	// Shutdown 关闭所有 OTEL provider，应在进程退出时调用（defer）。
	Shutdown func(context.Context) error
	// ForceFlush 立即将 batcher 中的所有待发 span 推送到后端。
	// 在 interaction 结束时调用，确保 span 不因等待 batcher 定时而延迟上报。
	ForceFlush func(context.Context) error
}

// Setup 根据 cfg 初始化 OTEL tracer 和 meter。
// 若 cfg.Enabled=false 或 cfg.Exporter=noop，返回零开销的 noop 实现。
// 对 OTLP exporter：
//   - endpoint 和 headers 均由 cfg 显式传入（不依赖 SDK 的 env var 读取），保证可靠性
//   - traces 发往 cfg.OTLPEndpoint/v1/traces，metrics 发往 cfg.OTLPEndpoint/v1/metrics
//   - URL scheme 决定 TLS（https:// → 加密，http:// → 不加密）
//   - 导出失败通过全局 OTEL error handler 打印到日志，不静默丢弃
func Setup(ctx context.Context, cfg Config) (*Providers, error) {
	if !cfg.Enabled || cfg.Exporter == ExporterNoop {
		return noopProviders(), nil
	}

	// 注册全局 OTEL error handler，直接写 os.Stderr 绕过 TUI 的 log.SetOutput(io.Discard)，
	// 确保 OTEL 导出失败即使在 TUI 模式下也能被观测到。
	// context.Canceled 是 SDK shutdown 取消后台导出 goroutine 时的预期行为，不记录以免误导。
	otelLogger := log.New(os.Stderr, "", log.LstdFlags)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if errors.Is(err, context.Canceled) {
			return
		}
		otelLogger.Printf("[OTEL] 导出错误: %v", err)
	}))

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(cfg.ServiceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("创建 otel resource 失败：%w", err)
	}

	// ---- Trace Exporter ----
	var spanExporter sdktrace.SpanExporter
	switch cfg.Exporter {
	case ExporterStdout:
		spanExporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("创建 stdout trace exporter 失败：%w", err)
		}

	case ExporterOTLP:
		if cfg.OTLPEndpoint == "" {
			return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT 未设置，OTLP 模式下必填")
		}
		// 显式拼接 /v1/traces，不依赖 SDK 自动追加（SDK 版本间行为存在差异）。
		tracesURL := strings.TrimSuffix(cfg.OTLPEndpoint, "/") + "/v1/traces"
		traceOpts := []otlptracehttp.Option{
			otlptracehttp.WithEndpointURL(tracesURL),
		}
		if strings.HasPrefix(tracesURL, "http://") {
			traceOpts = append(traceOpts, otlptracehttp.WithInsecure())
		}
		if len(cfg.OTLPHeaders) > 0 {
			traceOpts = append(traceOpts, otlptracehttp.WithHeaders(cfg.OTLPHeaders))
		}
		spanExporter, err = otlptracehttp.New(ctx, traceOpts...)
		if err != nil {
			return nil, fmt.Errorf("创建 OTLP trace exporter 失败：%w", err)
		}
		otelLogger.Printf("[OTEL] Trace exporter 初始化成功 → %s（headers: %d 个）", tracesURL, len(cfg.OTLPHeaders))

	default:
		return noopProviders(), nil
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(spanExporter,
			sdktrace.WithBatchTimeout(2*time.Second), // 默认 5s，缩短为 2s 加快首次上报
		),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// ---- Metric Exporter ----
	var metricExporter sdkmetric.Exporter
	switch cfg.Exporter {
	case ExporterStdout:
		metricExporter, err = stdoutmetric.New()
		if err != nil {
			_ = tp.Shutdown(ctx)
			return nil, fmt.Errorf("创建 stdout metric exporter 失败：%w", err)
		}
	case ExporterOTLP:
		// Langfuse 目前只支持 traces，metrics 端点可能返回 404。
		// 创建 metrics exporter 但允许导出失败（fail-open），不影响 trace 上报。
		metricsURL := strings.TrimSuffix(cfg.OTLPEndpoint, "/") + "/v1/metrics"
		metricOpts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpointURL(metricsURL),
		}
		if strings.HasPrefix(metricsURL, "http://") {
			metricOpts = append(metricOpts, otlpmetrichttp.WithInsecure())
		}
		if len(cfg.OTLPHeaders) > 0 {
			metricOpts = append(metricOpts, otlpmetrichttp.WithHeaders(cfg.OTLPHeaders))
		}
		metricExporter, err = otlpmetrichttp.New(ctx, metricOpts...)
		if err != nil {
			// metrics 失败不阻断 traces，fail-open。
			// 写 stderr 绕过 TUI 的 log.SetOutput(io.Discard)，保持可见性。
			otelLogger.Printf("[OTEL] Metric exporter 初始化失败（已跳过，不影响 trace）: %v", err)
			metricExporter = nil
		}
	}

	var mp *sdkmetric.MeterProvider
	if metricExporter != nil {
		mp = sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter,
				sdkmetric.WithInterval(30*time.Second))),
			sdkmetric.WithResource(res),
		)
		otel.SetMeterProvider(mp)
	}

	// 直接从 SDK TracerProvider 获取 tracer，不经过全局 wrapper（otel.Tracer 走 wrapper
	// 在某些 OTEL SDK 版本下 span 不经过 BatchSpanProcessor，导致静默丢失）。
	// 测试证明：tp.Tracer() 直接路径能正确发到 Langfuse；otel.Tracer() 全局路径不行。
	tracer := tp.Tracer(cfg.ServiceName)
	meter := otel.Meter(cfg.ServiceName)

	shutdown := func(ctx context.Context) error {
		// 两个 provider 均尝试关闭，不因前者失败而跳过后者。
		// 使用 errors.Join 合并所有错误（Go 1.20+），调用方能感知全部失败原因。
		var tpErr, mpErr error
		if err := tp.Shutdown(ctx); err != nil {
			tpErr = fmt.Errorf("trace provider shutdown: %w", err)
		}
		if mp != nil {
			if err := mp.Shutdown(ctx); err != nil {
				mpErr = fmt.Errorf("metric provider shutdown: %w", err)
			}
		}
		return errors.Join(tpErr, mpErr)
	}
	// ForceFlush 直接绑定 SDK TracerProvider（不经过全局 wrapper），确保类型断言成功。
	forceFlush := func(ctx context.Context) error {
		return tp.ForceFlush(ctx)
	}
	return &Providers{Tracer: tracer, Meter: meter, Shutdown: shutdown, ForceFlush: forceFlush}, nil
}

// NewNoopProviders 返回零开销的 noop 实现，供测试使用。
func NewNoopProviders() *Providers {
	return noopProviders()
}

// noopProviders 返回零开销的 noop 实现。
// tracer 使用 trace/noop，meter 使用 metric/noop，两者均不依赖全局 OTEL provider，
// 确保即使在全局 provider 未初始化时也能安全调用。
func noopProviders() *Providers {
	return &Providers{
		Tracer:     noop.NewTracerProvider().Tracer("harness9"),
		Meter:      noopmetric.NewMeterProvider().Meter("harness9"),
		Shutdown:   func(_ context.Context) error { return nil },
		ForceFlush: func(_ context.Context) error { return nil },
	}
}
