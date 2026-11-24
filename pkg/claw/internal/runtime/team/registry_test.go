package team

import (
	"strings"
	"sync"
	"testing"
)

// ── Team tests ──────────────────────────────────────

func TestCreatesAndRetrievesTeam(t *testing.T) {
	t.Parallel()
	reg := NewTeamRegistry()
	team := reg.Create("Alpha Squad", []string{"task_001", "task_002"})

	if team.Name != "Alpha Squad" {
		t.Errorf("Name = %q, want %q", team.Name, "Alpha Squad")
	}
	if len(team.TaskIDs) != 2 {
		t.Errorf("len(TaskIDs) = %d, want 2", len(team.TaskIDs))
	}
	if team.Status != TeamStatusCreated {
		t.Errorf("Status = %v, want %v", team.Status, TeamStatusCreated)
	}

	fetched, ok := reg.Get(team.TeamID)
	if !ok {
		t.Fatal("team should exist")
	}
	if fetched.TeamID != team.TeamID {
		t.Errorf("TeamID = %q, want %q", fetched.TeamID, team.TeamID)
	}
}

func TestListsAndDeletesTeams(t *testing.T) {
	t.Parallel()
	reg := NewTeamRegistry()
	t1 := reg.Create("Team A", nil)
	t2 := reg.Create("Team B", nil)

	all := reg.List()
	if len(all) != 2 {
		t.Errorf("len(all) = %d, want 2", len(all))
	}

	deleted, err := reg.Delete(t1.TeamID)
	if err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if deleted.Status != TeamStatusDeleted {
		t.Errorf("Status = %v, want %v", deleted.Status, TeamStatusDeleted)
	}

	// Team is still listable (soft delete)
	stillThere, ok := reg.Get(t1.TeamID)
	if !ok {
		t.Fatal("soft-deleted team should still exist")
	}
	if stillThere.Status != TeamStatusDeleted {
		t.Errorf("Status = %v, want %v", stillThere.Status, TeamStatusDeleted)
	}

	// Hard remove
	reg.Remove(t2.TeamID)
	if reg.Len() != 1 {
		t.Errorf("Len = %d, want 1", reg.Len())
	}
}

func TestRejectsMissingTeamOperations(t *testing.T) {
	t.Parallel()
	reg := NewTeamRegistry()
	_, err := reg.Delete("nonexistent")
	if err == nil {
		t.Error("Delete should fail for missing team")
	}
	_, ok := reg.Get("nonexistent")
	if ok {
		t.Error("Get should return false for missing team")
	}
}

func TestTeamStatusDisplayAllVariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status TeamStatus
		want   string
	}{
		{TeamStatusCreated, "created"},
		{TeamStatusRunning, "running"},
		{TeamStatusCompleted, "completed"},
		{TeamStatusDeleted, "deleted"},
	}
	for _, tc := range cases {
		if tc.status.String() != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.status, tc.status.String(), tc.want)
		}
	}
}

func TestNewTeamRegistryIsEmpty(t *testing.T) {
	t.Parallel()
	reg := NewTeamRegistry()
	teams := reg.List()
	if !reg.IsEmpty() {
		t.Error("registry should be empty")
	}
	if reg.Len() != 0 {
		t.Errorf("Len = %d, want 0", reg.Len())
	}
	if len(teams) != 0 {
		t.Errorf("len(teams) = %d, want 0", len(teams))
	}
}

func TestTeamRemoveNonexistentReturnsNil(t *testing.T) {
	t.Parallel()
	reg := NewTeamRegistry()
	if removed := reg.Remove("missing"); removed != nil {
		t.Errorf("expected nil, got %v", removed)
	}
}

func TestTeamLenTransitions(t *testing.T) {
	t.Parallel()
	reg := NewTeamRegistry()
	alpha := reg.Create("Alpha", nil)
	beta := reg.Create("Beta", nil)

	if reg.Len() != 2 {
		t.Errorf("after create: Len = %d, want 2", reg.Len())
	}
	reg.Remove(alpha.TeamID)
	if reg.Len() != 1 {
		t.Errorf("after first remove: Len = %d, want 1", reg.Len())
	}
	reg.Remove(beta.TeamID)
	if reg.Len() != 0 {
		t.Errorf("after second remove: Len = %d, want 0", reg.Len())
	}
	if !reg.IsEmpty() {
		t.Error("registry should be empty")
	}
}

func TestConcurrentTeamAccess(t *testing.T) {
	t.Parallel()
	reg := NewTeamRegistry()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			team := reg.Create("concurrent", nil)
			reg.Get(team.TeamID)
			reg.List()
		}()
	}
	wg.Wait()

	if reg.Len() != 100 {
		t.Errorf("Len = %d, want 100", reg.Len())
	}
}

// ── Cron tests ──────────────────────────────────────

func TestCreatesAndRetrievesCron(t *testing.T) {
	t.Parallel()
	reg := NewCronRegistry()
	desc := "hourly check"
	entry := reg.Create("0 * * * *", "Check status", &desc)

	if entry.Schedule != "0 * * * *" {
		t.Errorf("Schedule = %q, want %q", entry.Schedule, "0 * * * *")
	}
	if entry.Prompt != "Check status" {
		t.Errorf("Prompt = %q, want %q", entry.Prompt, "Check status")
	}
	if !entry.Enabled {
		t.Error("Enabled should be true")
	}
	if entry.RunCount != 0 {
		t.Errorf("RunCount = %d, want 0", entry.RunCount)
	}
	if entry.LastRunAt != nil {
		t.Errorf("LastRunAt = %v, want nil", entry.LastRunAt)
	}

	fetched, ok := reg.Get(entry.CronID)
	if !ok {
		t.Fatal("cron entry should exist")
	}
	if fetched.CronID != entry.CronID {
		t.Errorf("CronID = %q, want %q", fetched.CronID, entry.CronID)
	}
}

func TestListsWithEnabledFilter(t *testing.T) {
	t.Parallel()
	reg := NewCronRegistry()
	c1 := reg.Create("* * * * *", "Task 1", nil)
	c2 := reg.Create("0 * * * *", "Task 2", nil)
	reg.Disable(c1.CronID)

	all := reg.List(false)
	if len(all) != 2 {
		t.Errorf("len(all) = %d, want 2", len(all))
	}

	enabledOnly := reg.List(true)
	if len(enabledOnly) != 1 {
		t.Errorf("len(enabled) = %d, want 1", len(enabledOnly))
	}
	if enabledOnly[0].CronID != c2.CronID {
		t.Errorf("enabled[0].CronID = %q, want %q", enabledOnly[0].CronID, c2.CronID)
	}
}

func TestDeletesCronEntry(t *testing.T) {
	t.Parallel()
	reg := NewCronRegistry()
	entry := reg.Create("* * * * *", "To delete", nil)
	deleted, err := reg.Delete(entry.CronID)
	if err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if deleted.CronID != entry.CronID {
		t.Errorf("CronID = %q, want %q", deleted.CronID, entry.CronID)
	}
	_, ok := reg.Get(entry.CronID)
	if ok {
		t.Error("deleted cron entry should not be retrievable")
	}
	if !reg.IsEmpty() {
		t.Error("registry should be empty")
	}
}

func TestRecordsCronRuns(t *testing.T) {
	t.Parallel()
	reg := NewCronRegistry()
	entry := reg.Create("*/5 * * * *", "Recurring", nil)
	reg.RecordRun(entry.CronID)
	reg.RecordRun(entry.CronID)

	fetched, ok := reg.Get(entry.CronID)
	if !ok {
		t.Fatal("cron entry should exist")
	}
	if fetched.RunCount != 2 {
		t.Errorf("RunCount = %d, want 2", fetched.RunCount)
	}
	if fetched.LastRunAt == nil {
		t.Error("LastRunAt should not be nil")
	}
}

func TestRejectsMissingCronOperations(t *testing.T) {
	t.Parallel()
	reg := NewCronRegistry()
	_, err := reg.Delete("nonexistent")
	if err == nil {
		t.Error("Delete should fail for missing cron")
	}
	if err := reg.Disable("nonexistent"); err == nil {
		t.Error("Disable should fail for missing cron")
	}
	if err := reg.RecordRun("nonexistent"); err == nil {
		t.Error("RecordRun should fail for missing cron")
	}
	_, ok := reg.Get("nonexistent")
	if ok {
		t.Error("Get should return false for missing cron")
	}
}

func TestCronListAllDisabledReturnsEmptyForEnabledOnly(t *testing.T) {
	t.Parallel()
	reg := NewCronRegistry()
	first := reg.Create("* * * * *", "Task 1", nil)
	second := reg.Create("0 * * * *", "Task 2", nil)
	reg.Disable(first.CronID)
	reg.Disable(second.CronID)

	enabledOnly := reg.List(true)
	allEntries := reg.List(false)

	if len(enabledOnly) != 0 {
		t.Errorf("len(enabledOnly) = %d, want 0", len(enabledOnly))
	}
	if len(allEntries) != 2 {
		t.Errorf("len(allEntries) = %d, want 2", len(allEntries))
	}
}

func TestCronCreateWithoutDescription(t *testing.T) {
	t.Parallel()
	reg := NewCronRegistry()
	entry := reg.Create("*/15 * * * *", "Check health", nil)

	if !strings.HasPrefix(entry.CronID, "cron_") {
		t.Errorf("CronID = %q, want prefix cron_", entry.CronID)
	}
	if entry.Description != nil {
		t.Errorf("Description = %v, want nil", entry.Description)
	}
	if !entry.Enabled {
		t.Error("Enabled should be true")
	}
	if entry.RunCount != 0 {
		t.Errorf("RunCount = %d, want 0", entry.RunCount)
	}
	if entry.LastRunAt != nil {
		t.Errorf("LastRunAt = %v, want nil", entry.LastRunAt)
	}
}

func TestNewCronRegistryIsEmpty(t *testing.T) {
	t.Parallel()
	reg := NewCronRegistry()
	enabledOnly := reg.List(true)
	allEntries := reg.List(false)

	if !reg.IsEmpty() {
		t.Error("registry should be empty")
	}
	if reg.Len() != 0 {
		t.Errorf("Len = %d, want 0", reg.Len())
	}
	if len(enabledOnly) != 0 {
		t.Errorf("len(enabledOnly) = %d, want 0", len(enabledOnly))
	}
	if len(allEntries) != 0 {
		t.Errorf("len(allEntries) = %d, want 0", len(allEntries))
	}
}

func TestCronRecordRunUpdatesTimestampAndCounter(t *testing.T) {
	t.Parallel()
	reg := NewCronRegistry()
	entry := reg.Create("*/5 * * * *", "Recurring", nil)
	reg.RecordRun(entry.CronID)
	reg.RecordRun(entry.CronID)

	fetched, ok := reg.Get(entry.CronID)
	if !ok {
		t.Fatal("entry should exist")
	}
	if fetched.RunCount != 2 {
		t.Errorf("RunCount = %d, want 2", fetched.RunCount)
	}
	if fetched.LastRunAt == nil {
		t.Error("LastRunAt should not be nil")
	}
	if fetched.UpdatedAt < entry.UpdatedAt {
		t.Error("UpdatedAt should be >= entry.UpdatedAt")
	}
}

func TestCronDisableUpdatesTimestamp(t *testing.T) {
	t.Parallel()
	reg := NewCronRegistry()
	entry := reg.Create("0 0 * * *", "Nightly", nil)
	reg.Disable(entry.CronID)

	fetched, ok := reg.Get(entry.CronID)
	if !ok {
		t.Fatal("entry should exist")
	}
	if fetched.Enabled {
		t.Error("Enabled should be false")
	}
	if fetched.UpdatedAt < entry.UpdatedAt {
		t.Error("UpdatedAt should be >= entry.UpdatedAt")
	}
}

func TestConcurrentCronAccess(t *testing.T) {
	t.Parallel()
	reg := NewCronRegistry()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry := reg.Create("* * * * *", "concurrent", nil)
			reg.Get(entry.CronID)
			reg.List(false)
			reg.RecordRun(entry.CronID)
		}()
	}
	wg.Wait()

	if reg.Len() != 100 {
		t.Errorf("Len = %d, want 100", reg.Len())
	}
}
