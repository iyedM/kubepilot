package rules

import (
	"fmt"
	"time"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

const minPreInferenceGapMs = 500.0

var unknownNoInfraRule = Rule{
	Name:     "unknown-no-infrastructure-cause",
	Evaluate: evaluateUnknownNoInfra,
}

func evaluateUnknownNoInfra(snap *snapshot.Snapshot) []snapshot.Finding {
	// 1. Only fire when no infrastructure or configuration rule explains the slowdown.
	infraEvaluators := []func(*snapshot.Snapshot) []snapshot.Finding{
		evaluateOOMKilled,
		evaluateUnschedulable,
		evaluateReadinessFailing,
		evaluateColdStart,
		evaluateCPUThrottling,
		evaluateHostContention,
		evaluateConfigChange,
	}
	for _, eval := range infraEvaluators {
		if len(eval(snap)) > 0 {
			return nil
		}
	}

	// 2. Inspect PreInferenceGapMs (HTTP server span start -> GenAI client span start)
	// across traces in the analysis window (or the targeted trace).
	var gaps []float64
	var delayedCount int
	var lastDelayedTS time.Time
	var chatPod string

	for _, tr := range snap.Traces {
		inWin := snap.Window.InAnalysisWindow(tr.StartTime)
		isTarget := snap.TargetTraceID != "" && tr.TraceID == snap.TargetTraceID
		if !inWin && !isTarget {
			continue
		}
		gaps = append(gaps, tr.PreInferenceGapMs)
		if tr.PreInferenceGapMs >= minPreInferenceGapMs {
			delayedCount++
			if lastDelayedTS.IsZero() || tr.StartTime.After(lastDelayedTS) {
				lastDelayedTS = tr.StartTime
			}
			if chatPod == "" && tr.ClientPodName != "" {
				chatPod = tr.ClientPodName
			}
		}
	}

	if len(gaps) == 0 || delayedCount == 0 {
		return nil
	}

	p50Gap := percentileFloat64(gaps, 0.50)
	p90Gap := percentileFloat64(gaps, 0.90)
	maxGap := percentileFloat64(gaps, 1.0)

	// Require p90 >= 500 ms or >= 2 delayed traces in a multi-trace window
	// (handling both constant delay where p50 & p90 are high and intermittent
	// delay where p50 is ~0 ms but the tail / p90 is high), or >= 1 delayed
	// trace when evaluating a single trace / small fixture (< 3 traces).
	if len(gaps) >= 3 && p90Gap < minPreInferenceGapMs && delayedCount < 2 {
		return nil
	}

	if chatPod == "" {
		for _, podName := range candidateRequestPathPods(snap) {
			if workloadForPodName(podName) == "deployment/chat-api" {
				chatPod = podName
				break
			}
		}
	}

	summary := fmt.Sprintf(
		"Application pre-inference delay in deployment/chat-api (trace gap HTTP span start -> GenAI span start: p50=%.0f ms, p90=%.0f ms, %d/%d traces >= %.0f ms) with no infrastructure or configuration cause",
		p50Gap, p90Gap, delayedCount, len(gaps), minPreInferenceGapMs)

	return []snapshot.Finding{
		{
			Rule:       "unknown-no-infrastructure-cause",
			Cause:      snapshot.CauseUnknownNoInfra,
			Confidence: 0.85,
			Component: snapshot.Component{
				Namespace: targetNamespace,
				Kind:      "Pod",
				Workload:  "deployment/chat-api",
				Pod:       chatPod,
			},
			Summary: summary,
			Evidence: []snapshot.EvidenceItem{
				{
					Source:    snapshot.EvidenceTrace,
					Timestamp: lastDelayedTS,
					Summary: fmt.Sprintf(
						"trace gap HTTP span start -> GenAI span start: p50=%.0f ms, p90=%.0f ms, max=%.0f ms (%d/%d traces >= %.0f ms); no Kubernetes event, resource metric, or configuration change in genai",
						p50Gap, p90Gap, maxGap, delayedCount, len(gaps), minPreInferenceGapMs),
				},
			},
		},
	}
}
