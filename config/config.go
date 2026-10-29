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
	"net/url"
	"os"
	"time"

	pconfig "github.com/prometheus/common/config"
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

// Config contains multiple modules.
type Config struct {
	Modules map[string]Module `yaml:"modules"`
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
	var config Config
	data, err := os.ReadFile(configPath)
	if err != nil {
		return config, err
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return config, err
	}

	for name, module := range config.Modules {
		config.Modules[name] = module.WithDefaults()
	}
	return config, nil
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

// RuntimeConfig configures one target without depending on a CLI or HTTP handler.
// Parameters supplies the query parameters available to POST body templates.
type RuntimeConfig struct {
	Target     string
	Module     Module
	Parameters url.Values
	Timeout    time.Duration
}

// NewRuntimeConfigWithDefaults returns defaults for an embedded target.
func NewRuntimeConfigWithDefaults() RuntimeConfig {
	return RuntimeConfig{Timeout: 30 * time.Second}
}

// Validate checks configuration without contacting the target.
// A zero timeout leaves the deadline to the caller, as in the standalone probe.
func (c RuntimeConfig) Validate() error {
	target, err := url.Parse(c.Target)
	if err != nil {
		return fmt.Errorf("parse target: %w", err)
	}
	if (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return fmt.Errorf("target must be an absolute HTTP or HTTPS URL")
	}
	if c.Timeout < 0 {
		return fmt.Errorf("timeout cannot be negative")
	}
	for _, metric := range c.Module.WithDefaults().Metrics {
		if metric.Type != ValueScrape && metric.Type != ObjectScrape {
			return fmt.Errorf("unknown metric type %q", metric.Type)
		}
		switch metric.ValueType {
		case ValueTypeGauge, ValueTypeCounter, ValueTypeUntyped:
		default:
			return fmt.Errorf("unknown value type %q", metric.ValueType)
		}
	}
	httpConfig := c.Module.HTTPClientConfig
	return httpConfig.Validate()
}
