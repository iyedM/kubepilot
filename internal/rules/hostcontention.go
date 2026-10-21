package rules

import (
	"fmt"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

const (
	minPSIWaitingSeconds     = 3.0
	minPSIRateMultiplier     = 5.0
	maxContentionCFSFraction = 0.15
	minNeighborCPUCores      = 0.50
)

var hostContentionRule = Rule{
	Name:     "host-cpu-contention",
	Evaluate: evaluateHostContention,
}

func evaluateHostContention(snap *snapshot.Snapshot) []snapshot.Finding {
	var findings []snapshot.Finding

	for _, podName := range candidateRequestPathPods(snap) {
		// 1. Exclude CFS CPU throttling (which also increases container PSI waiting).
		throttleFrac, _, _, _, hasThrottle := podCFSThrottleStats(snap, podName)
		if hasThrottle && throttleFrac >= maxContentionCFSFraction {
			continue
		}

		// 2. Measure PSI CPU waiting increase in the analysis window vs baseline.
		psiWinDelta, psiTS, winDur, okWin := podCounterIncrease(snap, "cpu_pressure_waiting_seconds", targetNamespace, podName, false)
		if !okWin || winDur <= 0 || psiWinDelta < minPSIWaitingSeconds {
			continue
		}
		psiWinRate := psiWinDelta / winDur.Seconds()

		psiBaseDelta, _, baseDur, okBase := podCounterIncrease(snap, "cpu_pressure_waiting_seconds", targetNamespace, podName, true)
		var psiBaseRate float64
		if okBase && baseDur > 0 {
			psiBaseRate = psiBaseDelta / baseDur.Seconds()
			floorRate := psiBaseRate
			if floorRate < 0.0001 {
				floorRate = 0.0001
			}
			if psiWinRate < minPSIRateMultiplier*floorRate {
				continue
			}
		}

		// 3. Require a serving slowdown signal (latency rise and/or token/s drop)
		// when baseline telemetry is present.
		baseP50, winP50, latRatio, latTS, hasLat := requestLatencyComparison(snap, time.Time{})
		baseTPS, winTPS, minTPS, tpsTS, hasTPS := llamaTokensPerSecComparison(snap, time.Time{})
		latSlow := hasLat && latRatio >= 1.20
		tpsSlow := hasTPS && baseTPS > 0 && winTPS <= baseTPS*0.85
		if (hasLat || hasTPS) && !latSlow && !tpsSlow {
			continue
		}

		var evidence []snapshot.EvidenceItem
		if okBase && baseDur > 0 {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidencePrometheus,
				Timestamp: psiTS,
				Summary: fmt.Sprintf(
					"container_pressure_cpu_waiting_seconds_total on %s rose by %.2f s over %.0f s (%.4f s/s) vs %.2f s over %.0f s in baseline (CFS throttled fraction %.3f < %.2f)",
					podName, psiWinDelta, winDur.Seconds(), psiWinRate, psiBaseDelta, baseDur.Seconds(), throttleFrac, maxContentionCFSFraction),
			})
		} else {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidencePrometheus,
				Timestamp: psiTS,
				Summary: fmt.Sprintf(
					"container_pressure_cpu_waiting_seconds_total on %s rose by %.2f s over %.0f s (%.4f s/s) with CFS throttled fraction %.3f < %.2f",
					podName, psiWinDelta, winDur.Seconds(), psiWinRate, throttleFrac, maxContentionCFSFraction),
			})
		}

		if tpsSlow {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidencePrometheus,
				Timestamp: tpsTS,
				Summary:   fmt.Sprintf("llamacpp:predicted_tokens_seconds dropped from %.1f tok/s baseline to %.1f tok/s avg (min %.1f tok/s)", baseTPS, winTPS, minTPS),
			})
		}
		if latSlow {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceRequestLog,
				Timestamp: latTS,
				Summary:   fmt.Sprintf("request p50 latency increased to %.1f ms vs %.1f ms baseline (x%.2f)", winP50, baseP50, latRatio),
			})
		}

		// 4. Check for any CPU-hungry neighbor pod in container_cpu_usage_seconds_total.
		for _, nb := range findHeavyNeighborPods(snap) {
			evidence = append(evidence, nb)
		}

		workload := workloadForPodName(podName)
		summary := fmt.Sprintf(
			"Pod %s (%s) experiences host CPU contention (PSI CPU waiting +%.2f s over %.0f s, CFS throttled fraction %.3f)",
			podName, workload, psiWinDelta, winDur.Seconds(), throttleFrac)
		if latSlow {
			summary += fmt.Sprintf(", increasing p50 latency x%.2f", latRatio)
		}

		var nodeName string
		if p := findPodSnapshot(snap, targetNamespace, podName); p != nil {
			nodeName = p.NodeName
		}

		findings = append(findings, snapshot.Finding{
			Rule:       "host-cpu-contention",
			Cause:      snapshot.CauseHostContention,
			Confidence: 0.90,
			Component: snapshot.Component{
				Namespace: targetNamespace,
				Kind:      "Pod",
				Workload:  workload,
				Pod:       podName,
				Node:      nodeName,
			},
			Summary:  summary,
			Evidence: evidence,
		})
	}

	return findings
}

func findHeavyNeighborPods(snap *snapshot.Snapshot) []snapshot.EvidenceItem {
	series, ok := snap.Metrics["cpu_usage_seconds"]
	if !ok {
		return nil
	}
	type podKey struct {
		ns  string
		pod string
	}
	seen := make(map[podKey]struct{})
	var items []snapshot.EvidenceItem

	for _, stream := range series.Streams {
		ns := stream.Labels["namespace"]
		pod := stream.Labels["pod"]
		if pod == "" || isRequestPathPod(ns, pod) || strings.HasPrefix(pod, "loadgen-") {
			continue
		}
		k := podKey{ns: ns, pod: pod}
		if _, exists := seen[k]; exists {
			continue
		}
		seen[k] = struct{}{}

		cpuDelta, ts, dur, ok := podCounterIncrease(snap, "cpu_usage_seconds", ns, pod, false)
		if !ok || dur <= 0 {
			continue
		}
		cores := cpuDelta / dur.Seconds()
		if cores < minNeighborCPUCores {
			continue
		}

		nodeInfo := ""
		if p := findPodSnapshot(snap, ns, pod); p != nil && p.NodeName != "" {
			nodeInfo = fmt.Sprintf(" on node %s", p.NodeName)
		}
		items = append(items, snapshot.EvidenceItem{
			Source:    snapshot.EvidencePrometheus,
			Timestamp: ts,
			Summary:   fmt.Sprintf("neighbor pod %s/%s%s consuming %.2f CPU cores (container_cpu_usage_seconds_total)", ns, pod, nodeInfo, cores),
		})
	}
	return items
}
