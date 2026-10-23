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

// IncidentRecord wraps a snapshot.Report with metadata, trace/event counts, and
// optional cost estimation for the API, Web UI (P6), and Operator (P7).
type IncidentRecord struct {
	ID         string          `json:"id"`
	CreatedAt  time.Time       `json:"createdAt"`
	Source     string          `json:"source"` // "live", "poll", "run-dir", "snapshot"
	RunID      string          `json:"runId,omitempty"`
	Namespace  string          `json:"namespace"`
	TraceID    string          `json:"traceId,omitempty"`
	Report     snapshot.Report `json:"report"`
	TraceCount int             `json:"traceCount"`
	EventCount int             `json:"eventCount"`
	PodCount   int             `json:"podCount"`
	Cost       *CostSummary    `json:"cost,omitempty"`
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
	rec := IncidentRecord{
		CreatedAt:  time.Now().UTC(),
		Source:     source,
		RunID:      runID,
		Namespace:  s.cfg.LiveConfig.Namespace,
		TraceID:    snap.TargetTraceID,
		Report:     rep,
		TraceCount: len(snap.Traces),
		EventCount: len(snap.Events),
		PodCount:   len(snap.Pods),
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
