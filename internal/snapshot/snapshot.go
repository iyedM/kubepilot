// Package snapshot defines the flat, JSON-serializable data structures that
// connect collectors (offline run exports or live cluster/Tempo/Prometheus
// queries) to the pure rule engine.
//
// No OpenTelemetry GenAI attribute or metric names ("gen_ai.*") may appear in
// this package; they belong exclusively to internal/genai.
package snapshot

import "time"

// Cause identifies one of the closed catalog causes (eval/faults/catalog.v1.json).
type Cause string

const (
	CauseCPUThrottling  Cause = "cpu_throttling"
	CauseOOMKilled      Cause = "oom_killed"
	CauseUnschedulable  Cause = "unschedulable"
	CauseReadinessFail  Cause = "readiness_failing"
	CauseColdStart      Cause = "cold_start"
	CauseConfigChange   Cause = "config_change"
	CauseHostContention Cause = "host_cpu_contention"
	CauseUnknownNoInfra Cause = "unknown_no_infrastructure_cause"
)

// AllCatalogCauses returns the closed list of v1 catalog causes.
func AllCatalogCauses() []Cause {
	return []Cause{
		CauseCPUThrottling,
		CauseOOMKilled,
		CauseUnschedulable,
		CauseReadinessFail,
		CauseColdStart,
		CauseConfigChange,
		CauseHostContention,
		CauseUnknownNoInfra,
	}
}

// TimeRange is a half-open [From, To) UTC interval.
type TimeRange struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason,omitempty"`
}

// Contains reports whether t falls within [r.From, r.To).
func (r TimeRange) Contains(t time.Time) bool {
	if r.From.IsZero() || r.To.IsZero() {
		return false
	}
	return !t.Before(r.From) && t.Before(r.To)
}

// Window defines the analysis time window, optional baseline reference window,
// excluded sub-windows (such as the lab reset outage [reset_start, lab_healthy_again]),
// and clock-skew tolerance applied when correlating across telemetry sources.
type Window struct {
	From          time.Time     `json:"from"`
	To            time.Time     `json:"to"`
	Baseline      *TimeRange    `json:"baseline,omitempty"`
	Excluded      []TimeRange   `json:"excluded,omitempty"`
	SkewTolerance time.Duration `json:"skewTolerance"`
}

// IsExcluded reports whether timestamp t falls inside any excluded range.
func (w Window) IsExcluded(t time.Time) bool {
	for _, ex := range w.Excluded {
		if ex.Contains(t) {
			return true
		}
	}
	return false
}

// InAnalysisWindow reports whether t is inside [w.From, w.To) and not excluded.
func (w Window) InAnalysisWindow(t time.Time) bool {
	if w.IsExcluded(t) {
		return false
	}
	return !t.Before(w.From) && t.Before(w.To)
}

// InBaseline reports whether t is inside the optional baseline range and not excluded.
func (w Window) InBaseline(t time.Time) bool {
	if w.Baseline == nil || w.IsExcluded(t) {
		return false
	}
	return w.Baseline.Contains(t)
}

// InWindowWithSkew reports whether timestamp t falls within
// [w.From - w.SkewTolerance, w.To + w.SkewTolerance] and is not excluded.
func (w Window) InWindowWithSkew(t time.Time) bool {
	if w.IsExcluded(t) {
		return false
	}
	from := w.From.Add(-w.SkewTolerance)
	to := w.To.Add(w.SkewTolerance)
	return !t.Before(from) && !t.After(to)
}

// Snapshot is the complete, self-contained input to the diagnosis rule engine.
type Snapshot struct {
	Window        Window            `json:"window"`
	TargetTraceID string            `json:"targetTraceId,omitempty"`
	Traces        []TraceSummary    `json:"traces,omitempty"`
	Requests      []RequestSample   `json:"requests,omitempty"`
	Pods          []PodSnapshot     `json:"pods,omitempty"`
	PodsBefore    []PodIdentity     `json:"podsBefore,omitempty"`
	Events        []K8sEvent        `json:"events,omitempty"`
	Metrics       map[string]Series `json:"metrics,omitempty"`
}

// TraceSummary is the normalized, privacy-safe representation of an
// OpenTelemetry trace containing a GenAI inference call (and optional parent
// HTTP server span). Prompt and completion contents are never stored here.
type TraceSummary struct {
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId,omitempty"`

	// Timestamps and durations derived from the HTTP server span (if present)
	// and the GenAI client span.
	StartTime         time.Time `json:"startTime"`
	EndTime           time.Time `json:"endTime"`
	HTTPDurationMs    float64   `json:"httpDurationMs,omitempty"`
	InferenceMs       float64   `json:"inferenceMs"`
	PreInferenceGapMs float64   `json:"preInferenceGapMs,omitempty"`
	HTTPStatusCode    int       `json:"httpStatusCode,omitempty"`

	// Normalized GenAI call metadata (populated by internal/genai).
	Operation         string   `json:"operation"`
	RequestModel      string   `json:"requestModel,omitempty"`
	ResponseModel     string   `json:"responseModel,omitempty"`
	ResponseID        string   `json:"responseId,omitempty"`
	FinishReasons     []string `json:"finishReasons,omitempty"`
	MaxTokens         int      `json:"maxTokens,omitempty"`
	InputTokens       int      `json:"inputTokens,omitempty"`
	OutputTokens      int      `json:"outputTokens,omitempty"`
	CachedInputTokens int      `json:"cachedInputTokens,omitempty"`
	SystemFingerprint string   `json:"systemFingerprint,omitempty"`

	// Serving backend endpoint (identified from server.address / server.port,
	// never from the SDK provider name).
	ServerAddress string `json:"serverAddress,omitempty"`
	ServerPort    int    `json:"serverPort,omitempty"`

	// Outcome of the GenAI span.
	Error     bool   `json:"error"`
	ErrorType string `json:"errorType,omitempty"`
	StatusMsg string `json:"statusMsg,omitempty"`

	// Kubernetes resource identity attached to the client application span.
	ClientPodName    string `json:"clientPodName,omitempty"`
	ClientPodUID     string `json:"clientPodUid,omitempty"`
	ClientNamespace  string `json:"clientNamespace,omitempty"`
	ClientNodeName   string `json:"clientNodeName,omitempty"`
	ClientDeployment string `json:"clientDeployment,omitempty"`
	ServiceName      string `json:"serviceName,omitempty"`
}

// RequestSample is one client-side request record (e.g. from loadgen.jsonl),
// which also captures failed requests when the application itself is down and
// no trace could be emitted.
type RequestSample struct {
	Timestamp       time.Time `json:"ts"`
	PromptID        string    `json:"promptId"`
	Status          int       `json:"status"`
	ClientLatencyMs float64   `json:"clientLatencyMs"`
	TraceID         string    `json:"traceId,omitempty"`
	Error           string    `json:"error,omitempty"`
	CompletionToks  int       `json:"completionTokens,omitempty"`
}

// PodIdentity records basic pod state before or after a window.
type PodIdentity struct {
	Name        string    `json:"name"`
	UID         string    `json:"uid,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	Node        string    `json:"node,omitempty"`
	Phase       string    `json:"phase,omitempty"`
	ContainerID string    `json:"containerId,omitempty"`
	Restarts    int32     `json:"restarts"`
	Created     time.Time `json:"created,omitempty"`
}

// PodSnapshot captures the spec and status of a pod during the fault window.
type PodSnapshot struct {
	Name              string              `json:"name"`
	UID               string              `json:"uid,omitempty"`
	Namespace         string              `json:"namespace"`
	Workload          string              `json:"workload,omitempty"`
	NodeName          string              `json:"nodeName,omitempty"`
	Phase             string              `json:"phase"`
	CreationTimestamp time.Time           `json:"creationTimestamp"`
	DeletionTimestamp *time.Time          `json:"deletionTimestamp,omitempty"`
	Labels            map[string]string   `json:"labels,omitempty"`
	NodeSelector      map[string]string   `json:"nodeSelector,omitempty"`
	Volumes           []VolumeSpec        `json:"volumes,omitempty"`
	Conditions        []PodCondition      `json:"conditions,omitempty"`
	InitContainers    []ContainerSnapshot `json:"initContainers,omitempty"`
	Containers        []ContainerSnapshot `json:"containers,omitempty"`
}

// VolumeSpec summarizes a pod volume (e.g. PVC claim name or hostPath).
type VolumeSpec struct {
	Name     string `json:"name"`
	PVCName  string `json:"pvcName,omitempty"`
	HostPath string `json:"hostPath,omitempty"`
	EmptyDir bool   `json:"emptyDir,omitempty"`
}

// PodCondition represents one entry in pod.status.conditions.
type PodCondition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime,omitempty"`
}

// ContainerSnapshot merges container spec and status fields needed by rules.
type ContainerSnapshot struct {
	Name              string          `json:"name"`
	Image             string          `json:"image"`
	ImageID           string          `json:"imageId,omitempty"`
	IsInit            bool            `json:"isInit,omitempty"`
	Command           []string        `json:"command,omitempty"`
	Args              []string        `json:"args,omitempty"`
	Ports             []ContainerPort `json:"ports,omitempty"`
	RequestsCPUMillis int64           `json:"requestsCpuMillis,omitempty"`
	LimitsCPUMillis   int64           `json:"limitsCpuMillis,omitempty"`
	RequestsMemoryB   int64           `json:"requestsMemoryBytes,omitempty"`
	LimitsMemoryB     int64           `json:"limitsMemoryBytes,omitempty"`
	ReadinessProbe    *ProbeSpec      `json:"readinessProbe,omitempty"`
	LivenessProbe     *ProbeSpec      `json:"livenessProbe,omitempty"`
	StartupProbe      *ProbeSpec      `json:"startupProbe,omitempty"`
	Ready             bool            `json:"ready"`
	RestartCount      int32           `json:"restartCount"`
	State             ContainerState  `json:"state"`
	LastTerminated    *ContainerState `json:"lastTerminated,omitempty"`
}

// ContainerPort describes a named or numbered container port.
type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int32  `json:"containerPort"`
}

// ProbeSpec describes an HTTP/TCP/Exec container probe.
type ProbeSpec struct {
	Path                string `json:"path,omitempty"`
	Port                string `json:"port,omitempty"`
	InitialDelaySeconds int32  `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int32  `json:"periodSeconds,omitempty"`
}

// ContainerState describes a running, waiting, or terminated container state.
type ContainerState struct {
	Kind       string    `json:"kind"` // "running", "waiting", "terminated"
	Reason     string    `json:"reason,omitempty"`
	Message    string    `json:"message,omitempty"`
	ExitCode   int32     `json:"exitCode,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

// K8sEvent is a normalized Kubernetes Event.
type K8sEvent struct {
	Timestamp      time.Time `json:"timestamp"`
	FirstTimestamp time.Time `json:"firstTimestamp,omitempty"`
	LastTimestamp  time.Time `json:"lastTimestamp,omitempty"`
	Namespace      string    `json:"namespace"`
	InvolvedKind   string    `json:"involvedKind"`
	InvolvedName   string    `json:"involvedName"`
	Type           string    `json:"type"`
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	Count          int32     `json:"count,omitempty"`
}

// Series holds a named Prometheus query result over the snapshot window.
type Series struct {
	Query   string         `json:"query,omitempty"`
	Streams []MetricStream `json:"streams,omitempty"`
}

// MetricStream is one labeled time series in a Prometheus range result.
type MetricStream struct {
	Labels  map[string]string `json:"labels"`
	Samples []MetricSample    `json:"samples"`
}

// MetricSample is a (timestamp, float64 value) pair.
type MetricSample struct {
	Timestamp time.Time `json:"ts"`
	Value     float64   `json:"value"`
}

// Component identifies the Kubernetes object blamed by a Finding.
type Component struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Workload  string `json:"workload,omitempty"`
	Pod       string `json:"pod,omitempty"`
	Node      string `json:"node,omitempty"`
}

// EvidenceSource names the telemetry signal where an EvidenceItem came from.
type EvidenceSource string

const (
	EvidenceTrace      EvidenceSource = "trace"
	EvidenceK8sEvent   EvidenceSource = "k8s_event"
	EvidenceK8sPod     EvidenceSource = "k8s_pod"
	EvidencePrometheus EvidenceSource = "prometheus"
	EvidenceRequestLog EvidenceSource = "request_log"
)

// EvidenceItem is one timestamped piece of proof supporting a Finding.
type EvidenceItem struct {
	Source    EvidenceSource `json:"source"`
	Timestamp time.Time      `json:"timestamp,omitempty"`
	Summary   string         `json:"summary"`
}

// Finding is one candidate cause produced by a diagnosis rule.
type Finding struct {
	Rule       string         `json:"rule"`
	Cause      Cause          `json:"cause"`
	Confidence float64        `json:"confidence"`
	Component  Component      `json:"component"`
	Summary    string         `json:"summary"`
	Evidence   []EvidenceItem `json:"evidence"`
}

// ReportStatus indicates whether the engine found a probable cause or abstained.
type ReportStatus string

const (
	StatusDiagnosed ReportStatus = "diagnosed"
	StatusUnknown   ReportStatus = "unknown"
)

// Report is the top-level diagnosis output (at most top-3 causes, or "unknown").
type Report struct {
	Status    ReportStatus `json:"status"`
	Reason    string       `json:"reason,omitempty"`
	Window    Window       `json:"window"`
	TopCauses []Finding    `json:"topCauses,omitempty"`
}
