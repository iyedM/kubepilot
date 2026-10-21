// Package rules implements KubePilot's deterministic diagnosis engine.
//
// Modeled after kubectl-why-fail: every rule is a pure function over a
// collected snapshot.Snapshot (no network, no cluster access, no wall clock).
// No OpenTelemetry "gen_ai.*" attribute or metric names may appear here.
package rules

import (
	"sort"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

// MaxTopCauses is the maximum number of probable causes returned in a Report.
const MaxTopCauses = 3

// Rule is one deterministic diagnosis rule over a collected Snapshot.
type Rule struct {
	Name     string
	Evaluate func(snap *snapshot.Snapshot) []snapshot.Finding
}

// registry lists the registered diagnosis rules in deterministic tie-break
// priority order.
var registry = []Rule{
	oomKilledRule,
	unschedulableRule,
	readinessFailingRule,
	coldStartRule,
	cpuThrottlingRule,
	hostContentionRule,
	configChangeRule,
	unknownNoInfraRule,
}

// All returns a copy of the registered rules in priority order.
func All() []Rule {
	out := make([]Rule, len(registry))
	copy(out, registry)
	return out
}

// EvaluateAll runs every registered rule on snap and returns the findings
// sorted by descending confidence (breaking ties by rule registration order).
func EvaluateAll(snap *snapshot.Snapshot) []snapshot.Finding {
	if snap == nil {
		return nil
	}
	type indexedFinding struct {
		order   int
		finding snapshot.Finding
	}
	var all []indexedFinding
	for i, r := range registry {
		if r.Evaluate == nil {
			continue
		}
		for _, f := range r.Evaluate(snap) {
			if f.Confidence <= 0 {
				continue
			}
			if f.Rule == "" {
				f.Rule = r.Name
			}
			all = append(all, indexedFinding{order: i, finding: f})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].finding.Confidence != all[j].finding.Confidence {
			return all[i].finding.Confidence > all[j].finding.Confidence
		}
		return all[i].order < all[j].order
	})
	out := make([]snapshot.Finding, 0, len(all))
	for _, item := range all {
		out = append(out, item.finding)
	}
	return out
}

// Diagnose evaluates all rules on snap and produces the top-3 Report (or
// StatusUnknown when no rule matches).
func Diagnose(snap *snapshot.Snapshot) snapshot.Report {
	if snap == nil {
		return snapshot.Report{
			Status: snapshot.StatusUnknown,
			Reason: "empty snapshot",
		}
	}
	findings := EvaluateAll(snap)
	if len(findings) == 0 {
		return snapshot.Report{
			Status: snapshot.StatusUnknown,
			Reason: "no deterministic rule matched the snapshot evidence",
			Window: snap.Window,
		}
	}
	if len(findings) > MaxTopCauses {
		findings = findings[:MaxTopCauses]
	}
	return snapshot.Report{
		Status:    snapshot.StatusDiagnosed,
		Window:    snap.Window,
		TopCauses: findings,
	}
}
