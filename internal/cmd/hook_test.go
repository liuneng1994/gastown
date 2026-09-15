package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestHookPolecatEnvCheck verifies that the polecat guard in runHook uses
// GT_ROLE as the authoritative check, so coordinators with a stale GT_POLECAT
// in their environment are not blocked from hooking (GH #1707).
func TestHookPolecatEnvCheck(t *testing.T) {
	tests := []struct {
		name      string
		role      string
		polecat   string
		wantBlock bool
	}{
		{
			name:      "bare polecat role is blocked",
			role:      "polecat",
			polecat:   "alpha",
			wantBlock: true,
		},
		{
			name:      "compound polecat role is blocked",
			role:      "gastown/polecats/Toast",
			polecat:   "Toast",
			wantBlock: true,
		},
		{
			name:      "mayor with stale GT_POLECAT is NOT blocked",
			role:      "mayor",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "compound witness with stale GT_POLECAT is NOT blocked",
			role:      "gastown/witness",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "crew with stale GT_POLECAT is NOT blocked",
			role:      "crew",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "compound crew with stale GT_POLECAT is NOT blocked",
			role:      "gastown/crew/den",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "no GT_ROLE with GT_POLECAT set is blocked",
			role:      "",
			polecat:   "alpha",
			wantBlock: true,
		},
		{
			name:      "no GT_ROLE and no GT_POLECAT is not blocked",
			role:      "",
			polecat:   "",
			wantBlock: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GT_ROLE", tt.role)
			t.Setenv("GT_POLECAT", tt.polecat)

			// We only test the polecat guard, so we call runHook with a dummy arg.
			// It will either fail at the guard or fail later (missing bead, etc.).
			// We only care whether the error is the polecat-block message.
			var blocked bool
			func() {
				defer func() {
					if r := recover(); r != nil {
						// Panic means we got past the guard — not blocked
						blocked = false
					}
				}()
				err := runHook(nil, []string{"fake-bead-id"})
				blocked = err != nil && strings.Contains(err.Error(), "polecats cannot hook")
			}()

			if blocked != tt.wantBlock {
				if tt.wantBlock {
					t.Errorf("expected polecat block but was not blocked (GT_ROLE=%q GT_POLECAT=%q)", tt.role, tt.polecat)
				} else {
					t.Errorf("unexpected polecat block with GT_ROLE=%q GT_POLECAT=%q", tt.role, tt.polecat)
				}
			}
		})
	}
}

// TestHookRejectsNonBeadArg pins down GH#3701: when cobra fails to match a
// subcommand and falls through to the bead-id positional, args that don't
// look like bead IDs should produce a clear error pointing at --help rather
// than the misleading "bead 'set' not found" emitted by bd show.
func TestHookRejectsNonBeadArg(t *testing.T) {
	// Ensure we don't trip the polecat guard.
	t.Setenv("GT_ROLE", "")
	t.Setenv("GT_POLECAT", "")

	tests := []string{"set", "list", "delete", "nonexistentword12345"}
	for _, arg := range tests {
		t.Run(arg, func(t *testing.T) {
			err := runHook(nil, []string{arg})
			if err == nil {
				t.Fatalf("runHook(%q) returned nil, want error", arg)
			}
			if !strings.Contains(err.Error(), "is not a bead ID") {
				t.Errorf("runHook(%q) error = %q, want substring %q", arg, err.Error(), "is not a bead ID")
			}
			if !strings.Contains(err.Error(), "--help") {
				t.Errorf("runHook(%q) error = %q, want it to point at --help", arg, err.Error())
			}
		})
	}
}

func TestNormalizeHookShowTarget(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{
			name:   "shorthand polecat path resolves",
			target: "gastown/toast",
			want:   "gastown/polecats/toast",
		},
		{
			name:   "canonical polecat path stays canonical",
			target: "gastown/polecats/toast",
			want:   "gastown/polecats/toast",
		},
		{
			name:   "unknown target stays unchanged",
			target: "this-is-not-an-agent-path",
			want:   "this-is-not-an-agent-path",
		},
		{
			name:   "path traversal shorthand stays unchanged",
			target: "../toast",
			want:   "../toast",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeHookShowTarget(tt.target)
			if got != tt.want {
				t.Fatalf("normalizeHookShowTarget(%q) = %q, want %q", tt.target, got, tt.want)
			}
		})
	}
}

func TestScanAllRigsForHookedBeadsEmptyIsBoundedAndParallel(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeSlowEmptyHookBDStub(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logPath)
	t.Setenv("GT_BD_TIMEOUT_SEC", "5")

	oldTimeout := hookCrossRigScanTimeout
	hookCrossRigScanTimeout = 2 * time.Second
	t.Cleanup(func() {
		hookCrossRigScanTimeout = oldTimeout
	})

	townRoot := t.TempDir()
	townBeadsDir := filepath.Join(townRoot, ".beads")
	routes := []beads.Route{
		{Prefix: "a-", Path: "rig-a"},
		{Prefix: "b-", Path: "rig-b"},
		{Prefix: "c-", Path: "rig-c"},
	}
	if err := beads.WriteRoutes(townBeadsDir, routes); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	for _, route := range routes {
		if err := os.MkdirAll(filepath.Join(townRoot, route.Path, ".beads"), 0755); err != nil {
			t.Fatalf("mkdir route beads: %v", err)
		}
	}

	start := time.Now()
	got := scanAllRigsForHookedBeads(townRoot, "deacon")
	elapsed := time.Since(start)
	if len(got) != 0 {
		t.Fatalf("scanAllRigsForHookedBeads returned %d beads, want none", len(got))
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("scan took %v, expected concurrent route checks to finish near one stub delay", elapsed)
	}

	log := readHookStubLog(t, logPath)
	if count := strings.Count(log, "assignee=\"deacon\""); count != len(routes) {
		t.Fatalf("query count = %d, want %d\nlog:\n%s", count, len(routes), log)
	}
	if strings.Contains(log, "args:[list]") {
		t.Fatalf("scan used serial list fallback, want query-only lookup\nlog:\n%s", log)
	}
}

func TestRunMoleculeStatusEmptyTownHookJSONScansPromptly(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeSlowEmptyHookBDStub(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logPath)
	t.Setenv("GT_BD_TIMEOUT_SEC", "5")
	t.Setenv("GT_ROLE", "deacon")

	oldTimeout := hookCrossRigScanTimeout
	hookCrossRigScanTimeout = 2 * time.Second
	t.Cleanup(func() {
		hookCrossRigScanTimeout = oldTimeout
	})

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"type":"town","version":1,"name":"hook-test"}`), 0644); err != nil {
		t.Fatalf("write town config: %v", err)
	}
	townBeadsDir := filepath.Join(townRoot, ".beads")
	routes := []beads.Route{
		{Prefix: "hq-", Path: "."},
		{Prefix: "a-", Path: "rig-a"},
		{Prefix: "b-", Path: "rig-b"},
		{Prefix: "c-", Path: "rig-c"},
	}
	if err := beads.WriteRoutes(townBeadsDir, routes); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	for _, route := range routes[1:] {
		if err := os.MkdirAll(filepath.Join(townRoot, route.Path, ".beads"), 0755); err != nil {
			t.Fatalf("mkdir route beads: %v", err)
		}
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(filepath.Join(townRoot, "deacon")); err != nil {
		if mkErr := os.MkdirAll(filepath.Join(townRoot, "deacon"), 0755); mkErr != nil {
			t.Fatalf("mkdir deacon dir: %v", mkErr)
		}
		if chErr := os.Chdir(filepath.Join(townRoot, "deacon")); chErr != nil {
			t.Fatalf("chdir deacon dir: %v", chErr)
		}
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	prevJSON := moleculeJSON
	moleculeJSON = true
	t.Cleanup(func() {
		moleculeJSON = prevJSON
	})

	start := time.Now()
	out := captureStdout(t, func() {
		if err := runMoleculeStatus(nil, nil); err != nil {
			t.Fatalf("runMoleculeStatus: %v", err)
		}
	})
	elapsed := time.Since(start)
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("runMoleculeStatus took %v, want prompt empty-hook response", elapsed)
	}
	if !strings.Contains(out, `"target": "deacon/"`) || !strings.Contains(out, `"has_work": false`) {
		t.Fatalf("unexpected JSON output:\n%s", out)
	}

	log := readHookStubLog(t, logPath)
	if count := strings.Count(log, "assignee=\"deacon/\""); count != len(routes) {
		t.Fatalf("cross-rig query count = %d, want %d\nlog:\n%s", count, len(routes), log)
	}
	if strings.Contains(log, "args:[list]") {
		t.Fatalf("runMoleculeStatus used serial list fallback, want query-only lookup\nlog:\n%s", log)
	}
}

func TestHookShowEmptyTownHookJSONScansPromptly(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeSlowEmptyHookBDStub(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logPath)
	t.Setenv("GT_BD_TIMEOUT_SEC", "5")

	oldTimeout := hookCrossRigScanTimeout
	hookCrossRigScanTimeout = 2 * time.Second
	t.Cleanup(func() {
		hookCrossRigScanTimeout = oldTimeout
	})

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"type":"town","version":1,"name":"hook-test"}`), 0644); err != nil {
		t.Fatalf("write town config: %v", err)
	}
	townBeadsDir := filepath.Join(townRoot, ".beads")
	routes := []beads.Route{
		{Prefix: "hq-", Path: "."},
		{Prefix: "a-", Path: "rig-a"},
		{Prefix: "b-", Path: "rig-b"},
		{Prefix: "c-", Path: "rig-c"},
	}
	if err := beads.WriteRoutes(townBeadsDir, routes); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	for _, route := range routes[1:] {
		if err := os.MkdirAll(filepath.Join(townRoot, route.Path, ".beads"), 0755); err != nil {
			t.Fatalf("mkdir route beads: %v", err)
		}
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	deaconDir := filepath.Join(townRoot, "deacon")
	if err := os.MkdirAll(deaconDir, 0755); err != nil {
		t.Fatalf("mkdir deacon dir: %v", err)
	}
	if err := os.Chdir(deaconDir); err != nil {
		t.Fatalf("chdir deacon dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	prevJSON := moleculeJSON
	moleculeJSON = true
	t.Cleanup(func() {
		moleculeJSON = prevJSON
	})

	start := time.Now()
	out := captureStdout(t, func() {
		if err := runHookShow(nil, []string{"deacon"}); err != nil {
			t.Fatalf("runHookShow: %v", err)
		}
	})
	elapsed := time.Since(start)
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("runHookShow took %v, want prompt empty-hook response", elapsed)
	}
	if !strings.Contains(out, `"agent":"deacon/"`) || !strings.Contains(out, `"status":"empty"`) {
		t.Fatalf("unexpected JSON output:\n%s", out)
	}

	log := readHookStubLog(t, logPath)
	if count := strings.Count(log, "assignee=\"deacon/\""); count != len(routes) {
		t.Fatalf("cross-rig query count = %d, want %d\nlog:\n%s", count, len(routes), log)
	}
	if strings.Contains(log, "args:[list]") {
		t.Fatalf("runHookShow used serial list fallback, want query-only lookup\nlog:\n%s", log)
	}
}

func TestCloseCompletedHookedMoleculeUsesBdCmdEnv(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeHookBDStub(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logPath)
	t.Setenv("CLAUDE_SESSION_ID", "ses-hook-test")
	t.Setenv("BEADS_DIR", "/wrong")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "hq")
	t.Setenv("BD_READONLY", "true")
	t.Setenv("BD_DOLT_AUTO_COMMIT", "off")

	workDir := t.TempDir()
	beadsDir := filepath.Join(workDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_database":"hookdb"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := closeCompletedHookedMolecule(workDir, "gt-old"); err != nil {
		t.Fatalf("closeCompletedHookedMolecule: %v", err)
	}
	log := readHookStubLog(t, logPath)
	for _, want := range []string{
		"args:[close][gt-old][--force][--reason=Auto-replaced by gt hook (molecule complete)][--session=ses-hook-test]",
		"BEADS_DIR=" + beadsDir,
		"BEADS_DOLT_SERVER_DATABASE=hookdb",
		"\nBD_READONLY=\n",
		"BD_DOLT_AUTO_COMMIT=on",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("hook close log missing %q:\n%s", want, log)
		}
	}
}

func writeSlowEmptyHookBDStub(t *testing.T, binDir string) {
	t.Helper()
	script := `#!/usr/bin/env sh
line="args:"
for arg in "$@"; do
	line="${line}[$arg]"
done
printf '%s\n' "$line" >> "$BD_STUB_LOG"
case "$1" in
  version)
    printf 'bd test\n'
    ;;
  *)
    for arg in "$@"; do
      if [ "$arg" = "query" ]; then
        sleep 1
        printf '[]\n'
        exit 0
      fi
    done
    printf '[]\n'
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}

func writeHookBDStub(t *testing.T, binDir string) {
	t.Helper()
	script := `#!/usr/bin/env sh
{
	printf 'args:'
	for arg in "$@"; do
		printf '[%s]' "$arg"
	done
	printf '\n'
	printf 'BEADS_DIR=%s\n' "${BEADS_DIR-}"
	printf 'BEADS_DOLT_SERVER_DATABASE=%s\n' "${BEADS_DOLT_SERVER_DATABASE-}"
	printf 'BD_READONLY=%s\n' "${BD_READONLY-}"
	printf 'BD_DOLT_AUTO_COMMIT=%s\n' "${BD_DOLT_AUTO_COMMIT-}"
} >> "$BD_STUB_LOG"
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}

func readHookStubLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
