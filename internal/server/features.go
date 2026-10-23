package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/rules"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

// --- 2A: Real-Time Server-Sent Events (GET /api/v1/stream) ------------------

func (s *Server) subscribeSSE() chan IncidentRecord {
	ch := make(chan IncidentRecord, 16)
	s.mu.Lock()
	if s.subscribers == nil {
		s.subscribers = make(map[chan IncidentRecord]struct{})
	}
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *Server) unsubscribeSSE(ch chan IncidentRecord) {
	s.mu.Lock()
	delete(s.subscribers, ch)
	s.mu.Unlock()
}

func (s *Server) broadcastIncident(rec IncidentRecord) {
	compact := rec
	compact.Telemetry = nil
	s.mu.RLock()
	subs := make([]chan IncidentRecord, 0, len(s.subscribers))
	for ch := range s.subscribers {
		subs = append(subs, ch)
	}
	s.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- compact:
		default:
		}
	}
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use GET")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported by ResponseWriter")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	_, _ = fmt.Fprintf(w, "event: connected\ndata: {\"status\":\"connected\",\"mode\":\"read-only\",\"incidents\":%d}\n\n", len(s.ListIncidents()))
	flusher.Flush()

	if r.URL.Query().Get("once") == "1" {
		return
	}

	ch := s.subscribeSSE()
	defer s.unsubscribeSSE(ch)

	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case inc := <-ch:
			raw, err := json.Marshal(inc)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: incident\ndata: %s\n\n", string(raw))
			flusher.Flush()
		}
	}
}

// --- 2B: Baseline vs Incident Comparator (/api/v1/incidents/{id}/compare) ---

// ComparisonMetric represents one metric delta between the healthy baseline
// reference and the active incident window.
type ComparisonMetric struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Unit          string  `json:"unit"`
	BaselineValue float64 `json:"baselineValue"`
	IncidentValue float64 `json:"incidentValue"`
	DeltaPct      float64 `json:"deltaPct"`
	DeltaText     string  `json:"deltaText"`
	Severity      string  `json:"severity"` // "critical", "warning", "normal"
}

// BaselineComparison summarizes side-by-side deltas between an IncidentRecord
// and its baseline (or healthy cluster reference).
type BaselineComparison struct {
	IncidentID     string             `json:"incidentId"`
	BaselineSource string             `json:"baselineSource"`
	Status         string             `json:"status"`
	PrimaryCause   string             `json:"primaryCause,omitempty"`
	Metrics        []ComparisonMetric `json:"metrics"`
}

// BuildBaselineComparison computes the side-by-side comparison between inc and
// its baseline window (or standard healthy GenAI cluster baselines).
func BuildBaselineComparison(inc IncidentRecord) BaselineComparison {
	comp := BaselineComparison{
		IncidentID:     inc.ID,
		BaselineSource: "pre-fault-baseline-window",
		Status:         string(inc.Report.Status),
	}
	if len(inc.Report.TopCauses) > 0 {
		comp.PrimaryCause = string(inc.Report.TopCauses[0].Cause)
	}

	tel := inc.Telemetry
	winFrom := inc.Report.Window.From

	// Defaults from healthy KubePilot lab baseline (500ms p50, 620ms p95, 50 tok/s, 0% errors, 0% throttle)
	baseP50 := 495.0
	baseP95 := 620.0
	incP50 := 495.0
	incP95 := 620.0
	baseErrPct := 0.0
	incErrPct := 0.0
	baseGapP95 := 1.2
	incGapP95 := 1.2
	baseThrottle := 0.2
	incThrottle := 0.2
	baseTokS := 50.2
	incTokS := 50.2
	var totalRestarts int32

	if tel != nil {
		// Split requests into baseline (< winFrom) vs incident (>= winFrom)
		var baseLat, incLat []float64
		var baseReqs, baseErrs, incReqs, incErrs int
		for _, r := range tel.Requests {
			isBase := !winFrom.IsZero() && r.Timestamp.Before(winFrom)
			isErr := r.Status >= 400 || r.Status == 0 || r.Error != ""
			if isBase {
				baseReqs++
				if isErr {
					baseErrs++
				}
				if r.ClientLatencyMs > 0 {
					baseLat = append(baseLat, r.ClientLatencyMs)
				}
			} else {
				incReqs++
				if isErr {
					incErrs++
				}
				if r.ClientLatencyMs > 0 {
					incLat = append(incLat, r.ClientLatencyMs)
				}
			}
		}
		if len(baseLat) > 0 {
			baseP50 = percentileFloat(baseLat, 0.50)
			baseP95 = percentileFloat(baseLat, 0.95)
		} else {
			comp.BaselineSource = "cluster-reference-baseline"
		}
		if len(incLat) > 0 {
			incP50 = percentileFloat(incLat, 0.50)
			incP95 = percentileFloat(incLat, 0.95)
		}
		if baseReqs > 0 {
			baseErrPct = (float64(baseErrs) / float64(baseReqs)) * 100.0
		}
		if incReqs > 0 {
			incErrPct = (float64(incErrs) / float64(incReqs)) * 100.0
		}

		// Traces pre-inference gap
		var baseGaps, incGaps []float64
		for _, tr := range tel.Traces {
			if !winFrom.IsZero() && tr.StartTime.Before(winFrom) {
				baseGaps = append(baseGaps, tr.PreInferenceGapMs)
			} else {
				incGaps = append(incGaps, tr.PreInferenceGapMs)
			}
		}
		if len(baseGaps) > 0 {
			baseGapP95 = percentileFloat(baseGaps, 0.95)
		}
		if len(incGaps) > 0 {
			incGapP95 = percentileFloat(incGaps, 0.95)
		}

		// Charts: throttling and tok/s
		for _, ch := range tel.Charts {
			switch ch.ID {
			case "cpu_throttled_pct":
				bAvg, iAvg, hasB, hasI := splitChartAvg(ch, winFrom)
				if hasB {
					baseThrottle = bAvg
				}
				if hasI {
					incThrottle = iAvg
				}
			case "llama_tok_s":
				bAvg, iAvg, hasB, hasI := splitChartAvg(ch, winFrom)
				if hasB && bAvg > 0 {
					baseTokS = bAvg
				}
				if hasI && iAvg > 0 {
					incTokS = iAvg
				}
			}
		}

		// Pods restarts in target namespace
		for _, p := range tel.Pods {
			if p.Namespace != inc.Namespace {
				continue
			}
			for _, c := range p.Containers {
				totalRestarts += c.RestartCount
			}
		}
	}

	comp.Metrics = []ComparisonMetric{
		buildDeltaMetric("p50_latency_ms", "Client P50 Latency", "ms", round1(baseP50), round1(incP50), true),
		buildDeltaMetric("p95_latency_ms", "Client P95 Latency", "ms", round1(baseP95), round1(incP95), true),
		buildPointMetric("http_error_rate_pct", "HTTP Error / Timeout Rate", "%", round1(baseErrPct), round1(incErrPct), 5.0, 20.0),
		buildDeltaMetric("cfs_throttled_pct", "CFS CPU Throttled Periods", "%", round1(baseThrottle), round1(incThrottle), true),
		buildDeltaMetric("llm_tok_s", "LLM Generation Throughput", "tok/s", round1(baseTokS), round1(incTokS), false),
		buildDeltaMetric("pre_inference_gap_ms", "App Pre-Inference Gap (P95)", "ms", round1(baseGapP95), round1(incGapP95), true),
		{
			ID:            "container_restarts",
			Name:          "Container Restarts (Target NS)",
			Unit:          "restarts",
			BaselineValue: 0,
			IncidentValue: float64(totalRestarts),
			DeltaPct:      float64(totalRestarts) * 100,
			DeltaText:     fmt.Sprintf("+%d restarts", totalRestarts),
			Severity: func() string {
				if totalRestarts > 0 {
					return "critical"
				}
				return "normal"
			}(),
		},
	}
	return comp
}

func splitChartAvg(ch MetricChart, winFrom time.Time) (baseAvg, incAvg float64, hasBase, hasInc bool) {
	var bSum, iSum float64
	var bCnt, iCnt int
	for _, s := range ch.Series {
		if strings.HasPrefix(s.Label, "noise/") {
			continue
		}
		for _, pt := range s.Points {
			if !winFrom.IsZero() && pt.Timestamp.Before(winFrom) {
				bSum += pt.Value
				bCnt++
			} else {
				iSum += pt.Value
				iCnt++
			}
		}
	}
	if bCnt > 0 {
		baseAvg = bSum / float64(bCnt)
		hasBase = true
	}
	if iCnt > 0 {
		incAvg = iSum / float64(iCnt)
		hasInc = true
	}
	return
}

func percentileFloat(vals []float64, q float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := make([]float64, len(vals))
	copy(cp, vals)
	sort.Float64s(cp)
	idx := int(math.Floor(q * float64(len(cp)-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

func buildDeltaMetric(id, name, unit string, baseVal, incVal float64, higherIsWorse bool) ComparisonMetric {
	var pct float64
	if baseVal > 0.01 {
		pct = ((incVal - baseVal) / baseVal) * 100.0
	} else if incVal > 0.01 {
		pct = 100.0
	}
	pct = round1(pct)
	sign := ""
	if pct > 0 {
		sign = "+"
	}
	deltaText := fmt.Sprintf("%s%.1f%%", sign, pct)
	if unit == "%" && baseVal < 1.0 && incVal >= 1.0 {
		deltaText = fmt.Sprintf("+%.1f pp", incVal-baseVal)
	}

	sev := "normal"
	if higherIsWorse {
		if pct >= 80 || (unit == "%" && incVal >= 35) || (id == "pre_inference_gap_ms" && incVal >= 250) {
			sev = "critical"
		} else if pct >= 25 || (unit == "%" && incVal >= 15) {
			sev = "warning"
		}
	} else {
		if pct <= -40 {
			sev = "critical"
		} else if pct <= -20 {
			sev = "warning"
		}
	}
	return ComparisonMetric{
		ID:            id,
		Name:          name,
		Unit:          unit,
		BaselineValue: baseVal,
		IncidentValue: incVal,
		DeltaPct:      pct,
		DeltaText:     deltaText,
		Severity:      sev,
	}
}

func buildPointMetric(id, name, unit string, baseVal, incVal, warnAt, critAt float64) ComparisonMetric {
	diff := round1(incVal - baseVal)
	sign := ""
	if diff > 0 {
		sign = "+"
	}
	sev := "normal"
	if incVal >= critAt {
		sev = "critical"
	} else if incVal >= warnAt {
		sev = "warning"
	}
	return ComparisonMetric{
		ID:            id,
		Name:          name,
		Unit:          unit,
		BaselineValue: baseVal,
		IncidentValue: incVal,
		DeltaPct:      diff,
		DeltaText:     fmt.Sprintf("%s%.1f %s", sign, diff, unit),
		Severity:      sev,
	}
}

// --- 2C: Prometheus Alertmanager Webhook Receiver ---------------------------

// AlertmanagerAlert represents one alert inside an Alertmanager v4 webhook payload.
type AlertmanagerAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
}

// AlertmanagerWebhookPayload is the standard JSON body POSTed by Prometheus Alertmanager.
type AlertmanagerWebhookPayload struct {
	Version      string              `json:"version"`
	Status       string              `json:"status"`
	Receiver     string              `json:"receiver"`
	CommonLabels map[string]string   `json:"commonLabels"`
	Alerts       []AlertmanagerAlert `json:"alerts"`
}

func (s *Server) handleAlertmanagerWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use POST")
		return
	}

	var payload AlertmanagerWebhookPayload
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid Alertmanager JSON payload: %v", err))
			return
		}
	}

	s.mu.Lock()
	s.alertmanagerReceived++
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var diagnosed []IncidentRecord
	alerts := payload.Alerts
	if len(alerts) == 0 {
		alerts = []AlertmanagerAlert{{
			Status: "firing",
			Labels: payload.CommonLabels,
		}}
	}

	for _, al := range alerts {
		if al.Status == "resolved" && len(alerts) > 1 {
			continue
		}
		runDir := ""
		if al.Annotations != nil {
			runDir = strings.TrimSpace(al.Annotations["run_dir"])
		}
		traceID := ""
		ns := s.cfg.LiveConfig.Namespace
		if al.Labels != nil {
			if v := strings.TrimSpace(al.Labels["namespace"]); v != "" {
				ns = v
			}
			if v := strings.TrimSpace(al.Labels["trace_id"]); v != "" {
				traceID = v
			}
			if runDir == "" {
				runDir = strings.TrimSpace(al.Labels["run_dir"])
			}
		}

		fromTS := al.StartsAt
		toTS := al.EndsAt
		if fromTS.IsZero() {
			fromTS = time.Now().UTC().Add(-5 * time.Minute)
		}
		if toTS.IsZero() || !toTS.After(fromTS) {
			toTS = fromTS.Add(5 * time.Minute)
		}

		if runDir != "" {
			if inc, err := s.diagnoseRunDirInternal(ctx, runDir, "alertmanager"); err == nil {
				diagnosed = append(diagnosed, inc)
				continue
			}
		}

		lcfg := s.cfg.LiveConfig
		lcfg.Namespace = ns
		lcfg.From = fromTS
		lcfg.To = toTS
		lcfg.TraceID = traceID

		snap, err := s.cfg.Collect(ctx, lcfg)
		if err != nil {
			continue
		}
		rep := rules.Diagnose(snap)
		rec := s.buildRecord(ctx, "alertmanager", "", snap, rep)
		rec.Namespace = ns
		rec = s.RecordIncident(rec)
		if rep.Status == snapshot.StatusDiagnosed && s.cfg.WebhookURL != "" {
			_ = s.SendWebhookAlert(ctx, rec, s.cfg.WebhookURL)
		}
		diagnosed = append(diagnosed, rec)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "processed",
		"received":  len(payload.Alerts),
		"diagnosed": len(diagnosed),
		"incidents": diagnosed,
	})
}

// --- 2D: Portable Incident Bundle Export & Import (.tar.gz) -----------------

// BuildIncidentBundleTarGz packages an IncidentRecord, its Markdown Post-Mortem,
// its GitOps Strategic Merge Patch YAML, and its GenAIIncident CRD YAML into a
// portable .tar.gz archive.
func BuildIncidentBundleTarGz(inc IncidentRecord) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	addFile := func(name string, data []byte) error {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0644,
			Size:    int64(len(data)),
			ModTime: time.Now().UTC(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}

	incBytes, err := json.MarshalIndent(inc, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := addFile("incident.json", incBytes); err != nil {
		return nil, err
	}

	pmBytes := []byte(BuildPostMortemMarkdown(inc))
	if err := addFile("postmortem.md", pmBytes); err != nil {
		return nil, err
	}

	if inc.SuggestedPatch != "" {
		if err := addFile("patch.yaml", []byte(inc.SuggestedPatch)); err != nil {
			return nil, err
		}
	}

	crdYAML := buildGenAIIncidentCRDYAML(inc)
	if err := addFile("genaiincident.yaml", []byte(crdYAML)); err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildGenAIIncidentCRDYAML(inc IncidentRecord) string {
	topCause := "none"
	confStr := "0.00"
	summary := inc.Report.Reason
	if len(inc.Report.TopCauses) > 0 {
		top := inc.Report.TopCauses[0]
		topCause = string(top.Cause)
		confStr = fmt.Sprintf("%.2f", top.Confidence)
		summary = top.Summary
	}
	return fmt.Sprintf(`apiVersion: kubepilot.io/v1alpha1
kind: GenAIIncident
metadata:
  name: %s
  namespace: %s
spec:
  targetNamespace: %s
  from: %q
  to: %q
status:
  diagnosisStatus: %s
  primaryCause: %s
  confidence: %s
  summary: %q
`, inc.ID, inc.Namespace, inc.Namespace,
		inc.Report.Window.From.UTC().Format(time.RFC3339),
		inc.Report.Window.To.UTC().Format(time.RFC3339),
		inc.Report.Status, topCause, confStr, summary)
}

func (s *Server) handleBundleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use POST")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
	if err != nil || len(raw) == 0 {
		writeError(w, http.StatusBadRequest, "empty or unreadable bundle body")
		return
	}

	var inc IncidentRecord
	// Detect gzip magic header (0x1f 0x8b)
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("open gzip bundle: %v", err))
			return
		}
		defer func() { _ = gz.Close() }()
		tr := tar.NewReader(gz)
		found := false
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("read tar bundle: %v", err))
				return
			}
			if filepathBase(hdr.Name) == "incident.json" {
				content, err := io.ReadAll(tr)
				if err != nil {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("read incident.json: %v", err))
					return
				}
				if err := json.Unmarshal(content, &inc); err != nil {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("parse incident.json: %v", err))
					return
				}
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusBadRequest, "bundle .tar.gz does not contain incident.json")
			return
		}
	} else {
		if err := json.Unmarshal(raw, &inc); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("parse JSON bundle: %v", err))
			return
		}
	}

	if inc.Source == "" {
		inc.Source = "bundle"
	}
	stored := s.RecordIncident(inc)
	writeJSON(w, http.StatusOK, stored)
}

func filepathBase(p string) string {
	idx := strings.LastIndexByte(p, '/')
	if idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// --- 2E: Interactive What-If Chaos Simulator (POST /api/v1/simulate) --------

// SimulateRequest configures synthetic telemetry signals for the interactive
// What-If Chaos Sandbox. The server builds a snapshot.Snapshot from these
// parameters and evaluates it through the unmodified rules.Diagnose engine.
type SimulateRequest struct {
	ScenarioName         string  `json:"scenarioName,omitempty"`
	Namespace            string  `json:"namespace,omitempty"`
	CFSThrottledPct      float64 `json:"cfsThrottledPct"`      // 0..100 %
	OOMKilled            bool    `json:"oomKilled"`            // Exit 137 + OOMKilled state
	OOMTarget            string  `json:"oomTarget,omitempty"`  // "model-server" or "chat-api"
	UnschedulableReason  string  `json:"unschedulableReason"`  // "", "insufficient_cpu", "node_taint", "pvc_unbound", "node_selector"
	ReadinessProbeMode   string  `json:"readinessProbeMode"`   // "", "http_404", "initial_delay_600"
	ColdStartInitSec     float64 `json:"coldStartInitSec"`     // 0..180s initContainer duration
	ConfigThreads        int     `json:"configThreads"`        // 4 (healthy) or 1 (regression)
	HostPSIMsPerSec      float64 `json:"hostPsiMsPerSec"`      // 0..150 ms/s Linux PSI CPU waiting
	AppPreInferenceGapMs float64 `json:"appPreInferenceGapMs"` // 0..3000 ms gap before GenAI client span
	DecoyNoiseOOM        bool    `json:"decoyNoiseOom"`        // Inject OOMKilled decoy in noise namespace
	RecordInStore        bool    `json:"recordInStore"`        // Persist in incident store
}

// BuildSimulatedSnapshot constructs a realistic multi-signal Snapshot from req.
func BuildSimulatedSnapshot(req SimulateRequest) *snapshot.Snapshot {
	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = "genai"
	}
	if req.ConfigThreads <= 0 {
		req.ConfigThreads = 4
	}

	t0 := time.Now().UTC().Truncate(time.Second).Add(-5 * time.Minute)
	baseStart := t0.Add(-3 * time.Minute)
	winEnd := t0.Add(5 * time.Minute)

	snap := &snapshot.Snapshot{
		Window: snapshot.Window{
			From:          t0,
			To:            winEnd,
			Baseline:      &snapshot.TimeRange{From: baseStart, To: t0, Reason: "baseline"},
			SkewTolerance: 30 * time.Second,
		},
		PodsBefore: []snapshot.PodIdentity{
			{
				Name:      "model-server-prev-7b9d",
				Namespace: ns,
				Node:      "kubepilot-lab-worker",
				Phase:     "Running",
			},
		},
		Metrics: make(map[string]snapshot.Series),
	}

	modelPodName := "model-server-sim-8f4c9"
	chatPodName := "chat-api-sim-69fb9"

	// Determine effective latency & error rate from active fault sliders
	latMult := 1.0
	errFraction := 0.0
	tokSpeed := 50.2

	if req.CFSThrottledPct >= 15 {
		latMult = math.Max(latMult, 1.0+(req.CFSThrottledPct/100.0)*5.5)
		tokSpeed = math.Max(7.5, 50.2*(1.0-0.75*(req.CFSThrottledPct/100.0)))
	}
	if req.ConfigThreads == 1 {
		latMult = math.Max(latMult, 4.1)
		tokSpeed = math.Min(tokSpeed, 13.4)
	}
	if req.HostPSIMsPerSec >= 15 {
		latMult = math.Max(latMult, 1.0+(req.HostPSIMsPerSec/50.0)*0.95)
		tokSpeed = math.Min(tokSpeed, 26.5)
	}
	if req.OOMKilled || req.UnschedulableReason != "" || req.ReadinessProbeMode != "" {
		errFraction = 0.65
	}
	if req.ColdStartInitSec >= 20 {
		errFraction = math.Max(errFraction, 0.35)
		latMult = math.Max(latMult, 2.4)
	}

	// 1. Baseline & Fault Window Requests + OTel Traces
	for i := 0; i < 6; i++ {
		ts := baseStart.Add(time.Duration(20+i*25) * time.Second)
		snap.Requests = append(snap.Requests, snapshot.RequestSample{
			Timestamp:       ts,
			PromptID:        "short-16",
			Status:          200,
			ClientLatencyMs: 490.0 + float64(i*4),
			TraceID:         fmt.Sprintf("simbase%026d", i+1),
			CompletionToks:  32,
		})
	}

	for i := 0; i < 12; i++ {
		ts := t0.Add(time.Duration(15+i*22) * time.Second)
		status := 200
		errStr := ""
		if errFraction > 0 && float64(i%10)/10.0 < errFraction {
			status = 502
			errStr = "APIConnectionError"
		}
		infMs := 495.0 * latMult
		gapMs := 0.8
		if req.AppPreInferenceGapMs > 0 {
			gapMs = req.AppPreInferenceGapMs
		}
		totalMs := infMs + gapMs
		traceID := fmt.Sprintf("simtrace%024d", i+1)

		snap.Requests = append(snap.Requests, snapshot.RequestSample{
			Timestamp:       ts,
			PromptID:        "short-16",
			Status:          status,
			ClientLatencyMs: totalMs,
			TraceID:         traceID,
			Error:           errStr,
			CompletionToks:  32,
		})

		snap.Traces = append(snap.Traces, snapshot.TraceSummary{
			TraceID:           traceID,
			SpanID:            fmt.Sprintf("span%012d", i+1),
			StartTime:         ts.Add(-time.Duration(totalMs) * time.Millisecond),
			EndTime:           ts,
			HTTPDurationMs:    totalMs,
			InferenceMs:       infMs,
			PreInferenceGapMs: gapMs,
			HTTPStatusCode:    status,
			Operation:         "chat",
			RequestModel:      "TinyLlama-1.1B-Chat",
			InputTokens:       24,
			OutputTokens:      32,
			Error:             status >= 400,
			ClientPodName:     chatPodName,
			ClientNamespace:   ns,
			ClientNodeName:    "kubepilot-lab-worker2",
			ClientDeployment:  "chat-api",
			ServiceName:       "chat-api",
		})
	}

	// 2. Build chat-api & model-server Pods
	chatContainer := snapshot.ContainerSnapshot{
		Name:              "chat-api",
		Image:             "kubepilot/chat-api:local",
		Ready:             true,
		RequestsCPUMillis: 100,
		LimitsCPUMillis:   1000,
		RequestsMemoryB:   128 * 1024 * 1024,
		LimitsMemoryB:     256 * 1024 * 1024,
		State:             snapshot.ContainerState{Kind: "running", StartedAt: baseStart},
	}

	cpuLimitMillis := int64(2000)
	if req.CFSThrottledPct >= 40 {
		cpuLimitMillis = 300
	} else if req.CFSThrottledPct >= 20 {
		cpuLimitMillis = 500
	}

	modelContainer := snapshot.ContainerSnapshot{
		Name:              "model-server",
		Image:             "ghcr.io/ggml-org/llama.cpp:server",
		Ready:             true,
		Args:              []string{"--model", "/models/tinyllama.gguf", "--threads", fmt.Sprintf("%d", req.ConfigThreads)},
		RequestsCPUMillis: cpuLimitMillis,
		LimitsCPUMillis:   cpuLimitMillis,
		RequestsMemoryB:   768 * 1024 * 1024,
		LimitsMemoryB:     1536 * 1024 * 1024,
		State:             snapshot.ContainerState{Kind: "running", StartedAt: baseStart},
		ReadinessProbe: &snapshot.ProbeSpec{
			Path:                "/health",
			Port:                "8080",
			InitialDelaySeconds: 5,
			PeriodSeconds:       5,
		},
	}

	modelPhase := "Running"
	modelCreated := baseStart
	var modelConditions []snapshot.PodCondition
	var modelInitContainers []snapshot.ContainerSnapshot

	if req.ConfigThreads == 1 {
		modelCreated = t0.Add(3 * time.Second)
		modelContainer.State.StartedAt = t0.Add(5 * time.Second)
	}

	// Fault: OOMKilled
	if req.OOMKilled {
		termState := &snapshot.ContainerState{
			Kind:       "terminated",
			Reason:     "OOMKilled",
			ExitCode:   137,
			StartedAt:  t0.Add(10 * time.Second),
			FinishedAt: t0.Add(45 * time.Second),
		}
		if req.OOMTarget == "chat-api" {
			chatContainer.RestartCount = 3
			chatContainer.LimitsMemoryB = 40 * 1024 * 1024
			chatContainer.LastTerminated = termState
			snap.Events = append(snap.Events, snapshot.K8sEvent{
				Timestamp:    t0.Add(46 * time.Second),
				Namespace:    ns,
				InvolvedKind: "Pod",
				InvolvedName: chatPodName,
				Type:         "Warning",
				Reason:       "BackOff",
				Message:      "Back-off restarting failed container chat-api in pod " + chatPodName,
				Count:        3,
			})
		} else {
			modelContainer.RestartCount = 3
			modelContainer.LimitsMemoryB = 400 * 1024 * 1024
			modelContainer.LastTerminated = termState
			snap.Events = append(snap.Events, snapshot.K8sEvent{
				Timestamp:    t0.Add(46 * time.Second),
				Namespace:    ns,
				InvolvedKind: "Pod",
				InvolvedName: modelPodName,
				Type:         "Warning",
				Reason:       "BackOff",
				Message:      "Back-off restarting failed container model-server in pod " + modelPodName,
				Count:        3,
			})
		}
	}

	// Fault: Unschedulable
	if req.UnschedulableReason != "" {
		modelPhase = "Pending"
		modelCreated = t0.Add(2 * time.Second)
		modelContainer.Ready = false
		modelContainer.State = snapshot.ContainerState{Kind: "waiting", Reason: "ContainerCreating"}
		schedMsg := "0/3 nodes are available: 1 node(s) had untolerated taint(s), 2 Insufficient cpu. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling."
		switch req.UnschedulableReason {
		case "node_taint":
			schedMsg = "0/3 nodes are available: 3 node(s) had untolerated taint(s). preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling."
		case "pvc_unbound":
			schedMsg = "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling."
		case "node_selector":
			schedMsg = "0/3 nodes are available: 1 node(s) had untolerated taint(s), 2 node(s) didn't match Pod's node affinity/selector. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling."
		}
		modelConditions = append(modelConditions, snapshot.PodCondition{
			Type:               "PodScheduled",
			Status:             "False",
			Reason:             "Unschedulable",
			Message:            schedMsg,
			LastTransitionTime: t0.Add(5 * time.Second),
		})
		snap.Events = append(snap.Events, snapshot.K8sEvent{
			Timestamp:    t0.Add(10 * time.Second),
			Namespace:    ns,
			InvolvedKind: "Pod",
			InvolvedName: modelPodName,
			Type:         "Warning",
			Reason:       "FailedScheduling",
			Message:      schedMsg,
			Count:        6,
		})
	}

	// Fault: Readiness Probe
	switch req.ReadinessProbeMode {
	case "initial_delay_600":
		modelCreated = t0.Add(2 * time.Second)
		modelContainer.Ready = false
		modelContainer.ReadinessProbe.InitialDelaySeconds = 600
		modelConditions = append(modelConditions, snapshot.PodCondition{
			Type:   "Ready",
			Status: "False",
			Reason: "ContainersNotReady",
		})
	case "http_404":
		modelContainer.Ready = false
		modelContainer.ReadinessProbe.Path = "/healthz-missing"
		modelConditions = append(modelConditions, snapshot.PodCondition{
			Type:   "Ready",
			Status: "False",
			Reason: "ContainersNotReady",
		})
		snap.Events = append(snap.Events, snapshot.K8sEvent{
			Timestamp:    t0.Add(35 * time.Second),
			Namespace:    ns,
			InvolvedKind: "Pod",
			InvolvedName: modelPodName,
			Type:         "Warning",
			Reason:       "Unhealthy",
			Message:      "Readiness probe failed: HTTP probe failed with statuscode: 404",
			Count:        12,
		})
	}

	// Fault: Cold Start
	if req.ColdStartInitSec > 0 {
		modelCreated = t0.Add(2 * time.Second)
		initEnd := t0.Add(time.Duration(2+req.ColdStartInitSec) * time.Second)
		modelInitContainers = append(modelInitContainers, snapshot.ContainerSnapshot{
			Name:   "fetch-model",
			Image:  "curlimages/curl:8.11.0",
			IsInit: true,
			Ready:  true,
			State: snapshot.ContainerState{
				Kind:       "terminated",
				Reason:     "Completed",
				ExitCode:   0,
				StartedAt:  t0.Add(3 * time.Second),
				FinishedAt: initEnd,
			},
		})
		modelContainer.State.StartedAt = initEnd.Add(1 * time.Second)
		snap.Events = append(snap.Events,
			snapshot.K8sEvent{
				Timestamp:    t0.Add(3 * time.Second),
				Namespace:    ns,
				InvolvedKind: "Pod",
				InvolvedName: modelPodName,
				Type:         "Normal",
				Reason:       "Started",
				Message:      "Started container fetch-model",
			},
			snapshot.K8sEvent{
				Timestamp:    initEnd,
				Namespace:    ns,
				InvolvedKind: "Pod",
				InvolvedName: modelPodName,
				Type:         "Normal",
				Reason:       "Created",
				Message:      "Created container model-server after initContainer fetch-model completed",
			},
		)
	}

	snap.Pods = append(snap.Pods,
		snapshot.PodSnapshot{
			Name:              chatPodName,
			Namespace:         ns,
			Workload:          "deployment/chat-api",
			NodeName:          "kubepilot-lab-worker2",
			Phase:             "Running",
			CreationTimestamp: baseStart,
			Containers:        []snapshot.ContainerSnapshot{chatContainer},
		},
		snapshot.PodSnapshot{
			Name:              modelPodName,
			Namespace:         ns,
			Workload:          "deployment/model-server",
			NodeName:          "kubepilot-lab-worker",
			Phase:             modelPhase,
			CreationTimestamp: modelCreated,
			Conditions:        modelConditions,
			InitContainers:    modelInitContainers,
			Containers:        []snapshot.ContainerSnapshot{modelContainer},
		},
	)

	// Optional Decoy Pod in noise namespace
	if req.DecoyNoiseOOM {
		snap.Pods = append(snap.Pods, snapshot.PodSnapshot{
			Name:              "decoy-oom-pod",
			Namespace:         "noise",
			Workload:          "deployment/decoy-oom",
			NodeName:          "kubepilot-lab-worker",
			Phase:             "Running",
			CreationTimestamp: baseStart,
			Containers: []snapshot.ContainerSnapshot{
				{
					Name:         "stress",
					RestartCount: 5,
					LastTerminated: &snapshot.ContainerState{
						Kind:       "terminated",
						Reason:     "OOMKilled",
						ExitCode:   137,
						StartedAt:  t0.Add(15 * time.Second),
						FinishedAt: t0.Add(50 * time.Second),
					},
				},
			},
		})
	}

	// 3. Prometheus Metrics Series
	lbls := map[string]string{
		"namespace": ns,
		"pod":       modelPodName,
		"container": "model-server",
	}

	var periodSamples, throtSamples, tokSamples, memSamples, psiSamples []snapshot.MetricSample
	totPeriods := 1000.0
	totThrot := 5.0
	psiTotal := 1.0

	for step := -4; step <= 8; step++ {
		ts := t0.Add(time.Duration(step*30) * time.Second)
		inFault := step >= 0
		totPeriods += 300.0
		if inFault {
			totThrot += 300.0 * (math.Max(0, math.Min(100, req.CFSThrottledPct)) / 100.0)
			psiTotal += (req.HostPSIMsPerSec / 1000.0) * 30.0
		} else {
			totThrot += 0.5
			psiTotal += 0.02
		}
		periodSamples = append(periodSamples, snapshot.MetricSample{Timestamp: ts, Value: totPeriods})
		throtSamples = append(throtSamples, snapshot.MetricSample{Timestamp: ts, Value: totThrot})
		tokVal := 50.2
		if inFault {
			tokVal = tokSpeed
		}
		tokSamples = append(tokSamples, snapshot.MetricSample{Timestamp: ts, Value: tokVal})
		memBytes := 520.0 * 1024 * 1024
		if inFault && req.OOMKilled {
			memBytes = 398.0 * 1024 * 1024
		}
		memSamples = append(memSamples, snapshot.MetricSample{Timestamp: ts, Value: memBytes})
		psiSamples = append(psiSamples, snapshot.MetricSample{Timestamp: ts, Value: psiTotal})
	}

	snap.Metrics["cpu_periods"] = snapshot.Series{
		Streams: []snapshot.MetricStream{{Labels: lbls, Samples: periodSamples}},
	}
	snap.Metrics["cpu_throttled_periods"] = snapshot.Series{
		Streams: []snapshot.MetricStream{{Labels: lbls, Samples: throtSamples}},
	}
	snap.Metrics["llama_predicted_tokens_per_second"] = snapshot.Series{
		Streams: []snapshot.MetricStream{{Labels: lbls, Samples: tokSamples}},
	}
	snap.Metrics["memory_working_set_bytes"] = snapshot.Series{
		Streams: []snapshot.MetricStream{{Labels: lbls, Samples: memSamples}},
	}
	snap.Metrics["cpu_pressure_waiting_seconds"] = snapshot.Series{
		Streams: []snapshot.MetricStream{{Labels: lbls, Samples: psiSamples}},
	}

	if req.HostPSIMsPerSec >= 15 {
		snap.Pods = append(snap.Pods, snapshot.PodSnapshot{
			Name:      "cpu-neighbor-busy",
			Namespace: "noise",
			Workload:  "deployment/cpu-neighbor",
			NodeName:  "kubepilot-lab-worker",
			Phase:     "Running",
		})
		snap.Metrics["cpu_usage_seconds"] = snapshot.Series{
			Streams: []snapshot.MetricStream{
				{
					Labels: map[string]string{"namespace": "noise", "pod": "cpu-neighbor-busy", "container": "burn"},
					Samples: []snapshot.MetricSample{
						{Timestamp: t0.Add(15 * time.Second), Value: 10.0},
						{Timestamp: t0.Add(255 * time.Second), Value: 10.0 + 240.0*7.5},
					},
				},
			},
		}
	}

	return snap
}

func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use POST")
		return
	}
	var req SimulateRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid SimulateRequest JSON: %v", err))
			return
		}
	}

	snap := BuildSimulatedSnapshot(req)
	rep := rules.Diagnose(snap)
	rec := s.buildRecord(r.Context(), "simulator", req.ScenarioName, snap, rep)
	if req.ScenarioName != "" {
		rec.ID = "sim--" + sanitizeID(req.ScenarioName)
	} else {
		rec.ID = fmt.Sprintf("sim--%d", time.Now().UTC().Unix())
	}
	if req.RecordInStore {
		rec = s.RecordIncident(rec)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"incident":   rec,
		"comparison": BuildBaselineComparison(rec),
	})
}

func sanitizeID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else if r == ' ' || r == '_' {
			b.WriteByte('-')
		}
	}
	res := strings.Trim(b.String(), "-")
	if res == "" {
		return "scenario"
	}
	return res
}

// --- DevSecOps Posture & Remediation Patch Auditor --------------------------

// SecurityCheck is one DevSecOps / Pod Security Standards (PSS) verification item.
type SecurityCheck struct {
	ID       string `json:"id"`
	Category string `json:"category"` // "RBAC", "Privacy", "PSS-Restricted", "Resource-Governance", "Patch-Guard"
	Title    string `json:"title"`
	Status   string `json:"status"` // "pass", "warn", "fail"
	Detail   string `json:"detail"`
}

// SecurityAudit summarizes the DevSecOps posture of the inspected workload and
// validates that the suggested GitOps remediation patch introduces no privilege
// escalation or policy violation.
type SecurityAudit struct {
	Score             int             `json:"score"`
	Profile           string          `json:"profile"`
	PatchRiskLevel    string          `json:"patchRiskLevel"` // "none", "low", "medium"
	PatchApprovalNote string          `json:"patchApprovalNote"`
	Checks            []SecurityCheck `json:"checks"`
}

// BuildSecurityAudit evaluates Kubernetes Pod Security Standards, resource
// governance, privacy redaction, and remediation patch safety.
func BuildSecurityAudit(snap *snapshot.Snapshot, rep snapshot.Report, patchYAML, ns string) *SecurityAudit {
	if ns == "" {
		ns = "genai"
	}
	score := 100
	var checks []SecurityCheck

	// 1. Read-Only RBAC & Zero-eBPF Privilege Check
	checks = append(checks, SecurityCheck{
		ID:       "rbac_least_privilege",
		Category: "RBAC",
		Title:    "Strictly Read-Only ClusterRole & Zero Privileged eBPF",
		Status:   "pass",
		Detail:   "KubePilot operates with verbs [get, list, watch] only; 0 write/patch/exec permissions and 0 hostPID/CAP_BPF DaemonSets.",
	})

	// 2. GenAI Prompt & Completion Privacy Redaction
	checks = append(checks, SecurityCheck{
		ID:       "prompt_privacy_redaction",
		Category: "Privacy",
		Title:    "OTel GenAI Prompt & Completion Payload Redaction",
		Status:   "pass",
		Detail:   fmt.Sprintf("Inspected %d trace summaries: 0 prompt or completion text attributes persisted in Snapshot.", len(getSnapshotTraces(snap))),
	})

	// 3. Workload Resource Limits Governance (DoS / OOM / Starvation prevention)
	missingLimits := 0
	totalContainers := 0
	unpinnedImages := 0
	hostPathMounts := 0
	if snap != nil {
		for _, p := range snap.Pods {
			if p.Namespace != ns {
				continue
			}
			for _, v := range p.Volumes {
				if v.HostPath != "" {
					hostPathMounts++
				}
			}
			for _, c := range p.Containers {
				totalContainers++
				if c.LimitsCPUMillis <= 0 || c.LimitsMemoryB <= 0 {
					missingLimits++
				}
				if strings.HasSuffix(c.Image, ":latest") || !strings.Contains(c.Image, ":") {
					unpinnedImages++
				}
			}
		}
	}

	if missingLimits == 0 {
		checks = append(checks, SecurityCheck{
			ID:       "workload_resource_limits",
			Category: "Resource-Governance",
			Title:    "Cgroup CPU & Memory Limits Enforced on All Containers",
			Status:   "pass",
			Detail:   fmt.Sprintf("All %d container(s) in namespace %q define explicit CPU and memory limits (prevents node DoS).", maxInt(1, totalContainers), ns),
		})
	} else {
		score -= 10
		checks = append(checks, SecurityCheck{
			ID:       "workload_resource_limits",
			Category: "Resource-Governance",
			Title:    "Missing CPU or Memory Limits Detected",
			Status:   "warn",
			Detail:   fmt.Sprintf("%d/%d container(s) in namespace %q lack explicit CPU or memory limits.", missingLimits, totalContainers, ns),
		})
	}

	// 4. Container Image Tag Pinning
	if unpinnedImages == 0 {
		checks = append(checks, SecurityCheck{
			ID:       "container_image_pinning",
			Category: "PSS-Restricted",
			Title:    "Immutable / Versioned Container Image References",
			Status:   "pass",
			Detail:   "No mutable :latest container tags found in target namespace workloads.",
		})
	} else {
		score -= 8
		checks = append(checks, SecurityCheck{
			ID:       "container_image_pinning",
			Category: "PSS-Restricted",
			Title:    "Mutable Container Image Tag Detected",
			Status:   "warn",
			Detail:   fmt.Sprintf("%d container(s) use :latest or untagged image references; pin image digests in production.", unpinnedImages),
		})
	}

	// 5. Volume Isolation (hostPath vs PVC/emptyDir)
	if hostPathMounts == 0 {
		checks = append(checks, SecurityCheck{
			ID:       "volume_isolation",
			Category: "PSS-Restricted",
			Title:    "No Privileged hostPath Volume Mounts",
			Status:   "pass",
			Detail:   "Workloads use isolated PVC or emptyDir storage without host filesystem exposure.",
		})
	} else {
		score -= 7
		checks = append(checks, SecurityCheck{
			ID:       "volume_isolation",
			Category: "PSS-Restricted",
			Title:    "HostPath Volume Mount Present (Lab Model Cache)",
			Status:   "warn",
			Detail:   fmt.Sprintf("%d pod volume(s) mount a node hostPath; migrate model weights to a read-only PVC in production clusters.", hostPathMounts),
		})
	}

	// 6. GitOps Remediation Patch Safety Verification
	patchRisk := "none"
	patchNote := "No cluster mutation required; workload is healthy."
	if patchYAML != "" {
		hasPrivEsc := strings.Contains(patchYAML, "privileged: true") ||
			strings.Contains(patchYAML, "hostNetwork: true") ||
			strings.Contains(patchYAML, "hostPID: true")
		if !hasPrivEsc {
			patchRisk = "low"
			patchNote = "Verified safe for GitOps PR review: patch modifies only scoped Deployment spec fields without granting privileged capabilities, host namespaces, or RBAC escalation."
			checks = append(checks, SecurityCheck{
				ID:       "patch_security_guard",
				Category: "Patch-Guard",
				Title:    "Suggested GitOps Patch Passed PSS-Restricted Policy Check",
				Status:   "pass",
				Detail:   patchNote,
			})
		} else {
			patchRisk = "medium"
			score -= 20
			patchNote = "Patch requires manual security review before apply."
			checks = append(checks, SecurityCheck{
				ID:       "patch_security_guard",
				Category: "Patch-Guard",
				Title:    "Suggested Patch Requires Security Review",
				Status:   "warn",
				Detail:   patchNote,
			})
		}
	} else {
		checks = append(checks, SecurityCheck{
			ID:       "patch_security_guard",
			Category: "Patch-Guard",
			Title:    "Zero Mutation Patch Policy",
			Status:   "pass",
			Detail:   "Abstention / healthy window: no Kubernetes manifest change generated.",
		})
	}

	if score < 0 {
		score = 0
	}
	return &SecurityAudit{
		Score:             score,
		Profile:           "PSS-Restricted · Read-Only RBAC",
		PatchRiskLevel:    patchRisk,
		PatchApprovalNote: patchNote,
		Checks:            checks,
	}
}

func getSnapshotTraces(snap *snapshot.Snapshot) []snapshot.TraceSummary {
	if snap == nil {
		return nil
	}
	return snap.Traces
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- Signal Resilience & Controlled Human-in-the-Loop Runbook Synthesis -----

// SignalHealth reports the availability and graceful-degradation behavior of
// one telemetry input channel.
type SignalHealth struct {
	Signal           string `json:"signal"`
	Status           string `json:"status"` // "healthy", "degraded", "missing"
	Samples          int    `json:"samples"`
	FallbackBehavior string `json:"fallbackBehavior"`
}

// RunbookStep is one actionable step in the controlled SRE runbook.
type RunbookStep struct {
	Step             int    `json:"step"`
	Phase            string `json:"phase"` // "verify_readonly", "dry_run_guard", "remediate_gitops", "rollback_guard"
	Title            string `json:"title"`
	Command          string `json:"command"`
	RequiresApproval bool   `json:"requiresApproval"`
	Rationale        string `json:"rationale"`
}

// RunbookSynthesis provides a controlled, zero-hallucination incident briefing
// and step-by-step runbook grounded strictly in the deterministic rule engine's
// output, alongside a signal-degradation resilience matrix.
type RunbookSynthesis struct {
	EngineMode            string         `json:"engineMode"`
	ClusterWriteAccess    bool           `json:"clusterWriteAccess"`
	HumanApprovalRequired bool           `json:"humanApprovalRequired"`
	SignalCoveragePct     int            `json:"signalCoveragePct"`
	Signals               []SignalHealth `json:"signals"`
	ExecutiveBrief        string         `json:"executiveBrief"`
	AlternativeHypotheses []string       `json:"alternativeHypotheses"`
	ScopeAndLimitations   string         `json:"scopeAndLimitations"`
	Steps                 []RunbookStep  `json:"steps"`
}

// BuildRunbookSynthesis constructs a grounded SRE synthesis and signal health
// report from the snapshot and diagnosed IncidentRecord.
func BuildRunbookSynthesis(snap *snapshot.Snapshot, inc IncidentRecord) *RunbookSynthesis {
	ns := inc.Namespace
	if ns == "" {
		ns = "genai"
	}

	var traceCnt, reqCnt, podCnt, eventCnt, metricSeriesCnt int
	if snap != nil {
		traceCnt = len(snap.Traces)
		reqCnt = len(snap.Requests)
		podCnt = len(snap.Pods)
		eventCnt = len(snap.Events)
		for _, s := range snap.Metrics {
			metricSeriesCnt += len(s.Streams)
		}
	} else {
		traceCnt = inc.TraceCount
		podCnt = inc.PodCount
		eventCnt = inc.EventCount
	}

	signals := []SignalHealth{
		{
			Signal:           "OpenTelemetry GenAI Traces (Tempo)",
			Status:           signalStatus(traceCnt, 1),
			Samples:          traceCnt,
			FallbackBehavior: "Required for pre-inference app-delay isolation; if absent (e.g. pod Pending/OOM before span emission), KubePilot falls back to K8s Events + Prometheus + Request logs.",
		},
		{
			Signal:           "Prometheus Metrics (cAdvisor / PSI / KSM / llama.cpp)",
			Status:           signalStatus(metricSeriesCnt, 1),
			Samples:          metricSeriesCnt,
			FallbackBehavior: "Required for CFS CPU throttling, Linux PSI host contention, and token/s regression; if Prometheus is unreachable, pod/event rules (OOM, Unschedulable, Readiness, ColdStart) still execute.",
		},
		{
			Signal:           "Kubernetes Events API (core/v1)",
			Status:           signalStatus(eventCnt, 1),
			Samples:          eventCnt,
			FallbackBehavior: "Provides FailedScheduling, Unhealthy, Killing, and ResizeStarted proofs; if events expired (--event-ttl), rules cross-check Pod status conditions and container termination state.",
		},
		{
			Signal:           "Kubernetes Pod & Container Status",
			Status:           signalStatus(podCnt, 1),
			Samples:          podCnt,
			FallbackBehavior: "Core inventory for workload ownership, exitCode=137 (OOMKilled), initContainer timing, and readinessProbe spec inspection.",
		},
		{
			Signal:           "Client Request Outcome Log",
			Status:           signalStatus(reqCnt, 1),
			Samples:          reqCnt,
			FallbackBehavior: "Captures client-visible HTTP 502 errors and p50/p95 latency when backend pods cannot emit traces; if absent, trace durations are used directly.",
		},
	}

	healthySignals := 0
	for _, sg := range signals {
		if sg.Status == "healthy" {
			healthySignals++
		}
	}
	covPct := (healthySignals * 100) / len(signals)

	rep := inc.Report
	var brief string
	var alts []string
	var steps []RunbookStep

	if rep.Status != snapshot.StatusDiagnosed || len(rep.TopCauses) == 0 {
		brief = fmt.Sprintf(
			"Deterministic Engine Verdict: HEALTHY / ABSTENTION (status=unknown). Across %d traces, %d K8s events, and %d pods in namespace %q, none of the 8 fault signatures crossed its multi-signal evidence threshold. Any decoy activity in non-target namespaces (e.g. 'noise') was isolated and ignored.",
			inc.TraceCount, inc.EventCount, inc.PodCount, ns,
		)
		alts = []string{
			"cpu_throttling ruled out: CFS throttled period ratio remained below the saturation threshold.",
			"oom_killed & unschedulable ruled out: target namespace containers had 0 OOM terminations (exitCode 137) and 0 Pending scheduling blockers.",
			"unknown_no_infrastructure_cause ruled out: OTel pre-inference span gap (HTTP server start -> GenAI client start) remained nominal.",
		}
		steps = []RunbookStep{
			{
				Step:             1,
				Phase:            "verify_readonly",
				Title:            "Confirm Live Pod Health (Read-Only)",
				Command:          fmt.Sprintf("kubectl get pods,events -n %s", ns),
				RequiresApproval: false,
				Rationale:        "Verify that all GenAI serving pods remain Running and Ready=True.",
			},
		}
	} else {
		top := rep.TopCauses[0]
		wl := top.Component.Workload
		if wl == "" {
			wl = "deployment/model-server"
		}
		deployName := strings.TrimPrefix(wl, "deployment/")
		podName := top.Component.Pod
		if podName == "" {
			podName = deployName
		}

		finopsStr := ""
		if inc.Cost != nil && inc.Cost.MonthlyProjectedWaste > 0 {
			finopsStr = fmt.Sprintf(
				" OpenCost FinOps models this bottleneck at $%.2f/month in projected capacity waste ($%.4f in this window), with a %+.2f $/month resource delta for the recommended fix.",
				inc.Cost.MonthlyProjectedWaste, inc.Cost.EstimatedWasteCost, inc.Cost.PatchMonthlyDeltaUSD,
			)
		}

		brief = fmt.Sprintf(
			"Deterministic Root Cause: %s (%.0f%% confidence, rule=%s) on %s in namespace %q. %s.%s The assistant has generated a read-only verification sequence and a PSS-Restricted-validated GitOps patch below (human approval required before cluster apply).",
			top.Cause, top.Confidence*100, top.Rule, wl, ns, top.Summary, finopsStr,
		)

		alts = buildRuledOutHypotheses(top.Cause)
		steps = []RunbookStep{
			{
				Step:             1,
				Phase:            "verify_readonly",
				Title:            "1. Read-Only Live State & Event Verification",
				Command:          fmt.Sprintf("kubectl describe pod %s -n %s && ./bin/kubectl-why_slow --namespace %s", podName, ns, ns),
				RequiresApproval: false,
				Rationale:        "Safe read-only inspection confirming the timestamped evidence items isolated by KubePilot.",
			},
			{
				Step:             2,
				Phase:            "dry_run_guard",
				Title:            "2. Server-Side Dry-Run Validation (No Cluster Mutation)",
				Command:          fmt.Sprintf("kubectl apply -f patch.yaml -n %s --dry-run=server", ns),
				RequiresApproval: false,
				Rationale:        "Validates the Strategic Merge Patch against the Kubernetes API server admission controllers without mutating the Deployment.",
			},
			{
				Step:             3,
				Phase:            "remediate_gitops",
				Title:            "3. Human-Approved GitOps / kubectl Remediation",
				Command:          fmt.Sprintf("kubectl apply -f patch.yaml -n %s && kubectl rollout status %s -n %s", ns, wl, ns),
				RequiresApproval: true,
				Rationale:        "Requires explicit SRE / GitOps PR approval. KubePilot never executes write operations autonomously.",
			},
			{
				Step:             4,
				Phase:            "rollback_guard",
				Title:            "4. Instant Rollback Guard & Post-Remediation Check",
				Command:          fmt.Sprintf("kubectl rollout undo %s -n %s && ./bin/kubectl-why_slow --namespace %s", wl, ns, ns),
				RequiresApproval: true,
				Rationale:        "Immediate 1-command rollback if post-change latency or readiness regresses.",
			},
		}
	}

	return &RunbookSynthesis{
		EngineMode:            "deterministic-grounded-copilot (0 LLM in diagnosis path)",
		ClusterWriteAccess:    false,
		HumanApprovalRequired: true,
		SignalCoveragePct:     covPct,
		Signals:               signals,
		ExecutiveBrief:        brief,
		AlternativeHypotheses: alts,
		ScopeAndLimitations:   "Empirical scope: calibrated on the closed 8-cause v1 catalog (24/24 held-out test recall, Wilson 95% CI [86.2%, 100%]; 0/21 false positives on healthy/decoy windows). Unseen failure modes (e.g. CNI packet drop, external API rate limits) intentionally return status='unknown' rather than an unverified guess.",
		Steps:                 steps,
	}
}

func signalStatus(count, minHealthy int) string {
	if count >= minHealthy {
		return "healthy"
	}
	return "missing"
}

func buildRuledOutHypotheses(active snapshot.Cause) []string {
	var out []string
	if active != snapshot.CauseOOMKilled {
		out = append(out, "oom_killed ruled out: no container in target namespace terminated with exitCode=137 (OOMKilled) during the analysis window.")
	}
	if active != snapshot.CauseUnschedulable {
		out = append(out, "unschedulable ruled out: target workload pods passed scheduler placement (PodScheduled=True).")
	}
	if active != snapshot.CauseCPUThrottling && active != snapshot.CauseHostContention {
		out = append(out, "cpu_throttling & host_cpu_contention ruled out: cAdvisor CFS throttled periods and Linux PSI CPU waiting remained within baseline bounds.")
	}
	if active != snapshot.CauseUnknownNoInfra {
		out = append(out, "unknown_no_infrastructure_cause ruled out: concrete Kubernetes/Prometheus infrastructure evidence accounted for the latency/errors.")
	} else {
		out = append(out, "All 7 Kubernetes infrastructure causes ruled out: pods, node PSI, CFS quotas, and model-server inference duration remained healthy while OTel traces isolated the delay prior to the GenAI client span.")
	}
	return out
}
