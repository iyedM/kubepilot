// Package live collects a read-only snapshot.Snapshot from a live Kubernetes
// cluster, Tempo trace backend, and Prometheus server.
//
// In accordance with CLAUDE.md rule 3, this package never mutates cluster state
// (only HTTP GET requests and read-only `kubectl get` / `kubectl logs` calls).
// No OpenTelemetry "gen_ai.*" attribute or metric literals appear here; they
// are provided by internal/genai.
package live

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/collector/offline"
	"github.com/iyedM/kubepilot/internal/genai"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

// Config configures a read-only live collection pass.
type Config struct {
	Kubeconfig       string
	KubectlBin       string
	Namespace        string
	ObsNamespace     string
	KubeAPIURL       string
	PrometheusURL    string
	TempoURL         string
	From             time.Time
	To               time.Time
	BaselineDuration time.Duration
	TraceID          string
	SkewTolerance    time.Duration
	HTTPClient       *http.Client
}

// DefaultPrometheusQueries returns the named Prometheus range queries used to
// populate Snapshot.Metrics (matching lab/faults/lib.sh).
func DefaultPrometheusQueries() map[string]string {
	return map[string]string{
		"cpu_throttled_periods":             `container_cpu_cfs_throttled_periods_total{namespace=~"genai|noise"}`,
		"cpu_periods":                       `container_cpu_cfs_periods_total{namespace=~"genai|noise"}`,
		"cpu_usage_seconds":                 `container_cpu_usage_seconds_total{namespace=~"genai|noise",container!=""}`,
		"cpu_pressure_waiting_seconds":      `container_pressure_cpu_waiting_seconds_total{namespace=~"genai|noise",container!=""}`,
		"memory_working_set_bytes":          `container_memory_working_set_bytes{namespace=~"genai|noise",container!=""}`,
		"restarts":                          `kube_pod_container_status_restarts_total{namespace=~"genai|noise"}`,
		"last_terminated_reason":            `kube_pod_container_status_last_terminated_reason{namespace=~"genai|noise"}`,
		"pod_ready":                         `kube_pod_status_ready{namespace=~"genai|noise",condition="true"}`,
		"pod_phase":                         `kube_pod_status_phase{namespace=~"genai|noise"} == 1`,
		"genai_duration_count":              genai.PromQueryOperationDurationCount(),
		"genai_duration_sum":                genai.PromQueryOperationDurationSum(),
		"llama_predicted_tokens_per_second": `llamacpp:predicted_tokens_seconds`,
		"llama_requests_deferred":           `llamacpp:requests_deferred`,
	}
}

// Collect queries the live cluster, Tempo, and Prometheus in read-only mode
// and returns a normalized Snapshot ready for rules.Diagnose.
func Collect(ctx context.Context, cfg Config) (*snapshot.Snapshot, error) {
	cfg = applyDefaults(cfg)

	var targetTrace *snapshot.TraceSummary
	if cfg.TraceID != "" {
		raw, err := fetchTempoTrace(ctx, cfg, cfg.TraceID)
		if err == nil && len(raw) > 0 {
			if tr, err := genai.ParseOTLPTrace(cfg.TraceID, raw); err == nil {
				targetTrace = tr
			}
		}
	}

	win := resolveWindow(cfg, targetTrace)

	podsRaw, err := fetchPodsJSON(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("collect pods: %w", err)
	}
	pods, err := offline.ParsePodsJSON(bytes.NewReader(podsRaw))
	if err != nil {
		return nil, err
	}

	eventsRaw, err := fetchEventsJSON(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("collect events: %w", err)
	}
	events, err := offline.ParseEventsJSON(bytes.NewReader(eventsRaw), win)
	if err != nil {
		return nil, err
	}

	var requests []snapshot.RequestSample
	if logsRaw, err := fetchLoadgenLogs(ctx, cfg, win); err == nil && len(logsRaw) > 0 {
		requests, _ = offline.ParseRequestsJSONL(bytes.NewReader(logsRaw), win)
	}

	traces := collectTraces(ctx, cfg, win, targetTrace, requests)

	metrics, err := collectPrometheusMetrics(ctx, cfg, win)
	if err != nil {
		return nil, fmt.Errorf("collect prometheus metrics: %w", err)
	}

	return &snapshot.Snapshot{
		Window:        win,
		TargetTraceID: cfg.TraceID,
		Traces:        traces,
		Requests:      requests,
		Pods:          pods,
		Events:        events,
		Metrics:       metrics,
	}, nil
}

func applyDefaults(cfg Config) Config {
	if cfg.Namespace == "" {
		cfg.Namespace = "genai"
	}
	if cfg.ObsNamespace == "" {
		cfg.ObsNamespace = "observability"
	}
	if cfg.SkewTolerance <= 0 {
		cfg.SkewTolerance = offline.DefaultSkewTolerance
	}
	if cfg.BaselineDuration <= 0 {
		cfg.BaselineDuration = 3 * time.Minute
	}
	if cfg.KubectlBin == "" {
		if _, err := os.Stat(".tools/bin/kubectl"); err == nil {
			cfg.KubectlBin = ".tools/bin/kubectl"
		} else {
			cfg.KubectlBin = "kubectl"
		}
	}
	if cfg.Kubeconfig == "" {
		if _, err := os.Stat(".tools/kubeconfig"); err == nil {
			cfg.Kubeconfig = ".tools/kubeconfig"
		} else {
			cfg.Kubeconfig = os.Getenv("KUBECONFIG")
		}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return cfg
}

func resolveWindow(cfg Config, targetTrace *snapshot.TraceSummary) snapshot.Window {
	from := cfg.From.UTC()
	to := cfg.To.UTC()
	if (from.IsZero() || to.IsZero()) && targetTrace != nil && !targetTrace.StartTime.IsZero() {
		end := targetTrace.EndTime
		if end.IsZero() {
			end = targetTrace.StartTime
		}
		from = targetTrace.StartTime.Add(-2 * time.Minute)
		to = end.Add(30 * time.Second)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	if from.IsZero() {
		from = to.Add(-5 * time.Minute)
	}
	baseStart := from.Add(-cfg.BaselineDuration)
	return snapshot.Window{
		From: from,
		To:   to,
		Baseline: &snapshot.TimeRange{
			From:   baseStart,
			To:     from,
			Reason: "baseline",
		},
		SkewTolerance: cfg.SkewTolerance,
	}
}

func fetchPodsJSON(ctx context.Context, cfg Config) ([]byte, error) {
	if cfg.KubeAPIURL != "" {
		return httpGet(ctx, cfg.HTTPClient, strings.TrimRight(cfg.KubeAPIURL, "/")+"/api/v1/pods")
	}
	return runKubectlReadOnly(ctx, cfg, "get", "pods", "-A", "-o", "json")
}

func fetchEventsJSON(ctx context.Context, cfg Config) ([]byte, error) {
	if cfg.KubeAPIURL != "" {
		return httpGet(ctx, cfg.HTTPClient, strings.TrimRight(cfg.KubeAPIURL, "/")+"/api/v1/events")
	}
	return runKubectlReadOnly(ctx, cfg, "get", "events", "-A", "-o", "json")
}

func fetchLoadgenLogs(ctx context.Context, cfg Config, win snapshot.Window) ([]byte, error) {
	if cfg.KubeAPIURL != "" {
		return httpGet(ctx, cfg.HTTPClient, strings.TrimRight(cfg.KubeAPIURL, "/")+"/logs/loadgen")
	}
	sinceTime := win.From.Add(-cfg.BaselineDuration - time.Minute).Format(time.RFC3339)
	return runKubectlReadOnly(ctx, cfg, "-n", cfg.Namespace, "logs", "deploy/loadgen", "--since-time="+sinceTime)
}

func fetchTempoTrace(ctx context.Context, cfg Config, traceID string) ([]byte, error) {
	path := "/api/v2/traces/" + url.PathEscape(traceID)
	if cfg.TempoURL != "" {
		return httpGet(ctx, cfg.HTTPClient, strings.TrimRight(cfg.TempoURL, "/")+path)
	}
	rawPath := fmt.Sprintf("/api/v1/namespaces/%s/services/tempo:3200/proxy%s", cfg.ObsNamespace, path)
	return runKubectlReadOnly(ctx, cfg, "get", "--raw", rawPath)
}

func collectTraces(ctx context.Context, cfg Config, win snapshot.Window, targetTrace *snapshot.TraceSummary, requests []snapshot.RequestSample) []snapshot.TraceSummary {
	seen := make(map[string]struct{})
	var out []snapshot.TraceSummary
	if targetTrace != nil && targetTrace.TraceID != "" {
		seen[targetTrace.TraceID] = struct{}{}
		out = append(out, *targetTrace)
	}
	for _, req := range requests {
		if req.TraceID == "" {
			continue
		}
		if _, exists := seen[req.TraceID]; exists {
			continue
		}
		seen[req.TraceID] = struct{}{}
		raw, err := fetchTempoTrace(ctx, cfg, req.TraceID)
		if err != nil || len(raw) == 0 {
			continue
		}
		tr, err := genai.ParseOTLPTrace(req.TraceID, raw)
		if err != nil {
			continue
		}
		if win.InWindowWithSkew(tr.StartTime) || win.InBaseline(tr.StartTime) {
			out = append(out, *tr)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartTime.Before(out[j].StartTime)
	})
	return out
}

func collectPrometheusMetrics(ctx context.Context, cfg Config, win snapshot.Window) (map[string]snapshot.Series, error) {
	startSec := win.From.Add(-cfg.SkewTolerance).Unix()
	if win.Baseline != nil && win.Baseline.From.Before(win.From) {
		startSec = win.Baseline.From.Unix()
	}
	endSec := win.To.Add(cfg.SkewTolerance).Unix()

	queries := DefaultPrometheusQueries()
	out := make(map[string]snapshot.Series, len(queries))
	for name, q := range queries {
		params := url.Values{}
		params.Set("query", q)
		params.Set("start", fmt.Sprintf("%d", startSec))
		params.Set("end", fmt.Sprintf("%d", endSec))
		params.Set("step", "15")
		path := "/api/v1/query_range?" + params.Encode()

		var raw []byte
		var err error
		if cfg.PrometheusURL != "" {
			raw, err = httpGet(ctx, cfg.HTTPClient, strings.TrimRight(cfg.PrometheusURL, "/")+path)
		} else {
			rawPath := fmt.Sprintf("/api/v1/namespaces/%s/services/prometheus-server:80/proxy%s", cfg.ObsNamespace, path)
			raw, err = runKubectlReadOnly(ctx, cfg, "get", "--raw", rawPath)
			if err != nil {
				fallbackPath := fmt.Sprintf("/api/v1/namespaces/%s/services/kube-prometheus-stack-prometheus:9090/proxy%s", cfg.ObsNamespace, path)
				raw, err = runKubectlReadOnly(ctx, cfg, "get", "--raw", fallbackPath)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", name, err)
		}
		series, err := offline.ParsePrometheusRangeResponse(q, raw, win)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		out[name] = series
	}
	return out, nil
}

func httpGet(ctx context.Context, client *http.Client, fullURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", fullURL, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func runKubectlReadOnly(ctx context.Context, cfg Config, args ...string) ([]byte, error) {
	// Enforce read-only subcommands (CLAUDE.md rule 3).
	hasAllowedSubcmd := false
	for _, a := range args {
		if a == "get" || a == "logs" {
			hasAllowedSubcmd = true
			break
		}
	}
	if !hasAllowedSubcmd {
		return nil, fmt.Errorf("refusing non-read-only kubectl invocation: %v", args)
	}
	var fullArgs []string
	if cfg.Kubeconfig != "" {
		fullArgs = append(fullArgs, "--kubeconfig", cfg.Kubeconfig)
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.CommandContext(ctx, cfg.KubectlBin, fullArgs...)
	return cmd.Output()
}
