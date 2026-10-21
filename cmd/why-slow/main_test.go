package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iyedM/kubepilot/internal/snapshot"
)

func TestWhySlowTextAndJSON(t *testing.T) {
	runDir := filepath.Join("..", "..", "eval", "runs", "dev", "dev--oomkill--noload-400mi--r1")

	var textBuf bytes.Buffer
	if err := run([]string{"--run-dir", runDir, "--format", "text"}, &textBuf); err != nil {
		t.Fatalf("why-slow text: %v", err)
	}
	textOut := textBuf.String()
	if !strings.Contains(textOut, "Status: diagnosed") || !strings.Contains(textOut, "oom_killed") {
		t.Errorf("unexpected text output:\n%s", textOut)
	}

	var jsonBuf bytes.Buffer
	if err := run([]string{"--run-dir", runDir, "--format", "json"}, &jsonBuf); err != nil {
		t.Fatalf("why-slow json: %v", err)
	}
	var rep snapshot.Report
	if err := json.Unmarshal(jsonBuf.Bytes(), &rep); err != nil {
		t.Fatalf("parse json output: %v", err)
	}
	if rep.Status != snapshot.StatusDiagnosed || len(rep.TopCauses) == 0 || rep.TopCauses[0].Cause != snapshot.CauseOOMKilled {
		t.Errorf("unexpected json report: %+v", rep)
	}
}
