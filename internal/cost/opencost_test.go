package cost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

func TestOpenCostAPIEstimation(t *testing.T) {
	ocSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/allocation/compute" {
			http.NotFound(w, r)
			return
		}
		resp := openCostAllocationResponse{
			Code: 200,
			Data: []map[string]openCostAllocationRecord{
				{
					"genai/deployment/model-server": {
						Name:      "genai/deployment/model-server",
						CPUCost:   0.080,
						RAMCost:   0.020,
						TotalCost: 0.100,
						Minutes:   5.0,
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ocSrv.Close()

	est := NewEstimator(Config{
		OpenCostURL: ocSrv.URL,
	})
	from := time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC)
	to := from.Add(5 * time.Minute)
	snap := &snapshot.Snapshot{
		Window: snapshot.Window{From: from, To: to},
	}
	rep := snapshot.Report{
		Status: snapshot.StatusDiagnosed,
		Window: snap.Window,
		TopCauses: []snapshot.Finding{
			{
				Rule:       "oom-killed",
				Cause:      snapshot.CauseOOMKilled,
				Confidence: 0.96,
				Component: snapshot.Component{
					Namespace: "genai",
					Workload:  "deployment/model-server",
				},
			},
		},
	}

	summary, err := est.Estimate(context.Background(), snap, rep)
	if err != nil {
		t.Fatalf("Estimate failed: %v", err)
	}
	if summary.Source != "opencost-api" {
		t.Fatalf("got source %q, want opencost-api", summary.Source)
	}
	if summary.TotalWindowCost != 0.100 || summary.EstimatedWasteCost != 0.100 {
		t.Fatalf("unexpected cost summary: %+v", summary)
	}
	if summary.MonthlyProjectedCost <= 0 || summary.MonthlyProjectedWaste <= 0 {
		t.Fatalf("expected positive monthly FinOps projections, got %+v", summary)
	}
	if summary.PatchMonthlyDeltaUSD <= 0 || summary.FinOpsRecommendation == "" {
		t.Fatalf("expected FinOps patch delta and recommendation, got %+v", summary)
	}
}

func TestResourceModelFallbackEstimation(t *testing.T) {
	est := NewEstimator(Config{
		CPUCoreHourUSD: 0.04,
		RAMGiBHourUSD:  0.01,
	})
	from := time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC)
	to := from.Add(30 * time.Minute) // 0.5 hour
	snap := &snapshot.Snapshot{
		Window: snapshot.Window{From: from, To: to},
		Pods: []snapshot.PodSnapshot{
			{
				Name:      "model-server-1",
				Namespace: "genai",
				Workload:  "deployment/model-server",
				Containers: []snapshot.ContainerSnapshot{
					{
						Name:              "model-server",
						RequestsCPUMillis: 4000,                   // 4 cores
						RequestsMemoryB:   2 * 1024 * 1024 * 1024, // 2 GiB
					},
				},
			},
		},
	}
	rep := snapshot.Report{
		Status: snapshot.StatusDiagnosed,
		Window: snap.Window,
		TopCauses: []snapshot.Finding{
			{
				Rule:       "readiness-failing",
				Cause:      snapshot.CauseReadinessFail,
				Confidence: 0.94,
				Component: snapshot.Component{
					Namespace: "genai",
					Workload:  "deployment/model-server",
				},
			},
		},
	}

	summary, err := est.Estimate(context.Background(), snap, rep)
	if err != nil {
		t.Fatalf("Estimate fallback failed: %v", err)
	}
	if summary.Source != "opencost-resource-model" {
		t.Fatalf("got source %q, want opencost-resource-model", summary.Source)
	}
	// 4 cores * 0.5h * $0.04 = $0.08 CPU; 2 GiB * 0.5h * $0.01 = $0.01 RAM; Total = $0.09
	if summary.CPUCost != 0.08 || summary.RAMCost != 0.01 || summary.TotalWindowCost != 0.09 {
		t.Fatalf("unexpected fallback cost breakdown: %+v", summary)
	}
	if summary.EstimatedWasteCost != 0.09 {
		t.Fatalf("expected 100%% waste on readiness_failing ($0.09), got %f", summary.EstimatedWasteCost)
	}
}
