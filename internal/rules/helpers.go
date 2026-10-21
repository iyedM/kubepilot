package rules

import (
	"sort"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

const targetNamespace = "genai"

// isRequestPathPod reports whether a pod in namespace ns with name podName
// belongs to a workload on the GenAI request path (model-server or chat-api),
// excluding the synthetic load generator and any decoy namespace (noise).
func isRequestPathPod(ns, podName string) bool {
	if ns != "" && ns != targetNamespace {
		return false
	}
	w := workloadForPodName(podName)
	return w == "deployment/model-server" || w == "deployment/chat-api"
}

// workloadForPodName maps a pod name (e.g. "model-server-7d785c95dc-6slrm" or
// "chat-api-6f86945d96-fmcwp") to its owning Deployment workload identifier.
func workloadForPodName(podName string) string {
	switch {
	case strings.HasPrefix(podName, "model-server-"):
		return "deployment/model-server"
	case strings.HasPrefix(podName, "chat-api-"):
		return "deployment/chat-api"
	case strings.HasPrefix(podName, "loadgen-"):
		return "deployment/loadgen"
	default:
		return ""
	}
}

// isPodBefore reports whether podName was already running before the analysis
// window started (either listed in snap.PodsBefore, created before Window.From,
// or already emitting metrics during the pre-injection baseline window).
func isPodBefore(snap *snapshot.Snapshot, podName string) bool {
	for _, pb := range snap.PodsBefore {
		if pb.Name == podName {
			return true
		}
	}
	if !snap.Window.From.IsZero() {
		cutoff := snap.Window.From.Add(-5 * time.Second)
		if p := findPodSnapshot(snap, targetNamespace, podName); p != nil {
			if !p.CreationTimestamp.IsZero() && p.CreationTimestamp.Before(cutoff) {
				return true
			}
		}
		for _, series := range snap.Metrics {
			for _, stream := range series.Streams {
				if stream.Labels["pod"] != podName {
					continue
				}
				for _, s := range stream.Samples {
					if s.Timestamp.Before(cutoff) && snap.Window.InBaseline(s.Timestamp) {
						return true
					}
				}
			}
		}
	}
	return false
}

// findPodSnapshot returns the PodSnapshot for (ns, podName) if present in snap.Pods.
func findPodSnapshot(snap *snapshot.Snapshot, ns, podName string) *snapshot.PodSnapshot {
	for i := range snap.Pods {
		p := &snap.Pods[i]
		if p.Name == podName && (ns == "" || p.Namespace == ns) {
			return p
		}
	}
	return nil
}

// podEvents returns all K8sEvents in snap targeting podName in namespace ns.
func podEvents(snap *snapshot.Snapshot, ns, podName string) []snapshot.K8sEvent {
	var out []snapshot.K8sEvent
	for _, ev := range snap.Events {
		if ev.InvolvedName == podName && (ns == "" || ev.Namespace == ns) {
			out = append(out, ev)
		}
	}
	return out
}

// analysisFailedRequests counts RequestSamples inside the analysis window whose
// HTTP status is not 200, and returns the count and first failure timestamp.
func analysisFailedRequests(snap *snapshot.Snapshot) (int, time.Time) {
	count := 0
	var first time.Time
	for _, req := range snap.Requests {
		if !snap.Window.InAnalysisWindow(req.Timestamp) {
			continue
		}
		if req.Status != 200 {
			count++
			if first.IsZero() || req.Timestamp.Before(first) {
				first = req.Timestamp
			}
		}
	}
	return count, first
}

// analysisOKRequestsAfter counts HTTP 200 RequestSamples inside the analysis
// window at or after timestamp after.
func analysisOKRequestsAfter(snap *snapshot.Snapshot, after time.Time) (int, time.Time) {
	count := 0
	var first time.Time
	for _, req := range snap.Requests {
		if !snap.Window.InAnalysisWindow(req.Timestamp) {
			continue
		}
		if !after.IsZero() && req.Timestamp.Before(after) {
			continue
		}
		if req.Status == 200 {
			count++
			if first.IsZero() || req.Timestamp.Before(first) {
				first = req.Timestamp
			}
		}
	}
	return count, first
}

// candidateRequestPathPods collects all unique request-path pod names seen in
// snap.Pods, snap.Events, or snap.Metrics during the skew-adjusted analysis window,
// excluding pods created at or after the end of the analysis window (post-reset pods).
func candidateRequestPathPods(snap *snapshot.Snapshot) []string {
	seen := make(map[string]struct{})
	var pods []string
	add := func(ns, name string) {
		if !isRequestPathPod(ns, name) {
			return
		}
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			pods = append(pods, name)
		}
	}
	for _, p := range snap.Pods {
		if !snap.Window.To.IsZero() && !p.CreationTimestamp.IsZero() && !p.CreationTimestamp.Before(snap.Window.To) {
			continue
		}
		add(p.Namespace, p.Name)
	}
	for _, ev := range snap.Events {
		if !snap.Window.To.IsZero() && !ev.Timestamp.IsZero() && !ev.Timestamp.Before(snap.Window.To) {
			continue
		}
		if snap.Window.InWindowWithSkew(ev.Timestamp) {
			add(ev.Namespace, ev.InvolvedName)
		}
	}
	for _, series := range snap.Metrics {
		for _, stream := range series.Streams {
			pod := stream.Labels["pod"]
			ns := stream.Labels["namespace"]
			if pod == "" {
				continue
			}
			for _, s := range stream.Samples {
				if !snap.Window.To.IsZero() && !s.Timestamp.Before(snap.Window.To) {
					continue
				}
				if snap.Window.InWindowWithSkew(s.Timestamp) {
					add(ns, pod)
					break
				}
			}
		}
	}
	return pods
}

// podMetricWindowStats returns (hasSamples, minVal, maxVal, firstMaxTS) for a
// named metric series filtered to (namespace="genai", pod=podName) and optional
// extra label key/value within snap.Window.InWindowWithSkew.
func podMetricWindowStats(snap *snapshot.Snapshot, metricKey, podName, labelKey, labelVal string) (bool, float64, float64, time.Time) {
	series, ok := snap.Metrics[metricKey]
	if !ok {
		return false, 0, 0, time.Time{}
	}
	has := false
	var minV, maxV float64
	var maxTS time.Time
	for _, stream := range series.Streams {
		if stream.Labels["pod"] != podName {
			continue
		}
		if ns := stream.Labels["namespace"]; ns != "" && ns != targetNamespace {
			continue
		}
		if labelKey != "" && stream.Labels[labelKey] != labelVal {
			continue
		}
		for _, s := range stream.Samples {
			if !snap.Window.InWindowWithSkew(s.Timestamp) {
				continue
			}
			if !has {
				has = true
				minV = s.Value
				maxV = s.Value
				maxTS = s.Timestamp
				continue
			}
			if s.Value < minV {
				minV = s.Value
			}
			if s.Value > maxV {
				maxV = s.Value
				maxTS = s.Timestamp
			}
		}
	}
	return has, minV, maxV, maxTS
}

// podCounterIncrease computes the positive counter increase, last sample timestamp,
// and sample span duration for a counter metric series on (ns, podName) inside
// either the baseline window (inBaseline=true) or the analysis window (inBaseline=false).
// It prefers container-level streams (container != "" && container != "POD") over
// pod-level cgroup streams to avoid double-counting.
func podCounterIncrease(snap *snapshot.Snapshot, metricKey, ns, podName string, inBaseline bool) (float64, time.Time, time.Duration, bool) {
	series, ok := snap.Metrics[metricKey]
	if !ok {
		return 0, time.Time{}, 0, false
	}

	matchStream := func(requireContainer bool) []snapshot.MetricSample {
		for _, stream := range series.Streams {
			if stream.Labels["pod"] != podName {
				continue
			}
			if ns != "" && stream.Labels["namespace"] != "" && stream.Labels["namespace"] != ns {
				continue
			}
			c := stream.Labels["container"]
			if requireContainer && (c == "" || c == "POD") {
				continue
			}
			var filtered []snapshot.MetricSample
			for _, s := range stream.Samples {
				if inBaseline {
					if snap.Window.InBaseline(s.Timestamp) {
						filtered = append(filtered, s)
					}
				} else {
					if snap.Window.InAnalysisWindow(s.Timestamp) {
						filtered = append(filtered, s)
					}
				}
			}
			if len(filtered) >= 2 {
				return filtered
			}
		}
		return nil
	}

	samples := matchStream(true)
	if len(samples) < 2 {
		samples = matchStream(false)
	}
	if len(samples) < 2 {
		return 0, time.Time{}, 0, false
	}

	var delta float64
	for i := 1; i < len(samples); i++ {
		if d := samples[i].Value - samples[i-1].Value; d > 0 {
			delta += d
		}
	}
	firstTS := samples[0].Timestamp
	lastTS := samples[len(samples)-1].Timestamp
	return delta, lastTS, lastTS.Sub(firstTS), true
}

// podCFSThrottleStats returns the CFS throttled period fraction
// (throttledDelta / periodsDelta) for podName in namespace genai over the
// analysis window.
func podCFSThrottleStats(snap *snapshot.Snapshot, podName string) (fraction, throttledDelta, periodsDelta float64, ts time.Time, ok bool) {
	periodsDelta, pTS, _, okP := podCounterIncrease(snap, "cpu_periods", targetNamespace, podName, false)
	if !okP || periodsDelta < 10 {
		return 0, 0, 0, time.Time{}, false
	}
	throttledDelta, tTS, _, okT := podCounterIncrease(snap, "cpu_throttled_periods", targetNamespace, podName, false)
	if !okT {
		throttledDelta = 0
		tTS = pTS
	}
	return throttledDelta / periodsDelta, throttledDelta, periodsDelta, tTS, true
}

// requestLatencyComparison compares median (p50) HTTP 200 request latency in
// the analysis window (optionally at or after `after`) against the baseline
// window. It prefers the fixed-length "short-16" prompt when >= 2 samples exist
// in both windows, falling back to all HTTP 200 requests or trace durations.
func requestLatencyComparison(snap *snapshot.Snapshot, after time.Time) (baseP50, winP50, ratio float64, sampleTS time.Time, ok bool) {
	collectReqLat := func(promptFilter string, baseline bool) ([]float64, time.Time) {
		var vals []float64
		var lastTS time.Time
		for _, r := range snap.Requests {
			if r.Status != 200 || r.ClientLatencyMs <= 0 {
				continue
			}
			if promptFilter != "" && r.PromptID != promptFilter {
				continue
			}
			if baseline {
				if !snap.Window.InBaseline(r.Timestamp) {
					continue
				}
			} else {
				if !snap.Window.InAnalysisWindow(r.Timestamp) {
					continue
				}
				if !after.IsZero() && r.Timestamp.Before(after) {
					continue
				}
			}
			vals = append(vals, r.ClientLatencyMs)
			if lastTS.IsZero() || r.Timestamp.After(lastTS) {
				lastTS = r.Timestamp
			}
		}
		return vals, lastTS
	}

	bVals, _ := collectReqLat("short-16", true)
	wVals, lastTS := collectReqLat("short-16", false)
	if len(bVals) < 2 || len(wVals) < 2 {
		bVals, _ = collectReqLat("", true)
		wVals, lastTS = collectReqLat("", false)
	}
	if len(bVals) < 2 || len(wVals) < 2 {
		return 0, 0, 0, time.Time{}, false
	}
	baseP50 = percentileFloat64(bVals, 0.50)
	winP50 = percentileFloat64(wVals, 0.50)
	if baseP50 <= 0 {
		return 0, 0, 0, time.Time{}, false
	}
	return baseP50, winP50, winP50 / baseP50, lastTS, true
}

// llamaTokensPerSecComparison compares llamacpp:predicted_tokens_seconds in the
// analysis window (at or after `after`) against the baseline window.
func llamaTokensPerSecComparison(snap *snapshot.Snapshot, after time.Time) (baseAvg, winAvg, winMin float64, ts time.Time, ok bool) {
	series, exists := snap.Metrics["llama_predicted_tokens_per_second"]
	if !exists {
		return 0, 0, 0, time.Time{}, false
	}
	var bSum, wSum float64
	var bCount, wCount int
	for _, stream := range series.Streams {
		if ns := stream.Labels["namespace"]; ns != "" && ns != targetNamespace {
			continue
		}
		for _, s := range stream.Samples {
			if s.Value <= 0 {
				continue
			}
			if snap.Window.InBaseline(s.Timestamp) {
				bSum += s.Value
				bCount++
			} else if snap.Window.InAnalysisWindow(s.Timestamp) {
				if !after.IsZero() && s.Timestamp.Before(after) {
					continue
				}
				wSum += s.Value
				if wCount == 0 || s.Value < winMin {
					winMin = s.Value
				}
				wCount++
				if ts.IsZero() || s.Timestamp.After(ts) {
					ts = s.Timestamp
				}
			}
		}
	}
	if bCount == 0 || wCount == 0 {
		return 0, 0, 0, time.Time{}, false
	}
	return bSum / float64(bCount), wSum / float64(wCount), winMin, ts, true
}

func percentileFloat64(vals []float64, q float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := make([]float64, len(vals))
	copy(cp, vals)
	sort.Float64s(cp)
	if q <= 0 {
		return cp[0]
	}
	if q >= 1 {
		return cp[len(cp)-1]
	}
	idx := int(float64(len(cp)) * q)
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// hasOOMSignal reports whether podName has an OOMKilled termination in
// snap.Pods or in the Prometheus last_terminated_reason metric during the
// analysis window.
func hasOOMSignal(snap *snapshot.Snapshot, podName string) (bool, time.Time) {
	if p := findPodSnapshot(snap, targetNamespace, podName); p != nil {
		for _, c := range p.Containers {
			if c.LastTerminated != nil && c.LastTerminated.Reason == "OOMKilled" {
				return true, c.LastTerminated.FinishedAt
			}
			if c.State.Kind == "terminated" && c.State.Reason == "OOMKilled" {
				return true, c.State.FinishedAt
			}
		}
	}
	has, _, maxV, ts := podMetricWindowStats(snap, "last_terminated_reason", podName, "reason", "OOMKilled")
	if has && maxV >= 1 {
		return true, ts
	}
	return false, time.Time{}
}

// hasBackOffEvent returns the first Warning BackOff event on podName in the
// analysis window, if any.
func hasBackOffEvent(snap *snapshot.Snapshot, podName string) (snapshot.K8sEvent, bool) {
	for _, ev := range podEvents(snap, targetNamespace, podName) {
		if ev.Reason == "BackOff" && snap.Window.InWindowWithSkew(ev.Timestamp) {
			return ev, true
		}
	}
	return snapshot.K8sEvent{}, false
}
