// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");

package exporter

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/prometheus-community/json_exporter/config"
	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"
)

func testConfig(target string) config.Config {
	cfg := config.NewConfigWithDefaults()
	cfg.Targets = []config.Target{{Address: target}}
	cfg.Modules = map[string]config.Module{"default": {Metrics: []config.Metric{{Name: "example_value", Path: "{.value}", ValueType: config.ValueTypeGauge}}}}
	return cfg
}

func TestRuntimeRepeatedConcurrentGathers(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || string(body) != `{"key":"original"}` || r.Header.Get("X-Test") != "original" {
			t.Errorf("request changed: method=%s body=%s header=%s", r.Method, body, r.Header.Get("X-Test"))
		}
		_, _ = io.WriteString(w, `{"value":42}`)
	}))
	defer target.Close()
	cfg := testConfig(target.URL)
	module := cfg.Modules["default"]
	module.Headers = map[string]string{"X-Test": "original"}
	module.Body = config.Body{Content: `{"key":"{{ .key | first }}"}`, Templatize: true}
	cfg.Modules["default"] = module
	cfg.Targets[0].Parameters = url.Values{"key": {"original"}}
	runtime, err := NewRuntime(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown(context.Background())
	cfg.Modules["default"].Headers["X-Test"] = "mutated"
	cfg.Targets[0].Parameters["key"][0] = "mutated"
	cfg.Modules["default"].Metrics[0].Name = "mutated"
	registry := prometheus.NewPedanticRegistry()
	for _, c := range runtime.Collectors() {
		if err := registry.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			for range 2 {
				families, err := registry.Gather()
				if err != nil {
					t.Error(err)
					return
				}
				if len(families) != 1 || families[0].GetName() != "example_value" || families[0].Metric[0].GetGauge().GetValue() != 42 {
					t.Errorf("unexpected metrics: %v", families)
				}
			}
		})
	}
	wg.Wait()
}

func TestRuntimeCancellation(t *testing.T) {
	for _, mode := range []string{"shutdown", "caller", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
			defer target.Close()
			cfg := testConfig(target.URL)
			if mode == "timeout" {
				cfg.Timeout = 100 * time.Millisecond
			}
			runtime, err := NewRuntime(cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Shutdown(context.Background())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := runtime.Scrape(ctx); done <- err }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request never started")
			}
			if mode == "shutdown" {
				_ = runtime.Shutdown(context.Background())
			}
			if mode == "caller" {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected cancellation")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("fetch not cancelled")
			}
		})
	}
}

func TestRuntimeInvalidConfig(t *testing.T) {
	for _, target := range []string{"", "://bad", "file:///tmp/test", "http://"} {
		if _, err := NewRuntime(testConfig(target), nil); err == nil {
			t.Errorf("accepted %q", target)
		}
	}
	cfg := testConfig("http://localhost")
	cfg.Modules["default"].Metrics[0].ValueType = "invalid"
	if _, err := NewRuntime(cfg, nil); err == nil {
		t.Fatal("accepted invalid value type")
	}
	cfg = testConfig("http://localhost")
	cfg.Modules["default"].Metrics[0].Name = ""
	if _, err := NewRuntime(cfg, nil); err == nil {
		t.Fatal("accepted invalid descriptor")
	}
}

func TestRuntimeFetchErrorIsGatherError(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer target.Close()
	runtime, err := NewRuntime(testConfig(target.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown(context.Background())
	registry := prometheus.NewRegistry()
	registry.MustRegister(runtime.Collectors()...)
	if _, err := registry.Gather(); err == nil {
		t.Fatal("fetch error was swallowed")
	}
}

func TestJSONFetcherInvalidURL(t *testing.T) {
	fetcher := NewJSONFetcher(context.Background(), slog.Default(), config.Module{}, nil)
	if _, err := fetcher.FetchJSON("://invalid"); err == nil {
		t.Fatal("accepted invalid URL")
	}
}

func TestRuntimePreservesProxyCredentials(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Errorf("proxy credentials lost: %q", r.Header.Get("Proxy-Authorization"))
		}
		_, _ = io.WriteString(w, `{"value":9}`)
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.User = url.UserPassword("user", "pass")
	cfg := testConfig("http://json-target.invalid/data")
	module := cfg.Modules["default"]
	module.HTTPClientConfig.ProxyURL = pconfig.URL{URL: proxyURL}
	cfg.Modules["default"] = module
	runtime, err := NewRuntime(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown(context.Background())
	registry := prometheus.NewRegistry()
	registry.MustRegister(runtime.Collectors()...)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].Metric[0].GetGauge().GetValue() != 9 {
		t.Fatalf("unexpected metrics: %v", families)
	}
}

func TestRuntimeMultipleTargetsCollectConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.URL.Path
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		if r.URL.Path == "/one" {
			_, _ = io.WriteString(w, `{"value":1}`)
		} else {
			_, _ = io.WriteString(w, `{"value":2}`)
		}
	}))
	defer target.Close()
	defer unblock()
	cfg := testConfig(target.URL)
	cfg.Targets = []config.Target{
		{Name: "one", Address: target.URL + "/one"},
		{Name: "two", Address: target.URL + "/two"},
	}
	runtime, err := NewRuntime(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown(context.Background())
	if len(started) != 0 {
		t.Fatal("constructor fetched a target")
	}
	cfg.Targets[0].Address = "http://not-the-snapshot.invalid"
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(runtime.Collectors()...)
	done := make(chan error, 1)
	go func() {
		families, err := registry.Gather()
		if err == nil {
			if len(families) != 1 || len(families[0].Metric) != 2 {
				t.Errorf("unexpected metrics: %v", families)
			} else {
				seen := make(map[string]float64)
				for _, metric := range families[0].Metric {
					for _, label := range metric.Label {
						if label.GetName() == "target_name" {
							seen[label.GetValue()] = metric.GetGauge().GetValue()
						}
					}
				}
				if seen["one"] != 1 || seen["two"] != 2 {
					t.Errorf("targets not distinguished: %v", seen)
				}
			}
		}
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("targets were not fetched concurrently")
		}
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gather did not finish")
	}
}

func TestRuntimeMultiTargetScrapeRetainsSuccessfulSnapshots(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"value":3}`)
	}))
	defer target.Close()
	cfg := testConfig(target.URL)
	cfg.Targets = []config.Target{{Name: "healthy", Address: target.URL}, {Name: "failed", Address: target.URL + "/fail"}}
	runtime, err := NewRuntime(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown(context.Background())
	for range 2 {
		collector, err := runtime.Scrape(context.Background())
		if err == nil {
			t.Fatal("missing target failure")
		}
		registry := prometheus.NewPedanticRegistry()
		if err := registry.Register(collector); err != nil {
			t.Fatal(err)
		}
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		if len(families) != 1 || len(families[0].Metric) != 1 || families[0].Metric[0].GetGauge().GetValue() != 3 {
			t.Fatalf("healthy snapshot lost: %v", families)
		}
	}
}

func TestRuntimeRequiresTargets(t *testing.T) {
	cfg := testConfig("https://example.com")
	cfg.Targets = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("modules-only file rejected: %v", err)
	}
	if _, err := NewRuntime(cfg, nil); err == nil {
		t.Fatal("runtime accepted no targets")
	}
}
