// Package evalscore implements reproducible offline evaluation scoring of
// KubePilot's diagnosis engine over a split directory (eval/runs/<split>).
package evalscore

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/iyedM/kubepilot/internal/collector/offline"
	"github.com/iyedM/kubepilot/internal/rules"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

// MethodologicalDisclaimers holds the two mandatory reminders from eval/README.md
// that must accompany every published evaluation score.
var MethodologicalDisclaimers = []string{
	"All results are on faults injected in a local lab (single kind cluster, synthetic traffic), not on real production incidents.",
	"Evaluation cases are correlated (variants and repetitions of the same fault classes under the same conditions), so the 95% Wilson confidence intervals are optimistic.",
}

// BinomialMetric holds a k/n count, proportion, percentage, and 95% Wilson
// confidence interval.
type BinomialMetric struct {
	K          int     `json:"k"`
	N          int     `json:"n"`
	Proportion float64 `json:"proportion"`
	Percent    float64 `json:"percent"`
	WilsonLow  float64 `json:"wilson95Low"`
	WilsonHigh float64 `json:"wilson95High"`
}

// NewBinomialMetric computes the proportion and 95% Wilson score interval for k
// successes out of n trials.
func NewBinomialMetric(k, n int) BinomialMetric {
	if n <= 0 {
		return BinomialMetric{K: k, N: n}
	}
	p := float64(k) / float64(n)
	low, high := WilsonCI95(k, n)
	return BinomialMetric{
		K:          k,
		N:          n,
		Proportion: p,
		Percent:    p * 100.0,
		WilsonLow:  low,
		WilsonHigh: high,
	}
}

// Format returns a human-readable string showing k/n, percentage, and 95%
// Wilson confidence interval.
func (m BinomialMetric) Format() string {
	if m.N == 0 {
		return "0/0 (N/A)"
	}
	return fmt.Sprintf("%d/%d (%.1f%%, 95%% Wilson CI [%.1f%%, %.1f%%])",
		m.K, m.N, m.Percent, m.WilsonLow*100.0, m.WilsonHigh*100.0)
}

// WilsonCI95 returns the 95% Wilson score confidence interval [low, high] in
// [0, 1] for k successes in n trials (z = 1.959963984540054).
func WilsonCI95(k, n int) (low, high float64) {
	if n <= 0 {
		return 0, 0
	}
	if k < 0 {
		k = 0
	}
	if k > n {
		k = n
	}
	const z = 1.959963984540054
	z2 := z * z
	nf := float64(n)
	phat := float64(k) / nf

	denom := 1.0 + z2/nf
	center := (phat + z2/(2.0*nf)) / denom
	radicand := (phat*(1.0-phat))/nf + z2/(4.0*nf*nf)
	if radicand < 0 {
		radicand = 0
	}
	half := (z * math.Sqrt(radicand)) / denom

	low = center - half
	if low < 0 {
		low = 0
	}
	high = center + half
	if high > 1 {
		high = 1
	}
	return low, high
}

// RunScore records the diagnosis outcome for one completed run.
type RunScore struct {
	RunID             string         `json:"runId"`
	Kind              string         `json:"kind"` // "fault" or "nofault"
	ExpectedCause     snapshot.Cause `json:"expectedCause,omitempty"`
	ExpectedWorkload  string         `json:"expectedWorkload,omitempty"`
	Status            string         `json:"status"`
	PredictedTop1     snapshot.Cause `json:"predictedTop1,omitempty"`
	PredictedWorkload string         `json:"predictedWorkload,omitempty"`
	Top1Correct       bool           `json:"top1Correct,omitempty"`
	Top3Correct       bool           `json:"top3Correct,omitempty"`
	EvidenceSupported int            `json:"evidenceSupported,omitempty"`
	EvidenceTotal     int            `json:"evidenceTotal,omitempty"`
	FalsePositive     bool           `json:"falsePositive,omitempty"`
}

// CauseBreakdown summarizes accuracy for one fault cause class.
type CauseBreakdown struct {
	Cause        snapshot.Cause `json:"cause"`
	Top1Accuracy BinomialMetric `json:"top1Accuracy"`
	Top3Recall   BinomialMetric `json:"top3Recall"`
}

// SplitReport is the complete evaluation report over a split.
type SplitReport struct {
	Split                      string           `json:"split"`
	Disclaimers                []string         `json:"disclaimers"`
	FaultRunsScored            int              `json:"faultRunsScored"`
	NoFaultRunsScored          int              `json:"noFaultRunsScored"`
	SkippedNonCompleteRuns     int              `json:"skippedNonCompleteRuns"`
	Top1Accuracy               BinomialMetric   `json:"top1Accuracy"`
	Top3Recall                 BinomialMetric   `json:"top3Recall"`
	EvidencePrecision          BinomialMetric   `json:"evidencePrecision"`
	FalsePositiveRateOnHealthy BinomialMetric   `json:"falsePositiveRateOnHealthy"`
	ByCause                    []CauseBreakdown `json:"byCause,omitempty"`
	Runs                       []RunScore       `json:"runs"`
}

// ScoreSplit evaluates all runs with status=="complete" under splitDir
// (e.g. "eval/runs/dev" or "eval/runs/test").
func ScoreSplit(split, splitDir string) (*SplitReport, error) {
	entries, err := os.ReadDir(splitDir)
	if err != nil {
		return nil, fmt.Errorf("read split dir %s: %w", splitDir, err)
	}

	var dirNames []string
	for _, e := range entries {
		if e.IsDir() {
			dirNames = append(dirNames, e.Name())
		}
	}
	sort.Strings(dirNames)

	var (
		runs          []RunScore
		skipped       int
		top1K, faultN int
		top3K         int
		evSupportedK  int
		evTotalN      int
		fpK, nofaultN int
		causeTop1K    = make(map[snapshot.Cause]int)
		causeTop3K    = make(map[snapshot.Cause]int)
		causeTotalN   = make(map[snapshot.Cause]int)
	)

	for _, name := range dirNames {
		runDir := filepath.Join(splitDir, name)
		runJSONPath := filepath.Join(runDir, "run.json")
		rawMeta, err := os.ReadFile(runJSONPath)
		if err != nil {
			continue
		}
		var meta offline.RunMeta
		if err := json.Unmarshal(rawMeta, &meta); err != nil {
			return nil, fmt.Errorf("parse %s: %w", runJSONPath, err)
		}
		// Only score runs whose status is strictly "complete" (eval/README.md).
		if meta.Status != "complete" {
			skipped++
			continue
		}

		snap, _, err := offline.LoadRunDir(runDir, offline.Options{})
		if err != nil {
			return nil, fmt.Errorf("load run %s: %w", name, err)
		}
		rep := rules.Diagnose(snap)

		if meta.Kind == "nofault" || len(meta.Expected) == 0 {
			nofaultN++
			fp := rep.Status == snapshot.StatusDiagnosed || len(rep.TopCauses) > 0
			if fp {
				fpK++
			}
			var predTop1 snapshot.Cause
			var predWorkload string
			if len(rep.TopCauses) > 0 {
				predTop1 = rep.TopCauses[0].Cause
				predWorkload = rep.TopCauses[0].Component.Workload
			}
			runs = append(runs, RunScore{
				RunID:             meta.RunID,
				Kind:              "nofault",
				Status:            string(rep.Status),
				PredictedTop1:     predTop1,
				PredictedWorkload: predWorkload,
				FalsePositive:     fp,
			})
			continue
		}

		exp := meta.Expected[0]
		faultN++
		causeTotalN[exp.Cause]++

		var (
			predTop1     snapshot.Cause
			predWorkload string
			top1Hit      bool
			top3Hit      bool
			evSupp       int
			evTot        int
		)

		for idx, f := range rep.TopCauses {
			matchesCause := f.Cause == exp.Cause &&
				(exp.Component.Workload == "" || f.Component.Workload == exp.Component.Workload)
			if idx == 0 {
				predTop1 = f.Cause
				predWorkload = f.Component.Workload
				if matchesCause {
					top1Hit = true
				}
			}
			if idx < rules.MaxTopCauses && matchesCause {
				top3Hit = true
			}
			for _, ev := range f.Evidence {
				evTot++
				if isEvidenceSupported(ev, f, exp, snap.Window) {
					evSupp++
				}
			}
		}

		if top1Hit {
			top1K++
			causeTop1K[exp.Cause]++
		}
		if top3Hit {
			top3K++
			causeTop3K[exp.Cause]++
		}
		evSupportedK += evSupp
		evTotalN += evTot

		runs = append(runs, RunScore{
			RunID:             meta.RunID,
			Kind:              "fault",
			ExpectedCause:     exp.Cause,
			ExpectedWorkload:  exp.Component.Workload,
			Status:            string(rep.Status),
			PredictedTop1:     predTop1,
			PredictedWorkload: predWorkload,
			Top1Correct:       top1Hit,
			Top3Correct:       top3Hit,
			EvidenceSupported: evSupp,
			EvidenceTotal:     evTot,
		})
	}

	var byCause []CauseBreakdown
	for _, c := range snapshot.AllCatalogCauses() {
		n := causeTotalN[c]
		if n == 0 {
			continue
		}
		byCause = append(byCause, CauseBreakdown{
			Cause:        c,
			Top1Accuracy: NewBinomialMetric(causeTop1K[c], n),
			Top3Recall:   NewBinomialMetric(causeTop3K[c], n),
		})
	}

	return &SplitReport{
		Split:                      split,
		Disclaimers:                append([]string(nil), MethodologicalDisclaimers...),
		FaultRunsScored:            faultN,
		NoFaultRunsScored:          nofaultN,
		SkippedNonCompleteRuns:     skipped,
		Top1Accuracy:               NewBinomialMetric(top1K, faultN),
		Top3Recall:                 NewBinomialMetric(top3K, faultN),
		EvidencePrecision:          NewBinomialMetric(evSupportedK, evTotalN),
		FalsePositiveRateOnHealthy: NewBinomialMetric(fpK, nofaultN),
		ByCause:                    byCause,
		Runs:                       runs,
	}, nil
}

func isEvidenceSupported(ev snapshot.EvidenceItem, f snapshot.Finding, exp offline.ExpectedCause, win snapshot.Window) bool {
	if ev.Source == "" || ev.Summary == "" {
		return false
	}
	if !ev.Timestamp.IsZero() && !win.InWindowWithSkew(ev.Timestamp) && !win.InBaseline(ev.Timestamp) {
		return false
	}
	if f.Cause != exp.Cause {
		return false
	}
	if exp.Component.Namespace != "" && f.Component.Namespace != exp.Component.Namespace {
		return false
	}
	if exp.Component.Workload != "" && f.Component.Workload != exp.Component.Workload {
		return false
	}
	return true
}

// RenderText writes a formatted human-readable evaluation report to w.
func RenderText(w io.Writer, r *SplitReport) {
	fmt.Fprintf(w, "=== KubePilot Evaluation Score (split: %s) ===\n\n", r.Split)
	fmt.Fprintln(w, "Methodological Notes (eval/README.md):")
	for _, d := range r.Disclaimers {
		fmt.Fprintf(w, "  * %s\n", d)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "Runs scored: %d fault runs, %d no-fault windows (%d non-complete skipped)\n\n",
		r.FaultRunsScored, r.NoFaultRunsScored, r.SkippedNonCompleteRuns)

	fmt.Fprintln(w, "Summary Metrics:")
	fmt.Fprintf(w, "  top1_accuracy:                  %s\n", r.Top1Accuracy.Format())
	fmt.Fprintf(w, "  top3_recall:                    %s\n", r.Top3Recall.Format())
	fmt.Fprintf(w, "  evidence_precision:             %s\n", r.EvidencePrecision.Format())
	fmt.Fprintf(w, "  false_positive_rate_on_healthy: %s\n", r.FalsePositiveRateOnHealthy.Format())

	if len(r.ByCause) > 0 {
		fmt.Fprintln(w, "\nPer-Cause Breakdown:")
		for _, bc := range r.ByCause {
			fmt.Fprintf(w, "  %-34s top1=%s | top3=%s\n",
				bc.Cause, bc.Top1Accuracy.Format(), bc.Top3Recall.Format())
		}
	}

	if len(r.Runs) > 0 {
		fmt.Fprintln(w, "\nPer-Run Results:")
		for _, rs := range r.Runs {
			if rs.Kind == "nofault" {
				verdict := "PASS (no false positive)"
				if rs.FalsePositive {
					verdict = fmt.Sprintf("FAIL (false positive: %s on %s)", rs.PredictedTop1, rs.PredictedWorkload)
				}
				fmt.Fprintf(w, "  %-46s [nofault] -> %s\n", rs.RunID, verdict)
				continue
			}
			verdict := "FAIL"
			if rs.Top1Correct {
				verdict = "PASS (top-1)"
			} else if rs.Top3Correct {
				verdict = "PASS (top-3)"
			}
			fmt.Fprintf(w, "  %-46s [fault]   -> %s (want %s on %s, got %s on %s, evidence %d/%d)\n",
				rs.RunID, verdict, rs.ExpectedCause, rs.ExpectedWorkload,
				rs.PredictedTop1, rs.PredictedWorkload, rs.EvidenceSupported, rs.EvidenceTotal)
		}
	}
}
