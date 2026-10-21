package snapshot_test

import (
	"testing"
	"time"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

func TestWindowExclusionAndSkewTolerance(t *testing.T) {
	base := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	w := snapshot.Window{
		From:          base,
		To:            base.Add(5 * time.Minute),
		SkewTolerance: 30 * time.Second,
		Excluded: []snapshot.TimeRange{
			{
				From:   base.Add(5 * time.Minute),
				To:     base.Add(5*time.Minute + 10*time.Second),
				Reason: "lab reset outage",
			},
		},
	}

	// 20 seconds before From is inside skew tolerance.
	if !w.InWindowWithSkew(base.Add(-20 * time.Second)) {
		t.Errorf("expected -20s to be within 30s skew tolerance")
	}
	// 40 seconds before From is outside skew tolerance.
	if w.InWindowWithSkew(base.Add(-40 * time.Second)) {
		t.Errorf("expected -40s to be outside 30s skew tolerance")
	}
	// Timestamp inside [reset_start, lab_healthy_again) must be excluded even
	// though it is within From..To+SkewTolerance.
	if w.InWindowWithSkew(base.Add(5*time.Minute + 3*time.Second)) {
		t.Errorf("expected timestamp inside excluded reset range to be rejected")
	}
	// Timestamp right after lab_healthy_again (still within +30s skew) is allowed.
	if !w.InWindowWithSkew(base.Add(5*time.Minute + 15*time.Second)) {
		t.Errorf("expected timestamp after excluded range within skew to be accepted")
	}
}
