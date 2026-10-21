package rules

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

var unschedulableRule = Rule{
	Name:     "unschedulable",
	Evaluate: evaluateUnschedulable,
}

// SchedulingBlocker represents one parsed reason from a FailedScheduling event
// message (e.g. "2 Insufficient cpu" -> NodeCount=2, Reason="Insufficient cpu").
type SchedulingBlocker struct {
	NodeCount int
	Reason    string
}

// ParseFailedSchedulingReasons extracts the per-reason node counts from a
// Kubernetes scheduler FailedScheduling message and returns the primary
// blocker together with all parsed blockers.
//
// As documented in docs/lab.md ("Observations for P2"), a kind cluster with 1
// control-plane node and 2 workers reports "1 node(s) had untolerated taint(s)"
// on every FailedScheduling event where the pod does not tolerate the
// control-plane taint. Taints are only the primary blocker when no worker-specific
// reason (node affinity/selector, Insufficient cpu/memory, unbound PVC) accounts
// for the remaining nodes.
func ParseFailedSchedulingReasons(msg string) (primary string, blockers []SchedulingBlocker) {
	body := msg
	if idx := strings.Index(body, "nodes are available:"); idx >= 0 {
		body = strings.TrimSpace(body[idx+len("nodes are available:"):])
	}
	if idx := strings.Index(body, ". "); idx >= 0 {
		body = strings.TrimSpace(body[:idx])
	}
	body = strings.TrimSuffix(body, ".")

	parts := strings.Split(body, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Fields(part)
		if len(fields) >= 2 {
			if n, err := strconv.Atoi(fields[0]); err == nil {
				reason := strings.Join(fields[1:], " ")
				blockers = append(blockers, SchedulingBlocker{NodeCount: n, Reason: reason})
				continue
			}
		}
		blockers = append(blockers, SchedulingBlocker{NodeCount: 0, Reason: part})
	}

	if len(blockers) == 0 {
		return msg, nil
	}

	// Pick the primary blocker: prefer non-taint reasons when a 1-node
	// control-plane taint coexists with worker reasons.
	bestIdx := -1
	for i, b := range blockers {
		isTaint := strings.Contains(b.Reason, "untolerated taint")
		if isTaint && len(blockers) > 1 && b.NodeCount <= 1 {
			continue
		}
		if bestIdx == -1 || b.NodeCount > blockers[bestIdx].NodeCount {
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		bestIdx = 0
	}
	b := blockers[bestIdx]
	if b.NodeCount > 0 {
		return fmt.Sprintf("%d %s", b.NodeCount, b.Reason), blockers
	}
	return b.Reason, blockers
}

func evaluateUnschedulable(snap *snapshot.Snapshot) []snapshot.Finding {
	failedReqs, firstFailTS := analysisFailedRequests(snap)
	var findings []snapshot.Finding

	for _, podName := range candidateRequestPathPods(snap) {
		var schedEvents []snapshot.K8sEvent
		for _, ev := range podEvents(snap, targetNamespace, podName) {
			if ev.Reason == "FailedScheduling" && snap.Window.InWindowWithSkew(ev.Timestamp) {
				schedEvents = append(schedEvents, ev)
			}
		}

		p := findPodSnapshot(snap, targetNamespace, podName)
		podPendingUnsched := false
		var condMsg string
		var condTS time.Time
		if p != nil && p.Phase == "Pending" {
			for _, c := range p.Conditions {
				if c.Type == "PodScheduled" && c.Status == "False" && c.Reason == "Unschedulable" {
					podPendingUnsched = true
					condMsg = c.Message
					condTS = c.LastTransitionTime
					break
				}
			}
		}

		hasPendingMetric, _, maxPending, pendingTS := podMetricWindowStats(snap, "pod_phase", podName, "phase", "Pending")
		if len(schedEvents) == 0 && !podPendingUnsched {
			continue
		}

		var evidence []snapshot.EvidenceItem
		rawMsg := condMsg
		var msgTS time.Time = condTS
		if len(schedEvents) > 0 {
			lastEv := schedEvents[len(schedEvents)-1]
			rawMsg = lastEv.Message
			msgTS = lastEv.Timestamp
			primary, _ := ParseFailedSchedulingReasons(rawMsg)
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceK8sEvent,
				Timestamp: lastEv.Timestamp,
				Summary:   fmt.Sprintf("FailedScheduling on pod %s (primary blocker: %s): %s", podName, primary, rawMsg),
			})
		}

		if podPendingUnsched {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceK8sPod,
				Timestamp: condTS,
				Summary:   fmt.Sprintf("pod %s is Pending with PodScheduled=False/Unschedulable", podName),
			})
		}

		if hasPendingMetric && maxPending >= 1 {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidencePrometheus,
				Timestamp: pendingTS,
				Summary:   fmt.Sprintf("kube_pod_status_phase{pod=%q,phase=\"Pending\"} == 1", podName),
			})
		}

		// Attach any ProvisioningFailed event on a PVC in the same namespace.
		for _, ev := range snap.Events {
			if ev.Namespace == targetNamespace && ev.InvolvedKind == "PersistentVolumeClaim" &&
				ev.Reason == "ProvisioningFailed" && snap.Window.InWindowWithSkew(ev.Timestamp) {
				evidence = append(evidence, snapshot.EvidenceItem{
					Source:    snapshot.EvidenceK8sEvent,
					Timestamp: ev.Timestamp,
					Summary:   fmt.Sprintf("PVC %s ProvisioningFailed: %s", ev.InvolvedName, ev.Message),
				})
			}
		}

		if failedReqs > 0 {
			evidence = append(evidence, snapshot.EvidenceItem{
				Source:    snapshot.EvidenceRequestLog,
				Timestamp: firstFailTS,
				Summary:   fmt.Sprintf("%d requests failed while %s remained Pending", failedReqs, podName),
			})
		}

		primary, _ := ParseFailedSchedulingReasons(rawMsg)
		workload := workloadForPodName(podName)
		conf := 0.92
		if len(schedEvents) > 0 && failedReqs > 0 {
			conf = 0.96
		}
		_ = msgTS

		findings = append(findings, snapshot.Finding{
			Rule:       "unschedulable",
			Cause:      snapshot.CauseUnschedulable,
			Confidence: conf,
			Component: snapshot.Component{
				Namespace: targetNamespace,
				Kind:      "Pod",
				Workload:  workload,
				Pod:       podName,
			},
			Summary:  fmt.Sprintf("Pod %s (%s) is Pending/Unschedulable: %s", podName, workload, primary),
			Evidence: evidence,
		})
	}

	return findings
}
