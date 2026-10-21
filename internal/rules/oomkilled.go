package rules

import (
	"fmt"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

var oomKilledRule = Rule{
	Name:     "oom-killed",
	Evaluate: evaluateOOMKilled,
}

func evaluateOOMKilled(snap *snapshot.Snapshot) []snapshot.Finding {
	failedReqs, firstFailTS := analysisFailedRequests(snap)
	var findings []snapshot.Finding

	for _, podName := range candidateRequestPathPods(snap) {
		oom, oomTS := hasOOMSignal(snap, podName)
		if !oom {
			continue
		}

		var evidence []snapshot.EvidenceItem
		var memLimitB int64
		var restarts int32

		if p := findPodSnapshot(snap, targetNamespace, podName); p != nil {
			for _, c := range p.Containers {
				if c.LimitsMemoryB > 0 {
					memLimitB = c.LimitsMemoryB
				}
				if c.RestartCount > restarts {
					restarts = c.RestartCount
				}
				if (c.LastTerminated != nil && c.LastTerminated.Reason == "OOMKilled") ||
					(c.State.Kind == "terminated" && c.State.Reason == "OOMKilled") {
					evidence = append(evidence, snapshot.EvidenceItem{
						Source:    snapshot.EvidenceK8sPod,
						Timestamp: oomTS,
						Summary: fmt.Sprintf("pod %s container %s terminated with reason=OOMKilled (exitCode=137, restarts=%d, memoryLimit=%dMi)",
							podName, c.Name, c.RestartCount, c.LimitsMemoryB/(1024*1024)),
					})
				}
			}
		}

		if hasKSM, _, maxV, ksmTS := podMetricWindowStats(snap, "last_terminated_reason", podName, "reason", "OOMKilled"); hasKSM && maxV >= 1 {
			if oomTS.IsZero() {
				oomTS = ksmTS
			}
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidencePrometheus,
				Timestamp: ksmTS,
				Summary:   fmt.Sprintf("kube_pod_container_status_last_terminated_reason{pod=%q,reason=\"OOMKilled\"} == 1", podName),
			})
		}

		if hasRestarts, _, maxR, rTS := podMetricWindowStats(snap, "restarts", podName, "", ""); hasRestarts && int32(maxR) > restarts {
			restarts = int32(maxR)
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidencePrometheus,
				Timestamp: rTS,
				Summary:   fmt.Sprintf("kube_pod_container_status_restarts_total{pod=%q} reached %d", podName, restarts),
			})
		}

		backOffEv, hasBackOff := hasBackOffEvent(snap, podName)
		if hasBackOff {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceK8sEvent,
				Timestamp: backOffEv.Timestamp,
				Summary:   fmt.Sprintf("Warning BackOff on pod %s: %s", podName, backOffEv.Message),
			})
		}

		// Require corroboration (crash-loop restarts or BackOff or failed requests)
		// so a stale metric without activity cannot fire alone.
		if !hasBackOff && restarts == 0 && failedReqs == 0 {
			continue
		}

		if failedReqs > 0 {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceRequestLog,
				Timestamp: firstFailTS,
				Summary:   fmt.Sprintf("%d requests failed in the analysis window while %s was crash-looping", failedReqs, podName),
			})
		}

		workload := workloadForPodName(podName)
		conf := 0.90
		if (hasBackOff || restarts > 0) && failedReqs > 0 {
			conf = 0.96
		}

		summary := fmt.Sprintf("Container in pod %s (%s) was OOMKilled (exit code 137)", podName, workload)
		if memLimitB > 0 {
			summary += fmt.Sprintf(" at memory limit %dMi", memLimitB/(1024*1024))
		}
		if restarts > 0 {
			summary += fmt.Sprintf(" and restarted %d times", restarts)
		}

		findings = append(findings, snapshot.Finding{
			Rule:       "oom-killed",
			Cause:      snapshot.CauseOOMKilled,
			Confidence: conf,
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
