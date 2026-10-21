// Command eval-score computes reproducible evaluation metrics (top1_accuracy,
// top3_recall, evidence_precision, false_positive_rate_on_healthy) with 95%
// Wilson confidence intervals over a split directory (eval/runs/<split>).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/iyedM/kubepilot/internal/evalscore"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "eval-score: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("eval-score", flag.ContinueOnError)
	split := fs.String("split", "dev", "Evaluation split to score: dev or test")
	runsRoot := fs.String("runs-root", filepath.Join("eval", "runs"), "Root directory containing split subdirectories")
	format := fs.String("format", "text", "Output format: text or json")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *split != "dev" && *split != "test" {
		return fmt.Errorf("unsupported --split %q (only dev and test splits are scored; dryrun and manual are never scored)", *split)
	}

	splitDir := filepath.Join(*runsRoot, *split)
	rep, err := evalscore.ScoreSplit(*split, splitDir)
	if err != nil {
		return err
	}

	switch *format {
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	case "text":
		evalscore.RenderText(out, rep)
		return nil
	default:
		return fmt.Errorf("unsupported --format %q (want text or json)", *format)
	}
}
