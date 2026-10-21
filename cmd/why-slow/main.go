// Command why-slow explains why a GenAI request or time window on Kubernetes
// was slow or failed by correlating OpenTelemetry traces, Kubernetes events,
// and Prometheus metrics through deterministic rules (no LLM in the loop).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/iyedM/kubepilot/internal/collector/live"
	"github.com/iyedM/kubepilot/internal/collector/offline"
	"github.com/iyedM/kubepilot/internal/rules"
	"github.com/iyedM/kubepilot/internal/snapshot"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "why-slow: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("why-slow", flag.ContinueOnError)
	runDir := fs.String("run-dir", "", "Offline evaluation run directory (run.json + exports)")
	snapPath := fs.String("snapshot", "", "Path to a serialized snapshot.Snapshot JSON file")
	traceID := fs.String("trace-id", "", "Target OpenTelemetry trace ID")
	fromStr := fs.String("from", "", "Analysis window start timestamp (RFC3339 UTC)")
	toStr := fs.String("to", "", "Analysis window end timestamp (RFC3339 UTC)")
	skew := fs.Duration("skew-tolerance", offline.DefaultSkewTolerance, "Clock-skew tolerance when correlating telemetry sources")
	kubeconfig := fs.String("kubeconfig", "", "Path to kubeconfig for live cluster collection")
	namespace := fs.String("namespace", "genai", "Target workload Kubernetes namespace")
	obsNamespace := fs.String("obs-namespace", "observability", "Observability stack Kubernetes namespace")
	promURL := fs.String("prometheus-url", "", "Direct Prometheus base URL (optional; defaults to Kubernetes service proxy)")
	tempoURL := fs.String("tempo-url", "", "Direct Tempo base URL (optional; defaults to Kubernetes service proxy)")
	format := fs.String("format", "text", "Output format: text or json")

	if err := fs.Parse(args); err != nil {
		return err
	}

	var fromTS, toTS time.Time
	var err error
	if *fromStr != "" {
		fromTS, err = time.Parse(time.RFC3339Nano, *fromStr)
		if err != nil {
			return fmt.Errorf("invalid --from timestamp %q: %w", *fromStr, err)
		}
	}
	if *toStr != "" {
		toTS, err = time.Parse(time.RFC3339Nano, *toStr)
		if err != nil {
			return fmt.Errorf("invalid --to timestamp %q: %w", *toStr, err)
		}
	}

	var snap *snapshot.Snapshot
	switch {
	case *snapPath != "":
		raw, err := os.ReadFile(*snapPath)
		if err != nil {
			return fmt.Errorf("read snapshot %s: %w", *snapPath, err)
		}
		var s snapshot.Snapshot
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("parse snapshot %s: %w", *snapPath, err)
		}
		snap = &s
	case *runDir != "":
		snap, _, err = offline.LoadRunDir(*runDir, offline.Options{
			From:          fromTS,
			To:            toTS,
			TraceID:       *traceID,
			SkewTolerance: *skew,
		})
		if err != nil {
			return err
		}
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		snap, err = live.Collect(ctx, live.Config{
			Kubeconfig:    *kubeconfig,
			Namespace:     *namespace,
			ObsNamespace:  *obsNamespace,
			PrometheusURL: *promURL,
			TempoURL:      *tempoURL,
			From:          fromTS,
			To:            toTS,
			TraceID:       *traceID,
			SkewTolerance: *skew,
		})
		if err != nil {
			return err
		}
	}

	report := rules.Diagnose(snap)
	switch *format {
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	case "text":
		renderTextReport(out, report, snap.TargetTraceID)
		return nil
	default:
		return fmt.Errorf("unsupported --format %q (want text or json)", *format)
	}
}

func renderTextReport(w io.Writer, rep snapshot.Report, traceID string) {
	fmt.Fprintf(w, "Status: %s\n", rep.Status)
	if !rep.Window.From.IsZero() && !rep.Window.To.IsZero() {
		fmt.Fprintf(w, "Window: %s -> %s\n",
			rep.Window.From.Format(time.RFC3339),
			rep.Window.To.Format(time.RFC3339))
	}
	if traceID != "" {
		fmt.Fprintf(w, "Trace:  %s\n", traceID)
	}
	if rep.Status == snapshot.StatusUnknown || len(rep.TopCauses) == 0 {
		reason := rep.Reason
		if reason == "" {
			reason = "no deterministic rule matched the snapshot evidence"
		}
		fmt.Fprintf(w, "Reason: %s\n", reason)
		return
	}

	fmt.Fprintf(w, "\nTop Probable Causes (%d):\n", len(rep.TopCauses))
	for i, c := range rep.TopCauses {
		fmt.Fprintf(w, "  [%d] %s (confidence=%.2f, rule=%s)\n", i+1, c.Cause, c.Confidence, c.Rule)
		fmt.Fprintf(w, "      Component: namespace=%s kind=%s workload=%s",
			c.Component.Namespace, c.Component.Kind, c.Component.Workload)
		if c.Component.Pod != "" {
			fmt.Fprintf(w, " pod=%s", c.Component.Pod)
		}
		if c.Component.Node != "" {
			fmt.Fprintf(w, " node=%s", c.Component.Node)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "      Summary:   %s\n", c.Summary)
		if len(c.Evidence) > 0 {
			fmt.Fprintln(w, "      Evidence:")
			for _, ev := range c.Evidence {
				tsStr := "-"
				if !ev.Timestamp.IsZero() {
					tsStr = ev.Timestamp.Format(time.RFC3339)
				}
				fmt.Fprintf(w, "        - [%s @ %s] %s\n", ev.Source, tsStr, ev.Summary)
			}
		}
	}
}
