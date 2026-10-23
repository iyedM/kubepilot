// Package server implements the read-only KubePilot HTTP API and incident
// store used by the in-cluster service (P5) and Web UI (P6).
//
// In accordance with CLAUDE.md, this package is strictly read-only on the
// target Kubernetes cluster, contains no LLM calls in the diagnosis path, and
// never hard-codes OpenTelemetry "gen_ai.*" attribute or metric names.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iyedM/kubepilot/internal/collector/live"
	"github.com/iyedM/kubepilot/internal/collector/offline"
	"github.com/iyedM/kubepilot/internal/rules"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

// CollectFunc abstracts live or mock snapshot collection for testability.
type CollectFunc func(ctx context.Context, cfg live.Config) (*snapshot.Snapshot, error)

// CostSummary represents the optional OpenCost incident cost estimate (P8)
// attached to a diagnosed incident window.
type CostSummary struct {
	Currency           string  `json:"currency"`
	Workload           string  `json:"workload"`
	Namespace          string  `json:"namespace"`
	WindowDurationSec  float64 `json:"windowDurationSec"`
	TotalWindowCost    float64 `json:"totalWindowCost"`
	EstimatedWasteCost float64 `json:"estimatedWasteCost"`
	CPUCost            float64 `json:"cpuCost"`
	RAMCost            float64 `json:"ramCost"`
	Source             string  `json:"source"`
	Explanation        string  `json:"explanation"`
}

// CostEstimator computes an optional cost estimate for a diagnosed snapshot and report.
type CostEstimator interface {
	Estimate(ctx context.Context, snap *snapshot.Snapshot, rep snapshot.Report) (*CostSummary, error)
}

// MetricPoint is a single (timestamp, value) sample for UI time-series charts.
type MetricPoint struct {
	Timestamp time.Time `json:"ts"`
	Value     float64   `json:"value"`
}

// MetricLine is one labeled series inside a MetricChart.
type MetricLine struct {
	Label  string        `json:"label"`
	Points []MetricPoint `json:"points"`
}

// MetricChart holds a named time-series chart extracted from Snapshot.Metrics.
type MetricChart struct {
	ID     string       `json:"id"`
	Title  string       `json:"title"`
	Unit   string       `json:"unit"`
	Series []MetricLine `json:"series"`
}

// TelemetryOverview bundles the correlated traces, requests, pods, events, and
// Prometheus metric curves from a Snapshot so the Web UI can render interactive
// topology diagrams, swimlane timelines, trace waterfalls, and metric charts.
type TelemetryOverview struct {
	Baseline *snapshot.TimeRange      `json:"baseline,omitempty"`
	Requests []snapshot.RequestSample `json:"requests,omitempty"`
	Traces   []snapshot.TraceSummary  `json:"traces,omitempty"`
	Pods     []snapshot.PodSnapshot   `json:"pods,omitempty"`
	Events   []snapshot.K8sEvent      `json:"events,omitempty"`
	Charts   []MetricChart            `json:"charts,omitempty"`
}

// IncidentRecord wraps a snapshot.Report with metadata, trace/event counts,
// optional cost estimation, and rich telemetry overview for the API, Web UI (P6),
// and Operator (P7).
type IncidentRecord struct {
	ID         string             `json:"id"`
	CreatedAt  time.Time          `json:"createdAt"`
	Source     string             `json:"source"` // "live", "poll", "run-dir", "snapshot"
	RunID      string             `json:"runId,omitempty"`
	Namespace  string             `json:"namespace"`
	TraceID    string             `json:"traceId,omitempty"`
	Report     snapshot.Report    `json:"report"`
	TraceCount int                `json:"traceCount"`
	EventCount int                `json:"eventCount"`
	PodCount   int                `json:"podCount"`
	Cost       *CostSummary       `json:"cost,omitempty"`
	Telemetry  *TelemetryOverview `json:"telemetry,omitempty"`
}

// Config configures a KubePilot HTTP Server.
type Config struct {
	LiveConfig    live.Config
	Collect       CollectFunc
	CostEstimator CostEstimator
	UIHandler     http.Handler
	MaxIncidents  int
}

// Server serves the KubePilot HTTP API and optional embedded Web UI.
type Server struct {
	cfg       Config
	mu        sync.RWMutex
	seq       int64
	incidents []IncidentRecord
	mux       *http.ServeMux
}

// New creates and initializes a Server.
func New(cfg Config) *Server {
	if cfg.Collect == nil {
		cfg.Collect = live.Collect
	}
	if cfg.LiveConfig.Namespace == "" {
		cfg.LiveConfig.Namespace = "genai"
	}
	if cfg.LiveConfig.ObsNamespace == "" {
		cfg.LiveConfig.ObsNamespace = "observability"
	}
	if cfg.LiveConfig.SkewTolerance <= 0 {
		cfg.LiveConfig.SkewTolerance = offline.DefaultSkewTolerance
	}
	if cfg.MaxIncidents <= 0 {
		cfg.MaxIncidents = 200
	}

	s := &Server{
		cfg: cfg,
		mux: http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// RecordIncident stores a diagnosed or inspected IncidentRecord in the bounded
// in-memory store and returns the stored record with its assigned ID.
func (s *Server) RecordIncident(rec IncidentRecord) IncidentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	if rec.ID == "" {
		rec.ID = fmt.Sprintf("inc-%04d", s.seq)
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	if rec.Namespace == "" {
		rec.Namespace = s.cfg.LiveConfig.Namespace
	}

	// Deduplicate by ID if updating an existing seeded incident.
	for i := range s.incidents {
		if s.incidents[i].ID == rec.ID {
			s.incidents[i] = rec
			return rec
		}
	}

	s.incidents = append([]IncidentRecord{rec}, s.incidents...)
	if len(s.incidents) > s.cfg.MaxIncidents {
		s.incidents = s.incidents[:s.cfg.MaxIncidents]
	}
	return rec
}

// ListIncidents returns a copy of all stored IncidentRecords (newest first).
func (s *Server) ListIncidents() []IncidentRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]IncidentRecord, len(s.incidents))
	copy(out, s.incidents)
	return out
}

// GetIncident looks up an IncidentRecord by ID.
func (s *Server) GetIncident(id string) (IncidentRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, inc := range s.incidents {
		if inc.ID == id {
			return inc, true
		}
	}
	return IncidentRecord{}, false
}

// SeedFromRunsDir loads up to limit recorded run directories from dir (e.g.
// eval/runs/dev) into the incident store so users and tests can browse real
// lab incidents immediately in the Web UI.
func (s *Server) SeedFromRunsDir(ctx context.Context, dir string, limit int) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if limit > 0 && len(names) > limit {
		names = names[:limit]
	}

	loaded := 0
	for _, name := range names {
		runPath := filepath.Join(dir, name)
		snap, meta, err := offline.LoadRunDir(runPath, offline.Options{
			SkewTolerance: s.cfg.LiveConfig.SkewTolerance,
		})
		if err != nil {
			continue
		}
		rep := rules.Diagnose(snap)
		rec := s.buildRecord(ctx, "run-dir", name, snap, rep)
		if meta != nil && meta.RunID != "" {
			rec.ID = meta.RunID
		} else {
			rec.ID = name
		}
		if !snap.Window.To.IsZero() {
			rec.CreatedAt = snap.Window.To
		}
		s.RecordIncident(rec)
		loaded++
	}
	return loaded, nil
}

// PollOnce executes one read-only live collection + diagnosis pass and records
// the resulting incident if status == StatusDiagnosed (or always if recordAll
// is true).
func (s *Server) PollOnce(ctx context.Context, from, to time.Time, traceID string, recordAll bool) (IncidentRecord, error) {
	lcfg := s.cfg.LiveConfig
	lcfg.From = from
	lcfg.To = to
	lcfg.TraceID = traceID

	snap, err := s.cfg.Collect(ctx, lcfg)
	if err != nil {
		return IncidentRecord{}, err
	}
	rep := rules.Diagnose(snap)
	rec := s.buildRecord(ctx, "live", "", snap, rep)
	if recordAll || rep.Status == snapshot.StatusDiagnosed {
		rec = s.RecordIncident(rec)
	}
	return rec, nil
}

func (s *Server) buildRecord(ctx context.Context, source, runID string, snap *snapshot.Snapshot, rep snapshot.Report) IncidentRecord {
	ns := s.cfg.LiveConfig.Namespace
	if ns == "" {
		ns = "genai"
	}
	rec := IncidentRecord{
		CreatedAt:  time.Now().UTC(),
		Source:     source,
		RunID:      runID,
		Namespace:  ns,
		TraceID:    snap.TargetTraceID,
		Report:     rep,
		TraceCount: len(snap.Traces),
		EventCount: len(snap.Events),
		PodCount:   len(snap.Pods),
		Telemetry:  buildTelemetryOverview(snap, ns),
	}
	if s.cfg.CostEstimator != nil {
		if cost, err := s.cfg.CostEstimator.Estimate(ctx, snap, rep); err == nil && cost != nil {
			rec.Cost = cost
		}
	}
	return rec
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/readyz", s.handleHealth)
	s.mux.HandleFunc("/api/v1/diagnose", s.handleDiagnose)
	s.mux.HandleFunc("/api/v1/seed", s.handleSeed)
	s.mux.HandleFunc("/api/v1/incidents", s.handleIncidents)
	s.mux.HandleFunc("/api/v1/incidents/", s.handleIncidentByID)
	if s.cfg.UIHandler != nil {
		s.mux.Handle("/", s.cfg.UIHandler)
	} else {
		s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"service": "kubepilot-server",
				"mode":    "read-only",
				"routes": []string{
					"/healthz",
					"/readyz",
					"/api/v1/diagnose",
					"/api/v1/seed",
					"/api/v1/incidents",
				},
			})
		})
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"mode":   "read-only",
	})
}

// DiagnoseRequest describes the query or JSON payload accepted by /api/v1/diagnose.
type DiagnoseRequest struct {
	RunDir       string `json:"runDir,omitempty"`
	SnapshotPath string `json:"snapshotPath,omitempty"`
	TraceID      string `json:"traceId,omitempty"`
	From         string `json:"from,omitempty"`
	To           string `json:"to,omitempty"`
	Namespace    string `json:"namespace,omitempty"`
}

func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use GET or POST")
		return
	}

	req := DiagnoseRequest{
		RunDir:       strings.TrimSpace(r.URL.Query().Get("run_dir")),
		SnapshotPath: strings.TrimSpace(r.URL.Query().Get("snapshot")),
		TraceID:      strings.TrimSpace(r.URL.Query().Get("trace_id")),
		From:         strings.TrimSpace(r.URL.Query().Get("from")),
		To:           strings.TrimSpace(r.URL.Query().Get("to")),
		Namespace:    strings.TrimSpace(r.URL.Query().Get("namespace")),
	}
	if r.Method == http.MethodPost && r.Body != nil {
		var bodyReq DiagnoseRequest
		if err := json.NewDecoder(r.Body).Decode(&bodyReq); err == nil {
			if bodyReq.RunDir != "" {
				req.RunDir = bodyReq.RunDir
			}
			if bodyReq.SnapshotPath != "" {
				req.SnapshotPath = bodyReq.SnapshotPath
			}
			if bodyReq.TraceID != "" {
				req.TraceID = bodyReq.TraceID
			}
			if bodyReq.From != "" {
				req.From = bodyReq.From
			}
			if bodyReq.To != "" {
				req.To = bodyReq.To
			}
			if bodyReq.Namespace != "" {
				req.Namespace = bodyReq.Namespace
			}
		}
	}

	var fromTS, toTS time.Time
	var err error
	if req.From != "" {
		fromTS, err = time.Parse(time.RFC3339Nano, req.From)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid from timestamp %q: %v", req.From, err))
			return
		}
	}
	if req.To != "" {
		toTS, err = time.Parse(time.RFC3339Nano, req.To)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid to timestamp %q: %v", req.To, err))
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var snap *snapshot.Snapshot
	source := "live"
	runID := ""

	switch {
	case req.SnapshotPath != "":
		raw, err := os.ReadFile(req.SnapshotPath)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("read snapshot: %v", err))
			return
		}
		var parsed snapshot.Snapshot
		if err := json.Unmarshal(raw, &parsed); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("parse snapshot: %v", err))
			return
		}
		snap = &parsed
		source = "snapshot"
		runID = filepath.Base(req.SnapshotPath)
	case req.RunDir != "":
		var meta *offline.RunMeta
		snap, meta, err = offline.LoadRunDir(req.RunDir, offline.Options{
			From:          fromTS,
			To:            toTS,
			TraceID:       req.TraceID,
			SkewTolerance: s.cfg.LiveConfig.SkewTolerance,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("load run-dir: %v", err))
			return
		}
		source = "run-dir"
		if meta != nil && meta.RunID != "" {
			runID = meta.RunID
		} else {
			runID = filepath.Base(req.RunDir)
		}
	default:
		lcfg := s.cfg.LiveConfig
		if req.Namespace != "" {
			lcfg.Namespace = req.Namespace
		}
		lcfg.From = fromTS
		lcfg.To = toTS
		lcfg.TraceID = req.TraceID
		snap, err = s.cfg.Collect(ctx, lcfg)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("live collection failed: %v", err))
			return
		}
	}

	rep := rules.Diagnose(snap)
	rec := s.buildRecord(ctx, source, runID, snap, rep)
	rec = s.RecordIncident(rec)
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use GET")
		return
	}
	list := s.ListIncidents()
	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
	causeFilter := strings.TrimSpace(r.URL.Query().Get("cause"))
	if statusFilter != "" || causeFilter != "" {
		var filtered []IncidentRecord
		for _, inc := range list {
			if statusFilter != "" && string(inc.Report.Status) != statusFilter {
				continue
			}
			if causeFilter != "" {
				matched := false
				for _, c := range inc.Report.TopCauses {
					if string(c.Cause) == causeFilter {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			filtered = append(filtered, inc)
		}
		list = filtered
	}
	if r.URL.Query().Get("full") != "1" {
		compact := make([]IncidentRecord, len(list))
		for i := range list {
			compact[i] = list[i]
			compact[i].Telemetry = nil
		}
		list = compact
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":     len(list),
		"incidents": list,
	})
}

func (s *Server) handleIncidentByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use GET")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/incidents/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing incident id")
		return
	}
	inc, ok := s.GetIncident(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("incident %q not found", id))
		return
	}
	writeJSON(w, http.StatusOK, inc)
}

// SeedRequest describes the payload accepted by POST /api/v1/seed.
type SeedRequest struct {
	Dir   string `json:"dir"`
	Limit int    `json:"limit,omitempty"`
	Clear bool   `json:"clear,omitempty"`
}

func (s *Server) handleSeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use POST")
		return
	}
	var req SeedRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	dir := filepath.Clean(strings.TrimSpace(req.Dir))
	if dir == "" || dir == "." {
		dir = filepath.Join("eval", "runs", "dev")
	}
	if req.Clear {
		s.mu.Lock()
		s.incidents = nil
		s.mu.Unlock()
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var loaded int
	if dir == "all" {
		n1, _ := s.SeedFromRunsDir(ctx, filepath.Join("eval", "runs", "dev"), limit)
		n2, _ := s.SeedFromRunsDir(ctx, filepath.Join("eval", "runs", "test"), limit)
		loaded = n1 + n2
	} else {
		n, err := s.SeedFromRunsDir(ctx, dir, limit)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("seed %q: %v", dir, err))
			return
		}
		loaded = n
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"seeded": loaded,
		"total":  len(s.ListIncidents()),
	})
}

func buildTelemetryOverview(snap *snapshot.Snapshot, targetNS string) *TelemetryOverview {
	if snap == nil {
		return nil
	}
	ov := &TelemetryOverview{
		Baseline: snap.Window.Baseline,
	}

	// 1. Requests (sorted by timestamp, capped at 80)
	if len(snap.Requests) > 0 {
		reqs := make([]snapshot.RequestSample, len(snap.Requests))
		copy(reqs, snap.Requests)
		sort.Slice(reqs, func(i, j int) bool {
			return reqs[i].Timestamp.Before(reqs[j].Timestamp)
		})
		if len(reqs) > 80 {
			reqs = reqs[:80]
		}
		ov.Requests = reqs
	}

	// 2. Traces (sorted by StartTime, capped at 60)
	if len(snap.Traces) > 0 {
		trs := make([]snapshot.TraceSummary, len(snap.Traces))
		copy(trs, snap.Traces)
		sort.Slice(trs, func(i, j int) bool {
			return trs[i].StartTime.Before(trs[j].StartTime)
		})
		if len(trs) > 60 {
			trs = trs[:60]
		}
		ov.Traces = trs
	}

	// 3. Pods (prioritize target namespace + noise namespace, capped at 20)
	for _, p := range snap.Pods {
		if p.Namespace == targetNS || p.Namespace == "noise" {
			ov.Pods = append(ov.Pods, p)
		}
	}
	if len(ov.Pods) == 0 && len(snap.Pods) > 0 {
		limit := len(snap.Pods)
		if limit > 20 {
			limit = 20
		}
		ov.Pods = append(ov.Pods, snap.Pods[:limit]...)
	}

	// 4. Events (sorted by Timestamp, capped at 60)
	if len(snap.Events) > 0 {
		evs := make([]snapshot.K8sEvent, len(snap.Events))
		copy(evs, snap.Events)
		sort.Slice(evs, func(i, j int) bool {
			return evs[i].Timestamp.Before(evs[j].Timestamp)
		})
		if len(evs) > 60 {
			evs = evs[:60]
		}
		ov.Events = evs
	}

	// 5. Prometheus Metric Time-Series Charts
	ov.Charts = buildMetricCharts(snap, targetNS)
	return ov
}

func buildMetricCharts(snap *snapshot.Snapshot, targetNS string) []MetricChart {
	if len(snap.Metrics) == 0 {
		return nil
	}
	var charts []MetricChart

	// Chart A: CFS CPU Throttling Ratio (%) per pod
	if throtChart := buildThrottlingChart(snap, targetNS); len(throtChart.Series) > 0 {
		charts = append(charts, throtChart)
	}

	// Chart B: Container CPU Usage Rate (cores)
	if cpuChart := buildCounterRateChart(
		snap.Metrics["cpu_usage_seconds"],
		"cpu_usage_cores",
		"Container CPU Usage Rate",
		"cores",
		1.0,
		targetNS,
	); len(cpuChart.Series) > 0 {
		charts = append(charts, cpuChart)
	}

	// Chart C: Container Memory Working Set (MiB)
	if memChart := buildGaugeChart(
		snap.Metrics["memory_working_set_bytes"],
		"memory_mib",
		"Container Memory Working Set",
		"MiB",
		1.0/(1024.0*1024.0),
		targetNS,
	); len(memChart.Series) > 0 {
		charts = append(charts, memChart)
	}

	// Chart D: LLM Predicted Token Generation Speed (tok/s)
	if tokChart := buildGaugeChart(
		snap.Metrics["llama_predicted_tokens_per_second"],
		"llama_tok_s",
		"LLM Token Generation Throughput",
		"tok/s",
		1.0,
		targetNS,
	); len(tokChart.Series) > 0 {
		charts = append(charts, tokChart)
	}

	// Chart E: Linux PSI CPU Waiting Pressure (ms/s)
	if psiChart := buildCounterRateChart(
		snap.Metrics["cpu_pressure_waiting_seconds"],
		"cpu_pressure_ms",
		"Host CPU Runqueue Pressure (Linux PSI)",
		"ms/s",
		1000.0,
		targetNS,
	); len(psiChart.Series) > 0 {
		charts = append(charts, psiChart)
	}

	return charts
}

func streamPodLabel(labels map[string]string, targetNS string) (string, bool) {
	ns := labels["namespace"]
	if ns != "" && ns != targetNS && ns != "noise" {
		return "", false
	}
	pod := labels["pod"]
	container := labels["container"]
	if container == "POD" {
		return "", false
	}
	if pod == "" {
		if job := labels["job"]; job != "" {
			return job, true
		}
		return "series", true
	}
	if ns == "noise" {
		return "noise/" + pod, true
	}
	return pod, true
}

func buildThrottlingChart(snap *snapshot.Snapshot, targetNS string) MetricChart {
	chart := MetricChart{
		ID:    "cpu_throttled_pct",
		Title: "CFS CPU Throttled Periods (%)",
		Unit:  "%",
	}
	throtSeries, ok1 := snap.Metrics["cpu_throttled_periods"]
	totalSeries, ok2 := snap.Metrics["cpu_periods"]
	if !ok1 || !ok2 {
		return chart
	}
	totalsByPod := make(map[string][]snapshot.MetricSample)
	for _, st := range totalSeries.Streams {
		lbl, keep := streamPodLabel(st.Labels, targetNS)
		if !keep || len(st.Samples) < 2 {
			continue
		}
		totalsByPod[lbl] = st.Samples
	}
	for _, st := range throtSeries.Streams {
		lbl, keep := streamPodLabel(st.Labels, targetNS)
		if !keep || len(st.Samples) < 2 {
			continue
		}
		totSamples, exists := totalsByPod[lbl]
		if !exists || len(totSamples) < 2 {
			continue
		}
		totByTS := make(map[int64]float64, len(totSamples))
		for _, s := range totSamples {
			totByTS[s.Timestamp.Unix()] = s.Value
		}
		var pts []MetricPoint
		for i := 1; i < len(st.Samples); i++ {
			prev := st.Samples[i-1]
			curr := st.Samples[i]
			tPrev, okP := totByTS[prev.Timestamp.Unix()]
			tCurr, okC := totByTS[curr.Timestamp.Unix()]
			if !okP || !okC || tCurr <= tPrev {
				continue
			}
			dThrot := curr.Value - prev.Value
			if dThrot < 0 {
				dThrot = 0
			}
			pct := (dThrot / (tCurr - tPrev)) * 100.0
			if pct > 100 {
				pct = 100
			}
			pts = append(pts, MetricPoint{
				Timestamp: curr.Timestamp,
				Value:     pct,
			})
		}
		if len(pts) > 0 {
			chart.Series = append(chart.Series, MetricLine{
				Label:  lbl,
				Points: pts,
			})
		}
	}
	return chart
}

func buildCounterRateChart(s snapshot.Series, id, title, unit string, scale float64, targetNS string) MetricChart {
	chart := MetricChart{
		ID:    id,
		Title: title,
		Unit:  unit,
	}
	seen := make(map[string]bool)
	for _, st := range s.Streams {
		lbl, keep := streamPodLabel(st.Labels, targetNS)
		if !keep || seen[lbl] || len(st.Samples) < 2 {
			continue
		}
		var pts []MetricPoint
		for i := 1; i < len(st.Samples); i++ {
			dt := st.Samples[i].Timestamp.Sub(st.Samples[i-1].Timestamp).Seconds()
			dv := st.Samples[i].Value - st.Samples[i-1].Value
			if dt <= 0 || dv < 0 {
				continue
			}
			pts = append(pts, MetricPoint{
				Timestamp: st.Samples[i].Timestamp,
				Value:     (dv / dt) * scale,
			})
		}
		if len(pts) > 0 {
			seen[lbl] = true
			chart.Series = append(chart.Series, MetricLine{
				Label:  lbl,
				Points: pts,
			})
		}
	}
	return chart
}

func buildGaugeChart(s snapshot.Series, id, title, unit string, scale float64, targetNS string) MetricChart {
	chart := MetricChart{
		ID:    id,
		Title: title,
		Unit:  unit,
	}
	seen := make(map[string]bool)
	for _, st := range s.Streams {
		lbl, keep := streamPodLabel(st.Labels, targetNS)
		if !keep || seen[lbl] || len(st.Samples) == 0 {
			continue
		}
		var pts []MetricPoint
		for _, smp := range st.Samples {
			pts = append(pts, MetricPoint{
				Timestamp: smp.Timestamp,
				Value:     smp.Value * scale,
			})
		}
		if len(pts) > 0 {
			seen[lbl] = true
			chart.Series = append(chart.Series, MetricLine{
				Label:  lbl,
				Points: pts,
			})
		}
	}
	return chart
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{
		"error": msg,
	})
}
