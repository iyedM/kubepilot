package operator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iyedM/kubepilot/internal/collector/live"
	"github.com/iyedM/kubepilot/internal/server"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

func TestReconcileOnceAndPublishIncident(t *testing.T) {
	var mu sync.Mutex
	store := map[string]GenAIIncident{
		"investigate-slow-model": {
			APIVersion: APIVersion,
			Kind:       Kind,
			Metadata: ObjectMeta{
				Name:      "investigate-slow-model",
				Namespace: "genai",
			},
			Spec: GenAIIncidentSpec{
				TargetNamespace: "genai",
				Workload:        "deployment/model-server",
				WindowFrom:      "2026-10-23T01:40:00Z",
				WindowTo:        "2026-10-23T01:43:00Z",
			},
		},
	}

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ensure the operator never sends mutating requests to core/apps API groups.
		if r.Method != http.MethodGet && !strings.HasPrefix(r.URL.Path, "/apis/kubepilot.io/v1alpha1/") {
			t.Errorf("unexpected mutating request outside kubepilot.io: %s %s", r.Method, r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kubepilot.io/v1alpha1/namespaces/genai/genaiincidents":
			var items []GenAIIncident
			for _, v := range store {
				items = append(items, v)
			}
			_ = json.NewEncoder(w).Encode(GenAIIncidentList{
				APIVersion: APIVersion,
				Kind:       "GenAIIncidentList",
				Items:      items,
			})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/status"):
			var updated GenAIIncident
			if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			store[updated.Metadata.Name] = updated
			_ = json.NewEncoder(w).Encode(updated)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kubepilot.io/v1alpha1/namespaces/genai/genaiincidents":
			var created GenAIIncident
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			store[created.Metadata.Name] = created
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(created)
		default:
			http.NotFound(w, r)
		}
	}))
	defer apiSrv.Close()

	now := time.Date(2026, 10, 23, 1, 42, 0, 0, time.UTC)
	ctrl := NewController(Config{
		KubeAPIURL: apiSrv.URL,
		Namespace:  "genai",
		Collect: func(_ context.Context, _ live.Config) (*snapshot.Snapshot, error) {
			return &snapshot.Snapshot{
				Window: snapshot.Window{
					From:          now.Add(-2 * time.Minute),
					To:            now,
					SkewTolerance: 30 * time.Second,
				},
				Pods: []snapshot.PodSnapshot{
					{
						Name:      "model-server-oom-1",
						Namespace: "genai",
						Workload:  "deployment/model-server",
						Phase:     "Running",
						Containers: []snapshot.ContainerSnapshot{
							{
								Name:          "model-server",
								RestartCount:  3,
								LimitsMemoryB: 400 * 1024 * 1024,
								LastTerminated: &snapshot.ContainerState{
									Kind:       "terminated",
									Reason:     "OOMKilled",
									ExitCode:   137,
									FinishedAt: now.Add(-20 * time.Second),
								},
							},
						},
					},
				},
			}, nil
		},
	})

	n, err := ctrl.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReconcileOnce reconciled %d items, want 1", n)
	}

	mu.Lock()
	reconciled := store["investigate-slow-model"]
	mu.Unlock()
	if reconciled.Status.DiagnosisStatus != string(snapshot.StatusDiagnosed) {
		t.Fatalf("expected status diagnosed, got %+v", reconciled.Status)
	}
	if reconciled.Status.TopCause != string(snapshot.CauseOOMKilled) {
		t.Fatalf("expected topCause oom_killed, got %q", reconciled.Status.TopCause)
	}

	// Second ReconcileOnce should be a no-op since status is now populated.
	n2, err := ctrl.ReconcileOnce(context.Background())
	if err != nil || n2 != 0 {
		t.Fatalf("second ReconcileOnce got (%d, %v), want (0, nil)", n2, err)
	}

	// Test PublishIncident
	published, err := ctrl.PublishIncident(context.Background(), server.IncidentRecord{
		ID:        "dev--cpu-throttle--limit-300m--r1",
		Namespace: "genai",
		Report: snapshot.Report{
			Status: snapshot.StatusDiagnosed,
			Window: snapshot.Window{From: now.Add(-time.Minute), To: now},
			TopCauses: []snapshot.Finding{
				{
					Rule:       "cpu-throttling",
					Cause:      snapshot.CauseCPUThrottling,
					Confidence: 0.92,
					Component:  snapshot.Component{Namespace: "genai", Workload: "deployment/model-server"},
					Summary:    "throttled",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("PublishIncident: %v", err)
	}
	if published.Status.TopCause != "cpu_throttling" {
		t.Fatalf("unexpected published topCause: %+v", published.Status)
	}

	// Verify safety guard blocks mutating requests outside /apis/kubepilot.io/
	if _, _, err := ctrl.doJSON(context.Background(), http.MethodDelete, apiSrv.URL+"/api/v1/namespaces/genai/pods/x", nil); err == nil {
		t.Fatalf("expected safety guard to reject DELETE on core /api/v1/pods")
	}
}
