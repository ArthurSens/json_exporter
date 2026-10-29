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

func testRuntimeConfig(target string) config.RuntimeConfig {
	cfg := config.NewRuntimeConfigWithDefaults()
	cfg.Target = target
	cfg.Module.Metrics = []config.Metric{{Name: "example_value", Path: "{.value}", ValueType: config.ValueTypeGauge}}
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
	cfg := testRuntimeConfig(target.URL)
	cfg.Module.Headers = map[string]string{"X-Test": "original"}
	cfg.Module.Body = config.Body{Content: `{"key":"{{ .key | first }}"}`, Templatize: true}
	cfg.Parameters = url.Values{"key": {"original"}}
	runtime, err := NewRuntime(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Shutdown(context.Background())
	cfg.Module.Headers["X-Test"] = "mutated"
	cfg.Parameters["key"][0] = "mutated"
	cfg.Module.Metrics[0].Name = "mutated"
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
			cfg := testRuntimeConfig(target.URL)
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
		if _, err := NewRuntime(testRuntimeConfig(target), nil); err == nil {
			t.Errorf("accepted %q", target)
		}
	}
	cfg := testRuntimeConfig("http://localhost")
	cfg.Module.Metrics[0].ValueType = "invalid"
	if _, err := NewRuntime(cfg, nil); err == nil {
		t.Fatal("accepted invalid value type")
	}
	cfg = testRuntimeConfig("http://localhost")
	cfg.Module.Metrics[0].Name = ""
	if _, err := NewRuntime(cfg, nil); err == nil {
		t.Fatal("accepted invalid descriptor")
	}
}

func TestRuntimeFetchErrorIsGatherError(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer target.Close()
	runtime, err := NewRuntime(testRuntimeConfig(target.URL), nil)
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
	cfg := testRuntimeConfig("http://json-target.invalid/data")
	cfg.Module.HTTPClientConfig.ProxyURL = pconfig.URL{URL: proxyURL}
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
