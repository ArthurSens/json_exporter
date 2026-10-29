// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"time"

	pconfig "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"go.yaml.in/yaml/v2"
)

// Metric contains values that define a metric
type Metric struct {
	Name            string
	Path            string
	Labels          map[string]string
	Type            ScrapeType
	ValueType       ValueType
	EpochTimestamp  string
	Help            string
	Values          map[string]string
	AllowMissingKey bool `yaml:"allow_missing_key,omitempty"`
}

type ScrapeType string

const (
	ValueScrape  ScrapeType = "value" // default
	ObjectScrape ScrapeType = "object"
)

type ValueType string

const (
	ValueTypeGauge   ValueType = "gauge"
	ValueTypeCounter ValueType = "counter"
	ValueTypeUntyped ValueType = "untyped"
)

// Config contains reusable modules and optional targets for scheduled collection.
// Modules-only configurations remain valid for the standalone /probe handler.
type Config struct {
	Modules map[string]Module `yaml:"modules"`
	Targets []Target          `yaml:"targets,omitempty"`
	Timeout time.Duration     `yaml:"timeout,omitempty"`
}

// Target selects a module and supplies its HTTP endpoint and body-template parameters.
// Named targets attach target, target_name and module labels to every metric.
// An unnamed single target preserves the standalone /probe metric format.
type Target struct {
	Name       string            `yaml:"name,omitempty"`
	Address    string            `yaml:"address"`
	Module     string            `yaml:"module,omitempty"`
	Labels     map[string]string `yaml:"labels,omitempty"`
	Parameters url.Values        `yaml:"parameters,omitempty"`
}

// NewConfigWithDefaults returns defaults shared by file-based and embedded callers.
func NewConfigWithDefaults() Config {
	return Config{Timeout: 30 * time.Second}
}

// Module contains metrics and headers defining a configuration
type Module struct {
	Headers          map[string]string        `yaml:"headers,omitempty"`
	Metrics          []Metric                 `yaml:"metrics"`
	HTTPClientConfig pconfig.HTTPClientConfig `yaml:"http_client_config,omitempty"`
	Body             Body                     `yaml:"body,omitempty"`
	ValidStatusCodes []int                    `yaml:"valid_status_codes,omitempty"`
}

type Body struct {
	Content    string `yaml:"content"`
	Templatize bool   `yaml:"templatize,omitempty"`
}

func LoadConfig(configPath string) (Config, error) {
	cfg := NewConfigWithDefaults()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return cfg, err
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}

	return cfg.WithDefaults(), nil
}

// WithDefaults applies module defaults and resolves omitted target module names.
// It does not replace an explicit zero timeout, which leaves deadlines to the caller.
func (c Config) WithDefaults() Config {
	c.Modules = maps.Clone(c.Modules)
	for name, module := range c.Modules {
		c.Modules[name] = module.WithDefaults()
	}
	c.Targets = slices.Clone(c.Targets)
	for i := range c.Targets {
		if c.Targets[i].Module == "" {
			c.Targets[i].Module = "default"
		}
	}
	return c
}

// WithDefaults returns a module with the same defaults used by file-based configuration.
// The caller must finish constructing the module before handing it to a runtime.
func (m Module) WithDefaults() Module {
	m.Metrics = append([]Metric(nil), m.Metrics...)
	for i := range m.Metrics {
		metric := &m.Metrics[i]
		if metric.Type == "" {
			metric.Type = ValueScrape
		}
		if metric.Help == "" {
			metric.Help = metric.Name
		}
		if metric.ValueType == "" {
			metric.ValueType = ValueTypeUntyped
		}
	}
	return m
}

// Validate checks modules and configured targets without contacting any target.
// Targets are optional here because /probe supplies a target with each request.
func (c Config) Validate() error {
	c = c.WithDefaults()
	if len(c.Modules) == 0 {
		return fmt.Errorf("at least one module is required")
	}
	if c.Timeout < 0 {
		return fmt.Errorf("timeout cannot be negative")
	}
	for name, module := range c.Modules {
		for _, metric := range module.Metrics {
			if metric.Type != ValueScrape && metric.Type != ObjectScrape {
				return fmt.Errorf("module %q: unknown metric type %q", name, metric.Type)
			}
			switch metric.ValueType {
			case ValueTypeGauge, ValueTypeCounter, ValueTypeUntyped:
			default:
				return fmt.Errorf("module %q: unknown value type %q", name, metric.ValueType)
			}
		}
		httpConfig := module.HTTPClientConfig
		if err := httpConfig.Validate(); err != nil {
			return fmt.Errorf("module %q: %w", name, err)
		}
	}
	names := make(map[string]bool, len(c.Targets))
	for i, target := range c.Targets {
		if target.Name == "" && len(c.Targets) > 1 {
			return fmt.Errorf("target %d: name is required when multiple targets are configured", i)
		}
		if names[target.Name] {
			return fmt.Errorf("duplicate target name %q", target.Name)
		}
		names[target.Name] = true
		address, err := url.Parse(target.Address)
		if err != nil {
			return fmt.Errorf("target %q: parse address: %w", target.Name, err)
		}
		if (address.Scheme != "http" && address.Scheme != "https") || address.Host == "" {
			return fmt.Errorf("target %q: address must be an absolute HTTP or HTTPS URL", target.Name)
		}
		module, ok := c.Modules[target.Module]
		if !ok {
			return fmt.Errorf("target %q: unknown module %q", target.Name, target.Module)
		}
		labels := maps.Clone(target.Labels)
		if target.Name != "" {
			if labels == nil {
				labels = make(map[string]string)
			}
			for _, key := range []string{"target", "target_name", "module"} {
				if _, exists := labels[key]; exists {
					return fmt.Errorf("target %q: label %q is reserved", target.Name, key)
				}
				labels[key] = ""
			}
		}
		for key := range labels {
			if !model.LabelName(key).IsValid() {
				return fmt.Errorf("target %q: invalid label %q", target.Name, key)
			}
			for _, metric := range module.Metrics {
				if _, exists := metric.Labels[key]; exists {
					return fmt.Errorf("target %q: label %q collides with a metric label", target.Name, key)
				}
			}
		}
	}
	return nil
}
