// Package server implements the read-only KubePilot HTTP API and incident
// store used by the in-cluster service (P5) and Web UI (P6).
//
// In accordance with CLAUDE.md, this package is strictly read-only on the
// target Kubernetes cluster, contains no LLM calls in the diagnosis path, and
// never hard-codes OpenTelemetry "gen_ai.*" attribute or metric names.
package server

import (
	"bytes"
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

// CostSummary represents the OpenCost incident cost estimate (P8) and
// operational FinOps unit-economics / monthly projection attached to a window.
type CostSummary struct {
	Currency              string  `json:"currency"`
	Workload              string  `json:"workload"`
	Namespace             string  `json:"namespace"`
	WindowDurationSec     float64 `json:"windowDurationSec"`
	TotalWindowCost       float64 `json:"totalWindowCost"`
	EstimatedWasteCost    float64 `json:"estimatedWasteCost"`
	CPUCost               float64 `json:"cpuCost"`
	RAMCost               float64 `json:"ramCost"`
	MonthlyProjectedCost  float64 `json:"monthlyProjectedCost"`
	MonthlyProjectedWaste float64 `json:"monthlyProjectedWaste"`
	CostPer1kRequestsUSD  float64 `json:"costPer1kRequestsUsd"`
	CostPer1MTokensUSD    float64 `json:"costPer1mTokensUsd"`
	PatchMonthlyDeltaUSD  float64 `json:"patchMonthlyDeltaUsd"`
	FinOpsRecommendation  string  `json:"finopsRecommendation,omitempty"`
	Source                string  `json:"source"`
	Explanation           string  `json:"explanation"`
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
// OpenCost FinOps estimation, DevSecOps posture audit, controlled SRE runbook
// synthesis, read-only GitOps patch suggestion, and rich telemetry overview.
type IncidentRecord struct {
	ID             string              `json:"id"`
	CreatedAt      time.Time           `json:"createdAt"`
	Source         string              `json:"source"` // "live", "poll", "run-dir", "snapshot"
	RunID          string              `json:"runId,omitempty"`
	Namespace      string              `json:"namespace"`
	TraceID        string              `json:"traceId,omitempty"`
	Report         snapshot.Report     `json:"report"`
	TraceCount     int                 `json:"traceCount"`
	EventCount     int                 `json:"eventCount"`
	PodCount       int                 `json:"podCount"`
	Cost           *CostSummary        `json:"cost,omitempty"`
	SuggestedPatch string              `json:"suggestedPatch,omitempty"`
	Comparison     *BaselineComparison `json:"comparison,omitempty"`
	Security       *SecurityAudit      `json:"security,omitempty"`
	Runbook        *RunbookSynthesis   `json:"runbook,omitempty"`
	Telemetry      *TelemetryOverview  `json:"telemetry,omitempty"`
}

// Config configures a KubePilot HTTP Server.
type Config struct {
	LiveConfig    live.Config
	Collect       CollectFunc
	CostEstimator CostEstimator
	UIHandler     http.Handler
	MaxIncidents  int
	WebhookURL    string
	HTTPClient    *http.Client
}

// Server serves the KubePilot HTTP API and optional embedded Web UI.
type Server struct {
	cfg                  Config
	mu                   sync.RWMutex
	seq                  int64
	webhookSent          int64
	webhookFailed        int64
	alertmanagerReceived int64
	incidents            []IncidentRecord
	subscribers          map[chan IncidentRecord]struct{}
	mux                  *http.ServeMux
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
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}

	s := &Server{
		cfg:         cfg,
		subscribers: make(map[chan IncidentRecord]struct{}),
		mux:         http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// RecordIncident stores a diagnosed or inspected IncidentRecord in the bounded
// in-memory store, broadcasts it to SSE subscribers, and returns the stored
// record with its assigned ID.
func (s *Server) RecordIncident(rec IncidentRecord) IncidentRecord {
	s.mu.Lock()
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

	updated := false
	for i := range s.incidents {
		if s.incidents[i].ID == rec.ID {
			s.incidents[i] = rec
			updated = true
			break
		}
	}
	if !updated {
		s.incidents = append([]IncidentRecord{rec}, s.incidents...)
		if len(s.incidents) > s.cfg.MaxIncidents {
			s.incidents = s.incidents[:s.cfg.MaxIncidents]
		}
	}
	s.mu.Unlock()

	s.broadcastIncident(rec)
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
// is true). If WebhookURL is configured and a fault is diagnosed, an alert is
// dispatched asynchronously.
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
		if rep.Status == snapshot.StatusDiagnosed && s.cfg.WebhookURL != "" {
			_ = s.SendWebhookAlert(ctx, rec, s.cfg.WebhookURL)
		}
	}
	return rec, nil
}

func (s *Server) buildRecord(ctx context.Context, source, runID string, snap *snapshot.Snapshot, rep snapshot.Report) IncidentRecord {
	ns := s.cfg.LiveConfig.Namespace
	if ns == "" {
		ns = "genai"
	}
	rec := IncidentRecord{
		CreatedAt:      time.Now().UTC(),
		Source:         source,
		RunID:          runID,
		Namespace:      ns,
		TraceID:        snap.TargetTraceID,
		Report:         rep,
		TraceCount:     len(snap.Traces),
		EventCount:     len(snap.Events),
		PodCount:       len(snap.Pods),
		SuggestedPatch: buildSuggestedPatch(snap, rep, ns),
		Telemetry:      buildTelemetryOverview(snap, ns),
	}
	if s.cfg.CostEstimator != nil {
		if cost, err := s.cfg.CostEstimator.Estimate(ctx, snap, rep); err == nil && cost != nil {
			rec.Cost = cost
		}
	}
	comp := BuildBaselineComparison(rec)
	rec.Comparison = &comp
	rec.Security = BuildSecurityAudit(snap, rep, rec.SuggestedPatch, ns)
	rec.Runbook = BuildRunbookSynthesis(snap, rec)
	return rec
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/readyz", s.handleHealth)
	s.mux.HandleFunc("/metrics", s.handleMetrics)
	s.mux.HandleFunc("/api/v1/stream", s.handleStream)
	s.mux.HandleFunc("/api/v1/diagnose", s.handleDiagnose)
	s.mux.HandleFunc("/api/v1/simulate", s.handleSimulate)
	s.mux.HandleFunc("/api/v1/seed", s.handleSeed)
	s.mux.HandleFunc("/api/v1/webhook/test", s.handleWebhookTest)
	s.mux.HandleFunc("/api/v1/alerts/alertmanager", s.handleAlertmanagerWebhook)
	s.mux.HandleFunc("/api/v1/incidents", s.handleIncidents)
	s.mux.HandleFunc("/api/v1/incidents/bundle", s.handleBundleUpload)
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
					"/metrics",
					"/api/v1/stream",
					"/api/v1/diagnose",
					"/api/v1/simulate",
					"/api/v1/seed",
					"/api/v1/webhook/test",
					"/api/v1/alerts/alertmanager",
					"/api/v1/incidents",
					"/api/v1/incidents/bundle",
				},
			})
		})
	}
}

func (s *Server) diagnoseRunDirInternal(ctx context.Context, runDir, source string) (IncidentRecord, error) {
	snap, meta, err := offline.LoadRunDir(runDir, offline.Options{
		SkewTolerance: s.cfg.LiveConfig.SkewTolerance,
	})
	if err != nil {
		return IncidentRecord{}, err
	}
	runID := filepath.Base(runDir)
	if meta != nil && meta.RunID != "" {
		runID = meta.RunID
	}
	rep := rules.Diagnose(snap)
	rec := s.buildRecord(ctx, source, runID, snap, rep)
	rec.ID = runID
	rec = s.RecordIncident(rec)
	if rep.Status == snapshot.StatusDiagnosed && s.cfg.WebhookURL != "" {
		_ = s.SendWebhookAlert(ctx, rec, s.cfg.WebhookURL)
	}
	return rec, nil
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
	if rep.Status == snapshot.StatusDiagnosed && s.cfg.WebhookURL != "" {
		_ = s.SendWebhookAlert(ctx, rec, s.cfg.WebhookURL)
	}
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
	rel := strings.TrimPrefix(r.URL.Path, "/api/v1/incidents/")
	if rel == "" {
		writeError(w, http.StatusBadRequest, "missing incident id")
		return
	}
	subAction := ""
	switch {
	case strings.HasSuffix(rel, "/postmortem"):
		subAction = "postmortem"
		rel = strings.TrimSuffix(rel, "/postmortem")
	case strings.HasSuffix(rel, "/compare"):
		subAction = "compare"
		rel = strings.TrimSuffix(rel, "/compare")
	case strings.HasSuffix(rel, "/bundle"):
		subAction = "bundle"
		rel = strings.TrimSuffix(rel, "/bundle")
	case strings.HasSuffix(rel, "/security"):
		subAction = "security"
		rel = strings.TrimSuffix(rel, "/security")
	case strings.HasSuffix(rel, "/runbook"):
		subAction = "runbook"
		rel = strings.TrimSuffix(rel, "/runbook")
	case strings.HasSuffix(rel, "/explain"):
		subAction = "explain"
		rel = strings.TrimSuffix(rel, "/explain")
	}

	if subAction == "explain" {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed; use GET or POST")
			return
		}
	} else if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use GET")
		return
	}

	inc, ok := s.GetIncident(rel)
	if !ok && !strings.Contains(rel, "/") && !strings.Contains(rel, "..") {
		for _, split := range []string{"dev", "test"} {
			candidate := filepath.Join("eval", "runs", split, rel)
			if snap, meta, err := offline.LoadRunDir(candidate, offline.Options{
				SkewTolerance: s.cfg.LiveConfig.SkewTolerance,
			}); err == nil {
				rep := rules.Diagnose(snap)
				loaded := s.buildRecord(r.Context(), "run-dir", rel, snap, rep)
				if meta != nil && meta.RunID != "" {
					loaded.ID = meta.RunID
				} else {
					loaded.ID = rel
				}
				inc = s.RecordIncident(loaded)
				ok = true
				break
			}
		}
	}
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("incident %q not found", rel))
		return
	}

	switch subAction {
	case "postmortem":
		md := BuildPostMortemMarkdown(inc)
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"postmortem-%s.md\"", inc.ID))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(md))
		return
	case "compare":
		writeJSON(w, http.StatusOK, BuildBaselineComparison(inc))
		return
	case "security":
		writeJSON(w, http.StatusOK, inc.Security)
		return
	case "runbook":
		writeJSON(w, http.StatusOK, inc.Runbook)
		return
	case "explain":
		var req ExplainRequest
		if r.Method == http.MethodPost && r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		if req.LLMEndpoint == "" {
			req.LLMEndpoint = r.URL.Query().Get("llm_endpoint")
		}
		if req.Model == "" {
			req.Model = r.URL.Query().Get("model")
		}
		writeJSON(w, http.StatusOK, BuildCopilotExplanation(r.Context(), inc, req))
		return
	case "bundle":
		tgz, err := BuildIncidentBundleTarGz(inc)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("build bundle: %v", err))
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"kubepilot-bundle-%s.tar.gz\"", inc.ID))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tgz)
		return
	default:
		writeJSON(w, http.StatusOK, inc)
	}
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

// --- Prometheus /metrics exporter (1A) --------------------------------------

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use GET")
		return
	}
	list := s.ListIncidents()
	s.mu.RLock()
	whSent := s.webhookSent
	whFailed := s.webhookFailed
	amRecv := s.alertmanagerReceived
	s.mu.RUnlock()

	var diagCount, unknownCount int
	type causeKey struct {
		ns, cause, workload string
	}
	causeTotals := make(map[causeKey]int)
	lastConf := make(map[causeKey]float64)

	type costKey struct {
		ns, workload string
	}
	wasteByWorkload := make(map[costKey]float64)
	allocByWorkload := make(map[costKey]float64)

	for _, inc := range list {
		ns := inc.Namespace
		if ns == "" {
			ns = "genai"
		}
		if inc.Report.Status == snapshot.StatusDiagnosed && len(inc.Report.TopCauses) > 0 {
			diagCount++
			top := inc.Report.TopCauses[0]
			wl := top.Component.Workload
			if wl == "" {
				wl = "unknown"
			}
			ck := causeKey{ns: ns, cause: string(top.Cause), workload: wl}
			causeTotals[ck]++
			if _, exists := lastConf[ck]; !exists {
				lastConf[ck] = top.Confidence
			}
		} else {
			unknownCount++
		}
		if inc.Cost != nil {
			wl := inc.Cost.Workload
			if wl == "" {
				wl = "namespace/" + ns
			}
			wk := costKey{ns: ns, workload: wl}
			wasteByWorkload[wk] += inc.Cost.EstimatedWasteCost
			allocByWorkload[wk] += inc.Cost.TotalWindowCost
		}
	}

	var b strings.Builder
	b.WriteString("# HELP kubepilot_build_info Build and operating mode metadata for KubePilot.\n")
	b.WriteString("# TYPE kubepilot_build_info gauge\n")
	b.WriteString("kubepilot_build_info{mode=\"read-only\",engine=\"deterministic-go\"} 1\n\n")

	b.WriteString("# HELP kubepilot_windows_inspected_total Number of telemetry windows currently loaded in the KubePilot store.\n")
	b.WriteString("# TYPE kubepilot_windows_inspected_total gauge\n")
	fmt.Fprintf(&b, "kubepilot_windows_inspected_total{status=\"diagnosed\"} %d\n", diagCount)
	fmt.Fprintf(&b, "kubepilot_windows_inspected_total{status=\"unknown\"} %d\n\n", unknownCount)

	b.WriteString("# HELP kubepilot_incidents_by_cause_total Diagnosed incidents grouped by namespace, catalog cause, and workload.\n")
	b.WriteString("# TYPE kubepilot_incidents_by_cause_total gauge\n")
	for k, cnt := range causeTotals {
		fmt.Fprintf(&b, "kubepilot_incidents_by_cause_total{namespace=%q,cause=%q,workload=%q} %d\n",
			k.ns, k.cause, k.workload, cnt)
	}
	b.WriteString("\n")

	b.WriteString("# HELP kubepilot_last_incident_confidence Confidence score (0..1) of the most recent incident for each cause.\n")
	b.WriteString("# TYPE kubepilot_last_incident_confidence gauge\n")
	for k, conf := range lastConf {
		fmt.Fprintf(&b, "kubepilot_last_incident_confidence{namespace=%q,cause=%q,workload=%q} %.4f\n",
			k.ns, k.cause, k.workload, conf)
	}
	b.WriteString("\n")

	b.WriteString("# HELP kubepilot_incident_waste_dollars_total Cumulative OpenCost estimated incident waste in USD.\n")
	b.WriteString("# TYPE kubepilot_incident_waste_dollars_total gauge\n")
	for k, val := range wasteByWorkload {
		fmt.Fprintf(&b, "kubepilot_incident_waste_dollars_total{namespace=%q,workload=%q} %.6f\n",
			k.ns, k.workload, val)
	}
	b.WriteString("\n")

	b.WriteString("# HELP kubepilot_window_allocation_dollars_total Cumulative OpenCost total window resource cost in USD.\n")
	b.WriteString("# TYPE kubepilot_window_allocation_dollars_total gauge\n")
	for k, val := range allocByWorkload {
		fmt.Fprintf(&b, "kubepilot_window_allocation_dollars_total{namespace=%q,workload=%q} %.6f\n",
			k.ns, k.workload, val)
	}
	b.WriteString("\n")

	b.WriteString("# HELP kubepilot_webhook_notifications_total Total webhook alert deliveries attempted.\n")
	b.WriteString("# TYPE kubepilot_webhook_notifications_total counter\n")
	fmt.Fprintf(&b, "kubepilot_webhook_notifications_total{status=\"sent\"} %d\n", whSent)
	fmt.Fprintf(&b, "kubepilot_webhook_notifications_total{status=\"failed\"} %d\n\n", whFailed)

	b.WriteString("# HELP kubepilot_alertmanager_webhooks_received_total Total Prometheus Alertmanager firing webhooks received for automatic triage.\n")
	b.WriteString("# TYPE kubepilot_alertmanager_webhooks_received_total counter\n")
	fmt.Fprintf(&b, "kubepilot_alertmanager_webhooks_received_total %d\n", amRecv)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// --- Webhook Alerting (1C) --------------------------------------------------

// WebhookAlertPayload is the JSON body POSTed to a configured webhook endpoint
// (compatible with Slack/Discord incoming webhooks via `text` + `content` and
// with custom SRE / Alertmanager receivers via `incident`).
type WebhookAlertPayload struct {
	Text      string         `json:"text"`
	Content   string         `json:"content"`
	Timestamp time.Time      `json:"timestamp"`
	Service   string         `json:"service"`
	Mode      string         `json:"mode"`
	Incident  IncidentRecord `json:"incident"`
}

// SendWebhookAlert sends a JSON alert for rec to targetURL.
func (s *Server) SendWebhookAlert(ctx context.Context, rec IncidentRecord, targetURL string) error {
	targetURL = strings.TrimSpace(targetURL)
	if targetURL == "" {
		return fmt.Errorf("empty webhook URL")
	}

	summaryText := fmt.Sprintf("[KubePilot] Incident %s in namespace %s: status=%s",
		rec.ID, rec.Namespace, rec.Report.Status)
	if len(rec.Report.TopCauses) > 0 {
		top := rec.Report.TopCauses[0]
		wasteStr := ""
		if rec.Cost != nil && rec.Cost.EstimatedWasteCost > 0 {
			wasteStr = fmt.Sprintf(" | Waste: $%.4f %s", rec.Cost.EstimatedWasteCost, rec.Cost.Currency)
		}
		summaryText = fmt.Sprintf("[KubePilot Alert] %s (%.0f%% confidence) on %s in ns/%s — %s%s",
			top.Cause, top.Confidence*100, top.Component.Workload, rec.Namespace, top.Summary, wasteStr)
	}

	compactRec := rec
	compactRec.Telemetry = nil

	payload := WebhookAlertPayload{
		Text:      summaryText,
		Content:   summaryText,
		Timestamp: time.Now().UTC(),
		Service:   "kubepilot-server",
		Mode:      "read-only",
		Incident:  compactRec,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(raw))
	if err != nil {
		s.mu.Lock()
		s.webhookFailed++
		s.mu.Unlock()
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		s.mu.Lock()
		s.webhookFailed++
		s.mu.Unlock()
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		s.mu.Lock()
		s.webhookFailed++
		s.mu.Unlock()
		return fmt.Errorf("webhook HTTP %d", resp.StatusCode)
	}

	s.mu.Lock()
	s.webhookSent++
	s.mu.Unlock()
	return nil
}

// WebhookTestRequest is accepted by POST /api/v1/webhook/test.
type WebhookTestRequest struct {
	IncidentID string `json:"incidentId"`
	WebhookURL string `json:"webhookUrl,omitempty"`
}

func (s *Server) handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed; use POST")
		return
	}
	var req WebhookTestRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	targetURL := strings.TrimSpace(req.WebhookURL)
	if targetURL == "" {
		targetURL = s.cfg.WebhookURL
	}
	if targetURL == "" {
		writeError(w, http.StatusBadRequest, "no webhookUrl provided and --webhook-url not configured on server")
		return
	}

	var inc IncidentRecord
	if req.IncidentID != "" {
		found, ok := s.GetIncident(req.IncidentID)
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Sprintf("incident %q not found", req.IncidentID))
			return
		}
		inc = found
	} else {
		list := s.ListIncidents()
		if len(list) == 0 {
			writeError(w, http.StatusBadRequest, "no incidents loaded to send")
			return
		}
		inc = list[0]
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.SendWebhookAlert(ctx, inc, targetURL); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("webhook delivery failed: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "sent",
		"incidentId": inc.ID,
		"webhookUrl": targetURL,
	})
}

// --- Read-Only GitOps Patch Suggestion & Post-Mortem Markdown (1B) ----------

func buildSuggestedPatch(snap *snapshot.Snapshot, rep snapshot.Report, ns string) string {
	if rep.Status != snapshot.StatusDiagnosed || len(rep.TopCauses) == 0 {
		return ""
	}
	top := rep.TopCauses[0]
	wl := top.Component.Workload
	deployName := strings.TrimPrefix(wl, "deployment/")
	if deployName == "" || deployName == wl {
		deployName = "model-server"
	}
	containerName := deployName

	switch top.Cause {
	case snapshot.CauseCPUThrottling:
		return fmt.Sprintf(`# Read-Only GitOps Strategic Merge Patch (kubectl apply -f patch.yaml)
# Cause: CFS CPU quota throttling on %s
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      containers:
        - name: %s
          resources:
            requests:
              cpu: "2000m"
            limits:
              cpu: "2000m"
`, wl, deployName, ns, containerName)

	case snapshot.CauseOOMKilled:
		recMem := "1536Mi"
		if deployName == "chat-api" {
			recMem = "256Mi"
		}
		return fmt.Sprintf(`# Read-Only GitOps Strategic Merge Patch (kubectl apply -f patch.yaml)
# Cause: Container OOMKilled (Exit Code 137) on %s
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      containers:
        - name: %s
          resources:
            requests:
              memory: "768Mi"
            limits:
              memory: %q
`, wl, deployName, ns, containerName, recMem)

	case snapshot.CauseReadinessFail:
		return fmt.Sprintf(`# Read-Only GitOps Strategic Merge Patch (kubectl apply -f patch.yaml)
# Cause: Failing or delayed readinessProbe on %s
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      containers:
        - name: %s
          readinessProbe:
            httpGet:
              path: /health
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 5
`, wl, deployName, ns, containerName)

	case snapshot.CauseUnschedulable:
		return fmt.Sprintf(`# Read-Only GitOps Strategic Merge Patch (kubectl apply -f patch.yaml)
# Cause: Pod Pending / Unschedulable on %s
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      nodeSelector: null
      tolerations: []
      containers:
        - name: %s
          resources:
            requests:
              cpu: "1000m"
              memory: "768Mi"
`, wl, deployName, ns, containerName)

	case snapshot.CauseColdStart:
		return fmt.Sprintf(`# Read-Only GitOps Strategic Merge Patch (kubectl apply -f patch.yaml)
# Cause: Slow initContainer model weight fetch / image pull on %s
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      initContainers: []
      containers:
        - name: %s
          imagePullPolicy: IfNotPresent
`, wl, deployName, ns, containerName)

	case snapshot.CauseConfigChange:
		return fmt.Sprintf(`# Read-Only GitOps Strategic Merge Patch (kubectl apply -f patch.yaml)
# Cause: Serving runtime flag regression (--threads) on %s
# Recommended action: Roll back Deployment or restore optimal thread count:
#   kubectl rollout undo deployment/%s -n %s
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      containers:
        - name: %s
          args:
            - "--model"
            - "/models/tinyllama.gguf"
            - "--threads"
            - "4"
`, wl, deployName, ns, deployName, ns, containerName)

	case snapshot.CauseHostContention:
		return fmt.Sprintf(`# Read-Only GitOps Strategic Merge Patch (kubectl apply -f patch.yaml)
# Cause: Worker node CPU contention (Linux PSI) from noisy neighbor pod
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  template:
    spec:
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                topologyKey: kubernetes.io/hostname
`, deployName, ns)

	case snapshot.CauseUnknownNoInfra:
		return fmt.Sprintf(`# Read-Only GitOps Strategic Merge Patch (kubectl apply -f patch.yaml)
# Cause: Application pre-inference delay inside %s (before GenAI client span)
apiVersion: apps/v1
kind: Deployment
metadata:
  name: chat-api
  namespace: %s
spec:
  template:
    spec:
      containers:
        - name: chat-api
          env:
            - name: CHAT_API_PRE_INFERENCE_DELAY_MS
              value: "0"
            - name: CHAT_API_DELAY_PROBABILITY
              value: "0"
`, wl, ns)
	}
	return ""
}

// BuildPostMortemMarkdown renders a structured SRE Post-Mortem Markdown report
// for an IncidentRecord.
func BuildPostMortemMarkdown(inc IncidentRecord) string {
	var b strings.Builder
	rep := inc.Report
	win := rep.Window

	fmt.Fprintf(&b, "# KubePilot Incident Post-Mortem Report: `%s`\n\n", inc.ID)
	fmt.Fprintf(&b, "- **Diagnosis Status**: `%s`\n", rep.Status)
	fmt.Fprintf(&b, "- **Target Namespace**: `%s`\n", inc.Namespace)
	fmt.Fprintf(&b, "- **Analysis Window (UTC)**: `%s` → `%s`\n",
		win.From.UTC().Format(time.RFC3339), win.To.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Correlated Signals**: `%d` OTel GenAI traces, `%d` Kubernetes events, `%d` pods\n",
		inc.TraceCount, inc.EventCount, inc.PodCount)
	b.WriteString("- **Engine Mode**: Deterministic Go Rule Engine (`0` LLM calls, Read-Only RBAC)\n\n")

	if rep.Status != snapshot.StatusDiagnosed || len(rep.TopCauses) == 0 {
		b.WriteString("## Executive Summary\n\n")
		b.WriteString("No infrastructure or application fault was detected in the target namespace (`0` false positives).\n")
		if rep.Reason != "" {
			fmt.Fprintf(&b, "\n> %s\n", rep.Reason)
		}
		return b.String()
	}

	top := rep.TopCauses[0]
	b.WriteString("## 1. Executive Summary & Root Cause\n\n")
	fmt.Fprintf(&b, "- **Primary Root Cause**: `%s` (Rule: `%s`)\n", top.Cause, top.Rule)
	fmt.Fprintf(&b, "- **Confidence Score**: **%.0f%%** (`%.2f`)\n", top.Confidence*100, top.Confidence)
	fmt.Fprintf(&b, "- **Blamed Component**: `namespace=%s` `workload=%s` `pod=%s`\n",
		top.Component.Namespace, top.Component.Workload, top.Component.Pod)
	fmt.Fprintf(&b, "- **Finding Summary**: %s\n\n", top.Summary)

	b.WriteString("## 2. Correlated Multi-Signal Evidence Timeline\n\n")
	b.WriteString("| Signal Source | Timestamp (UTC) | Correlated Proof |\n")
	b.WriteString("| :--- | :--- | :--- |\n")
	for _, ev := range top.Evidence {
		ts := "-"
		if !ev.Timestamp.IsZero() {
			ts = ev.Timestamp.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", ev.Source, ts, ev.Summary)
	}
	b.WriteString("\n")

	if inc.Cost != nil {
		b.WriteString("## 3. OpenCost Resource & Operational FinOps 2.0 Analysis\n\n")
		fmt.Fprintf(&b, "- **Total Window Resource Cost**: `$%.4f %s` (CPU: `$%.4f`, RAM: `$%.4f`)\n",
			inc.Cost.TotalWindowCost, inc.Cost.Currency, inc.Cost.CPUCost, inc.Cost.RAMCost)
		fmt.Fprintf(&b, "- **Estimated Incident Waste (Window)**: **`$%.4f %s`** (Source: `%s`)\n",
			inc.Cost.EstimatedWasteCost, inc.Cost.Currency, inc.Cost.Source)
		fmt.Fprintf(&b, "- **30-Day Monthly Projected Cost / Waste**: `$%.2f / mo` total (`$%.2f / mo` projected waste if unmitigated)\n",
			inc.Cost.MonthlyProjectedCost, inc.Cost.MonthlyProjectedWaste)
		if inc.Cost.CostPer1kRequestsUSD > 0 || inc.Cost.CostPer1MTokensUSD > 0 {
			fmt.Fprintf(&b, "- **GenAI Unit Economics**: `$%.4f / 1k requests` | `$%.4f / 1M output tokens`\n",
				inc.Cost.CostPer1kRequestsUSD, inc.Cost.CostPer1MTokensUSD)
		}
		fmt.Fprintf(&b, "- **Suggested Patch Monthly Cost Delta**: `%+.2f %s / mo`\n",
			inc.Cost.PatchMonthlyDeltaUSD, inc.Cost.Currency)
		if inc.Cost.FinOpsRecommendation != "" {
			fmt.Fprintf(&b, "- **FinOps Trade-Off Decision**: %s\n", inc.Cost.FinOpsRecommendation)
		}
		fmt.Fprintf(&b, "- **Model Explanation**: %s\n\n", inc.Cost.Explanation)
	}

	if inc.SuggestedPatch != "" {
		b.WriteString("## 4. Suggested Read-Only GitOps Remediation Patch\n\n")
		b.WriteString("```yaml\n")
		b.WriteString(strings.TrimSpace(inc.SuggestedPatch))
		b.WriteString("\n```\n\n")
	}

	if inc.Security != nil {
		b.WriteString("## 5. DevSecOps & Pod Security Standards (PSS) Audit\n\n")
		fmt.Fprintf(&b, "- **Security Posture Score**: **%d/100** (`%s`)\n", inc.Security.Score, inc.Security.Profile)
		fmt.Fprintf(&b, "- **Patch Governance**: %s\n\n", inc.Security.PatchApprovalNote)
		b.WriteString("| Check ID | Category | Status | Detail |\n")
		b.WriteString("| :--- | :--- | :--- | :--- |\n")
		for _, c := range inc.Security.Checks {
			fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s |\n", c.ID, c.Category, c.Status, c.Detail)
		}
		b.WriteString("\n")
	}

	if inc.Runbook != nil {
		b.WriteString("## 6. Multi-Signal Coverage & Controlled SRE Runbook\n\n")
		fmt.Fprintf(&b, "- **Signal Coverage**: **%d%%** (`%s`)\n", inc.Runbook.SignalCoveragePct, inc.Runbook.EngineMode)
		fmt.Fprintf(&b, "- **Executive Brief**: %s\n", inc.Runbook.ExecutiveBrief)
		if len(inc.Runbook.AlternativeHypotheses) > 0 {
			b.WriteString("- **Ruled-Out Alternative Hypotheses**:\n")
			for _, h := range inc.Runbook.AlternativeHypotheses {
				fmt.Fprintf(&b, "  - %s\n", h)
			}
		}
		b.WriteString("\n### Controlled Remediation Steps (Human-in-the-Loop)\n\n")
		for _, s := range inc.Runbook.Steps {
			approval := "Read-Only Safe"
			if s.RequiresApproval {
				approval = "Requires Human GitOps Approval"
			}
			fmt.Fprintf(&b, "%d. **[%s — %s]** %s\n   - Command: `%s`\n   - Rationale: %s\n",
				s.Step, s.Phase, approval, s.Title, s.Command, s.Rationale)
		}
		b.WriteString("\n")
	}

	b.WriteString("## 7. CLI Reproduction\n\n")
	b.WriteString("```bash\n")
	if inc.RunID != "" {
		split := "dev"
		if strings.HasPrefix(inc.RunID, "test--") {
			split = "test"
		}
		fmt.Fprintf(&b, "./bin/why-slow --run-dir eval/runs/%s/%s --format text\n", split, inc.RunID)
	} else {
		fmt.Fprintf(&b, "./bin/why-slow --namespace %s --from %q --to %q\n",
			inc.Namespace, win.From.UTC().Format(time.RFC3339), win.To.UTC().Format(time.RFC3339))
	}
	b.WriteString("```\n")
	return b.String()
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
