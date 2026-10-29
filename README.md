json_exporter
========================
[![Build Status](https://github.com/prometheus-community/json_exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/prometheus-community/json_exporter/actions/workflows/ci.yml)


A [prometheus](https://prometheus.io/) exporter which scrapes remote JSON by JSONPath.

- [Supported JSONPath Syntax](https://kubernetes.io/docs/reference/kubectl/jsonpath/)
- [Examples configurations](/examples)

## Example Usage

```console
## SETUP

$ make build
$ ./json_exporter --config.file examples/config.yml &
$ python3 -m http.server 8000 &
Serving HTTP on :: port 8000 (http://[::]:8000/) ...


## TEST with 'default' module

$ curl "http://localhost:7979/probe?module=default&target=http://localhost:8000/examples/data.json"
# HELP example_global_value Example of a top-level global value scrape in the json
# TYPE example_global_value untyped
example_global_value{environment="beta",location="planet-mars"} 1234
# HELP example_timestamped_value_count Example of a timestamped value scrape in the json
# TYPE example_timestamped_value_count untyped
example_timestamped_value_count{environment="beta"} 2
# HELP example_value_active Example of sub-level value scrapes from a json
# TYPE example_value_active untyped
example_value_active{environment="beta",id="id-A"} 1
example_value_active{environment="beta",id="id-C"} 1
# HELP example_value_boolean Example of sub-level value scrapes from a json
# TYPE example_value_boolean untyped
example_value_boolean{environment="beta",id="id-A"} 1
example_value_boolean{environment="beta",id="id-C"} 0
# HELP example_value_count Example of sub-level value scrapes from a json
# TYPE example_value_count untyped
example_value_count{environment="beta",id="id-A"} 1
example_value_count{environment="beta",id="id-C"} 3


## TEST with a different module for different json file

$ curl "http://localhost:7979/probe?module=animals&target=http://localhost:8000/examples/animal-data.json"
# HELP animal_population Example of top-level lists in a separate module
# TYPE animal_population untyped
animal_population{name="deer",predator="false"} 456
animal_population{name="lion",predator="true"} 123
animal_population{name="pigeon",predator="false"} 789


## TEST through prometheus:

$ docker run --rm -it -p 9090:9090 -v $PWD/examples/prometheus.yml:/etc/prometheus/prometheus.yml prom/prometheus
```
Then head over to http://localhost:9090/graph?g0.range_input=1h&g0.expr=example_value_active&g0.tab=1 or http://localhost:9090/targets to check the scraped metrics or the targets.

## Using custom timestamps

This exporter allows you to use a field of the metric as the (unix/epoch) timestamp for the data as an int64. However, this may lead to unexpected behaviour, as the prometheus implements a [Staleness](https://prometheus.io/docs/prometheus/latest/querying/basics/#staleness) mechanism.

:warning: Including timestamps in metrics disables the staleness handling and can make data visible for longer than expected.

## Exposing metrics through HTTPS

TLS configuration supported by this exporter can be found at [exporter-toolkit/web](https://github.com/prometheus/exporter-toolkit/blob/v0.9.0/docs/web-configuration.md)

## Sending body content for HTTP `POST`

If `modules.<module_name>.body` paramater is set in config, it will be sent by the exporter as the body content in the scrape request. The HTTP method will also be set as 'POST' in this case.
```yaml
body:
  content: |
    My static information: {"time_diff": "1m25s", "anotherVar": "some value"}
```

The body content can also be a [Go Template](https://golang.org/pkg/text/template). All the functions from the [Sprig library](https://masterminds.github.io/sprig/) can be used in the template.
All the query parameters sent by prometheus in the scrape query to the exporter, are available as values while rendering the template.

Example using template functions:
```yaml
body:
  content: |
    {"time_diff": "{{ duration `95` }}","anotherVar": "{{ randInt 12 30 }}"}
  templatize: true
```

Example using template functions with values from the query parameters:
```yaml
body:
  content: |
    {"time_diff": "{{ duration `95` }}","anotherVar": "{{ .myVal | first }}"}
  templatize: true
```
Then `curl "http://exporter:7979/probe?target=http://scrape_target:8080/test/data.json&myVal=something"`, would result in sending the following body as the HTTP POST payload to `http://scrape_target:8080/test/data.json`:
```
{"time_diff": "1m35s","anotherVar": "something"}.
```

## Docker

```console
$ docker run -v $PWD/examples/config.yml:/config.yml quay.io/prometheuscommunity/json-exporter --config.file=/config.yml
```

## Embedding as a Go library

The `config` and `exporter` packages can collect metrics without CLI flags, a
listening HTTP server, or registration in the default Prometheus registry.
`config.Config` is the shared model for standalone and embedded callers:

```go
cfg := config.NewConfigWithDefaults()
cfg.Modules = map[string]config.Module{
    "queue": {
        Metrics: []config.Metric{{
            Name: "api_queue_depth",
            Path: "{.queue.depth}",
            ValueType: config.ValueTypeGauge,
        }},
    },
}
cfg.Targets = []config.Target{
    {Name: "orders", Address: "https://orders.example.com/status", Module: "queue"},
    {Name: "payments", Address: "https://payments.example.com/status", Module: "queue"},
}
runtime, err := exporter.NewRuntime(cfg, logger)
if err != nil {
    return err
}
defer runtime.Shutdown(context.Background())
registry := prometheus.NewRegistry()
for _, collector := range runtime.Collectors() {
    if err := registry.Register(collector); err != nil {
        return err
    }
}
families, err := registry.Gather()
```

The same configuration can be loaded with `config.LoadConfig`:

```yaml
modules:
  queue:
    metrics:
      - name: api_queue_depth
        path: '{.queue.depth}'
        valuetype: gauge
targets:
  - name: orders
    address: https://orders.example.com/status
    module: queue
  - name: payments
    address: https://payments.example.com/status
    module: queue
timeout: 30s
```

A runtime snapshots the entire configuration and owns one collector and HTTP
client per target, with a shared lifetime context. Registry gathers fetch all
targets concurrently, using fresh documents and request bodies. Named targets
add `target`, `target_name`, and `module` labels, plus their optional `labels`.
These three label names are reserved for named targets and cannot collide with
module metric labels. Multiple targets require unique names. A target's `module`
defaults to `default`; its `parameters` supplies lists of strings to POST body
templates. The runtime sets `target` and `module` template parameters itself.

`NewConfigWithDefaults` and `LoadConfig` use a 30-second timeout for each HTTP
request. Set `timeout: 0s` to leave deadlines to the caller; embedded applications
can require a positive timeout. Configurations containing only modules remain
valid: the standalone `/probe` handler translates each request's query parameters
into a single unnamed target, then uses the same runtime. Unnamed targets keep
the standalone metric format, without extra target-identification labels.

Fetch failures become registry gather errors; successful target families remain
available alongside those errors. JSONPath extraction keeps the exporter's
existing behavior: extraction errors are logged and affected samples are omitted.

`runtime.Scrape(ctx)` fetches the configured targets with a caller-supplied context
and returns a collector of successful snapshots plus any fetch errors. The
standalone handler uses it to retain its request cancellation and HTTP error
handling. Shutdown cancels all in-flight requests in that runtime; stopping one
runtime does not affect another.
