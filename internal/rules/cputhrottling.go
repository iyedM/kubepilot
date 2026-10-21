package rules

import (
	"fmt"
	"time"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

const minThrottledFraction = 0.20

var cpuThrottlingRule = Rule{
	Name:     "cpu-throttling",
	Evaluate: evaluateCPUThrottling,
}

func evaluateCPUThrottling(snap *snapshot.Snapshot) []snapshot.Finding {
	var findings []snapshot.Finding

	for _, podName := range candidateRequestPathPods(snap) {
		fraction, throttled, periods, metricTS, ok := podCFSThrottleStats(snap, podName)
		if !ok || fraction < minThrottledFraction {
			continue
		}

		workload := workloadForPodName(podName)
		evidence := []snapshot.EvidenceItem{
			{
				Source:    snapshot.EvidencePrometheus,
				Timestamp: metricTS,
				Summary: fmt.Sprintf(
					"container_cpu_cfs_throttled_periods_total / container_cpu_cfs_periods_total = %.3f (%.0f/%.0f periods) on pod %s",
					fraction, throttled, periods, podName),
			},
		}

		// Latency comparison vs baseline.
		baseP50, winP50, latRatio, latTS, hasLat := requestLatencyComparison(snap, time.Time{})
		if hasLat && latRatio >= 1.25 {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceRequestLog,
				Timestamp: latTS,
				Summary:   fmt.Sprintf("request p50 latency increased to %.1f ms vs %.1f ms baseline (x%.2f)", winP50, baseP50, latRatio),
			})
		}

		// Token throughput drop in llama.cpp.
		if workload == "deployment/model-server" {
			baseTPS, winTPS, minTPS, tpsTS, hasTPS := llamaTokensPerSecComparison(snap, time.Time{})
			if hasTPS && baseTPS > 0 && winTPS <= baseTPS*0.85 {
				evidence = append(evidence, snapshot.EvidenceItem{
					Source:    snapshot.EvidencePrometheus,
					Timestamp: tpsTS,
					Summary:   fmt.Sprintf("llamacpp:predicted_tokens_seconds dropped from %.1f tok/s baseline to %.1f tok/s avg (min %.1f tok/s)", baseTPS, winTPS, minTPS),
				})
			}
		}

		// Check in-place resize events or rollout events on the pod.
		for _, ev := range podEvents(snap, targetNamespace, podName) {
			if !snap.Window.InWindowWithSkew(ev.Timestamp) {
				continue
			}
			if ev.Reason == "ResizeCompleted" || ev.Reason == "ResizeStarted" {
				evidence = append(evidence, snapshot.EvidenceItem{
					Source:    snapshot.EvidenceK8sEvent,
					Timestamp: ev.Timestamp,
					Summary:   fmt.Sprintf("%s on pod %s: %s", ev.Reason, podName, ev.Message),
				})
				break
			}
		}

		if p := findPodSnapshot(snap, targetNamespace, podName); p != nil && len(p.Containers) > 0 {
			if lim := p.Containers[0].LimitsCPUMillis; lim > 0 && lim <= 1000 {
				evidence = append(evidence, snapshot.EvidenceItem{
					Source:    snapshot.EvidenceK8sPod,
					Timestamp: p.CreationTimestamp,
					Summary:   fmt.Sprintf("pod %s container %s has CPU limit %dm", podName, p.Containers[0].Name, lim),
				})
			}
		}

		if failedReqs, failTS := analysisFailedRequests(snap); failedReqs > 0 {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceRequestLog,
				Timestamp: failTS,
				Summary:   fmt.Sprintf("%d requests failed or timed out while %s was CPU-throttled", failedReqs, podName),
			})
		}

		summary := fmt.Sprintf("Pod %s (%s) is CFS CPU-throttled (%.1f%% of periods throttled)", podName, workload, fraction*100)
		if hasLat && latRatio >= 1.25 {
			summary += fmt.Sprintf(", increasing p50 latency x%.2f (%.0f ms -> %.0f ms)", latRatio, baseP50, winP50)
		}

		findings = append(findings, snapshot.Finding{
			Rule:       "cpu-throttling",
			Cause:      snapshot.CauseCPUThrottling,
			Confidence: 0.92,
			Component: snapshot.Component{
				Namespace: targetNamespace,
				Kind:      "Pod",
				Workload:  workload,
				Pod:       podName,
			},
			Summary:  summary,
			Evidence: evidence,
		})
	}

	return findings
}
