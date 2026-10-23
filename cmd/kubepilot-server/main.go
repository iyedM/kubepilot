// Command kubepilot-server runs the read-only KubePilot HTTP diagnosis service
// (Phase P5), embedded Web UI (Phase P6), optional GenAIIncident CRD controller
// loop (Phase P7), and OpenCost incident cost estimator (Phase P8).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/iyedM/kubepilot/internal/collector/live"
	"github.com/iyedM/kubepilot/internal/collector/offline"
	"github.com/iyedM/kubepilot/internal/server"
	"github.com/iyedM/kubepilot/internal/ui"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "kubepilot-server: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("kubepilot-server", flag.ContinueOnError)
	listenAddr := fs.String("listen", ":8090", "HTTP listen address")
	kubeconfig := fs.String("kubeconfig", "", "Path to kubeconfig for live cluster collection")
	namespace := fs.String("namespace", "genai", "Target workload Kubernetes namespace")
	obsNamespace := fs.String("obs-namespace", "observability", "Observability stack Kubernetes namespace")
	promURL := fs.String("prometheus-url", "", "Direct Prometheus base URL (optional)")
	tempoURL := fs.String("tempo-url", "", "Direct Tempo base URL (optional)")
	skew := fs.Duration("skew-tolerance", offline.DefaultSkewTolerance, "Clock-skew tolerance when correlating telemetry sources")
	pollInterval := fs.Duration("poll-interval", 0, "Optional background diagnosis poll interval (0 disables polling)")
	seedRunsDir := fs.String("seed-runs-dir", "", "Optional directory of recorded runs (e.g. eval/runs/dev) to preload on startup")
	seedLimit := fs.Int("seed-limit", 25, "Maximum number of recorded runs to preload from --seed-runs-dir")

	if err := fs.Parse(args); err != nil {
		return err
	}

	srv := server.New(server.Config{
		LiveConfig: live.Config{
			Kubeconfig:    *kubeconfig,
			Namespace:     *namespace,
			ObsNamespace:  *obsNamespace,
			PrometheusURL: *promURL,
			TempoURL:      *tempoURL,
			SkewTolerance: *skew,
		},
		UIHandler: ui.NewHandler(),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *seedRunsDir != "" {
		n, err := srv.SeedFromRunsDir(ctx, *seedRunsDir, *seedLimit)
		if err != nil {
			log.Printf("warning: seed from %s: %v", *seedRunsDir, err)
		} else {
			log.Printf("seeded %d incident records from %s", n, *seedRunsDir)
		}
	}

	if *pollInterval > 0 {
		go func() {
			ticker := time.NewTicker(*pollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
					_, err := srv.PollOnce(pctx, now.Add(-3*time.Minute), now, "", false)
					cancel()
					if err != nil {
						log.Printf("poll warning: %v", err)
					}
				}
			}
		}()
	}

	httpSrv := &http.Server{
		Addr:              *listenAddr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("kubepilot-server listening on %s (namespace=%s, mode=read-only)", *listenAddr, *namespace)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}
}
