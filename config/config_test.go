// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.yaml.in/yaml/v2"
)

func TestConfigFileWithAndWithoutTargets(t *testing.T) {
	for _, targets := range []string{"", `targets:
  - name: orders
    address: https://orders.example.com/status
    parameters:
      tenant: [example]
timeout: 10s
`} {
		content := `modules:
  default:
    metrics:
      - name: queue_depth
        path: '{.queue.depth}'
` + targets
		file := filepath.Join(t.TempDir(), "config.yml")
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.Modules["default"].Metrics[0].Type != ValueScrape {
			t.Fatal("metric defaults missing")
		}
		if targets == "" {
			if len(cfg.Targets) != 0 || cfg.Timeout != 30*time.Second {
				t.Fatalf("bad legacy defaults: %v", cfg)
			}
		} else if cfg.Timeout != 10*time.Second || cfg.Targets[0].Module != "default" || cfg.Targets[0].Parameters.Get("tenant") != "example" {
			t.Fatalf("bad target config: %v", cfg)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	base := func() Config {
		return Config{Modules: map[string]Module{"default": {}}, Targets: []Target{{Name: "api", Address: "https://example.com"}}}
	}
	cases := map[string]func(*Config){
		"unknown module":           func(c *Config) { c.Targets[0].Module = "missing" },
		"invalid URL":              func(c *Config) { c.Targets[0].Address = "file:///tmp/data" },
		"negative timeout":         func(c *Config) { c.Timeout = -time.Second },
		"duplicate name":           func(c *Config) { c.Targets = append(c.Targets, c.Targets[0]) },
		"unnamed multiple targets": func(c *Config) { c.Targets = append(c.Targets, Target{Address: "https://other.example.com"}) },
		"reserved label":           func(c *Config) { c.Targets[0].Labels = map[string]string{"module": "bad"} },
		"metric label collision": func(c *Config) {
			c.Modules["default"] = Module{Metrics: []Metric{{Name: "test", Labels: map[string]string{"target": "bad"}}}}
		},
		"custom label collision": func(c *Config) {
			c.Targets[0].Labels = map[string]string{"team": "one"}
			c.Modules["default"] = Module{Metrics: []Metric{{Name: "test", Labels: map[string]string{"team": "two"}}}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestConfigStrictYAMLAndExplicitZeroTimeout(t *testing.T) {
	cfg := NewConfigWithDefaults()
	if err := yaml.UnmarshalStrict([]byte("modules: {default: {}}\ntimeout: 0s\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.WithDefaults().Timeout != 0 {
		t.Fatal("explicit zero timeout replaced")
	}
	if err := yaml.UnmarshalStrict([]byte("modules: {default: {}}\ntargets: [{address: 'https://example.com', typo: true}]\n"), &cfg); err == nil {
		t.Fatal("accepted unknown target field")
	}
}
