// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");

package exporter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"

	"github.com/mitchellh/copystructure"
	"github.com/prometheus-community/json_exporter/config"
	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"
)

// Runtime owns the configuration, HTTP client and cancellation of one JSON target.
// It can be registered in a caller-owned registry without opening an HTTP listener.
type Runtime struct {
	config  config.RuntimeConfig
	metrics []JSONMetric
	client  *http.Client
	logger  *slog.Logger
	ctx     context.Context
	cancel  context.CancelFunc
}

// NewRuntime validates and snapshots configuration. Construction makes no target requests.
func NewRuntime(cfg config.RuntimeConfig, logger *slog.Logger) (*Runtime, error) {
	// URL user information is immutable and has unexported fields. Preserve it
	// explicitly rather than letting a structural copier erase proxy credentials.
	copier := copystructure.Config{Copiers: map[reflect.Type]copystructure.CopierFunc{
		reflect.TypeFor[url.Userinfo](): func(value interface{}) (interface{}, error) { return value, nil },
	}}
	snapshot, err := copier.Copy(cfg)
	if err != nil {
		return nil, fmt.Errorf("copy runtime config: %w", err)
	}
	cfg = snapshot.(config.RuntimeConfig)
	cfg.Module = cfg.Module.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	metrics, err := CreateMetricsList(cfg.Module)
	if err != nil {
		return nil, err
	}
	// Validate descriptors before accepting a runtime; registration must not panic.
	if err := prometheus.NewRegistry().Register(JSONMetricCollector{JSONMetrics: metrics}); err != nil {
		return nil, fmt.Errorf("validate metric descriptors: %w", err)
	}
	client, err := pconfig.NewClientFromConfig(cfg.Module.HTTPClientConfig, "json_exporter", pconfig.WithKeepAlivesDisabled(), pconfig.WithHTTP2Disabled())
	if err != nil {
		return nil, fmt.Errorf("create HTTP client: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runtime{config: cfg, metrics: metrics, client: client, logger: logger, ctx: ctx, cancel: cancel}, nil
}

// Scrape fetches a fresh document and returns its metric collector. Both the
// caller's context and Shutdown cancel the fetch. The collector owns its data.
func (r *Runtime) Scrape(ctx context.Context) (prometheus.Collector, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()
	if r.ctx.Err() != nil {
		return nil, r.ctx.Err()
	}
	if r.config.Timeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, r.config.Timeout)
		defer timeoutCancel()
	}
	// Render for each scrape: template functions and request readers are not shared.
	method, body := renderBody(r.logger, r.config.Module.Body, r.config.Parameters)
	data, err := fetchJSON(ctx, r.client, r.logger, r.config.Module, r.config.Target, method, body)
	if err != nil {
		return nil, err
	}
	return JSONMetricCollector{JSONMetrics: r.metrics, Data: data, Logger: r.logger}, nil
}

// Collectors returns the target's collectors for registration by the caller.
func (r *Runtime) Collectors() []prometheus.Collector { return []prometheus.Collector{r} }

// Describe supplies the configured metric descriptors without fetching a document.
func (r *Runtime) Describe(ch chan<- *prometheus.Desc) {
	JSONMetricCollector{JSONMetrics: r.metrics}.Describe(ch)
}

// Collect fetches the target once for each registry gather. Fetch failures are
// returned through the registry's normal gather-error mechanism.
func (r *Runtime) Collect(ch chan<- prometheus.Metric) {
	collector, err := r.Scrape(r.ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(prometheus.NewDesc("json_exporter_scrape_error", "JSON target fetch failed", nil, nil), err)
		return
	}
	collector.Collect(ch)
}

// Shutdown cancels in-flight requests and releases idle HTTP connections.
func (r *Runtime) Shutdown(context.Context) error {
	r.cancel()
	r.client.CloseIdleConnections()
	return nil
}
