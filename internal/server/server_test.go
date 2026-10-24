package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iyedM/kubepilot/internal/collector/live"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

type stubCostEstimator struct{}

func (stubCostEstimator) Estimate(_ context.Context, _ *snapshot.Snapshot, rep snapshot.Report) (*CostSummary, error) {
	if rep.Status != snapshot.StatusDiagnosed {
		return nil, nil
	}
	return &CostSummary{
		Currency:           "USD",
		Workload:           "deployment/model-server",
		Namespace:          "genai",
		WindowDurationSec:  180,
		TotalWindowCost:    0.042,
		EstimatedWasteCost: 0.031,
		CPUCost:            0.035,
		RAMCost:            0.007,
		Source:             "opencost-stub",
		Explanation:        "stub waste estimate",
	}, nil
}

func TestHealthAndReadyEndpoints(t *testing.T) {
	srv := New(Config{})
	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned status %d, want 200", path, rec.Code)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal %s: %v", path, err)
		}
		if body["status"] != "ok" || body["mode"] != "read-only" {
			t.Fatalf("unexpected %s body: %+v", path, body)
		}
	}
}

func TestDiagnoseOfflineDevRunAndIncidentsAPI(t *testing.T) {
	srv := New(Config{
		CostEstimator: stubCostEstimator{},
	})

	runDir := filepath.Join("..", "..", "eval", "runs", "dev", "dev--cpu-throttle--limit-300m--r1")
	payload, _ := json.Marshal(DiagnoseRequest{
		RunDir: runDir,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/diagnose", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/diagnose status=%d body=%s", rec.Code, rec.Body.String())
	}

	var inc IncidentRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &inc); err != nil {
		t.Fatalf("unmarshal IncidentRecord: %v", err)
	}
	if inc.Report.Status != snapshot.StatusDiagnosed {
		t.Fatalf("got status %q, want %q", inc.Report.Status, snapshot.StatusDiagnosed)
	}
	if len(inc.Report.TopCauses) == 0 || inc.Report.TopCauses[0].Cause != snapshot.CauseCPUThrottling {
		t.Fatalf("unexpected top cause: %+v", inc.Report.TopCauses)
	}
	if inc.Cost == nil || inc.Cost.Source != "opencost-stub" {
		t.Fatalf("expected attached CostSummary, got %+v", inc.Cost)
	}

	// Query /api/v1/incidents?cause=cpu_throttling
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/incidents?cause=cpu_throttling", nil)
	listRec := httptest.NewRecorder()
	srv.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/incidents status=%d", listRec.Code)
	}
	var listResp struct {
		Count     int              `json:"count"`
		Incidents []IncidentRecord `json:"incidents"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if listResp.Count != 1 || listResp.Incidents[0].ID != inc.ID {
		t.Fatalf("unexpected incidents list: %+v", listResp)
	}

	// Query /api/v1/incidents/{id}
	byIDReq := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/"+inc.ID, nil)
	byIDRec := httptest.NewRecorder()
	srv.ServeHTTP(byIDRec, byIDReq)
	if byIDRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/incidents/%s status=%d", inc.ID, byIDRec.Code)
	}
}

func TestLiveCollectMockAndSeedFromRunsDir(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	srv := New(Config{
		Collect: func(_ context.Context, cfg live.Config) (*snapshot.Snapshot, error) {
			return &snapshot.Snapshot{
				Window: snapshot.Window{
					From:          now.Add(-2 * time.Minute),
					To:            now,
					SkewTolerance: 30 * time.Second,
				},
				TargetTraceID: cfg.TraceID,
				Pods: []snapshot.PodSnapshot{
					{
						Name:      "model-server-abc",
						Namespace: "genai",
						Workload:  "deployment/model-server",
						Phase:     "Pending",
					},
				},
				Events: []snapshot.K8sEvent{
					{
						Timestamp:    now.Add(-30 * time.Second),
						Namespace:    "genai",
						InvolvedKind: "Pod",
						InvolvedName: "model-server-abc",
						Type:         "Warning",
						Reason:       "FailedScheduling",
						Message:      "0/3 nodes are available: 3 Insufficient cpu.",
					},
				},
			}, nil
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/diagnose?trace_id=trace-123", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/diagnose status=%d body=%s", rec.Code, rec.Body.String())
	}
	var inc IncidentRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &inc); err != nil {
		t.Fatalf("unmarshal live incident: %v", err)
	}
	if inc.Report.Status != snapshot.StatusDiagnosed || inc.Report.TopCauses[0].Cause != snapshot.CauseUnschedulable {
		t.Fatalf("expected unschedulable diagnosis, got %+v", inc.Report)
	}

	devDir := filepath.Join("..", "..", "eval", "runs", "dev")
	n, err := srv.SeedFromRunsDir(context.Background(), devDir, 3)
	if err != nil {
		t.Fatalf("SeedFromRunsDir: %v", err)
	}
	if n != 3 {
		t.Fatalf("SeedFromRunsDir loaded %d runs, want 3", n)
	}
}

func TestMetricsPostMortemAndWebhook(t *testing.T) {
	var receivedAlert WebhookAlertPayload
	whServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedAlert)
		w.WriteHeader(http.StatusOK)
	}))
	defer whServer.Close()

	srv := New(Config{
		CostEstimator: stubCostEstimator{},
		WebhookURL:    whServer.URL,
	})

	runDir := filepath.Join("..", "..", "eval", "runs", "dev", "dev--cpu-throttle--limit-300m--r1")
	payload, _ := json.Marshal(DiagnoseRequest{RunDir: runDir})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/diagnose", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnose status=%d", rec.Code)
	}

	var inc IncidentRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &inc); err != nil {
		t.Fatalf("unmarshal incident: %v", err)
	}
	if !strings.Contains(inc.SuggestedPatch, "kind: Deployment") {
		t.Fatalf("expected SuggestedPatch YAML, got %q", inc.SuggestedPatch)
	}
	if receivedAlert.Incident.ID != inc.ID || !strings.Contains(receivedAlert.Text, "cpu_throttling") {
		t.Fatalf("webhook alert not received properly: %+v", receivedAlert)
	}

	// Verify GET /api/v1/incidents/{id}/postmortem
	pmReq := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/"+inc.ID+"/postmortem", nil)
	pmRec := httptest.NewRecorder()
	srv.ServeHTTP(pmRec, pmReq)
	if pmRec.Code != http.StatusOK || !strings.Contains(pmRec.Body.String(), "# KubePilot Incident Post-Mortem Report") {
		t.Fatalf("unexpected postmortem response: code=%d body=%s", pmRec.Code, pmRec.Body.String())
	}

	// Verify GET /metrics
	mReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	mRec := httptest.NewRecorder()
	srv.ServeHTTP(mRec, mReq)
	if mRec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status=%d", mRec.Code)
	}
	mBody := mRec.Body.String()
	for _, want := range []string{
		"kubepilot_build_info{mode=\"read-only\",engine=\"deterministic-go\"} 1",
		"kubepilot_windows_inspected_total{status=\"diagnosed\"} 1",
		"kubepilot_incidents_by_cause_total",
		"kubepilot_incident_waste_dollars_total",
		"kubepilot_webhook_notifications_total{status=\"sent\"} 1",
	} {
		if !strings.Contains(mBody, want) {
			t.Fatalf("GET /metrics missing %q in:\n%s", want, mBody)
		}
	}
}

func TestSSECompareAlertmanagerBundleAndSimulate(t *testing.T) {
	srv := New(Config{
		CostEstimator: stubCostEstimator{},
	})

	// 2A: SSE /api/v1/stream?once=1
	sseReq := httptest.NewRequest(http.MethodGet, "/api/v1/stream?once=1", nil)
	sseRec := httptest.NewRecorder()
	srv.ServeHTTP(sseRec, sseReq)
	if sseRec.Code != http.StatusOK || !strings.Contains(sseRec.Body.String(), "event: connected") {
		t.Fatalf("unexpected SSE response: code=%d body=%s", sseRec.Code, sseRec.Body.String())
	}

	// 2C: Alertmanager webhook auto-triage
	runDir := filepath.Join("..", "..", "eval", "runs", "dev", "dev--cpu-throttle--limit-300m--r1")
	amPayload, _ := json.Marshal(AlertmanagerWebhookPayload{
		Version:  "4",
		Status:   "firing",
		Receiver: "kubepilot",
		Alerts: []AlertmanagerAlert{
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "GenAIHighLatency", "namespace": "genai"},
				Annotations: map[string]string{"run_dir": runDir},
			},
		},
	})
	amReq := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/alertmanager", bytes.NewReader(amPayload))
	amRec := httptest.NewRecorder()
	srv.ServeHTTP(amRec, amReq)
	if amRec.Code != http.StatusOK {
		t.Fatalf("Alertmanager webhook status=%d body=%s", amRec.Code, amRec.Body.String())
	}

	// 2B: Baseline vs Incident Comparison
	cmpReq := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/dev--cpu-throttle--limit-300m--r1/compare", nil)
	cmpRec := httptest.NewRecorder()
	srv.ServeHTTP(cmpRec, cmpReq)
	if cmpRec.Code != http.StatusOK {
		t.Fatalf("GET /compare status=%d body=%s", cmpRec.Code, cmpRec.Body.String())
	}
	var cmp BaselineComparison
	if err := json.Unmarshal(cmpRec.Body.Bytes(), &cmp); err != nil {
		t.Fatalf("unmarshal BaselineComparison: %v", err)
	}
	if cmp.PrimaryCause != "cpu_throttling" || len(cmp.Metrics) == 0 {
		t.Fatalf("unexpected BaselineComparison: %+v", cmp)
	}

	// 2D: Export .tar.gz bundle and re-import into a fresh Server
	bndReq := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/dev--cpu-throttle--limit-300m--r1/bundle", nil)
	bndRec := httptest.NewRecorder()
	srv.ServeHTTP(bndRec, bndReq)
	if bndRec.Code != http.StatusOK || bndRec.Body.Len() < 100 {
		t.Fatalf("GET /bundle status=%d len=%d", bndRec.Code, bndRec.Body.Len())
	}

	srv2 := New(Config{})
	upReq := httptest.NewRequest(http.MethodPost, "/api/v1/incidents/bundle", bytes.NewReader(bndRec.Body.Bytes()))
	upRec := httptest.NewRecorder()
	srv2.ServeHTTP(upRec, upReq)
	if upRec.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/incidents/bundle status=%d body=%s", upRec.Code, upRec.Body.String())
	}
	if _, ok := srv2.GetIncident("dev--cpu-throttle--limit-300m--r1"); !ok {
		t.Fatalf("expected imported incident in srv2 store")
	}

	// 2E: What-If Simulator (1. Honest abstention with noise decoy, 2. CPU Throttling)
	simHealthyBody, _ := json.Marshal(SimulateRequest{
		ScenarioName:    "healthy-with-decoy",
		CFSThrottledPct: 3.0,
		DecoyNoiseOOM:   true,
	})
	simHReq := httptest.NewRequest(http.MethodPost, "/api/v1/simulate", bytes.NewReader(simHealthyBody))
	simHRec := httptest.NewRecorder()
	srv.ServeHTTP(simHRec, simHReq)
	if simHRec.Code != http.StatusOK || !strings.Contains(simHRec.Body.String(), "\"status\": \"unknown\"") {
		t.Fatalf("expected unknown status on healthy simulation, got: %s", simHRec.Body.String())
	}

	simFaultBody, _ := json.Marshal(SimulateRequest{
		ScenarioName:    "throttle-92",
		CFSThrottledPct: 92.0,
		RecordInStore:   true,
	})
	simFReq := httptest.NewRequest(http.MethodPost, "/api/v1/simulate", bytes.NewReader(simFaultBody))
	simFRec := httptest.NewRecorder()
	srv.ServeHTTP(simFRec, simFReq)
	if simFRec.Code != http.StatusOK || !strings.Contains(simFRec.Body.String(), "\"cause\": \"cpu_throttling\"") {
		t.Fatalf("expected cpu_throttling diagnosis on simulated fault, got: %s", simFRec.Body.String())
	}

	// Brick 2: DevSecOps & PSS Posture Audit endpoint (/security)
	secReq := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/dev--cpu-throttle--limit-300m--r1/security", nil)
	secRec := httptest.NewRecorder()
	srv.ServeHTTP(secRec, secReq)
	if secRec.Code != http.StatusOK {
		t.Fatalf("GET /security status=%d body=%s", secRec.Code, secRec.Body.String())
	}
	var sec SecurityAudit
	if err := json.Unmarshal(secRec.Body.Bytes(), &sec); err != nil {
		t.Fatalf("unmarshal SecurityAudit: %v", err)
	}
	if sec.Score < 80 || len(sec.Checks) < 5 {
		t.Fatalf("unexpected SecurityAudit: %+v", sec)
	}

	// Brick 3: Signal Coverage Resilience & Controlled SRE Runbook endpoint (/runbook)
	rbReq := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/dev--cpu-throttle--limit-300m--r1/runbook", nil)
	rbRec := httptest.NewRecorder()
	srv.ServeHTTP(rbRec, rbReq)
	if rbRec.Code != http.StatusOK {
		t.Fatalf("GET /runbook status=%d body=%s", rbRec.Code, rbRec.Body.String())
	}
	var rb RunbookSynthesis
	if err := json.Unmarshal(rbRec.Body.Bytes(), &rb); err != nil {
		t.Fatalf("unmarshal RunbookSynthesis: %v", err)
	}
	if rb.SignalCoveragePct < 80 || len(rb.Signals) != 5 || len(rb.Steps) < 4 {
		t.Fatalf("unexpected RunbookSynthesis: %+v", rb)
	}

	// Controlled Agentic Explanation endpoint (/explain) — deterministic + guardrail-checked mock LLM
	expReq := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/dev--cpu-throttle--limit-300m--r1/explain", nil)
	expRec := httptest.NewRecorder()
	srv.ServeHTTP(expRec, expReq)
	if expRec.Code != http.StatusOK {
		t.Fatalf("GET /explain status=%d body=%s", expRec.Code, expRec.Body.String())
	}
	var exp CopilotExplanation
	if err := json.Unmarshal(expRec.Body.Bytes(), &exp); err != nil {
		t.Fatalf("unmarshal CopilotExplanation: %v", err)
	}
	if exp.Mode != "deterministic-grounded-synthesis" || len(exp.Guardrails) < 5 || !strings.Contains(exp.DeterministicNarrative, "cpu_throttling") {
		t.Fatalf("unexpected CopilotExplanation: %+v", exp)
	}

	// Verify guardrail blocks unsafe LLM command ("kubectl delete") and falls back to deterministic narrative
	unsafeLLMSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "Run kubectl delete pod --force immediately"}},
			},
		})
	}))
	defer unsafeLLMSrv.Close()

	expPostBody, _ := json.Marshal(ExplainRequest{LLMEndpoint: unsafeLLMSrv.URL})
	expPostReq := httptest.NewRequest(http.MethodPost, "/api/v1/incidents/dev--cpu-throttle--limit-300m--r1/explain", bytes.NewReader(expPostBody))
	expPostRec := httptest.NewRecorder()
	srv.ServeHTTP(expPostRec, expPostReq)
	var expBlocked CopilotExplanation
	_ = json.Unmarshal(expPostRec.Body.Bytes(), &expBlocked)
	if !strings.Contains(expBlocked.LLMSafetyStatus, "blocked-by-guardrail") || expBlocked.LLMExplanation != "" {
		t.Fatalf("expected guardrail to block unsafe LLM output, got: %+v", expBlocked)
	}

	// Verify Controlled GitOps PR Synthesis endpoint (/gitops-pr)
	prReq := httptest.NewRequest(http.MethodGet, "/api/v1/incidents/dev--cpu-throttle--limit-300m--r1/gitops-pr", nil)
	prRec := httptest.NewRecorder()
	srv.ServeHTTP(prRec, prReq)
	if prRec.Code != http.StatusOK {
		t.Fatalf("GET /gitops-pr status=%d body=%s", prRec.Code, prRec.Body.String())
	}
	var prPlan GitOpsPRPlan
	if err := json.Unmarshal(prRec.Body.Bytes(), &prPlan); err != nil {
		t.Fatalf("unmarshal GitOpsPRPlan: %v", err)
	}
	if !prPlan.ReadyForPR || prPlan.ClusterWriteAccess || !prPlan.HumanReviewRequired || !strings.Contains(prPlan.KustomizationYAML, "kind: Kustomization") || !strings.Contains(prPlan.PRBodyMarkdown, "PSS-Restricted") {
		t.Fatalf("unexpected GitOpsPRPlan: %+v", prPlan)
	}
}

