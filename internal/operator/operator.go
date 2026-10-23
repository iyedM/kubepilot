// Package operator implements the Phase P7 read-only GenAIIncident Custom
// Resource controller.
//
// In strict compliance with CLAUDE.md, this operator is read-only on all
// cluster workloads (no pod/deployment patching, restarting, or autoscaling).
// Its only write operations target the kubepilot.io/v1alpha1 GenAIIncident
// custom resource and its /status subresource so users and GitOps/alerting
// pipelines can inspect diagnosed incidents via `kubectl get genaiincidents`.
package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/collector/live"
	"github.com/iyedM/kubepilot/internal/rules"
	"github.com/iyedM/kubepilot/internal/server"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

const (
	// Group is the API group for KubePilot custom resources.
	Group = "kubepilot.io"
	// Version is the API version for GenAIIncident.
	Version = "v1alpha1"
	// Kind is the CustomResource kind name.
	Kind = "GenAIIncident"
	// APIVersion is the full group/version string.
	APIVersion = Group + "/" + Version
)

// ObjectMeta is the subset of Kubernetes metav1.ObjectMeta used by GenAIIncident.
type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	CreationTimestamp string            `json:"creationTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
}

// GenAIIncidentSpec defines the desired investigation parameters for a GenAIIncident CR.
type GenAIIncidentSpec struct {
	TargetNamespace string `json:"targetNamespace,omitempty"`
	Workload        string `json:"workload,omitempty"`
	TraceID         string `json:"traceId,omitempty"`
	WindowFrom      string `json:"windowFrom,omitempty"`
	WindowTo        string `json:"windowTo,omitempty"`
}

// GenAIIncidentStatus records the deterministic diagnosis output on the CR status subresource.
type GenAIIncidentStatus struct {
	DiagnosisStatus   string             `json:"diagnosisStatus"`
	Reason            string             `json:"reason,omitempty"`
	TopCause          string             `json:"topCause,omitempty"`
	TopConfidence     float64            `json:"topConfidence,omitempty"`
	Workload          string             `json:"workload,omitempty"`
	EvaluatedAt       string             `json:"evaluatedAt"`
	EstimatedWasteUSD float64            `json:"estimatedWasteUSD,omitempty"`
	TopCauses         []snapshot.Finding `json:"topCauses,omitempty"`
}

// GenAIIncident is the kubepilot.io/v1alpha1 GenAIIncident Custom Resource.
type GenAIIncident struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   ObjectMeta          `json:"metadata"`
	Spec       GenAIIncidentSpec   `json:"spec"`
	Status     GenAIIncidentStatus `json:"status,omitempty"`
}

// GenAIIncidentList is a Kubernetes list of GenAIIncident resources.
type GenAIIncidentList struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Items      []GenAIIncident `json:"items"`
}

// Config configures the GenAIIncident operator controller.
type Config struct {
	KubeAPIURL    string
	BearerToken   string
	Namespace     string
	LiveConfig    live.Config
	Collect       server.CollectFunc
	CostEstimator server.CostEstimator
	HTTPClient    *http.Client
}

// Controller reconciles GenAIIncident custom resources in read-only mode on workloads.
type Controller struct {
	cfg Config
}

// NewController creates a new GenAIIncident Controller.
func NewController(cfg Config) *Controller {
	if cfg.Namespace == "" {
		cfg.Namespace = "genai"
	}
	if cfg.Collect == nil {
		cfg.Collect = live.Collect
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Controller{cfg: cfg}
}

// FromIncidentRecord converts a server.IncidentRecord into a GenAIIncident CR.
func FromIncidentRecord(rec server.IncidentRecord) GenAIIncident {
	ns := rec.Namespace
	if ns == "" {
		ns = "genai"
	}
	name := sanitizeK8sName(rec.ID)
	status := BuildStatus(rec.Report, rec.Cost, time.Now().UTC())
	workload := status.Workload
	return GenAIIncident{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata: ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kubepilot",
				"kubepilot.io/status":          string(rec.Report.Status),
			},
		},
		Spec: GenAIIncidentSpec{
			TargetNamespace: ns,
			Workload:        workload,
			TraceID:         rec.TraceID,
			WindowFrom:      rec.Report.Window.From.UTC().Format(time.RFC3339),
			WindowTo:        rec.Report.Window.To.UTC().Format(time.RFC3339),
		},
		Status: status,
	}
}

// BuildStatus builds a GenAIIncidentStatus from a snapshot.Report and optional CostSummary.
func BuildStatus(rep snapshot.Report, cost *server.CostSummary, evaluatedAt time.Time) GenAIIncidentStatus {
	st := GenAIIncidentStatus{
		DiagnosisStatus: string(rep.Status),
		Reason:          rep.Reason,
		EvaluatedAt:     evaluatedAt.UTC().Format(time.RFC3339),
		TopCauses:       rep.TopCauses,
	}
	if len(rep.TopCauses) > 0 {
		top := rep.TopCauses[0]
		st.TopCause = string(top.Cause)
		st.TopConfidence = top.Confidence
		st.Workload = top.Component.Workload
	}
	if cost != nil {
		st.EstimatedWasteUSD = cost.EstimatedWasteCost
		if st.Workload == "" {
			st.Workload = cost.Workload
		}
	}
	return st
}

// ReconcileOnce lists GenAIIncident resources in the configured namespace,
// evaluates any resource whose status.diagnosisStatus is empty, and updates
// its /status subresource.
func (c *Controller) ReconcileOnce(ctx context.Context) (int, error) {
	if c.cfg.KubeAPIURL == "" {
		return 0, fmt.Errorf("KubeAPIURL is required for CRD reconciliation")
	}

	listURL := fmt.Sprintf("%s/apis/%s/%s/namespaces/%s/genaiincidents",
		strings.TrimRight(c.cfg.KubeAPIURL, "/"),
		Group, Version, url.PathEscape(c.cfg.Namespace))

	raw, code, err := c.doJSON(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("GET %s: HTTP %d", listURL, code)
	}

	var list GenAIIncidentList
	if err := json.Unmarshal(raw, &list); err != nil {
		return 0, fmt.Errorf("decode GenAIIncidentList: %w", err)
	}

	reconciled := 0
	for _, item := range list.Items {
		if item.Status.DiagnosisStatus != "" {
			continue
		}
		if err := c.reconcileItem(ctx, item); err != nil {
			return reconciled, err
		}
		reconciled++
	}
	return reconciled, nil
}

func (c *Controller) reconcileItem(ctx context.Context, item GenAIIncident) error {
	lcfg := c.cfg.LiveConfig
	if item.Spec.TargetNamespace != "" {
		lcfg.Namespace = item.Spec.TargetNamespace
	} else if lcfg.Namespace == "" {
		lcfg.Namespace = c.cfg.Namespace
	}
	lcfg.TraceID = item.Spec.TraceID
	if item.Spec.WindowFrom != "" {
		if ts, err := time.Parse(time.RFC3339Nano, item.Spec.WindowFrom); err == nil {
			lcfg.From = ts
		}
	}
	if item.Spec.WindowTo != "" {
		if ts, err := time.Parse(time.RFC3339Nano, item.Spec.WindowTo); err == nil {
			lcfg.To = ts
		}
	}

	snap, err := c.cfg.Collect(ctx, lcfg)
	if err != nil {
		return fmt.Errorf("collect for GenAIIncident %s/%s: %w", item.Metadata.Namespace, item.Metadata.Name, err)
	}
	rep := rules.Diagnose(snap)

	var cost *server.CostSummary
	if c.cfg.CostEstimator != nil {
		cost, _ = c.cfg.CostEstimator.Estimate(ctx, snap, rep)
	}

	item.Status = BuildStatus(rep, cost, time.Now().UTC())
	if item.Status.Workload == "" && item.Spec.Workload != "" {
		item.Status.Workload = item.Spec.Workload
	}

	ns := item.Metadata.Namespace
	if ns == "" {
		ns = c.cfg.Namespace
	}
	statusURL := fmt.Sprintf("%s/apis/%s/%s/namespaces/%s/genaiincidents/%s/status",
		strings.TrimRight(c.cfg.KubeAPIURL, "/"),
		Group, Version, url.PathEscape(ns), url.PathEscape(item.Metadata.Name))

	_, code, err := c.doJSON(ctx, http.MethodPut, statusURL, item)
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return fmt.Errorf("PUT %s: HTTP %d", statusURL, code)
	}
	return nil
}

// PublishIncident creates or updates a GenAIIncident custom resource for a
// diagnosed server.IncidentRecord.
func (c *Controller) PublishIncident(ctx context.Context, rec server.IncidentRecord) (*GenAIIncident, error) {
	if c.cfg.KubeAPIURL == "" {
		return nil, fmt.Errorf("KubeAPIURL is required to publish GenAIIncident")
	}
	cr := FromIncidentRecord(rec)
	colURL := fmt.Sprintf("%s/apis/%s/%s/namespaces/%s/genaiincidents",
		strings.TrimRight(c.cfg.KubeAPIURL, "/"),
		Group, Version, url.PathEscape(cr.Metadata.Namespace))

	raw, code, err := c.doJSON(ctx, http.MethodPost, colURL, cr)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return nil, fmt.Errorf("POST %s: HTTP %d", colURL, code)
	}
	var out GenAIIncident
	if err := json.Unmarshal(raw, &out); err != nil {
		return &cr, nil
	}
	return &out, nil
}

func (c *Controller) doJSON(ctx context.Context, method, fullURL string, payload any) ([]byte, int, error) {
	// Safety guard: never allow mutating HTTP methods outside /apis/kubepilot.io/ (CLAUDE.md rule 4).
	if method != http.MethodGet && !strings.Contains(fullURL, "/apis/"+Group+"/") {
		return nil, 0, fmt.Errorf("refusing non-read-only request %s %s outside %s", method, fullURL, Group)
	}

	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	}

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBytes, err := io.ReadAll(resp.Body)
	return respBytes, resp.StatusCode, err
}

func sanitizeK8sName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "genai-incident"
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}
