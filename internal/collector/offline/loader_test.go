package offline_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/iyedM/kubepilot/internal/collector/offline"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

func TestParseK8sQuantities(t *testing.T) {
	cpuCases := []struct {
		in   string
		want int64
	}{
		{"50m", 50},
		{"300m", 300},
		{"500m", 500},
		{"2", 2000},
		{"64", 64000},
		{"0.5", 500},
		{"", 0},
	}
	for _, tc := range cpuCases {
		if got := offline.ParseCPUMillis(tc.in); got != tc.want {
			t.Errorf("ParseCPUMillis(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	memCases := []struct {
		in   string
		want int64
	}{
		{"40Mi", 40 * 1024 * 1024},
		{"400Mi", 400 * 1024 * 1024},
		{"1536Mi", 1536 * 1024 * 1024},
		{"1Gi", 1024 * 1024 * 1024},
		{"512Ki", 512 * 1024},
		{"500M", 500 * 1000 * 1000},
		{"", 0},
	}
	for _, tc := range memCases {
		if got := offline.ParseMemoryBytes(tc.in); got != tc.want {
			t.Errorf("ParseMemoryBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLoadDevRunOOMKillAndAppDelay(t *testing.T) {
	devRoot := filepath.Join("..", "..", "..", "eval", "runs", "dev")

	// 1. Load dev--oomkill--noload-400mi--r1
	oomDir := filepath.Join(devRoot, "dev--oomkill--noload-400mi--r1")
	snap, meta, err := offline.LoadRunDir(oomDir, offline.Options{})
	if err != nil {
		t.Fatalf("LoadRunDir(oomkill): %v", err)
	}
	if meta.RunID != "dev--oomkill--noload-400mi--r1" || meta.Split != "dev" {
		t.Errorf("unexpected meta: %+v", meta)
	}
	if len(snap.Window.Excluded) != 1 {
		t.Fatalf("expected 1 excluded range ([reset_start, lab_healthy_again]), got %d", len(snap.Window.Excluded))
	}
	assertNothingInExcludedRange(t, snap)

	hasBackOff := false
	for _, ev := range snap.Events {
		if ev.Reason == "BackOff" {
			hasBackOff = true
			break
		}
	}
	if !hasBackOff {
		t.Errorf("expected BackOff event in oomkill dev run, got %d events", len(snap.Events))
	}

	faultErrors := 0
	for _, req := range snap.Requests {
		if snap.Window.InAnalysisWindow(req.Timestamp) && req.Status != 200 {
			faultErrors++
		}
	}
	if faultErrors == 0 {
		t.Errorf("expected failed loadgen requests in oomkill fault window")
	}

	if _, ok := snap.Metrics["last_terminated_reason"]; !ok {
		t.Errorf("expected last_terminated_reason metric series in oomkill snapshot")
	}

	// 2. Load dev--app-delay--delay-2000ms--r1
	delayDir := filepath.Join(devRoot, "dev--app-delay--delay-2000ms--r1")
	delaySnap, delayMeta, err := offline.LoadRunDir(delayDir, offline.Options{})
	if err != nil {
		t.Fatalf("LoadRunDir(app-delay): %v", err)
	}
	if delayMeta.Target.Workload != "deployment/chat-api" {
		t.Errorf("delayMeta.Target.Workload = %q, want deployment/chat-api", delayMeta.Target.Workload)
	}
	assertNothingInExcludedRange(t, delaySnap)

	var baseGaps, faultGaps []float64
	for _, tr := range delaySnap.Traces {
		if delaySnap.Window.InBaseline(tr.StartTime) {
			baseGaps = append(baseGaps, tr.PreInferenceGapMs)
		} else if delaySnap.Window.InAnalysisWindow(tr.StartTime) {
			faultGaps = append(faultGaps, tr.PreInferenceGapMs)
		}
	}
	if len(baseGaps) == 0 || len(faultGaps) == 0 {
		t.Fatalf("expected both baseline (%d) and fault (%d) traces in app-delay run", len(baseGaps), len(faultGaps))
	}
	for _, g := range baseGaps {
		if g > 200 {
			t.Errorf("unexpectedly large baseline PreInferenceGapMs: %v", g)
		}
	}
	delayedCount := 0
	for _, g := range faultGaps {
		if g >= 1600 {
			delayedCount++
		}
	}
	if delayedCount == 0 {
		t.Errorf("expected delayed traces (PreInferenceGapMs >= 1600) in app-delay fault window")
	}
}

func assertNothingInExcludedRange(t *testing.T, snap *snapshot.Snapshot) {
	t.Helper()
	for _, req := range snap.Requests {
		if snap.Window.IsExcluded(req.Timestamp) {
			t.Errorf("request at %s falls inside excluded reset range", req.Timestamp)
		}
	}
	for _, tr := range snap.Traces {
		if snap.Window.IsExcluded(tr.StartTime) {
			t.Errorf("trace %s at %s falls inside excluded reset range", tr.TraceID, tr.StartTime)
		}
	}
	for _, ev := range snap.Events {
		if snap.Window.IsExcluded(ev.Timestamp) {
			t.Errorf("event %s at %s falls inside excluded reset range", ev.Reason, ev.Timestamp)
		}
	}
	for name, series := range snap.Metrics {
		for _, stream := range series.Streams {
			for _, s := range stream.Samples {
				if snap.Window.IsExcluded(s.Timestamp) {
					t.Errorf("metric %s sample at %s falls inside excluded reset range", name, s.Timestamp)
				}
			}
		}
	}
}

func TestClockSkewTolerance(t *testing.T) {
	dir := t.TempDir()

	// Analysis window: 12:03:00Z .. 12:08:00Z
	// Excluded reset outage: 12:08:00Z .. 12:08:10Z
	runJSON := `{
  "schema_version": 1,
  "run_id": "synthetic-skew-test",
  "split": "dev",
  "kind": "fault",
  "status": "complete",
  "timeline": {
    "baseline_start": "2026-10-21T12:00:00.000Z",
    "inject_start":   "2026-10-21T12:03:00.000Z",
    "inject_done":    "2026-10-21T12:03:02.000Z",
    "reset_start":    "2026-10-21T12:08:00.000Z",
    "reset_done":     "2026-10-21T12:08:05.000Z",
    "lab_healthy_again": "2026-10-21T12:08:10.000Z",
    "end":            "2026-10-21T12:11:10.000Z"
  },
  "target": {"namespace": "genai", "workload": "deployment/model-server", "pods_before": []},
  "expected_causes": []
}`
	if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte(runJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	// Event 1: skewed -22s before inject_start (12:02:38Z).
	// Event 2: inside excluded reset outage (12:08:04Z) -> must always be dropped.
	eventsJSON := `{
  "items": [
    {
      "involvedObject": {"namespace": "genai", "kind": "Pod", "name": "model-server-xyz"},
      "type": "Warning",
      "reason": "BackOff",
      "message": "Back-off restarting failed container",
      "lastTimestamp": "2026-10-21T12:02:38Z"
    },
    {
      "involvedObject": {"namespace": "genai", "kind": "Pod", "name": "model-server-old"},
      "type": "Normal",
      "reason": "Killing",
      "message": "Stopping container during reset",
      "lastTimestamp": "2026-10-21T12:08:04Z"
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "events.json"), []byte(eventsJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	// Metric sample 1: at 12:08:04Z (inside excluded reset range -> must be dropped).
	// Metric sample 2: at 12:08:18Z (+18s after reset_start, after lab_healthy_again -> kept under 30s skew, dropped under 5s skew).
	tExcluded := time.Date(2026, 10, 21, 12, 8, 4, 0, time.UTC).Unix()
	tSkewed := time.Date(2026, 10, 21, 12, 8, 18, 0, time.UTC).Unix()
	metricsJSON := `{
  "last_terminated_reason": {
    "query": "kube_pod_container_status_last_terminated_reason",
    "result": [
      {
        "metric": {"namespace": "genai", "pod": "model-server-xyz", "reason": "OOMKilled"},
        "values": [
          [` + strconv.FormatInt(tExcluded, 10) + `, "1"],
          [` + strconv.FormatInt(tSkewed, 10) + `, "1"]
        ]
      }
    ]
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "metrics.json"), []byte(metricsJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	// With default 30s skew tolerance: skewed event (-22s) and skewed metric (+18s) are kept;
	// reset-window event and metric (12:08:04Z) are excluded.
	snap30, _, err := offline.LoadRunDir(dir, offline.Options{SkewTolerance: 30 * time.Second})
	if err != nil {
		t.Fatalf("LoadRunDir(30s): %v", err)
	}
	if len(snap30.Events) != 1 || snap30.Events[0].Reason != "BackOff" {
		t.Errorf("with 30s skew, got events %+v, want [BackOff]", snap30.Events)
	}
	streams30 := snap30.Metrics["last_terminated_reason"].Streams
	if len(streams30) != 1 || len(streams30[0].Samples) != 1 || streams30[0].Samples[0].Timestamp.Unix() != tSkewed {
		t.Errorf("with 30s skew, got metric streams %+v, want single sample at %d", streams30, tSkewed)
	}

	// With tight 5s skew tolerance: both the -22s event and +18s metric fall outside the window.
	snap5, _, err := offline.LoadRunDir(dir, offline.Options{SkewTolerance: 5 * time.Second})
	if err != nil {
		t.Fatalf("LoadRunDir(5s): %v", err)
	}
	if len(snap5.Events) != 0 {
		t.Errorf("with 5s skew, expected 0 events, got %+v", snap5.Events)
	}
	if len(snap5.Metrics["last_terminated_reason"].Streams) != 0 {
		t.Errorf("with 5s skew, expected 0 metric streams, got %+v", snap5.Metrics["last_terminated_reason"].Streams)
	}
}
