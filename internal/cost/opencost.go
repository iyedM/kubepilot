// Package cost implements Phase P8 incident cost and resource waste estimation
// via the OpenCost Allocation API (/allocation/compute) with a deterministic
// fallback using OpenCost's standard default CPU/RAM hourly rates over the
// snapshot's pod requests/limits.
package cost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iyedM/kubepilot/internal/server"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

const (
	// DefaultCPUCoreHourUSD is OpenCost's standard default on-demand CPU price
	// per core-hour ($0.031611 / core-hour).
	DefaultCPUCoreHourUSD = 0.031611
	// DefaultRAMGiBHourUSD is OpenCost's standard default on-demand RAM price
	// per GiB-hour ($0.004237 / GiB-hour).
	DefaultRAMGiBHourUSD = 0.004237
)

// Config configures the OpenCost estimator.
type Config struct {
	OpenCostURL    string
	CPUCoreHourUSD float64
	RAMGiBHourUSD  float64
	HTTPClient     *http.Client
}

// Estimator implements server.CostEstimator.
type Estimator struct {
	cfg Config
}

// NewEstimator creates a new OpenCost Estimator.
func NewEstimator(cfg Config) *Estimator {
	if cfg.CPUCoreHourUSD <= 0 {
		cfg.CPUCoreHourUSD = DefaultCPUCoreHourUSD
	}
	if cfg.RAMGiBHourUSD <= 0 {
		cfg.RAMGiBHourUSD = DefaultRAMGiBHourUSD
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Estimator{cfg: cfg}
}

type openCostAllocationResponse struct {
	Code int                                   `json:"code"`
	Data []map[string]openCostAllocationRecord `json:"data"`
}

type openCostAllocationRecord struct {
	Name      string  `json:"name"`
	CPUCost   float64 `json:"cpuCost"`
	RAMCost   float64 `json:"ramCost"`
	TotalCost float64 `json:"totalCost"`
	Minutes   float64 `json:"minutes"`
}

// Estimate computes the window cost and estimated incident waste for a diagnosed
// snapshot and report.
func (e *Estimator) Estimate(ctx context.Context, snap *snapshot.Snapshot, rep snapshot.Report) (*server.CostSummary, error) {
	if snap == nil {
		return nil, nil
	}
	winSec := snap.Window.To.Sub(snap.Window.From).Seconds()
	if winSec <= 0 {
		winSec = 180
	}

	ns := "genai"
	workload := "deployment/model-server"
	var topCause snapshot.Cause
	if len(rep.TopCauses) > 0 {
		top := rep.TopCauses[0]
		topCause = top.Cause
		if top.Component.Namespace != "" {
			ns = top.Component.Namespace
		}
		if top.Component.Workload != "" {
			workload = top.Component.Workload
		}
	}

	wasteRatio, wasteReason := computeWasteRatio(snap, topCause, winSec)

	// 1. Try live OpenCost /allocation/compute endpoint when configured.
	if e.cfg.OpenCostURL != "" {
		if summary, err := e.queryOpenCostAPI(ctx, snap.Window, ns, workload, winSec, wasteRatio, wasteReason); err == nil && summary != nil {
			return summary, nil
		}
	}

	// 2. Deterministic resource-rate computation from Pod specs in the snapshot.
	cores, gib := workloadResources(snap, ns, workload)
	hours := winSec / 3600.0
	cpuCost := cores * hours * e.cfg.CPUCoreHourUSD
	ramCost := gib * hours * e.cfg.RAMGiBHourUSD
	totalCost := cpuCost + ramCost
	wasteCost := totalCost * wasteRatio

	return &server.CostSummary{
		Currency:           "USD",
		Workload:           workload,
		Namespace:          ns,
		WindowDurationSec:  math.Round(winSec*10) / 10,
		TotalWindowCost:    round6(totalCost),
		EstimatedWasteCost: round6(wasteCost),
		CPUCost:            round6(cpuCost),
		RAMCost:            round6(ramCost),
		Source:             "opencost-resource-model",
		Explanation: fmt.Sprintf(
			"Allocated %.2f CPU cores and %.2f GiB RAM over %.0fs ($%.4f/core-h, $%.4f/GiB-h); %s (waste factor %.0f%%)",
			cores, gib, winSec, e.cfg.CPUCoreHourUSD, e.cfg.RAMGiBHourUSD, wasteReason, wasteRatio*100,
		),
	}, nil
}

func (e *Estimator) queryOpenCostAPI(
	ctx context.Context,
	win snapshot.Window,
	ns, workload string,
	winSec, wasteRatio float64,
	wasteReason string,
) (*server.CostSummary, error) {
	windowParam := fmt.Sprintf("%s,%s",
		win.From.UTC().Format(time.RFC3339),
		win.To.UTC().Format(time.RFC3339))
	params := url.Values{}
	params.Set("window", windowParam)
	params.Set("aggregate", "namespace,controller")
	fullURL := strings.TrimRight(e.cfg.OpenCostURL, "/") + "/allocation/compute?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", fullURL, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var parsed openCostAllocationResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}

	shortWorkload := strings.TrimPrefix(workload, "deployment/")
	var cpuCost, ramCost, totalCost float64
	matched := false
	for _, step := range parsed.Data {
		for key, rec := range step {
			if strings.Contains(key, shortWorkload) || (strings.Contains(key, ns) && totalCost == 0) {
				cpuCost += rec.CPUCost
				ramCost += rec.RAMCost
				totalCost += rec.TotalCost
				matched = true
			}
		}
	}
	if !matched {
		return nil, fmt.Errorf("no OpenCost allocation entry matched %s/%s", ns, workload)
	}
	if totalCost == 0 {
		totalCost = cpuCost + ramCost
	}

	return &server.CostSummary{
		Currency:           "USD",
		Workload:           workload,
		Namespace:          ns,
		WindowDurationSec:  math.Round(winSec*10) / 10,
		TotalWindowCost:    round6(totalCost),
		EstimatedWasteCost: round6(totalCost * wasteRatio),
		CPUCost:            round6(cpuCost),
		RAMCost:            round6(ramCost),
		Source:             "opencost-api",
		Explanation: fmt.Sprintf(
			"OpenCost /allocation/compute for %s/%s over %.0fs; %s (waste factor %.0f%%)",
			ns, workload, winSec, wasteReason, wasteRatio*100,
		),
	}, nil
}

func workloadResources(snap *snapshot.Snapshot, ns, workload string) (cores float64, gib float64) {
	for _, pod := range snap.Pods {
		if pod.Namespace != ns {
			continue
		}
		if workload != "" && pod.Workload != "" && pod.Workload != workload {
			continue
		}
		for _, c := range pod.Containers {
			cpuMillis := c.RequestsCPUMillis
			if cpuMillis <= 0 {
				cpuMillis = c.LimitsCPUMillis
			}
			memBytes := c.RequestsMemoryB
			if memBytes <= 0 {
				memBytes = c.LimitsMemoryB
			}
			cores += float64(cpuMillis) / 1000.0
			gib += float64(memBytes) / (1024.0 * 1024.0 * 1024.0)
		}
	}
	if cores <= 0 {
		cores = 2.0
	}
	if gib <= 0 {
		gib = 1.0
	}
	return cores, gib
}

func computeWasteRatio(snap *snapshot.Snapshot, cause snapshot.Cause, winSec float64) (float64, string) {
	switch cause {
	case snapshot.CauseOOMKilled:
		return 1.0, "pod CrashLoopBackOff / OOMKilled unavailability"
	case snapshot.CauseReadinessFail:
		return 1.0, "pod Running but unready (serving zero traffic)"
	case snapshot.CauseUnschedulable:
		return 0.85, "rollout blocked by unschedulable pod"
	case snapshot.CauseColdStart:
		for _, p := range snap.Pods {
			for _, ic := range p.InitContainers {
				if ic.State.Kind == "terminated" && !ic.State.StartedAt.IsZero() && !ic.State.FinishedAt.IsZero() {
					delay := ic.State.FinishedAt.Sub(ic.State.StartedAt).Seconds()
					if delay > 0 && winSec > 0 {
						r := delay / winSec
						if r > 1.0 {
							r = 1.0
						}
						if r < 0.15 {
							r = 0.15
						}
						return r, fmt.Sprintf("initContainer %s startup delay %.0fs", ic.Name, delay)
					}
				}
			}
		}
		return 0.40, "slow cold-start initialization"
	case snapshot.CauseCPUThrottling:
		return 0.65, "CFS CPU throttling latency inflation"
	case snapshot.CauseHostContention:
		return 0.50, "host CPU contention waiting on runqueue"
	case snapshot.CauseConfigChange:
		return 0.45, "suboptimal serving configuration throughput drop"
	case snapshot.CauseUnknownNoInfra:
		return 0.55, "application pre-inference blocking delay"
	default:
		return 0.0, "healthy serving window (zero incident waste)"
	}
}

func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}
