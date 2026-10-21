// Package offline builds a snapshot.Snapshot from a recorded evaluation run
// directory (run.json + compressed telemetry exports).
package offline

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/genai"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

// DefaultSkewTolerance is the default clock-skew margin applied when
// correlating traces, Kubernetes events, and Prometheus samples.
const DefaultSkewTolerance = 30 * time.Second

// Options controls how LoadRunDir selects the analysis window and filters
// telemetry from a run directory.
type Options struct {
	From          time.Time
	To            time.Time
	TraceID       string
	SkewTolerance time.Duration
}

// RunMeta holds the ground-truth metadata parsed from run.json.
type RunMeta struct {
	SchemaVersion int             `json:"schema_version"`
	RunID         string          `json:"run_id"`
	Split         string          `json:"split"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status"`
	Timeline      RunTimeline     `json:"timeline"`
	Target        RunTarget       `json:"target"`
	Expected      []ExpectedCause `json:"expected_causes"`
}

// RunTimeline holds the UTC timestamps recorded by lab/faults/run.sh.
type RunTimeline struct {
	BaselineStart   string `json:"baseline_start"`
	InjectStart     string `json:"inject_start"`
	InjectDone      string `json:"inject_done"`
	ResetStart      string `json:"reset_start"`
	ResetDone       string `json:"reset_done"`
	LabHealthyAgain string `json:"lab_healthy_again"`
	End             string `json:"end"`
}

// RunTarget describes the target workload and its pods before/after the run.
type RunTarget struct {
	Namespace  string                 `json:"namespace"`
	Workload   string                 `json:"workload"`
	PodsBefore []snapshot.PodIdentity `json:"pods_before"`
	PodsAfter  []snapshot.PodIdentity `json:"pods_after"`
}

// ExpectedCause is one ground-truth entry in run.json.
type ExpectedCause struct {
	Cause      snapshot.Cause     `json:"cause"`
	Component  snapshot.Component `json:"component"`
	ActiveFrom string             `json:"active_from"`
	ActiveTo   string             `json:"active_to"`
}

// LoadRunDir reads a recorded run directory and returns a populated Snapshot
// together with the parsed RunMeta from run.json.
func LoadRunDir(runDir string, opts Options) (*snapshot.Snapshot, *RunMeta, error) {
	meta, err := loadRunMeta(filepath.Join(runDir, "run.json"))
	if err != nil {
		return nil, nil, err
	}

	skew := opts.SkewTolerance
	if skew <= 0 {
		skew = DefaultSkewTolerance
	}

	win, err := buildWindow(meta, opts, skew)
	if err != nil {
		return nil, nil, err
	}

	requests, err := loadRequests(runDir, win)
	if err != nil {
		return nil, nil, err
	}

	traces, err := loadTraces(runDir, win)
	if err != nil {
		return nil, nil, err
	}

	// If a specific TraceID was requested without explicit From/To bounds,
	// narrow the analysis window around that trace (or matching request).
	if opts.TraceID != "" && opts.From.IsZero() && opts.To.IsZero() {
		if tStart, tEnd, ok := findTraceBounds(opts.TraceID, traces, requests); ok {
			win.From = tStart.Add(-2 * time.Minute)
			win.To = tEnd.Add(30 * time.Second)
		}
	}

	pods, err := loadPods(runDir)
	if err != nil {
		return nil, nil, err
	}

	events, err := loadEvents(runDir, win)
	if err != nil {
		return nil, nil, err
	}

	metrics, err := loadMetrics(runDir, win)
	if err != nil {
		return nil, nil, err
	}

	snap := &snapshot.Snapshot{
		Window:        win,
		TargetTraceID: opts.TraceID,
		Traces:        traces,
		Requests:      requests,
		Pods:          pods,
		PodsBefore:    meta.Target.PodsBefore,
		Events:        events,
		Metrics:       metrics,
	}
	return snap, meta, nil
}

func loadRunMeta(path string) (*RunMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m RunMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &m, nil
}

func buildWindow(meta *RunMeta, opts Options, skew time.Duration) (snapshot.Window, error) {
	var win snapshot.Window
	win.SkewTolerance = skew

	// Always exclude [reset_start, lab_healthy_again] (or reset_done if
	// lab_healthy_again is absent) so the lab's Recreate reset outage is never
	// scored or blamed as a fault.
	resetStart := parseTime(meta.Timeline.ResetStart)
	resetEnd := parseTime(meta.Timeline.LabHealthyAgain)
	if resetEnd.IsZero() {
		resetEnd = parseTime(meta.Timeline.ResetDone)
	}
	if !resetStart.IsZero() && !resetEnd.IsZero() && resetEnd.After(resetStart) {
		win.Excluded = append(win.Excluded, snapshot.TimeRange{
			From:   resetStart,
			To:     resetEnd,
			Reason: "lab reset outage",
		})
	}

	baseStart := parseTime(meta.Timeline.BaselineStart)
	injectStart := parseTime(meta.Timeline.InjectStart)
	if !baseStart.IsZero() && !injectStart.IsZero() && injectStart.After(baseStart) {
		win.Baseline = &snapshot.TimeRange{
			From:   baseStart,
			To:     injectStart,
			Reason: "baseline",
		}
	}

	if !opts.From.IsZero() && !opts.To.IsZero() {
		win.From = opts.From.UTC()
		win.To = opts.To.UTC()
		return win, nil
	}

	win.From = injectStart
	if !resetStart.IsZero() {
		win.To = resetStart
	} else {
		win.To = parseTime(meta.Timeline.End)
	}
	if win.From.IsZero() || win.To.IsZero() {
		return win, fmt.Errorf("run %q has incomplete timeline in run.json (inject_start=%q, reset_start=%q)",
			meta.RunID, meta.Timeline.InjectStart, meta.Timeline.ResetStart)
	}
	return win, nil
}

func keepTimestamp(w snapshot.Window, t time.Time) bool {
	if t.IsZero() {
		return false
	}
	return w.InWindowWithSkew(t) || w.InBaseline(t)
}

func findTraceBounds(traceID string, traces []snapshot.TraceSummary, requests []snapshot.RequestSample) (time.Time, time.Time, bool) {
	for _, tr := range traces {
		if tr.TraceID == traceID && !tr.StartTime.IsZero() {
			end := tr.EndTime
			if end.IsZero() {
				end = tr.StartTime
			}
			return tr.StartTime, end, true
		}
	}
	for _, req := range requests {
		if req.TraceID == traceID && !req.Timestamp.IsZero() {
			dur := time.Duration(req.ClientLatencyMs * float64(time.Millisecond))
			return req.Timestamp.Add(-dur), req.Timestamp, true
		}
	}
	return time.Time{}, time.Time{}, false
}

// --- file helpers ------------------------------------------------------------

func openMaybeGzip(dir, baseName string) (io.ReadCloser, error) {
	gzPath := filepath.Join(dir, baseName+".gz")
	if f, err := os.Open(gzPath); err == nil {
		gz, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("gzip open %s: %w", gzPath, err)
		}
		return &gzipReadCloser{Reader: gz, file: f}, nil
	}
	plainPath := filepath.Join(dir, baseName)
	f, err := os.Open(plainPath)
	if err != nil {
		return nil, err
	}
	return f, nil
}

type gzipReadCloser struct {
	*gzip.Reader
	file *os.File
}

func (g *gzipReadCloser) Close() error {
	err1 := g.Reader.Close()
	err2 := g.file.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// --- loadgen.jsonl -----------------------------------------------------------

type rawLoadgenLine struct {
	TS               string  `json:"ts"`
	PromptID         string  `json:"prompt_id"`
	Status           int     `json:"status"`
	ClientLatencyMs  float64 `json:"client_latency_ms"`
	TraceID          *string `json:"trace_id"`
	Error            *string `json:"error"`
	CompletionTokens int     `json:"completion_tokens"`
}

func loadRequests(runDir string, win snapshot.Window) ([]snapshot.RequestSample, error) {
	rc, err := openMaybeGzip(runDir, "loadgen.jsonl")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var out []snapshot.RequestSample
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw rawLoadgenLine
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("parse loadgen.jsonl line: %w", err)
		}
		ts := parseTime(raw.TS)
		if !keepTimestamp(win, ts) {
			continue
		}
		sample := snapshot.RequestSample{
			Timestamp:       ts,
			PromptID:        raw.PromptID,
			Status:          raw.Status,
			ClientLatencyMs: raw.ClientLatencyMs,
			CompletionToks:  raw.CompletionTokens,
		}
		if raw.TraceID != nil {
			sample.TraceID = *raw.TraceID
		}
		if raw.Error != nil {
			sample.Error = *raw.Error
		}
		out = append(out, sample)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan loadgen.jsonl: %w", err)
	}
	return out, nil
}

// --- traces.jsonl ------------------------------------------------------------

func loadTraces(runDir string, win snapshot.Window) ([]snapshot.TraceSummary, error) {
	rc, err := openMaybeGzip(runDir, "traces.jsonl")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var out []snapshot.TraceSummary
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 128*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ts, err := genai.ParseOTLPTrace("", []byte(line))
		if err != nil {
			// A trace line may be empty or not yet ingested in Tempo; skip non-GenAI lines.
			continue
		}
		if !keepTimestamp(win, ts.StartTime) && !keepTimestamp(win, ts.EndTime) {
			continue
		}
		out = append(out, *ts)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan traces.jsonl: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartTime.Before(out[j].StartTime)
	})
	return out, nil
}

// --- pods.json ---------------------------------------------------------------

type rawPodList struct {
	Items []rawPod `json:"items"`
}

type rawPod struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		UID               string            `json:"uid"`
		CreationTimestamp string            `json:"creationTimestamp"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
		Labels            map[string]string `json:"labels"`
		OwnerReferences   []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		NodeName       string            `json:"nodeName"`
		NodeSelector   map[string]string `json:"nodeSelector"`
		Volumes        []rawVolume       `json:"volumes"`
		InitContainers []rawContainer    `json:"initContainers"`
		Containers     []rawContainer    `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase                 string               `json:"phase"`
		Conditions            []rawPodCondition    `json:"conditions"`
		InitContainerStatuses []rawContainerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []rawContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type rawVolume struct {
	Name                  string `json:"name"`
	PersistentVolumeClaim *struct {
		ClaimName string `json:"claimName"`
	} `json:"persistentVolumeClaim"`
	HostPath *struct {
		Path string `json:"path"`
	} `json:"hostPath"`
	EmptyDir map[string]any `json:"emptyDir"`
}

type rawContainer struct {
	Name      string        `json:"name"`
	Image     string        `json:"image"`
	Command   []string      `json:"command"`
	Args      []string      `json:"args"`
	Ports     []rawPort     `json:"ports"`
	Resources rawResources  `json:"resources"`
	Readiness *rawProbeSpec `json:"readinessProbe"`
	Liveness  *rawProbeSpec `json:"livenessProbe"`
	Startup   *rawProbeSpec `json:"startupProbe"`
}

type rawPort struct {
	Name          string `json:"name"`
	ContainerPort int32  `json:"containerPort"`
}

type rawResources struct {
	Requests map[string]string `json:"requests"`
	Limits   map[string]string `json:"limits"`
}

type rawProbeSpec struct {
	HTTPGet *struct {
		Path string `json:"path"`
		Port any    `json:"port"`
	} `json:"httpGet"`
	InitialDelaySeconds int32 `json:"initialDelaySeconds"`
	PeriodSeconds       int32 `json:"periodSeconds"`
}

type rawPodCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

type rawContainerStatus struct {
	Name         string          `json:"name"`
	ImageID      string          `json:"imageID"`
	Ready        bool            `json:"ready"`
	RestartCount int32           `json:"restartCount"`
	Resources    rawResources    `json:"resources"`
	State        rawStateWrapper `json:"state"`
	LastState    rawStateWrapper `json:"lastState"`
}

type rawStateWrapper struct {
	Running *struct {
		StartedAt string `json:"startedAt"`
	} `json:"running"`
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Terminated *struct {
		Reason     string `json:"reason"`
		Message    string `json:"message"`
		ExitCode   int32  `json:"exitCode"`
		StartedAt  string `json:"startedAt"`
		FinishedAt string `json:"finishedAt"`
	} `json:"terminated"`
}

func loadPods(runDir string) ([]snapshot.PodSnapshot, error) {
	rc, err := openMaybeGzip(runDir, "pods.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var list rawPodList
	if err := json.NewDecoder(rc).Decode(&list); err != nil {
		return nil, fmt.Errorf("parse pods.json: %w", err)
	}

	out := make([]snapshot.PodSnapshot, 0, len(list.Items))
	for _, rp := range list.Items {
		p := snapshot.PodSnapshot{
			Name:              rp.Metadata.Name,
			UID:               rp.Metadata.UID,
			Namespace:         rp.Metadata.Namespace,
			Workload:          inferWorkload(rp),
			NodeName:          rp.Spec.NodeName,
			Phase:             rp.Status.Phase,
			CreationTimestamp: parseTime(rp.Metadata.CreationTimestamp),
			Labels:            rp.Metadata.Labels,
			NodeSelector:      rp.Spec.NodeSelector,
		}
		if rp.Metadata.DeletionTimestamp != "" {
			dt := parseTime(rp.Metadata.DeletionTimestamp)
			if !dt.IsZero() {
				p.DeletionTimestamp = &dt
			}
		}
		for _, v := range rp.Spec.Volumes {
			vs := snapshot.VolumeSpec{Name: v.Name, EmptyDir: v.EmptyDir != nil}
			if v.PersistentVolumeClaim != nil {
				vs.PVCName = v.PersistentVolumeClaim.ClaimName
			}
			if v.HostPath != nil {
				vs.HostPath = v.HostPath.Path
			}
			p.Volumes = append(p.Volumes, vs)
		}
		for _, c := range rp.Status.Conditions {
			p.Conditions = append(p.Conditions, snapshot.PodCondition{
				Type:               c.Type,
				Status:             c.Status,
				Reason:             c.Reason,
				Message:            c.Message,
				LastTransitionTime: parseTime(c.LastTransitionTime),
			})
		}
		p.InitContainers = mergeContainers(rp.Spec.InitContainers, rp.Status.InitContainerStatuses, true)
		p.Containers = mergeContainers(rp.Spec.Containers, rp.Status.ContainerStatuses, false)
		out = append(out, p)
	}
	return out, nil
}

func inferWorkload(rp rawPod) string {
	if app := rp.Metadata.Labels["app.kubernetes.io/name"]; app != "" {
		return "deployment/" + app
	}
	for _, owner := range rp.Metadata.OwnerReferences {
		if owner.Kind == "ReplicaSet" && owner.Name != "" {
			if idx := strings.LastIndex(owner.Name, "-"); idx > 0 {
				return "deployment/" + owner.Name[:idx]
			}
		}
		if owner.Kind != "" && owner.Name != "" {
			return strings.ToLower(owner.Kind) + "/" + owner.Name
		}
	}
	return "pod/" + rp.Metadata.Name
}

func mergeContainers(specs []rawContainer, statuses []rawContainerStatus, isInit bool) []snapshot.ContainerSnapshot {
	statusByName := make(map[string]rawContainerStatus, len(statuses))
	for _, st := range statuses {
		statusByName[st.Name] = st
	}
	out := make([]snapshot.ContainerSnapshot, 0, len(specs))
	for _, sp := range specs {
		st := statusByName[sp.Name]
		reqCPU := sp.Resources.Requests["cpu"]
		if st.Resources.Requests["cpu"] != "" {
			reqCPU = st.Resources.Requests["cpu"]
		}
		limCPU := sp.Resources.Limits["cpu"]
		if st.Resources.Limits["cpu"] != "" {
			limCPU = st.Resources.Limits["cpu"]
		}
		reqMem := sp.Resources.Requests["memory"]
		if st.Resources.Requests["memory"] != "" {
			reqMem = st.Resources.Requests["memory"]
		}
		limMem := sp.Resources.Limits["memory"]
		if st.Resources.Limits["memory"] != "" {
			limMem = st.Resources.Limits["memory"]
		}

		cs := snapshot.ContainerSnapshot{
			Name:              sp.Name,
			Image:             sp.Image,
			ImageID:           st.ImageID,
			IsInit:            isInit,
			Command:           sp.Command,
			Args:              sp.Args,
			RequestsCPUMillis: ParseCPUMillis(reqCPU),
			LimitsCPUMillis:   ParseCPUMillis(limCPU),
			RequestsMemoryB:   ParseMemoryBytes(reqMem),
			LimitsMemoryB:     ParseMemoryBytes(limMem),
			ReadinessProbe:    convertProbe(sp.Readiness),
			LivenessProbe:     convertProbe(sp.Liveness),
			StartupProbe:      convertProbe(sp.Startup),
			Ready:             st.Ready,
			RestartCount:      st.RestartCount,
			State:             convertState(st.State),
		}
		for _, port := range sp.Ports {
			cs.Ports = append(cs.Ports, snapshot.ContainerPort{
				Name:          port.Name,
				ContainerPort: port.ContainerPort,
			})
		}
		if term := convertState(st.LastState); term.Kind != "" {
			cs.LastTerminated = &term
		}
		out = append(out, cs)
	}
	return out
}

func convertProbe(rp *rawProbeSpec) *snapshot.ProbeSpec {
	if rp == nil {
		return nil
	}
	ps := &snapshot.ProbeSpec{
		InitialDelaySeconds: rp.InitialDelaySeconds,
		PeriodSeconds:       rp.PeriodSeconds,
	}
	if rp.HTTPGet != nil {
		ps.Path = rp.HTTPGet.Path
		if rp.HTTPGet.Port != nil {
			ps.Port = fmt.Sprint(rp.HTTPGet.Port)
		}
	}
	return ps
}

func convertState(w rawStateWrapper) snapshot.ContainerState {
	switch {
	case w.Running != nil:
		return snapshot.ContainerState{
			Kind:      "running",
			StartedAt: parseTime(w.Running.StartedAt),
		}
	case w.Waiting != nil:
		return snapshot.ContainerState{
			Kind:    "waiting",
			Reason:  w.Waiting.Reason,
			Message: w.Waiting.Message,
		}
	case w.Terminated != nil:
		return snapshot.ContainerState{
			Kind:       "terminated",
			Reason:     w.Terminated.Reason,
			Message:    w.Terminated.Message,
			ExitCode:   w.Terminated.ExitCode,
			StartedAt:  parseTime(w.Terminated.StartedAt),
			FinishedAt: parseTime(w.Terminated.FinishedAt),
		}
	default:
		return snapshot.ContainerState{}
	}
}

// --- events.json -------------------------------------------------------------

type rawEventList struct {
	Items []rawEvent `json:"items"`
}

type rawEvent struct {
	Metadata struct {
		Namespace         string `json:"namespace"`
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
	InvolvedObject struct {
		Namespace string `json:"namespace"`
		Kind      string `json:"kind"`
		Name      string `json:"name"`
	} `json:"involvedObject"`
	Type           string `json:"type"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	Count          int32  `json:"count"`
	FirstTimestamp string `json:"firstTimestamp"`
	LastTimestamp  string `json:"lastTimestamp"`
	EventTime      string `json:"eventTime"`
}

func loadEvents(runDir string, win snapshot.Window) ([]snapshot.K8sEvent, error) {
	rc, err := openMaybeGzip(runDir, "events.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var list rawEventList
	if err := json.NewDecoder(rc).Decode(&list); err != nil {
		return nil, fmt.Errorf("parse events.json: %w", err)
	}

	var out []snapshot.K8sEvent
	for _, re := range list.Items {
		firstTS := parseTime(re.FirstTimestamp)
		lastTS := parseTime(re.LastTimestamp)
		evTime := parseTime(re.EventTime)
		createdTS := parseTime(re.Metadata.CreationTimestamp)

		primaryTS := lastTS
		if primaryTS.IsZero() {
			primaryTS = evTime
		}
		if primaryTS.IsZero() {
			primaryTS = firstTS
		}
		if primaryTS.IsZero() {
			primaryTS = createdTS
		}
		if primaryTS.IsZero() {
			continue
		}

		// Keep the event if its primary timestamp or firstTimestamp falls inside
		// the skew-adjusted analysis window (and is not in the excluded reset range).
		if !win.InWindowWithSkew(primaryTS) && (firstTS.IsZero() || !win.InWindowWithSkew(firstTS)) {
			continue
		}

		ns := re.InvolvedObject.Namespace
		if ns == "" {
			ns = re.Metadata.Namespace
		}
		out = append(out, snapshot.K8sEvent{
			Timestamp:      primaryTS,
			FirstTimestamp: firstTS,
			LastTimestamp:  lastTS,
			Namespace:      ns,
			InvolvedKind:   re.InvolvedObject.Kind,
			InvolvedName:   re.InvolvedObject.Name,
			Type:           re.Type,
			Reason:         re.Reason,
			Message:        re.Message,
			Count:          re.Count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out, nil
}

// --- metrics.json ------------------------------------------------------------

type rawMetricEntry struct {
	Query  string            `json:"query"`
	Result []rawMetricStream `json:"result"`
}

type rawMetricStream struct {
	Metric map[string]string `json:"metric"`
	Values [][]any           `json:"values"`
}

func loadMetrics(runDir string, win snapshot.Window) (map[string]snapshot.Series, error) {
	rc, err := openMaybeGzip(runDir, "metrics.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	var rawMap map[string]rawMetricEntry
	if err := json.NewDecoder(rc).Decode(&rawMap); err != nil {
		return nil, fmt.Errorf("parse metrics.json: %w", err)
	}

	out := make(map[string]snapshot.Series, len(rawMap))
	for name, entry := range rawMap {
		s := snapshot.Series{Query: entry.Query}
		for _, rs := range entry.Result {
			var samples []snapshot.MetricSample
			for _, pair := range rs.Values {
				if len(pair) < 2 {
					continue
				}
				sec, ok := toFloat64(pair[0])
				if !ok {
					continue
				}
				val, ok := toFloat64(pair[1])
				if !ok {
					continue
				}
				ts := time.Unix(int64(sec), 0).UTC()
				if !keepTimestamp(win, ts) {
					continue
				}
				samples = append(samples, snapshot.MetricSample{
					Timestamp: ts,
					Value:     val,
				})
			}
			if len(samples) > 0 {
				s.Streams = append(s.Streams, snapshot.MetricStream{
					Labels:  rs.Metric,
					Samples: samples,
				})
			}
		}
		out[name] = s
	}
	return out, nil
}

// --- Kubernetes quantity & timestamp parsers ---------------------------------

// ParseCPUMillis converts a Kubernetes CPU quantity string ("50m", "300m", "2",
// "0.5") into millicores. Returns 0 on empty or invalid input.
func ParseCPUMillis(q string) int64 {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0
	}
	if strings.HasSuffix(q, "m") {
		n, err := strconv.ParseInt(strings.TrimSuffix(q, "m"), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	f, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return 0
	}
	return int64(f * 1000)
}

// ParseMemoryBytes converts a Kubernetes memory quantity string ("40Mi",
// "400Mi", "1536Mi", "1Gi", "512Ki", "500M") into bytes.
func ParseMemoryBytes(q string) int64 {
	q = strings.TrimSpace(q)
	if q == "" {
		return 0
	}
	suffixes := []struct {
		suf  string
		mult int64
	}{
		{"Gi", 1024 * 1024 * 1024},
		{"Mi", 1024 * 1024},
		{"Ki", 1024},
		{"G", 1000 * 1000 * 1000},
		{"M", 1000 * 1000},
		{"K", 1000},
	}
	for _, s := range suffixes {
		if strings.HasSuffix(q, s.suf) {
			num := strings.TrimSuffix(q, s.suf)
			if n, err := strconv.ParseInt(num, 10, 64); err == nil {
				return n * s.mult
			}
			if f, err := strconv.ParseFloat(num, 64); err == nil {
				return int64(f * float64(s.mult))
			}
			return 0
		}
	}
	n, err := strconv.ParseInt(q, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	default:
		return 0, false
	}
}
