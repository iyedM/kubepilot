package rules

import (
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

// isPodBefore reports whether podName was already running before the fault
// window started (and is therefore the old pod being replaced during a rollout).
func isPodBefore(snap *snapshot.Snapshot, podName string) bool {
	for _, pb := range snap.PodsBefore {
		if pb.Name == podName {
			return true
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
// snap.Pods, snap.Events, or snap.Metrics during the skew-adjusted analysis window.
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
		add(p.Namespace, p.Name)
	}
	for _, ev := range snap.Events {
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
