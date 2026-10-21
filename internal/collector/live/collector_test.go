package live_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/iyedM/kubepilot/internal/collector/live"
	"github.com/iyedM/kubepilot/internal/rules"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

func TestLiveCollectReadOnlyAndDiagnoseOOM(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("non-GET method %s on %s violates read-only requirement", r.Method, r.URL.Path)
			http.Error(w, "read-only violation", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/pods":
			_, _ = fmt.Fprintf(w, `{
				"items": [{
					"metadata": {
						"name": "model-server-7d785c95dc-6slrm",
						"namespace": "genai",
						"creationTimestamp": %q,
						"labels": {"app.kubernetes.io/name": "model-server"}
					},
					"spec": {
						"nodeName": "kubepilot-lab-worker",
						"containers": [{
							"name": "model-server",
							"image": "ghcr.io/ggml-org/llama.cpp:b5092",
							"resources": {"limits": {"memory": "400Mi"}}
						}]
					},
					"status": {
						"phase": "Running",
						"containerStatuses": [{
							"name": "model-server",
							"ready": false,
							"restartCount": 3,
							"lastState": {
								"terminated": {
									"reason": "OOMKilled",
									"exitCode": 137,
									"finishedAt": %q
								}
							}
						}]
					}
				}]
			}`, t0.Add(2*time.Second).Format(time.RFC3339), t0.Add(15*time.Second).Format(time.RFC3339))
		case "/api/v1/events":
			_, _ = fmt.Fprintf(w, `{
				"items": [{
					"involvedObject": {"namespace": "genai", "kind": "Pod", "name": "model-server-7d785c95dc-6slrm"},
					"type": "Warning",
					"reason": "BackOff",
					"message": "Back-off restarting failed container model-server",
					"lastTimestamp": %q
				}]
			}`, t0.Add(25*time.Second).Format(time.RFC3339))
		case "/logs/loadgen":
			_, _ = fmt.Fprintf(w, `{"ts":%q,"prompt_id":"short-16","status":502,"client_latency_ms":12.4,"error":"APIConnectionError"}`+"\n",
				t0.Add(20*time.Second).Format(time.RFC3339))
		case "/api/v1/query_range":
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	snap, err := live.Collect(context.Background(), live.Config{
		KubeAPIURL:    srv.URL,
		PrometheusURL: srv.URL,
		TempoURL:      srv.URL,
		From:          t0,
		To:            t0.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("live.Collect: %v", err)
	}

	rep := rules.Diagnose(snap)
	if rep.Status != snapshot.StatusDiagnosed || len(rep.TopCauses) == 0 {
		t.Fatalf("expected StatusDiagnosed, got %+v", rep)
	}
	if rep.TopCauses[0].Cause != snapshot.CauseOOMKilled {
		t.Errorf("top-1 cause = %q, want %q", rep.TopCauses[0].Cause, snapshot.CauseOOMKilled)
	}
}
