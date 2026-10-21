package rules_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iyedM/kubepilot/internal/collector/offline"
	"github.com/iyedM/kubepilot/internal/rules"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

func TestParseFailedSchedulingReasons(t *testing.T) {
	cases := []struct {
		name        string
		msg         string
		wantPrimary string
	}{
		{
			name:        "nodeselector-gpu ignores 1 control-plane taint",
			msg:         "0/3 nodes are available: 1 node(s) had untolerated taint(s), 2 node(s) didn't match Pod's node affinity/selector. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling.",
			wantPrimary: "2 node(s) didn't match Pod's node affinity/selector",
		},
		{
			name:        "cpu-request-64 ignores 1 control-plane taint",
			msg:         "0/3 nodes are available: 1 node(s) had untolerated taint(s), 2 Insufficient cpu. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling.",
			wantPrimary: "2 Insufficient cpu",
		},
		{
			name:        "all nodes tainted blames taints",
			msg:         "0/3 nodes are available: 3 node(s) had untolerated taint(s). preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling.",
			wantPrimary: "3 node(s) had untolerated taint(s)",
		},
		{
			name:        "unbound pvc",
			msg:         "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims. not found",
			wantPrimary: "pod has unbound immediate PersistentVolumeClaims",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := rules.ParseFailedSchedulingReasons(tc.msg)
			if got != tc.wantPrimary {
				t.Errorf("ParseFailedSchedulingReasons(%q) = %q, want %q", tc.msg, got, tc.wantPrimary)
			}
		})
	}
}

func TestReadinessIgnoresOldPodRecreateArtifact(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 13, 9, 0, 0, time.UTC)
	snap := &snapshot.Snapshot{
		Window: snapshot.Window{
			From:          t0,
			To:            t0.Add(5 * time.Minute),
			SkewTolerance: 30 * time.Second,
		},
		PodsBefore: []snapshot.PodIdentity{
			{Name: "model-server-old-57p7c", Namespace: "genai", Phase: "Running"},
		},
		Requests: []snapshot.RequestSample{
			{Timestamp: t0.Add(10 * time.Second), PromptID: "short-16", Status: 502, Error: "APIConnectionError"},
		},
		Events: []snapshot.K8sEvent{
			{
				Timestamp:    t0.Add(2 * time.Second),
				Namespace:    "genai",
				InvolvedKind: "Pod",
				InvolvedName: "model-server-old-57p7c",
				Type:         "Normal",
				Reason:       "Killing",
				Message:      "Stopping container model-server",
			},
			{
				Timestamp:    t0.Add(5 * time.Second),
				Namespace:    "genai",
				InvolvedKind: "Pod",
				InvolvedName: "model-server-old-57p7c",
				Type:         "Warning",
				Reason:       "Unhealthy",
				Message:      "Readiness probe failed: Get \"http://10.244.2.11:8080/health\": dial tcp 10.244.2.11:8080: connect: connection refused",
			},
		},
	}

	rep := rules.Diagnose(snap)
	for _, f := range rep.TopCauses {
		if f.Cause == snapshot.CauseReadinessFail {
			t.Fatalf("readiness_failing falsely triggered on terminating old pod: %+v", f)
		}
	}
}

func TestReadinessInitialDelaySpecOnly(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	snap := &snapshot.Snapshot{
		Window: snapshot.Window{
			From:          t0,
			To:            t0.Add(5 * time.Minute),
			SkewTolerance: 30 * time.Second,
		},
		PodsBefore: []snapshot.PodIdentity{
			{Name: "model-server-old", Namespace: "genai", Phase: "Running"},
		},
		Pods: []snapshot.PodSnapshot{
			{
				Name:              "model-server-new-delay600",
				Namespace:         "genai",
				Workload:          "deployment/model-server",
				Phase:             "Running",
				CreationTimestamp: t0.Add(2 * time.Second),
				Conditions: []snapshot.PodCondition{
					{Type: "Ready", Status: "False"},
				},
				Containers: []snapshot.ContainerSnapshot{
					{
						Name:  "model-server",
						Ready: false,
						ReadinessProbe: &snapshot.ProbeSpec{
							Path:                "/health",
							Port:                "http",
							InitialDelaySeconds: 600,
						},
					},
				},
			},
		},
		Requests: []snapshot.RequestSample{
			{Timestamp: t0.Add(20 * time.Second), PromptID: "short-16", Status: 502, Error: "APIConnectionError"},
		},
	}

	rep := rules.Diagnose(snap)
	if rep.Status != snapshot.StatusDiagnosed || len(rep.TopCauses) == 0 {
		t.Fatalf("expected StatusDiagnosed, got %+v", rep)
	}
	top := rep.TopCauses[0]
	if top.Cause != snapshot.CauseReadinessFail {
		t.Errorf("top.Cause = %q, want %q", top.Cause, snapshot.CauseReadinessFail)
	}
	if !strings.Contains(top.Summary, "initialDelaySeconds=600") {
		t.Errorf("expected initialDelaySeconds=600 in summary, got %q", top.Summary)
	}
}

func TestHostContentionWithNeighborPod(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	baseStart := t0.Add(-3 * time.Minute)
	snap := &snapshot.Snapshot{
		Window: snapshot.Window{
			From:          t0,
			To:            t0.Add(5 * time.Minute),
			Baseline:      &snapshot.TimeRange{From: baseStart, To: t0, Reason: "baseline"},
			SkewTolerance: 30 * time.Second,
		},
		Pods: []snapshot.PodSnapshot{
			{
				Name:      "model-server-5bd4cc46f9-xd8d4",
				Namespace: "genai",
				Workload:  "deployment/model-server",
				NodeName:  "kubepilot-lab-worker",
				Phase:     "Running",
			},
			{
				Name:      "cpu-neighbor",
				Namespace: "noise",
				NodeName:  "kubepilot-lab-worker",
				Phase:     "Running",
			},
		},
		Requests: []snapshot.RequestSample{
			{Timestamp: baseStart.Add(30 * time.Second), PromptID: "short-16", Status: 200, ClientLatencyMs: 500},
			{Timestamp: baseStart.Add(90 * time.Second), PromptID: "short-16", Status: 200, ClientLatencyMs: 510},
			{Timestamp: t0.Add(30 * time.Second), PromptID: "short-16", Status: 200, ClientLatencyMs: 910},
			{Timestamp: t0.Add(90 * time.Second), PromptID: "short-16", Status: 200, ClientLatencyMs: 930},
		},
		Metrics: map[string]snapshot.Series{
			"cpu_periods": {
				Streams: []snapshot.MetricStream{
					{
						Labels: map[string]string{"namespace": "genai", "pod": "model-server-5bd4cc46f9-xd8d4", "container": "model-server"},
						Samples: []snapshot.MetricSample{
							{Timestamp: t0.Add(15 * time.Second), Value: 1000},
							{Timestamp: t0.Add(285 * time.Second), Value: 4000},
						},
					},
				},
			},
			"cpu_throttled_periods": {
				Streams: []snapshot.MetricStream{
					{
						Labels: map[string]string{"namespace": "genai", "pod": "model-server-5bd4cc46f9-xd8d4", "container": "model-server"},
						Samples: []snapshot.MetricSample{
							{Timestamp: t0.Add(15 * time.Second), Value: 10},
							{Timestamp: t0.Add(285 * time.Second), Value: 13},
						},
					},
				},
			},
			"cpu_pressure_waiting_seconds": {
				Streams: []snapshot.MetricStream{
					{
						Labels: map[string]string{"namespace": "genai", "pod": "model-server-5bd4cc46f9-xd8d4", "container": "model-server"},
						Samples: []snapshot.MetricSample{
							{Timestamp: baseStart.Add(15 * time.Second), Value: 1.0},
							{Timestamp: baseStart.Add(165 * time.Second), Value: 1.15},
							{Timestamp: t0.Add(15 * time.Second), Value: 1.20},
							{Timestamp: t0.Add(285 * time.Second), Value: 19.50},
						},
					},
				},
			},
			"cpu_usage_seconds": {
				Streams: []snapshot.MetricStream{
					{
						Labels: map[string]string{"namespace": "noise", "pod": "cpu-neighbor", "container": "burn"},
						Samples: []snapshot.MetricSample{
							{Timestamp: t0.Add(15 * time.Second), Value: 10.0},
							{Timestamp: t0.Add(285 * time.Second), Value: 2035.0}, // 7.5 cores over 270s
						},
					},
				},
			},
		},
	}

	rep := rules.Diagnose(snap)
	if rep.Status != snapshot.StatusDiagnosed || len(rep.TopCauses) == 0 {
		t.Fatalf("expected StatusDiagnosed, got %+v", rep)
	}
	top := rep.TopCauses[0]
	if top.Cause != snapshot.CauseHostContention {
		t.Fatalf("top.Cause = %q, want %q", top.Cause, snapshot.CauseHostContention)
	}
	foundNeighborEvidence := false
	for _, ev := range top.Evidence {
		if strings.Contains(ev.Summary, "noise/cpu-neighbor") {
			foundNeighborEvidence = true
			break
		}
	}
	if !foundNeighborEvidence {
		t.Errorf("expected evidence citing neighbor pod noise/cpu-neighbor, got %+v", top.Evidence)
	}
}

func TestIntermittentAppDelayTail(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC)
	var traces []snapshot.TraceSummary
	for i := 0; i < 9; i++ {
		gap := 0.4
		if i%3 == 2 {
			gap = 2001.0 // 1 request in 3 delayed by ~2000 ms (p50 ~ 0.4 ms, p90 = 2001 ms)
		}
		traces = append(traces, snapshot.TraceSummary{
			TraceID:           "trace-" + string(rune('a'+i)),
			StartTime:         t0.Add(time.Duration(i*20+10) * time.Second),
			EndTime:           t0.Add(time.Duration(i*20+13) * time.Second),
			PreInferenceGapMs: gap,
			InferenceMs:       500,
			HTTPDurationMs:    500 + gap,
			ClientPodName:     "chat-api-69fb959bd4-2lst4",
			ClientNamespace:   "genai",
			ClientDeployment:  "chat-api",
		})
	}

	snap := &snapshot.Snapshot{
		Window: snapshot.Window{
			From:          t0,
			To:            t0.Add(5 * time.Minute),
			SkewTolerance: 30 * time.Second,
		},
		Traces: traces,
	}

	rep := rules.Diagnose(snap)
	if rep.Status != snapshot.StatusDiagnosed || len(rep.TopCauses) == 0 {
		t.Fatalf("expected StatusDiagnosed on intermittent app-delay, got %+v", rep)
	}
	top := rep.TopCauses[0]
	if top.Cause != snapshot.CauseUnknownNoInfra {
		t.Errorf("top.Cause = %q, want %q", top.Cause, snapshot.CauseUnknownNoInfra)
	}
	if top.Component.Workload != "deployment/chat-api" {
		t.Errorf("top.Component.Workload = %q, want deployment/chat-api", top.Component.Workload)
	}
	if len(top.Evidence) == 0 || top.Evidence[0].Source != snapshot.EvidenceTrace {
		t.Errorf("expected trace gap evidence item, got %+v", top.Evidence)
	}
}

func TestNoiseDecoyThrottlingAndOOMIgnored(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 17, 0, 0, 0, time.UTC)
	snap := &snapshot.Snapshot{
		Window: snapshot.Window{
			From:          t0,
			To:            t0.Add(5 * time.Minute),
			SkewTolerance: 30 * time.Second,
		},
		Pods: []snapshot.PodSnapshot{
			{
				Name:      "decoy-oom",
				Namespace: "noise",
				Phase:     "Running",
				Containers: []snapshot.ContainerSnapshot{
					{
						Name:         "decoy",
						RestartCount: 4,
						LastTerminated: &snapshot.ContainerState{
							Kind:     "terminated",
							Reason:   "OOMKilled",
							ExitCode: 137,
						},
					},
				},
			},
		},
		Events: []snapshot.K8sEvent{
			{
				Timestamp:    t0.Add(30 * time.Second),
				Namespace:    "noise",
				InvolvedKind: "Pod",
				InvolvedName: "decoy-oom",
				Type:         "Warning",
				Reason:       "BackOff",
				Message:      "Back-off restarting failed container",
			},
		},
		Metrics: map[string]snapshot.Series{
			"cpu_periods": {
				Streams: []snapshot.MetricStream{
					{
						Labels: map[string]string{"namespace": "noise", "pod": "decoy-oom", "container": "decoy"},
						Samples: []snapshot.MetricSample{
							{Timestamp: t0.Add(15 * time.Second), Value: 100},
							{Timestamp: t0.Add(120 * time.Second), Value: 1100},
						},
					},
				},
			},
			"cpu_throttled_periods": {
				Streams: []snapshot.MetricStream{
					{
						Labels: map[string]string{"namespace": "noise", "pod": "decoy-oom", "container": "decoy"},
						Samples: []snapshot.MetricSample{
							{Timestamp: t0.Add(15 * time.Second), Value: 50},
							{Timestamp: t0.Add(120 * time.Second), Value: 980},
						},
					},
				},
			},
		},
	}

	rep := rules.Diagnose(snap)
	if rep.Status != snapshot.StatusUnknown || len(rep.TopCauses) != 0 {
		t.Fatalf("expected StatusUnknown when only noise namespace pod fails/throttles, got %+v", rep)
	}
}

func TestAll19DevRuns(t *testing.T) {
	devRoot := filepath.Join("..", "..", "eval", "runs", "dev")

	faultRuns := []string{
		"dev--cpu-throttle--limit-300m--r1",
		"dev--cpu-throttle--limit-500m--r1",
		"dev--oomkill--noload-400mi--r1",
		"dev--oomkill--noload-450mi--r1",
		"dev--unschedulable--nodeselector-gpu--r1",
		"dev--unschedulable--cpu-request-64--r1",
		"dev--readiness-fail--probe-path-404--r1",
		"dev--readiness-fail--probe-wrong-port--r1",
		"dev--cold-start--model-download-10mbs--r1",
		"dev--cold-start--model-download-20mbs--r1",
		"dev--config-regression--threads-1--r1",
		"dev--host-contention--host-busy-loops-8--r1",
		"dev--app-delay--delay-2000ms--r1",
	}

	for _, runID := range faultRuns {
		t.Run(runID, func(t *testing.T) {
			snap, meta, err := offline.LoadRunDir(filepath.Join(devRoot, runID), offline.Options{})
			if err != nil {
				t.Fatalf("LoadRunDir(%s): %v", runID, err)
			}
			if len(meta.Expected) == 0 {
				t.Fatalf("run %s has no expected_causes in run.json", runID)
			}
			wantCause := meta.Expected[0].Cause
			wantWorkload := meta.Expected[0].Component.Workload

			rep := rules.Diagnose(snap)
			if rep.Status != snapshot.StatusDiagnosed || len(rep.TopCauses) == 0 {
				t.Fatalf("Diagnose(%s) returned %s (%s), want top-1 %s", runID, rep.Status, rep.Reason, wantCause)
			}
			got := rep.TopCauses[0]
			if got.Cause != wantCause {
				t.Errorf("Diagnose(%s) top-1 cause = %q (summary: %s), want %q (all findings: %+v)",
					runID, got.Cause, got.Summary, wantCause, rep.TopCauses)
			}
			if got.Component.Workload != wantWorkload {
				t.Errorf("Diagnose(%s) top-1 workload = %q, want %q", runID, got.Component.Workload, wantWorkload)
			}
			if len(got.Evidence) == 0 {
				t.Errorf("Diagnose(%s) top-1 finding has no evidence items", runID)
			}
			t.Logf("%s -> top1=%s workload=%s pod=%s conf=%.2f summary=%q",
				runID, got.Cause, got.Component.Workload, got.Component.Pod, got.Confidence, got.Summary)
		})
	}

	nofaultRuns := []string{
		"dev--nofault--quiet--r1",
		"dev--nofault--quiet--r2",
		"dev--nofault--decoy-same-node--r1",
		"dev--nofault--decoy-same-node--r2",
		"dev--nofault--decoy-other-node-oom--r1",
		"dev--nofault--decoy-other-node-oom--r2",
	}

	for _, runID := range nofaultRuns {
		t.Run(runID, func(t *testing.T) {
			snap, _, err := offline.LoadRunDir(filepath.Join(devRoot, runID), offline.Options{})
			if err != nil {
				t.Fatalf("LoadRunDir(%s): %v", runID, err)
			}
			rep := rules.Diagnose(snap)
			if rep.Status != snapshot.StatusUnknown || len(rep.TopCauses) != 0 {
				t.Errorf("Diagnose(%s) produced false positive on no-fault window: %+v", runID, rep.TopCauses)
			}
		})
	}
}
