// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");

package exporter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/mitchellh/copystructure"
	"github.com/prometheus-community/json_exporter/config"
	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"
)

// Runtime owns the collectors, HTTP clients and cancellation for configured targets.
// Gathering its collectors fetches those targets without opening an HTTP listener.
type Runtime struct {
	targets    []*targetCollector
	collectors []prometheus.Collector
	logger     *slog.Logger
	timeout    time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
}

type targetCollector struct {
	runtime *Runtime
	target  config.Target
	module  config.Module
	metrics []JSONMetric
	client  *http.Client
	labels  prometheus.Labels
}

// NewRuntime validates and snapshots the same Config used by the standalone
// exporter. At least one target is required; construction makes no target requests.
func NewRuntime(cfg config.Config, logger *slog.Logger) (*Runtime, error) {
	// URL user information is immutable and has unexported fields. Preserve it
	// explicitly rather than letting a structural copier erase proxy credentials.
	copier := copystructure.Config{Copiers: map[reflect.Type]copystructure.CopierFunc{
		reflect.TypeFor[url.Userinfo](): func(value interface{}) (interface{}, error) { return value, nil },
	}}
	snapshot, err := copier.Copy(cfg)
	if err != nil {
		return nil, fmt.Errorf("copy config: %w", err)
	}
	cfg = snapshot.(config.Config).WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if len(cfg.Targets) == 0 {
		return nil, fmt.Errorf("at least one target is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &Runtime{logger: logger, timeout: cfg.Timeout, ctx: ctx, cancel: cancel}
	// Check all descriptors together, including conflicts between target modules.
	registry := prometheus.NewRegistry()
	for _, target := range cfg.Targets {
		module := cfg.Modules[target.Module]
		metrics, err := CreateMetricsList(module)
		if err != nil {
			_ = runtime.Shutdown(context.Background())
			return nil, err
		}
		client, err := pconfig.NewClientFromConfig(module.HTTPClientConfig, "json_exporter", pconfig.WithKeepAlivesDisabled(), pconfig.WithHTTP2Disabled())
		if err != nil {
			_ = runtime.Shutdown(context.Background())
			return nil, fmt.Errorf("target %q: create HTTP client: %w", target.Name, err)
		}
		labels := prometheus.Labels(maps.Clone(target.Labels))
		if target.Name != "" {
			if labels == nil {
				labels = make(prometheus.Labels)
			}
			labels["target"] = target.Address
			labels["target_name"] = target.Name
			labels["module"] = target.Module
		}
		collector := &targetCollector{runtime: runtime, target: target, module: module, metrics: metrics, client: client, labels: labels}
		runtime.targets = append(runtime.targets, collector)
		wrapped := prometheus.WrapCollectorWith(labels, collector)
		if err := registry.Register(wrapped); err != nil {
			_ = runtime.Shutdown(context.Background())
			return nil, fmt.Errorf("target %q: validate metric descriptors: %w", target.Name, err)
		}
		runtime.collectors = append(runtime.collectors, wrapped)
	}
	return runtime, nil
}

// Collectors returns one collector per target for registration by the caller.
// Registry gathers collect the targets concurrently and preserve healthy targets
// when another target fails. No collectors are registered globally.
func (r *Runtime) Collectors() []prometheus.Collector { return slices.Clone(r.collectors) }

// Scrape fetches all targets concurrently with a caller-supplied context and
// returns a collector over the successful snapshots, plus any fetch errors.
// The standalone /probe handler uses this with one request-selected target.
func (r *Runtime) Scrape(ctx context.Context) (prometheus.Collector, error) {
	collectors := make(collectorGroup, len(r.targets))
	errs := make([]error, len(r.targets))
	var wg sync.WaitGroup
	for i, target := range r.targets {
		wg.Go(func() {
			collector, err := target.scrape(ctx)
			if err != nil {
				errs[i] = target.wrapError(err)
				return
			}
			collectors[i] = prometheus.WrapCollectorWith(target.labels, collector)
		})
	}
	wg.Wait()
	return collectors, errors.Join(errs...)
}

func (t *targetCollector) scrape(ctx context.Context) (prometheus.Collector, error) {
	r := t.runtime
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()
	if r.ctx.Err() != nil {
		return nil, r.ctx.Err()
	}
	if r.timeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, r.timeout)
		defer timeoutCancel()
	}
	// Parameters have /probe query semantics; do not share mutable request readers.
	params := make(url.Values, len(t.target.Parameters)+2)
	for key, values := range t.target.Parameters {
		params[key] = slices.Clone(values)
	}
	params.Set("target", t.target.Address)
	params.Set("module", t.target.Module)
	method, body := renderBody(r.logger, t.module.Body, params)
	data, err := fetchJSON(ctx, t.client, r.logger, t.module, t.target.Address, method, body)
	if err != nil {
		return nil, err
	}
	return JSONMetricCollector{JSONMetrics: t.metrics, Data: data, Logger: r.logger}, nil
}

func (t *targetCollector) wrapError(err error) error {
	if t.target.Name == "" {
		return err
	}
	return fmt.Errorf("target %q: %w", t.target.Name, err)
}

func (t *targetCollector) Describe(ch chan<- *prometheus.Desc) {
	JSONMetricCollector{JSONMetrics: t.metrics}.Describe(ch)
}

func (t *targetCollector) Collect(ch chan<- prometheus.Metric) {
	collector, err := t.scrape(t.runtime.ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(prometheus.NewDesc("json_exporter_scrape_error", "JSON target fetch failed", nil, nil), t.wrapError(err))
		return
	}
	collector.Collect(ch)
}

// Shutdown cancels all of this runtime's requests and releases idle connections.
func (r *Runtime) Shutdown(context.Context) error {
	r.cancel()
	for _, target := range r.targets {
		target.client.CloseIdleConnections()
	}
	return nil
}

type collectorGroup []prometheus.Collector

func (g collectorGroup) Describe(ch chan<- *prometheus.Desc) {
	for _, collector := range g {
		if collector != nil {
			collector.Describe(ch)
		}
	}
}

func (g collectorGroup) Collect(ch chan<- prometheus.Metric) {
	for _, collector := range g {
		if collector != nil {
			collector.Collect(ch)
		}
	}
}
