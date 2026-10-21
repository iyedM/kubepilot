package rules

import (
	"fmt"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

const (
	maxConfigChangeCFSFraction = 0.05
	minConfigChangeLatRatio    = 1.25
	maxConfigChangeTPSRatio    = 0.80
)

var configChangeRule = Rule{
	Name:     "config-change",
	Evaluate: evaluateConfigChange,
}

func evaluateConfigChange(snap *snapshot.Snapshot) []snapshot.Finding {
	// Collect any pods already diagnosed as cold_start so we never double-diagnose
	// a slow image pull or model download initContainer as a config regression.
	coldStartPods := make(map[string]struct{})
	for _, f := range evaluateColdStart(snap) {
		if f.Component.Pod != "" {
			coldStartPods[f.Component.Pod] = struct{}{}
		}
	}

	var findings []snapshot.Finding

	for _, podName := range candidateRequestPathPods(snap) {
		if _, isCold := coldStartPods[podName]; isCold {
			continue
		}
		if oom, _ := hasOOMSignal(snap, podName); oom {
			continue
		}
		if _, backOff := hasBackOffEvent(snap, podName); backOff {
			continue
		}

		p := findPodSnapshot(snap, targetNamespace, podName)
		threadsArg := extractContainerArg(p, "--threads")
		hasConfigArg := threadsArg != "" && threadsArg != "2"

		workload := workloadForPodName(podName)
		deployName := strings.TrimPrefix(workload, "deployment/")
		rolloutEv, hasRolloutEv := findRolloutEvent(snap, deployName, podName)

		// A config regression arrives through a rollout (new pod not in PodsBefore
		// with a rollout event) or carries a non-default container configuration in its spec.
		if isPodBefore(snap, podName) && !hasConfigArg {
			continue
		}
		if !hasRolloutEv && !hasConfigArg {
			continue
		}

		// Must be actively running in the window with 0 / negligible CFS throttling
		// and low PSI CPU waiting.
		throttleFrac, throttled, periods, metricTS, hasPeriods := podCFSThrottleStats(snap, podName)
		if !hasPeriods || throttleFrac >= maxConfigChangeCFSFraction {
			continue
		}
		if psiDelta, _, _, okPSI := podCounterIncrease(snap, "cpu_pressure_waiting_seconds", targetNamespace, podName, false); okPSI && psiDelta >= minPSIWaitingSeconds {
			continue
		}

		// Determine when the rollout pod started serving so brief rollout transition
		// connection errors before Ready are not counted as steady-state failures.
		servingAfter := podServingStartTime(snap, podName)
		okReqs, _ := analysisOKRequestsAfter(snap, servingAfter)
		failedAfter := analysisFailedRequestsAfter(snap, servingAfter)
		if okReqs < 3 || failedAfter > 0 {
			continue
		}

		// Require latency increase and/or token generation throughput drop vs baseline.
		baseP50, winP50, latRatio, latTS, hasLat := requestLatencyComparison(snap, servingAfter)
		baseTPS, winTPS, minTPS, tpsTS, hasTPS := llamaTokensPerSecComparison(snap, servingAfter)
		latSlow := hasLat && latRatio >= minConfigChangeLatRatio
		tpsSlow := hasTPS && baseTPS > 0 && winTPS <= baseTPS*maxConfigChangeTPSRatio
		if !latSlow && !tpsSlow {
			continue
		}

		var evidence []snapshot.EvidenceItem
		if hasRolloutEv {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceK8sEvent,
				Timestamp: rolloutEv.Timestamp,
				Summary:   fmt.Sprintf("%s on %s/%s: %s", rolloutEv.Reason, rolloutEv.InvolvedKind, rolloutEv.InvolvedName, rolloutEv.Message),
			})
		}
		if hasConfigArg && p != nil {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceK8sPod,
				Timestamp: p.CreationTimestamp,
				Summary:   fmt.Sprintf("pod %s spec sets container arg --threads %s", podName, threadsArg),
			})
		}

		cpuCoresStr := ""
		if cpuDelta, _, cpuDur, okCPU := podCounterIncrease(snap, "cpu_usage_seconds", targetNamespace, podName, false); okCPU && cpuDur > 0 {
			cpuCoresStr = fmt.Sprintf(", CPU usage %.2f cores", cpuDelta/cpuDur.Seconds())
		}
		evidence = append(evidence, snapshot.EvidenceItem{
			Source:    snapshot.EvidencePrometheus,
			Timestamp: metricTS,
			Summary: fmt.Sprintf(
				"no resource pressure on %s: CFS throttled fraction %.3f (%.0f/%.0f periods)%s, 0 HTTP errors after rollout",
				podName, throttleFrac, throttled, periods, cpuCoresStr),
		})

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
				Summary:   fmt.Sprintf("request p50 latency increased to %.1f ms after rollout vs %.1f ms baseline (x%.2f)", winP50, baseP50, latRatio),
			})
		}

		summary := fmt.Sprintf("Rollout of %s (pod %s) degraded serving without resource pressure", workload, podName)
		if hasConfigArg {
			summary += fmt.Sprintf(" (--threads %s)", threadsArg)
		}
		if latSlow {
			summary += fmt.Sprintf(": p50 latency x%.2f (%.0f ms -> %.0f ms)", latRatio, baseP50, winP50)
		} else if tpsSlow {
			summary += fmt.Sprintf(": throughput %.1f -> %.1f tok/s", baseTPS, winTPS)
		}

		findings = append(findings, snapshot.Finding{
			Rule:       "config-change",
			Cause:      snapshot.CauseConfigChange,
			Confidence: 0.88,
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

func extractContainerArg(p *snapshot.PodSnapshot, flag string) string {
	if p == nil {
		return ""
	}
	for _, c := range p.Containers {
		for i := 0; i+1 < len(c.Args); i++ {
			if c.Args[i] == flag {
				return c.Args[i+1]
			}
		}
	}
	return ""
}

func podServingStartTime(snap *snapshot.Snapshot, podName string) time.Time {
	var startTS time.Time
	for _, ev := range podEvents(snap, targetNamespace, podName) {
		if ev.Reason == "Started" && (startTS.IsZero() || ev.Timestamp.Before(startTS)) {
			startTS = ev.Timestamp
		}
	}
	if !startTS.IsZero() {
		return startTS.Add(5 * time.Second)
	}
	if series, ok := snap.Metrics["pod_ready"]; ok {
		for _, stream := range series.Streams {
			if stream.Labels["pod"] != podName {
				continue
			}
			for _, s := range stream.Samples {
				if snap.Window.InAnalysisWindow(s.Timestamp) && s.Value >= 1 {
					return s.Timestamp
				}
			}
		}
	}
	return snap.Window.From.Add(5 * time.Second)
}

func analysisFailedRequestsAfter(snap *snapshot.Snapshot, after time.Time) int {
	count := 0
	for _, req := range snap.Requests {
		if !snap.Window.InAnalysisWindow(req.Timestamp) {
			continue
		}
		if !after.IsZero() && req.Timestamp.Before(after) {
			continue
		}
		if req.Status != 200 {
			count++
		}
	}
	return count
}

func findRolloutEvent(snap *snapshot.Snapshot, deployName, podName string) (snapshot.K8sEvent, bool) {
	// Kubernetes v1 Event timestamps have 1-second resolution (truncated to whole
	// seconds), so an event emitted at 06:19:26.500Z after inject_start=06:19:26.155Z
	// carries timestamp 06:19:26Z. Allow a 2-second pre-From margin while strictly
	// excluding events at or after Window.To (reset).
	inRolloutWin := func(t time.Time) bool {
		if t.IsZero() || snap.Window.IsExcluded(t) {
			return false
		}
		if !snap.Window.To.IsZero() && !t.Before(snap.Window.To) {
			return false
		}
		return !t.Before(snap.Window.From.Add(-2 * time.Second))
	}
	for _, ev := range snap.Events {
		if ev.Namespace != targetNamespace {
			continue
		}
		if !inRolloutWin(ev.Timestamp) && !inRolloutWin(ev.FirstTimestamp) {
			continue
		}
		if ev.InvolvedKind == "Deployment" && ev.InvolvedName == deployName && ev.Reason == "ScalingReplicaSet" {
			return ev, true
		}
	}
	for _, ev := range podEvents(snap, targetNamespace, podName) {
		if inRolloutWin(ev.Timestamp) && (ev.Reason == "Started" || ev.Reason == "Created") {
			return ev, true
		}
	}
	return snapshot.K8sEvent{}, false
}
