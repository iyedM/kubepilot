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

func TestDevRunsLot1AndNoFaults(t *testing.T) {
	devRoot := filepath.Join("..", "..", "eval", "runs", "dev")

	lot1Runs := []string{
		"dev--oomkill--noload-400mi--r1",
		"dev--oomkill--noload-450mi--r1",
		"dev--unschedulable--nodeselector-gpu--r1",
		"dev--unschedulable--cpu-request-64--r1",
		"dev--readiness-fail--probe-path-404--r1",
		"dev--readiness-fail--probe-wrong-port--r1",
		"dev--cold-start--model-download-10mbs--r1",
		"dev--cold-start--model-download-20mbs--r1",
	}

	for _, runID := range lot1Runs {
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
