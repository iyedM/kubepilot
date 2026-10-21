package genai_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iyedM/kubepilot/internal/genai"
)

func TestParseCompatTelemetryCaseC(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "eval", "fixtures", "genai-instrumentation", "C", "telemetry.json"))
	if err != nil {
		t.Fatalf("read C/telemetry.json: %v", err)
	}

	traces, metrics, err := genai.ParseCompatTelemetry(raw)
	if err != nil {
		t.Fatalf("ParseCompatTelemetry: %v", err)
	}
	if len(traces) != 2 {
		t.Fatalf("got %d traces, want 2", len(traces))
	}

	okSpan := traces[0]
	if okSpan.Operation != "chat" {
		t.Errorf("okSpan.Operation = %q, want \"chat\"", okSpan.Operation)
	}
	if okSpan.RequestModel != "qwen2.5-0.5b-instruct" || okSpan.ResponseModel != "qwen2.5-0.5b-instruct" {
		t.Errorf("models = (%q, %q), want qwen2.5-0.5b-instruct", okSpan.RequestModel, okSpan.ResponseModel)
	}
	if okSpan.InputTokens != 47 || okSpan.OutputTokens != 16 || okSpan.CachedInputTokens != 24 {
		t.Errorf("tokens = (in=%d, out=%d, cache=%d), want (47, 16, 24)",
			okSpan.InputTokens, okSpan.OutputTokens, okSpan.CachedInputTokens)
	}
	if okSpan.SystemFingerprint != "b11723-31288c9c6" {
		t.Errorf("SystemFingerprint = %q, want b11723-31288c9c6", okSpan.SystemFingerprint)
	}
	if okSpan.ServerAddress != "kp-compat-llama" || okSpan.ServerPort != 8080 {
		t.Errorf("server = %s:%d, want kp-compat-llama:8080", okSpan.ServerAddress, okSpan.ServerPort)
	}
	if okSpan.Error {
		t.Errorf("okSpan.Error = true, want false")
	}

	errSpan := traces[1]
	if !errSpan.Error {
		t.Errorf("errSpan.Error = false, want true")
	}
	if errSpan.ErrorType != "openai.APIConnectionError" {
		t.Errorf("errSpan.ErrorType = %q, want openai.APIConnectionError", errSpan.ErrorType)
	}
	if errSpan.ServerAddress != "127.0.0.1" || errSpan.ServerPort != 9 {
		t.Errorf("errSpan server = %s:%d, want 127.0.0.1:9", errSpan.ServerAddress, errSpan.ServerPort)
	}
	if errSpan.StatusMsg != "Connection error." {
		t.Errorf("errSpan.StatusMsg = %q, want \"Connection error.\"", errSpan.StatusMsg)
	}

	if len(metrics) != 4 {
		t.Fatalf("got %d metric points, want 4", len(metrics))
	}
	if metrics[0].Kind != "duration" || metrics[0].Count != 1 || metrics[0].ServerAddress != "kp-compat-llama" {
		t.Errorf("unexpected first metric point: %+v", metrics[0])
	}
	if metrics[1].Kind != "duration" || metrics[1].ErrorType != "openai.APIConnectionError" {
		t.Errorf("unexpected second metric point: %+v", metrics[1])
	}
	if metrics[2].Kind != "token_usage" || metrics[2].TokenType != "input" || int(metrics[2].Sum) != 47 {
		t.Errorf("unexpected input token metric point: %+v", metrics[2])
	}
	if metrics[3].Kind != "token_usage" || metrics[3].TokenType != "output" || int(metrics[3].Sum) != 16 {
		t.Errorf("unexpected output token metric point: %+v", metrics[3])
	}
}

func TestParseCompatTelemetryPrivacyCaseCCapture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "eval", "fixtures", "genai-instrumentation", "C-capture", "telemetry.json"))
	if err != nil {
		t.Fatalf("read C-capture/telemetry.json: %v", err)
	}
	// Sanity check: the raw fixture actually contains both the prompt canary and completion text.
	if !strings.Contains(string(raw), "KPCANARY7F3A") || !strings.Contains(string(raw), "Kubernetes is a scalable") {
		t.Fatalf("C-capture fixture missing expected canary or completion text")
	}

	traces, _, err := genai.ParseCompatTelemetry(raw)
	if err != nil {
		t.Fatalf("ParseCompatTelemetry: %v", err)
	}
	if len(traces) != 2 {
		t.Fatalf("got %d traces, want 2", len(traces))
	}

	serialized, err := json.Marshal(traces)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	s := string(serialized)
	for _, forbidden := range []string{
		"KPCANARY7F3A",
		"Kubernetes is a scalable",
		"unreachable",
	} {
		if strings.Contains(s, forbidden) {
			t.Errorf("privacy leak: serialized TraceSummary contains %q: %s", forbidden, s)
		}
	}
}

func TestParseOTLPTraceWithPreInferenceGap(t *testing.T) {
	// Representative Tempo export line with a 2001 ms gap between the HTTP server
	// span start and the GenAI client span start, plus a canary message attribute
	// that must be stripped.
	sample := `{
  "trace_id": "0519a502138b3dfcb2be5ccdc46fb6af",
  "trace": {
    "trace": {
      "resourceSpans": [
        {
          "resource": {
            "attributes": [
              {"key": "k8s.pod.name", "value": {"stringValue": "chat-api-7f9456fbd4-tg7r8"}},
              {"key": "k8s.pod.uid", "value": {"stringValue": "d4a376e6-4ac0-44d9-9ec9-c6668930ade5"}},
              {"key": "k8s.namespace.name", "value": {"stringValue": "genai"}},
              {"key": "k8s.node.name", "value": {"stringValue": "kubepilot-lab-worker"}},
              {"key": "k8s.deployment.name", "value": {"stringValue": "chat-api"}},
              {"key": "service.name", "value": {"stringValue": "chat-api"}}
            ]
          },
          "scopeSpans": [
            {
              "scope": {"name": "opentelemetry.instrumentation.fastapi"},
              "spans": [
                {
                  "traceId": "BRmlAhOLPfyyvlzNxG+2rw==",
                  "spanId": "ZICPqnYr250=",
                  "name": "POST /chat",
                  "kind": "SPAN_KIND_SERVER",
                  "startTimeUnixNano": "1792592067351000000",
                  "endTimeUnixNano": "1792592069808000000",
                  "attributes": [
                    {"key": "http.status_code", "value": {"intValue": "200"}}
                  ],
                  "status": {}
                }
              ]
            },
            {
              "scope": {"name": "opentelemetry.instrumentation.genai.openai", "version": "1.2b0"},
              "spans": [
                {
                  "traceId": "BRmlAhOLPfyyvlzNxG+2rw==",
                  "spanId": "hLmkic0VYZk=",
                  "parentSpanId": "ZICPqnYr250=",
                  "name": "chat qwen2.5-0.5b-instruct",
                  "kind": "SPAN_KIND_CLIENT",
                  "startTimeUnixNano": "1792592069352000000",
                  "endTimeUnixNano": "1792592069807000000",
                  "attributes": [
                    {"key": "gen_ai.operation.name", "value": {"stringValue": "chat"}},
                    {"key": "gen_ai.request.model", "value": {"stringValue": "qwen2.5-0.5b-instruct"}},
                    {"key": "gen_ai.response.model", "value": {"stringValue": "qwen2.5-0.5b-instruct"}},
                    {"key": "gen_ai.usage.input_tokens", "value": {"intValue": "35"}},
                    {"key": "gen_ai.usage.output_tokens", "value": {"intValue": "16"}},
                    {"key": "gen_ai.input.messages", "value": {"stringValue": "SECRET_PROMPT_DO_NOT_COLLECT"}},
                    {"key": "server.address", "value": {"stringValue": "model-server.genai.svc.cluster.local"}},
                    {"key": "server.port", "value": {"intValue": "8080"}}
                  ],
                  "status": {}
                }
              ]
            }
          ]
        }
      ]
    }
  }
}`

	ts, err := genai.ParseOTLPTrace("", []byte(sample))
	if err != nil {
		t.Fatalf("ParseOTLPTrace: %v", err)
	}
	if ts.TraceID != "0519a502138b3dfcb2be5ccdc46fb6af" {
		t.Errorf("TraceID = %q, want 0519a502138b3dfcb2be5ccdc46fb6af", ts.TraceID)
	}
	if ts.ClientPodName != "chat-api-7f9456fbd4-tg7r8" || ts.ClientDeployment != "chat-api" || ts.ClientNamespace != "genai" {
		t.Errorf("unexpected client K8s identity: %+v", ts)
	}
	if ts.HTTPStatusCode != 200 {
		t.Errorf("HTTPStatusCode = %d, want 200", ts.HTTPStatusCode)
	}
	if ts.PreInferenceGapMs < 2000.9 || ts.PreInferenceGapMs > 2001.1 {
		t.Errorf("PreInferenceGapMs = %v, want ~2001", ts.PreInferenceGapMs)
	}
	if ts.InferenceMs < 454.9 || ts.InferenceMs > 455.1 {
		t.Errorf("InferenceMs = %v, want ~455", ts.InferenceMs)
	}
	if ts.HTTPDurationMs < 2456.9 || ts.HTTPDurationMs > 2457.1 {
		t.Errorf("HTTPDurationMs = %v, want ~2457", ts.HTTPDurationMs)
	}

	b, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(b), "SECRET_PROMPT_DO_NOT_COLLECT") {
		t.Errorf("privacy leak in OTLP trace summary: %s", string(b))
	}
}

func TestDurationMetricNameDriftAndPromLabels(t *testing.T) {
	// Must accept both 1.2b0 (operation.duration) and post-2026-10-06 semconv (inference.duration).
	for _, valid := range []string{
		"gen_ai.client.operation.duration",
		"gen_ai.client.inference.duration",
		"gen_ai_client_operation_duration_seconds_bucket",
		"gen_ai_client_operation_duration_seconds_count",
		"gen_ai_client_operation_duration_seconds_sum",
		"gen_ai_client_inference_duration_seconds_bucket",
		"gen_ai_client_inference_duration_seconds_count",
		"gen_ai_client_inference_duration_seconds_sum",
	} {
		if !genai.IsDurationMetric(valid) {
			t.Errorf("IsDurationMetric(%q) = false, want true", valid)
		}
	}
	if genai.IsDurationMetric("http_server_duration_milliseconds_sum") {
		t.Errorf("IsDurationMetric accepted non-GenAI metric")
	}

	for _, validTok := range []string{
		"gen_ai.client.token.usage",
		"gen_ai_client_token_usage_bucket",
		"gen_ai_client_token_usage_count",
		"gen_ai_client_token_usage_sum",
	} {
		if !genai.IsTokenUsageMetric(validTok) {
			t.Errorf("IsTokenUsageMetric(%q) = false, want true", validTok)
		}
	}

	got := genai.ExtractPromLabels(map[string]string{
		"gen_ai_operation_name": "chat",
		"gen_ai_request_model":  "qwen2.5-0.5b-instruct",
		"gen_ai_response_model": "qwen2.5-0.5b-instruct",
		"gen_ai_token_type":     "output",
		"server_address":        "model-server.genai.svc.cluster.local",
		"server_port":           "8080",
		"k8s_pod_name":          "chat-api-abc",
		"k8s_namespace_name":    "genai",
		"k8s_node_name":         "kubepilot-lab-worker",
		"k8s_deployment_name":   "chat-api",
	})
	if got.Operation != "chat" || got.TokenType != "output" || got.PodName != "chat-api-abc" || got.Deployment != "chat-api" {
		t.Errorf("ExtractPromLabels mismatch: %+v", got)
	}
}

// TestNoGenAILiteralsOutsideAdapter enforces CLAUDE.md rule 5: no Go file
// outside internal/genai may hard-code "gen_ai." or "gen_ai_" names.
func TestNoGenAILiteralsOutsideAdapter(t *testing.T) {
	root := filepath.Join("..", "..")
	adapterDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs adapter dir: %v", err)
	}

	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == ".tools" || name == "eval" || name == "lab" {
				return filepath.SkipDir
			}
			absDir, err := filepath.Abs(path)
			if err == nil && absDir == adapterDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fileAST, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(fileAST, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(lit.Value, "gen_ai.") || strings.Contains(lit.Value, "gen_ai_") {
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d: forbidden gen_ai literal %s outside internal/genai (CLAUDE.md rule 5)",
					pos.Filename, pos.Line, lit.Value)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
}
