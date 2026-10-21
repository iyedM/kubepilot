// Package genai is the single versioned adapter package allowed to reference
// OpenTelemetry GenAI semantic convention attribute, event, and metric names
// ("gen_ai.*" and Prometheus-translated "gen_ai_*").
//
// See CLAUDE.md rule 5 and docs/genai-conventions.md for the measured telemetry
// emitted by opentelemetry-instrumentation-genai-openai==1.2b0.
package genai

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

// Span attribute keys (OTel GenAI semantic conventions).
const (
	attrOperationName     = "gen_ai.operation.name"
	attrProviderName      = "gen_ai.provider.name" // SDK name ("openai"), NOT the serving backend
	attrRequestModel      = "gen_ai.request.model"
	attrResponseModel     = "gen_ai.response.model"
	attrRequestMaxTokens  = "gen_ai.request.max_tokens"
	attrResponseID        = "gen_ai.response.id"
	attrFinishReasons     = "gen_ai.response.finish_reasons"
	attrInputTokens       = "gen_ai.usage.input_tokens"
	attrOutputTokens      = "gen_ai.usage.output_tokens"
	attrCachedInputTokens = "gen_ai.usage.cache_read.input_tokens"
	attrTokenType         = "gen_ai.token.type"

	// Privacy-sensitive attributes that MUST NEVER be copied into a Snapshot
	// (CLAUDE.md rule 4).
	attrInputMessages      = "gen_ai.input.messages"
	attrOutputMessages     = "gen_ai.output.messages"
	attrSystemInstructions = "gen_ai.system_instructions"
	eventInferenceDetails  = "gen_ai.client.inference.operation.details"

	// Non-gen_ai attributes carried on GenAI spans.
	attrSystemFingerprint = "openai.response.system_fingerprint"
	attrServerAddress     = "server.address"
	attrServerPort        = "server.port"
	attrErrorType         = "error.type"
	attrHTTPStatusCode    = "http.status_code"
	attrHTTPResponseCode  = "http.response.status_code"
)

// OTLP metric names and their Prometheus translations (handling the 2026-10-06
// rename from gen_ai.client.operation.duration to gen_ai.client.inference.duration).
const (
	metricOTLPOperationDuration = "gen_ai.client.operation.duration"
	metricOTLPInferenceDuration = "gen_ai.client.inference.duration"
	metricOTLPTokenUsage        = "gen_ai.client.token.usage"

	promPrefixOperationDuration = "gen_ai_client_operation_duration_seconds"
	promPrefixInferenceDuration = "gen_ai_client_inference_duration_seconds"
	promPrefixTokenUsage        = "gen_ai_client_token_usage"

	promLabelOperationName = "gen_ai_operation_name"
	promLabelRequestModel  = "gen_ai_request_model"
	promLabelResponseModel = "gen_ai_response_model"
	promLabelTokenType     = "gen_ai_token_type"
)

// IsSensitiveKey reports whether an attribute or event name carries prompt or
// completion text that must never be collected by default.
func IsSensitiveKey(key string) bool {
	switch key {
	case attrInputMessages, attrOutputMessages, attrSystemInstructions, eventInferenceDetails:
		return true
	default:
		return false
	}
}

// IsDurationMetric reports whether an OTLP or Prometheus metric name represents
// the GenAI client operation/inference duration histogram.
func IsDurationMetric(name string) bool {
	if name == metricOTLPOperationDuration || name == metricOTLPInferenceDuration {
		return true
	}
	return strings.HasPrefix(name, promPrefixOperationDuration) ||
		strings.HasPrefix(name, promPrefixInferenceDuration)
}

// IsTokenUsageMetric reports whether an OTLP or Prometheus metric name
// represents the GenAI token usage histogram.
func IsTokenUsageMetric(name string) bool {
	return name == metricOTLPTokenUsage || strings.HasPrefix(name, promPrefixTokenUsage)
}

// PromSeriesLabels holds the normalized GenAI labels extracted from a
// Prometheus series label map.
type PromSeriesLabels struct {
	Operation     string
	RequestModel  string
	ResponseModel string
	TokenType     string
	ServerAddress string
	ServerPort    string
	ErrorType     string
	PodName       string
	Namespace     string
	NodeName      string
	Deployment    string
}

// ExtractPromLabels normalizes a Prometheus series label map without leaking
// raw gen_ai_* label names to callers.
func ExtractPromLabels(labels map[string]string) PromSeriesLabels {
	return PromSeriesLabels{
		Operation:     labels[promLabelOperationName],
		RequestModel:  labels[promLabelRequestModel],
		ResponseModel: labels[promLabelResponseModel],
		TokenType:     labels[promLabelTokenType],
		ServerAddress: labels["server_address"],
		ServerPort:    labels["server_port"],
		ErrorType:     labels["error_type"],
		PodName:       labels["k8s_pod_name"],
		Namespace:     labels["k8s_namespace_name"],
		NodeName:      labels["k8s_node_name"],
		Deployment:    labels["k8s_deployment_name"],
	}
}

// --- OTLP / Tempo trace parsing ----------------------------------------------

type otlpEnvelope struct {
	TraceID       string          `json:"trace_id"`
	Trace         json.RawMessage `json:"trace"`
	ResourceSpans []otlpResSpan   `json:"resourceSpans"`
	Batches       []otlpResSpan   `json:"batches"`
}

type otlpResSpan struct {
	Resource   otlpResource    `json:"resource"`
	ScopeSpans []otlpScopeSpan `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpan struct {
	Scope struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId"`
	Name              string         `json:"name"`
	Kind              any            `json:"kind"`
	StartTimeUnixNano any            `json:"startTimeUnixNano"`
	EndTimeUnixNano   any            `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes"`
	Status            otlpStatus     `json:"status"`
}

type otlpStatus struct {
	Code    any    `json:"code"`
	Message string `json:"message"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue *string         `json:"stringValue,omitempty"`
	IntValue    any             `json:"intValue,omitempty"`
	DoubleValue *float64        `json:"doubleValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
	ArrayValue  *otlpArrayValue `json:"arrayValue,omitempty"`
}

type otlpArrayValue struct {
	Values []otlpValue `json:"values"`
}

// ParseOTLPTrace parses a single trace payload in any of the three forms used
// by Tempo and the lab exports:
//   - {"trace_id": "...", "trace": {"trace": {"resourceSpans": [...]}}}
//   - {"trace": {"resourceSpans": [...]}}
//   - {"resourceSpans": [...]} or {"batches": [...]}
//
// It returns a normalized snapshot.TraceSummary with all prompt/completion
// content stripped.
func ParseOTLPTrace(traceIDHint string, raw []byte) (*snapshot.TraceSummary, error) {
	resSpans, traceID, err := unwrapOTLPResourceSpans(traceIDHint, raw)
	if err != nil {
		return nil, err
	}
	if len(resSpans) == 0 {
		return nil, fmt.Errorf("no resourceSpans in trace payload")
	}

	out := &snapshot.TraceSummary{
		TraceID: traceID,
	}

	var (
		httpStartNs  int64
		httpEndNs    int64
		hasHTTPSpan  bool
		genaiStartNs int64
		genaiEndNs   int64
		hasGenAISpan bool
	)

	for _, rs := range resSpans {
		resAttrs := kvSliceToMap(rs.Resource.Attributes)
		if out.ClientPodName == "" {
			out.ClientPodName = asString(resAttrs["k8s.pod.name"])
			out.ClientPodUID = asString(resAttrs["k8s.pod.uid"])
			out.ClientNamespace = asString(resAttrs["k8s.namespace.name"])
			out.ClientNodeName = asString(resAttrs["k8s.node.name"])
			out.ClientDeployment = asString(resAttrs["k8s.deployment.name"])
			out.ServiceName = asString(resAttrs["service.name"])
		}

		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				attrs := kvSliceToMap(sp.Attributes)
				if out.TraceID == "" && sp.TraceID != "" {
					out.TraceID = decodeID(sp.TraceID)
				}
				startNs := parseNano(sp.StartTimeUnixNano)
				endNs := parseNano(sp.EndTimeUnixNano)

				if isGenAISpan(sp.Name, ss.Scope.Name, attrs) {
					hasGenAISpan = true
					genaiStartNs = startNs
					genaiEndNs = endNs
					out.SpanID = decodeID(sp.SpanID)
					populateFromGenAIAttrs(out, attrs)
					if isErrorStatus(sp.Status.Code) || out.ErrorType != "" {
						out.Error = true
						if sp.Status.Message != "" {
							out.StatusMsg = sp.Status.Message
						}
					}
					continue
				}

				if isServerHTTPSpan(sp.Kind, attrs) {
					if !hasHTTPSpan || startNs < httpStartNs {
						hasHTTPSpan = true
						httpStartNs = startNs
						httpEndNs = endNs
					}
					if code := asInt(attrs[attrHTTPStatusCode]); code > 0 {
						out.HTTPStatusCode = code
					} else if code := asInt(attrs[attrHTTPResponseCode]); code > 0 {
						out.HTTPStatusCode = code
					}
				}
			}
		}
	}

	if hasGenAISpan {
		out.StartTime = time.Unix(0, genaiStartNs).UTC()
		out.EndTime = time.Unix(0, genaiEndNs).UTC()
		if genaiEndNs >= genaiStartNs {
			out.InferenceMs = float64(genaiEndNs-genaiStartNs) / 1e6
		}
	}
	if hasHTTPSpan {
		httpStart := time.Unix(0, httpStartNs).UTC()
		httpEnd := time.Unix(0, httpEndNs).UTC()
		if !hasGenAISpan {
			out.StartTime = httpStart
			out.EndTime = httpEnd
		}
		if httpEndNs >= httpStartNs {
			out.HTTPDurationMs = float64(httpEndNs-httpStartNs) / 1e6
		}
		if hasGenAISpan && genaiStartNs >= httpStartNs {
			out.PreInferenceGapMs = float64(genaiStartNs-httpStartNs) / 1e6
		}
	}

	if !hasGenAISpan && !hasHTTPSpan {
		return nil, fmt.Errorf("trace %q contains neither a GenAI span nor an HTTP server span", out.TraceID)
	}
	return out, nil
}

func unwrapOTLPResourceSpans(traceIDHint string, raw []byte) ([]otlpResSpan, string, error) {
	traceID := traceIDHint
	cur := raw
	for depth := 0; depth < 4; depth++ {
		var env otlpEnvelope
		if err := json.Unmarshal(cur, &env); err != nil {
			return nil, traceID, err
		}
		if env.TraceID != "" && traceID == "" {
			traceID = env.TraceID
		}
		if len(env.ResourceSpans) > 0 {
			return env.ResourceSpans, traceID, nil
		}
		if len(env.Batches) > 0 {
			return env.Batches, traceID, nil
		}
		if len(env.Trace) == 0 || string(env.Trace) == "null" {
			break
		}
		cur = env.Trace
	}
	return nil, traceID, nil
}

func isGenAISpan(spanName, scopeName string, attrs map[string]any) bool {
	if _, ok := attrs[attrOperationName]; ok {
		return true
	}
	if _, ok := attrs[attrRequestModel]; ok {
		return true
	}
	if strings.Contains(scopeName, "genai") {
		return true
	}
	return strings.HasPrefix(spanName, "chat ")
}

func isServerHTTPSpan(kind any, attrs map[string]any) bool {
	switch v := kind.(type) {
	case string:
		if v == "SPAN_KIND_SERVER" || v == "SpanKind.SERVER" {
			return true
		}
	case float64:
		if int(v) == 2 { // OTLP SPAN_KIND_SERVER = 2
			return true
		}
	}
	_, hasStatus := attrs[attrHTTPStatusCode]
	_, hasRespCode := attrs[attrHTTPResponseCode]
	return hasStatus || hasRespCode
}

func isErrorStatus(code any) bool {
	switch v := code.(type) {
	case string:
		return strings.Contains(strings.ToUpper(v), "ERROR")
	case float64:
		return int(v) == 2 // OTLP STATUS_CODE_ERROR = 2
	default:
		return false
	}
}

func populateFromGenAIAttrs(out *snapshot.TraceSummary, attrs map[string]any) {
	// Note: attrInputMessages, attrOutputMessages, attrSystemInstructions and
	// attrProviderName are intentionally never copied to out.
	out.Operation = asString(attrs[attrOperationName])
	out.RequestModel = asString(attrs[attrRequestModel])
	out.ResponseModel = asString(attrs[attrResponseModel])
	out.ResponseID = asString(attrs[attrResponseID])
	out.FinishReasons = asStringSlice(attrs[attrFinishReasons])
	out.MaxTokens = asInt(attrs[attrRequestMaxTokens])
	out.InputTokens = asInt(attrs[attrInputTokens])
	out.OutputTokens = asInt(attrs[attrOutputTokens])
	out.CachedInputTokens = asInt(attrs[attrCachedInputTokens])
	out.SystemFingerprint = asString(attrs[attrSystemFingerprint])
	out.ServerAddress = asString(attrs[attrServerAddress])
	out.ServerPort = asInt(attrs[attrServerPort])
	if errType := asString(attrs[attrErrorType]); errType != "" {
		out.ErrorType = errType
		out.Error = true
	}
}

func kvSliceToMap(kvs []otlpKeyValue) map[string]any {
	m := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		if IsSensitiveKey(kv.Key) {
			continue
		}
		m[kv.Key] = extractOTLPValue(kv.Value)
	}
	return m
}

func extractOTLPValue(v otlpValue) any {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		return v.IntValue
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.ArrayValue != nil:
		out := make([]any, 0, len(v.ArrayValue.Values))
		for _, item := range v.ArrayValue.Values {
			out = append(out, extractOTLPValue(item))
		}
		return out
	default:
		return nil
	}
}

func decodeID(s string) string {
	if s == "" {
		return ""
	}
	// Tempo OTLP JSON encodes traceId/spanId in standard base64 if not already hex.
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && (len(b) == 16 || len(b) == 8) {
		return hex.EncodeToString(b)
	}
	return s
}

func parseNano(v any) int64 {
	switch x := v.(type) {
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case float64:
		return int64(x)
	case int64:
		return x
	default:
		return 0
	}
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	default:
		return ""
	}
}

func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	default:
		return 0
	}
}

func asStringSlice(v any) []string {
	switch xs := v.(type) {
	case []string:
		return xs
	case []any:
		out := make([]string, 0, len(xs))
		for _, item := range xs {
			if s := asString(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// --- Compat-matrix fixture parsing (eval/fixtures/genai-instrumentation) -----

// CompatMetricPoint is a normalized metric datapoint from a compat-matrix
// fixture (duration or token usage).
type CompatMetricPoint struct {
	Kind              string // "duration" or "token_usage"
	Operation         string
	RequestModel      string
	ResponseModel     string
	TokenType         string
	SystemFingerprint string
	ServerAddress     string
	ServerPort        int
	ErrorType         string
	Count             int
	Sum               float64
}

type compatPayload struct {
	Spans   []compatSpan   `json:"spans"`
	Metrics []compatMetric `json:"metrics"`
}

type compatSpan struct {
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Scope      string         `json:"scope"`
	Attributes map[string]any `json:"attributes"`
	Resource   map[string]any `json:"resource"`
	Status     struct {
		Code        string  `json:"code"`
		Description *string `json:"description"`
	} `json:"status"`
}

type compatMetric struct {
	Name       string            `json:"name"`
	Unit       string            `json:"unit"`
	Datapoints []compatDatapoint `json:"datapoints"`
}

type compatDatapoint struct {
	Attributes map[string]any `json:"attributes"`
	Count      int            `json:"count"`
	Sum        float64        `json:"sum"`
}

// ParseCompatTelemetry parses a telemetry.json fixture generated by
// `make compat-matrix`, stripping any captured prompt/completion messages.
func ParseCompatTelemetry(raw []byte) ([]snapshot.TraceSummary, []CompatMetricPoint, error) {
	var p compatPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, err
	}

	traces := make([]snapshot.TraceSummary, 0, len(p.Spans))
	for _, sp := range p.Spans {
		cleanAttrs := make(map[string]any, len(sp.Attributes))
		for k, v := range sp.Attributes {
			if IsSensitiveKey(k) {
				continue
			}
			cleanAttrs[k] = v
		}
		var ts snapshot.TraceSummary
		populateFromGenAIAttrs(&ts, cleanAttrs)
		ts.ServiceName = asString(sp.Resource["service.name"])
		if isErrorStatus(sp.Status.Code) || ts.ErrorType != "" {
			ts.Error = true
			if sp.Status.Description != nil {
				ts.StatusMsg = *sp.Status.Description
			}
		}
		traces = append(traces, ts)
	}

	var points []CompatMetricPoint
	for _, m := range p.Metrics {
		var kind string
		switch {
		case IsDurationMetric(m.Name):
			kind = "duration"
		case IsTokenUsageMetric(m.Name):
			kind = "token_usage"
		default:
			continue
		}
		for _, dp := range m.Datapoints {
			points = append(points, CompatMetricPoint{
				Kind:              kind,
				Operation:         asString(dp.Attributes[attrOperationName]),
				RequestModel:      asString(dp.Attributes[attrRequestModel]),
				ResponseModel:     asString(dp.Attributes[attrResponseModel]),
				TokenType:         asString(dp.Attributes[attrTokenType]),
				SystemFingerprint: asString(dp.Attributes[attrSystemFingerprint]),
				ServerAddress:     asString(dp.Attributes[attrServerAddress]),
				ServerPort:        asInt(dp.Attributes[attrServerPort]),
				ErrorType:         asString(dp.Attributes[attrErrorType]),
				Count:             dp.Count,
				Sum:               dp.Sum,
			})
		}
	}

	return traces, points, nil
}
