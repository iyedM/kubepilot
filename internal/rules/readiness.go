package rules

import (
	"fmt"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

var readinessFailingRule = Rule{
	Name:     "readiness-failing",
	Evaluate: evaluateReadinessFailing,
}

func evaluateReadinessFailing(snap *snapshot.Snapshot) []snapshot.Finding {
	failedReqs, firstFailTS := analysisFailedRequests(snap)
	if failedReqs == 0 {
		return nil
	}

	var findings []snapshot.Finding
	for _, podName := range candidateRequestPathPods(snap) {
		// 1. Exclude the old pod being terminated by a Recreate rollout
		// (docs/lab.md: "a Readiness probe failed ... connection refused on the
		// old pod of a Recreate rollout is NOT readiness_failing").
		if isPodBefore(snap, podName) {
			continue
		}

		// 2. Exclude pods failing because of OOMKilled or CrashLoopBackOff.
		if oom, _ := hasOOMSignal(snap, podName); oom {
			continue
		}
		if _, backOff := hasBackOffEvent(snap, podName); backOff {
			continue
		}

		// 3. Exclude pods that became Ready during the analysis window (e.g. cold
		// start that eventually succeeded, or the post-reset replacement pod).
		hasReadyMetric, _, maxReady, readyTS := podMetricWindowStats(snap, "pod_ready", podName, "", "")
		if hasReadyMetric && maxReady >= 1 {
			continue
		}
		p := findPodSnapshot(snap, targetNamespace, podName)
		if p != nil {
			isReadyInSnap := false
			for _, cond := range p.Conditions {
				if cond.Type == "Ready" && cond.Status == "True" {
					isReadyInSnap = true
					break
				}
			}
			if isReadyInSnap {
				continue
			}
			if p.Phase == "Pending" {
				continue
			}
		}

		// Find any Killing event timestamp on this pod (at reset) so we only
		// count Unhealthy events emitted while the pod was meant to be serving,
		// not while it was being stopped at the end of the window.
		var killTS time.Time
		for _, ev := range podEvents(snap, targetNamespace, podName) {
			if ev.Reason == "Killing" && (killTS.IsZero() || ev.Timestamp.Before(killTS)) {
				killTS = ev.Timestamp
			}
		}

		var readinessEvents []snapshot.K8sEvent
		for _, ev := range podEvents(snap, targetNamespace, podName) {
			if !snap.Window.InWindowWithSkew(ev.Timestamp) &&
				(ev.FirstTimestamp.IsZero() || !snap.Window.InWindowWithSkew(ev.FirstTimestamp)) {
				continue
			}
			evStart := ev.FirstTimestamp
			if evStart.IsZero() {
				evStart = ev.Timestamp
			}
			// Ignore Unhealthy events that only started at or after a Killing event
			// (termination artifact at reset); keep coalesced events that started
			// while the pod was running before reset.
			if !killTS.IsZero() && !evStart.Before(killTS) {
				continue
			}
			if ev.Reason == "Unhealthy" && strings.HasPrefix(ev.Message, "Readiness probe failed") {
				readinessEvents = append(readinessEvents, ev)
			}
		}

		var initialDelaySec int32
		var serverArgPort string
		var probePort string
		if p != nil && len(p.Containers) > 0 {
			c := p.Containers[0]
			if c.ReadinessProbe != nil {
				initialDelaySec = c.ReadinessProbe.InitialDelaySeconds
				probePort = c.ReadinessProbe.Port
			}
			for i := 0; i+1 < len(c.Args); i++ {
				if c.Args[i] == "--port" {
					serverArgPort = c.Args[i+1]
					break
				}
			}
		}

		if len(readinessEvents) == 0 && initialDelaySec < 300 {
			continue
		}

		var evidence []snapshot.EvidenceItem
		if len(readinessEvents) > 0 {
			lastEv := readinessEvents[len(readinessEvents)-1]
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceK8sEvent,
				Timestamp: lastEv.Timestamp,
				Summary:   fmt.Sprintf("Unhealthy on pod %s: %s", podName, lastEv.Message),
			})
		}
		if initialDelaySec >= 300 && p != nil {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceK8sPod,
				Timestamp: p.CreationTimestamp,
				Summary:   fmt.Sprintf("pod %s running with Ready=False and readinessProbe.initialDelaySeconds=%d", podName, initialDelaySec),
			})
		}
		if serverArgPort != "" && serverArgPort != "8080" {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceK8sPod,
				Timestamp: p.CreationTimestamp,
				Summary:   fmt.Sprintf("pod %s container args set --port %s while readinessProbe targets port %s (8080)", podName, serverArgPort, probePort),
			})
		}
		if hasReadyMetric {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidencePrometheus,
				Timestamp: readyTS,
				Summary:   fmt.Sprintf("kube_pod_status_ready{condition=\"true\",pod=%q} == 0 throughout the fault window", podName),
			})
		}
		evidence = append(evidence, snapshot.EvidenceItem{
			Source:    snapshot.EvidenceRequestLog,
			Timestamp: firstFailTS,
			Summary:   fmt.Sprintf("%d requests failed while %s had no Ready endpoint", failedReqs, podName),
		})

		workload := workloadForPodName(podName)
		summary := fmt.Sprintf("Pod %s (%s) is Running but never became Ready", podName, workload)
		if len(readinessEvents) > 0 {
			summary += fmt.Sprintf(" (%s)", readinessEvents[len(readinessEvents)-1].Message)
		} else if initialDelaySec >= 300 {
			summary += fmt.Sprintf(" (readinessProbe initialDelaySeconds=%d)", initialDelaySec)
		}

		findings = append(findings, snapshot.Finding{
			Rule:       "readiness-failing",
			Cause:      snapshot.CauseReadinessFail,
			Confidence: 0.94,
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
