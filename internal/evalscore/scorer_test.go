package evalscore_test

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iyedM/kubepilot/internal/evalscore"
)

func TestWilsonCI95(t *testing.T) {
	// 13/13: high=1.0, low ~ 0.7719
	low, high := evalscore.WilsonCI95(13, 13)
	if math.Abs(high-1.0) > 1e-6 {
		t.Errorf("WilsonCI95(13, 13) high = %f, want 1.0", high)
	}
	if low < 0.770 || low > 0.775 {
		t.Errorf("WilsonCI95(13, 13) low = %f, want ~0.772", low)
	}

	// 0/6: low=0.0, high ~ 0.3903
	low0, high0 := evalscore.WilsonCI95(0, 6)
	if math.Abs(low0-0.0) > 1e-6 {
		t.Errorf("WilsonCI95(0, 6) low = %f, want 0.0", low0)
	}
	if high0 < 0.388 || high0 > 0.393 {
		t.Errorf("WilsonCI95(0, 6) high = %f, want ~0.390", high0)
	}
}

func TestScoreSplitSkipsNonCompleteRuns(t *testing.T) {
	tmp := t.TempDir()

	// Create a non-complete run directory ("status": "invalid").
	invDir := filepath.Join(tmp, "dev--oomkill--invalid--r1")
	if err := os.MkdirAll(invDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invDir, "run.json"), []byte(`{
		"schema_version": 1,
		"run_id": "dev--oomkill--invalid--r1",
		"split": "dev",
		"kind": "fault",
		"status": "invalid"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := evalscore.ScoreSplit("dev", tmp)
	if err != nil {
		t.Fatalf("ScoreSplit: %v", err)
	}
	if rep.SkippedNonCompleteRuns != 1 || rep.FaultRunsScored != 0 {
		t.Errorf("expected 1 skipped non-complete run and 0 scored, got skipped=%d fault=%d",
			rep.SkippedNonCompleteRuns, rep.FaultRunsScored)
	}
}

func TestScoreDevSplit(t *testing.T) {
	devDir := filepath.Join("..", "..", "eval", "runs", "dev")
	rep, err := evalscore.ScoreSplit("dev", devDir)
	if err != nil {
		t.Fatalf("ScoreSplit(dev): %v", err)
	}

	if rep.FaultRunsScored != 13 {
		t.Errorf("FaultRunsScored = %d, want 13", rep.FaultRunsScored)
	}
	if rep.NoFaultRunsScored != 6 {
		t.Errorf("NoFaultRunsScored = %d, want 6", rep.NoFaultRunsScored)
	}
	if rep.Top1Accuracy.K != 13 || rep.Top1Accuracy.N != 13 {
		t.Errorf("Top1Accuracy = %s, want 13/13", rep.Top1Accuracy.Format())
	}
	if rep.Top3Recall.K != 13 || rep.Top3Recall.N != 13 {
		t.Errorf("Top3Recall = %s, want 13/13", rep.Top3Recall.Format())
	}
	if rep.EvidencePrecision.K != rep.EvidencePrecision.N || rep.EvidencePrecision.N == 0 {
		t.Errorf("EvidencePrecision = %s, want 100%%", rep.EvidencePrecision.Format())
	}
	if rep.FalsePositiveRateOnHealthy.K != 0 || rep.FalsePositiveRateOnHealthy.N != 6 {
		t.Errorf("FalsePositiveRateOnHealthy = %s, want 0/6", rep.FalsePositiveRateOnHealthy.Format())
	}

	var buf bytes.Buffer
	evalscore.RenderText(&buf, rep)
	out := buf.String()
	for _, want := range []string{
		"faults injected in a local lab",
		"correlated",
		"95% Wilson CI",
		"top1_accuracy:",
		"top3_recall:",
		"evidence_precision:",
		"false_positive_rate_on_healthy:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderText output missing %q:\n%s", want, out)
		}
	}
}
