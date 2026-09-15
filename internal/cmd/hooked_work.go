package cmd

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// listBeadsAcrossTables lists matching durable issues and ephemeral wisps.
// Hooked molecule roots can live in either table, so active-work readers must
// explicitly merge both sources instead of relying on issue-only bd list output.
func listBeadsAcrossTables(b *beads.Beads, opts beads.ListOptions) ([]*beads.Issue, error) {
	return listBeadsAcrossTablesContext(context.Background(), b, opts)
}

func listBeadsAcrossTablesContext(ctx context.Context, b *beads.Beads, opts beads.ListOptions) ([]*beads.Issue, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	limit := opts.Limit

	issueOpts := opts
	issueOpts.Ephemeral = false
	issueOpts.Limit = 0
	issues, err := b.ListContext(ctx, issueOpts)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	wispOpts := opts
	wispOpts.Ephemeral = true
	wispOpts.Limit = 0
	wisps, err := b.ListContext(ctx, wispOpts)
	if err != nil {
		return nil, err
	}

	merged := mergeBeadLists(issues, wisps)
	if limit > 0 && len(merged) > limit {
		return merged[:limit], nil
	}
	return merged, nil
}

func listAssignedActiveWork(b *beads.Beads, assignee string) ([]*beads.Issue, error) {
	return listAssignedActiveWorkContext(context.Background(), b, assignee)
}

func listAssignedActiveWorkContext(ctx context.Context, b *beads.Beads, assignee string) ([]*beads.Issue, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, status := range activeWorkStatuses() {
		beadsForStatus, err := listBeadsAcrossTablesContext(ctx, b, beads.ListOptions{
			Status:   status,
			Assignee: assignee,
			Priority: -1,
		})
		if err != nil {
			return nil, err
		}
		if len(beadsForStatus) > 0 {
			return beadsForStatus, nil
		}
	}
	return nil, nil
}

func listAssignedActiveWorkAcrossStatuses(b *beads.Beads, assignee string) ([]*beads.Issue, error) {
	return listAssignedActiveWorkAcrossStatusesContext(context.Background(), b, assignee)
}

func listAssignedActiveWorkAcrossStatusesContext(ctx context.Context, b *beads.Beads, assignee string) ([]*beads.Issue, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var assigned []*beads.Issue
	for _, status := range activeWorkStatuses() {
		beadsForStatus, err := listBeadsAcrossTablesContext(ctx, b, beads.ListOptions{
			Status:   status,
			Assignee: assignee,
			Priority: -1,
		})
		if err != nil {
			return nil, err
		}
		assigned = append(assigned, beadsForStatus...)
	}
	return mergeBeadLists(assigned, nil), nil
}

func listHookedWorkWithFallbacksContext(ctx context.Context, b *beads.Beads, workDir, townRoot, target string) ([]*beads.Issue, error) {
	hookedBeads, err := listAssignedActiveWorkContext(ctx, b, target)
	if err != nil {
		return nil, err
	}
	if len(hookedBeads) > 0 || townRoot == "" {
		return hookedBeads, nil
	}

	townBeadsDir := filepath.Join(townRoot, ".beads")
	if cleanedWorkDir, cleanedTown := filepath.Clean(workDir), filepath.Clean(townRoot); cleanedWorkDir != cleanedTown {
		if _, err := os.Stat(townBeadsDir); err == nil {
			townB := beads.New(townBeadsDir)
			townWork, err := listAssignedActiveWorkContext(ctx, townB, target)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
			} else if len(townWork) > 0 {
				return townWork, nil
			}
		}
	}

	if isTownLevelRole(target) {
		return scanAllRigsForHookedBeadsContext(ctx, townRoot, target), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

func listChildrenAcrossTables(b *beads.Beads, parentID string) ([]*beads.Issue, error) {
	return listChildrenAcrossTablesContext(context.Background(), b, parentID)
}

func listChildrenAcrossTablesContext(ctx context.Context, b *beads.Beads, parentID string) ([]*beads.Issue, error) {
	return listBeadsAcrossTablesContext(ctx, b, beads.ListOptions{
		Parent:   parentID,
		Status:   "all",
		Priority: -1,
	})
}

func resolveHookLookupWorkDir(workDir, target, townRoot string) string {
	target = strings.TrimSpace(target)
	if townRoot == "" || isTownLevelRole(target) {
		return workDir
	}
	if !safeAgentTargetPath(target) {
		return workDir
	}

	rigName := strings.Split(target, "/")[0]
	if rigName == "" || rigName == "mayor" || rigName == "deacon" {
		return workDir
	}
	if rigDir := beads.GetRigDirForName(townRoot, rigName); rigDir != "" {
		return rigDir
	}
	return filepath.Join(townRoot, rigName)
}

func activeWorkStatuses() []string {
	return []string{beads.StatusHooked, string(beads.StatusInProgress)}
}

func safeAgentTargetPath(target string) bool {
	parts := strings.Split(target, "/")
	for _, part := range parts {
		if !safeAgentPathSegment(part) {
			return false
		}
	}
	return len(parts) > 0
}

func safeAgentPathSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func mergeBeadLists(primary, secondary []*beads.Issue) []*beads.Issue {
	merged := make([]*beads.Issue, 0, len(primary)+len(secondary))
	seen := make(map[string]struct{}, len(primary)+len(secondary))
	for _, issue := range append(primary, secondary...) {
		if issue == nil || issue.ID == "" {
			continue
		}
		if _, ok := seen[issue.ID]; ok {
			continue
		}
		seen[issue.ID] = struct{}{}
		merged = append(merged, issue)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		left := beadRecencyTime(merged[i])
		right := beadRecencyTime(merged[j])
		if !left.Equal(right) {
			return left.After(right)
		}
		return beadSortID(merged[i]) > beadSortID(merged[j])
	})
	return merged
}

func beadRecencyTime(issue *beads.Issue) time.Time {
	if issue == nil {
		return time.Time{}
	}
	if ts := parseBeadTime(issue.UpdatedAt); !ts.IsZero() {
		return ts
	}
	return parseBeadTime(issue.CreatedAt)
}

func parseBeadTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return ts
}

func beadSortID(issue *beads.Issue) string {
	if issue == nil {
		return ""
	}
	return issue.ID
}
