package rules

import (
	"fmt"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

const minColdStartDelay = 15 * time.Second

var coldStartRule = Rule{
	Name:     "cold-start",
	Evaluate: evaluateColdStart,
}

func evaluateColdStart(snap *snapshot.Snapshot) []snapshot.Finding {
	failedReqs, firstFailTS := analysisFailedRequests(snap)
	if failedReqs == 0 {
		return nil
	}

	var findings []snapshot.Finding
	for _, podName := range candidateRequestPathPods(snap) {
		if isPodBefore(snap, podName) {
			continue
		}
		if oom, _ := hasOOMSignal(snap, podName); oom {
			continue
		}
		if _, backOff := hasBackOffEvent(snap, podName); backOff {
			continue
		}

		// Unlike readiness_failing or unschedulable, a cold-start pod eventually
		// becomes Ready during the fault window and requests succeed again.
		hasReady, _, maxReady, readyTS := podMetricWindowStats(snap, "pod_ready", podName, "", "")
		okAfterOutage, firstOKTS := analysisOKRequestsAfter(snap, firstFailTS)
		if (!hasReady || maxReady < 1) && okAfterOutage == 0 {
			continue
		}

		events := podEvents(snap, targetNamespace, podName)
		var scheduledTS, pullingTS, lastPulledTS, firstStartedTS, lastStartedTS time.Time
		var initImage string
		for _, ev := range events {
			if !snap.Window.InWindowWithSkew(ev.Timestamp) {
				continue
			}
			switch ev.Reason {
			case "Scheduled":
				if scheduledTS.IsZero() || ev.Timestamp.Before(scheduledTS) {
					scheduledTS = ev.Timestamp
				}
			case "Pulling":
				if pullingTS.IsZero() || ev.Timestamp.Before(pullingTS) {
					pullingTS = ev.Timestamp
				}
			case "Pulled":
				if strings.Contains(ev.Message, "curl") {
					initImage = "curl"
				}
				if lastPulledTS.IsZero() || ev.Timestamp.After(lastPulledTS) {
					lastPulledTS = ev.Timestamp
				}
			case "Started":
				if firstStartedTS.IsZero() || ev.Timestamp.Before(firstStartedTS) {
					firstStartedTS = ev.Timestamp
				}
				if lastStartedTS.IsZero() || ev.Timestamp.After(lastStartedTS) {
					lastStartedTS = ev.Timestamp
				}
			}
		}

		var startupDelay time.Duration
		var mechanism string
		var delayTS time.Time

		if !scheduledTS.IsZero() && !lastStartedTS.IsZero() && lastStartedTS.Sub(scheduledTS) >= minColdStartDelay {
			startupDelay = lastStartedTS.Sub(scheduledTS)
			delayTS = lastStartedTS
			if initImage != "" || (!firstStartedTS.IsZero() && lastStartedTS.Sub(firstStartedTS) >= minColdStartDelay) {
				mechanism = fmt.Sprintf("initContainer model fetch delayed main container start by %s", startupDelay.Round(time.Second))
			} else {
				mechanism = fmt.Sprintf("container startup/image pull took %s after scheduling", startupDelay.Round(time.Second))
			}
		} else if !pullingTS.IsZero() && !lastPulledTS.IsZero() && lastPulledTS.Sub(pullingTS) >= minColdStartDelay {
			startupDelay = lastPulledTS.Sub(pullingTS)
			delayTS = lastPulledTS
			mechanism = fmt.Sprintf("container image pull took %s", startupDelay.Round(time.Second))
		}

		if p := findPodSnapshot(snap, targetNamespace, podName); p != nil && startupDelay < minColdStartDelay {
			for _, ic := range p.InitContainers {
				if ic.LastTerminated != nil && !ic.LastTerminated.StartedAt.IsZero() && !ic.LastTerminated.FinishedAt.IsZero() {
					d := ic.LastTerminated.FinishedAt.Sub(ic.LastTerminated.StartedAt)
					if d >= minColdStartDelay {
						startupDelay = d
						delayTS = ic.LastTerminated.FinishedAt
						mechanism = fmt.Sprintf("initContainer %s ran for %s", ic.Name, d.Round(time.Second))
					}
				}
			}
		}

		if startupDelay < minColdStartDelay {
			continue
		}

		var evidence []snapshot.EvidenceItem
		if !scheduledTS.IsZero() {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceK8sEvent,
				Timestamp: scheduledTS,
				Summary:   fmt.Sprintf("pod %s Scheduled at %s", podName, scheduledTS.Format(time.RFC3339)),
			})
		}
		evidence = append(evidence, snapshot.EvidenceItem{
			Source:    snapshot.EvidenceK8sEvent,
			Timestamp: delayTS,
			Summary:   fmt.Sprintf("pod %s cold start: %s", podName, mechanism),
		})
		evidence = append(evidence, snapshot.EvidenceItem{
			Source:    snapshot.EvidenceRequestLog,
			Timestamp: firstFailTS,
			Summary:   fmt.Sprintf("%d requests failed while %s was starting", failedReqs, podName),
		})
		if okAfterOutage > 0 {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceRequestLog,
				Timestamp: firstOKTS,
				Summary:   fmt.Sprintf("serving recovered at %s (%d requests succeeded afterwards)", firstOKTS.Format(time.RFC3339), okAfterOutage),
			})
		} else if hasReady && maxReady >= 1 {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidencePrometheus,
				Timestamp: readyTS,
				Summary:   fmt.Sprintf("kube_pod_status_ready{condition=\"true\",pod=%q} transitioned to 1", podName),
			})
		}

		workload := workloadForPodName(podName)
		findings = append(findings, snapshot.Finding{
			Rule:       "cold-start",
			Cause:      snapshot.CauseColdStart,
			Confidence: 0.93,
			Component: snapshot.Component{
				Namespace: targetNamespace,
				Kind:      "Pod",
				Workload:  workload,
				Pod:       podName,
			},
			Summary:  fmt.Sprintf("Slow cold start on pod %s (%s): %s before becoming Ready", podName, workload, mechanism),
			Evidence: evidence,
		})
	}

	return findings
}
