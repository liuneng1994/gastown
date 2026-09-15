package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestOutputMoleculeStatus_StandaloneFormulaShowsVars(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir tempDir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	status := MoleculeStatusInfo{
		HasWork:         true,
		PinnedBead:      &beads.Issue{ID: "gt-wisp-xyz", Title: "Standalone formula work"},
		AttachedFormula: "mol-release",
		AttachedVars:    []string{"version=1.2.3", "channel=stable"},
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputMoleculeStatus(status)

	w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stdout = oldStdout
	output := buf.String()

	if !strings.Contains(output, "📐 Formula: mol-release") {
		t.Fatalf("expected formula in output, got:\n%s", output)
	}
	if !strings.Contains(output, "--var version=1.2.3") || !strings.Contains(output, "--var channel=stable") {
		t.Fatalf("expected formula vars in output, got:\n%s", output)
	}
}

func TestOutputMoleculeStatus_FormulaWispShowsWorkflowContext(t *testing.T) {
	status := MoleculeStatusInfo{
		HasWork:         true,
		PinnedBead:      &beads.Issue{ID: "tool-wisp-demo", Title: "demo-hello"},
		AttachedFormula: "demo-hello",
		Progress: &MoleculeProgressInfo{
			RootID:     "tool-wisp-demo",
			RootTitle:  "demo-hello",
			TotalSteps: 3,
			DoneSteps:  0,
			ReadySteps: []string{"tool-wisp-step-1"},
		},
		NextAction: "Show the workflow steps: gt prime or bd mol current tool-wisp-demo",
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputMoleculeStatus(status)

	w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stdout = oldStdout
	output := buf.String()

	if !strings.Contains(output, "📐 Formula: demo-hello") {
		t.Fatalf("expected formula line in output, got:\n%s", output)
	}
	if strings.Contains(output, "No molecule attached") {
		t.Fatalf("formula wisp should not be rendered as naked work, got:\n%s", output)
	}
	if strings.Contains(output, "Attach a molecule to start work") {
		t.Fatalf("formula wisp should not suggest gt mol attach, got:\n%s", output)
	}
	if !strings.Contains(output, "Show the workflow steps: gt prime or bd mol current tool-wisp-demo") {
		t.Fatalf("expected workflow next action, got:\n%s", output)
	}
}

func TestGetMoleculeProgressInfoContextCancelsChildListing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell fake bd")
	}

	beads.ResetBdAllowStaleCacheForTest()
	t.Cleanup(beads.ResetBdAllowStaleCacheForTest)

	workDir := t.TempDir()
	beadsDir := filepath.Join(workDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	binDir := t.TempDir()
	started := filepath.Join(binDir, "list-started")
	writeBDStub(t, binDir, `#!/bin/sh
if [ "$1" = "--allow-stale" ]; then
  echo "Error: unknown flag: --allow-stale" >&2
  exit 0
fi
case "$1" in
  show)
    printf '%s\n' '[{"id":"gt-wisp-root","title":"mol-patrol","description":"","status":"hooked","priority":2,"issue_type":"task","created_at":"2026-09-15T00:00:00Z","updated_at":"2026-09-15T00:00:00Z"}]'
    ;;
  list)
    : > "$BD_LIST_STARTED"
    sleep 60
    ;;
  *)
    printf 'unexpected bd args: %s\n' "$*" >&2
    exit 1
    ;;
esac
`, "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_LIST_STARTED", started)

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := getMoleculeProgressInfoContext(ctx, beads.New(workDir), "gt-wisp-root")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("getMoleculeProgressInfoContext returned nil, want cancellation error")
	}
	if !strings.Contains(err.Error(), "listing children") {
		t.Fatalf("error = %v, want child-listing phase", err)
	}
	if elapsed > time.Second {
		t.Fatalf("getMoleculeProgressInfoContext took %s, want bounded by caller context", elapsed)
	}
	if _, statErr := os.Stat(started); statErr != nil {
		t.Fatalf("fake bd list was not invoked: %v", statErr)
	}
}

func TestShouldRetryMoleculeStatusLookupRequiresBackoffBudget(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(50*time.Millisecond))
	defer cancel()
	if shouldRetryMoleculeStatusLookup(ctx, 100*time.Millisecond) {
		t.Fatal("expected retry to stop when remaining deadline cannot cover backoff")
	}

	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancel()
	if !shouldRetryMoleculeStatusLookup(ctx, 10*time.Millisecond) {
		t.Fatal("expected retry when deadline can cover backoff")
	}

	cancel()
	if shouldRetryMoleculeStatusLookup(ctx, 0) {
		t.Fatal("expected retry to stop after context cancellation")
	}
}
