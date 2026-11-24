package team

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// TeamStatus
// ---------------------------------------------------------------------------

// TeamStatus represents the lifecycle state of a team.
type TeamStatus int

const (
	TeamStatusCreated TeamStatus = iota
	TeamStatusRunning
	TeamStatusCompleted
	TeamStatusDeleted
)

var teamStatusStrings = [...]string{
	"created",
	"running",
	"completed",
	"deleted",
}

func (s TeamStatus) String() string {
	if int(s) < len(teamStatusStrings) {
		return teamStatusStrings[s]
	}
	return "unknown"
}

func (s TeamStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

func (s *TeamStatus) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	for i, name := range teamStatusStrings {
		if name == str {
			*s = TeamStatus(i)
			return nil
		}
	}
	return fmt.Errorf("unknown team status: %q", str)
}

// ---------------------------------------------------------------------------
// Team
// ---------------------------------------------------------------------------

// Team represents a group of tasks.
type Team struct {
	TeamID    string     `json:"team_id"`
	Name      string     `json:"name"`
	TaskIDs   []string   `json:"task_ids"`
	Status    TeamStatus `json:"status"`
	CreatedAt uint64     `json:"created_at"`
	UpdatedAt uint64     `json:"updated_at"`
}

func (t *Team) clone() Team {
	c := *t
	if t.TaskIDs != nil {
		c.TaskIDs = make([]string, len(t.TaskIDs))
		copy(c.TaskIDs, t.TaskIDs)
	}
	return c
}

// ---------------------------------------------------------------------------
// TeamRegistry
// ---------------------------------------------------------------------------

// TeamRegistry is a thread-safe in-memory team registry.
type TeamRegistry struct {
	mu      sync.Mutex
	teams   map[string]*Team
	counter uint64
}

// NewTeamRegistry creates a new empty team registry.
func NewTeamRegistry() *TeamRegistry {
	return &TeamRegistry{
		teams: make(map[string]*Team),
	}
}

func nowSecs() uint64 {
	return uint64(time.Now().Unix())
}

// Create creates a new team with the given name and task IDs.
func (r *TeamRegistry) Create(name string, taskIDs []string) Team {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.counter++
	ts := nowSecs()
	teamID := fmt.Sprintf("team_%08x_%d", ts, r.counter)

	ids := make([]string, len(taskIDs))
	copy(ids, taskIDs)

	t := &Team{
		TeamID:    teamID,
		Name:      name,
		TaskIDs:   ids,
		Status:    TeamStatusCreated,
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	r.teams[teamID] = t
	return t.clone()
}

// Get retrieves a team by ID.
func (r *TeamRegistry) Get(teamID string) (Team, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.teams[teamID]
	if !ok {
		return Team{}, false
	}
	return t.clone(), true
}

// List returns all teams.
func (r *TeamRegistry) List() []Team {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]Team, 0, len(r.teams))
	for _, t := range r.teams {
		result = append(result, t.clone())
	}
	return result
}

// Delete soft-deletes a team by marking it as Deleted.
func (r *TeamRegistry) Delete(teamID string) (Team, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.teams[teamID]
	if !ok {
		return Team{}, fmt.Errorf("team not found: %s", teamID)
	}
	t.Status = TeamStatusDeleted
	t.UpdatedAt = nowSecs()
	return t.clone(), nil
}

// Remove hard-deletes a team. Returns the team if it existed.
func (r *TeamRegistry) Remove(teamID string) *Team {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.teams[teamID]
	if !ok {
		return nil
	}
	delete(r.teams, teamID)
	c := t.clone()
	return &c
}

// Len returns the number of teams.
func (r *TeamRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.teams)
}

// IsEmpty returns true if the registry has no teams.
func (r *TeamRegistry) IsEmpty() bool {
	return r.Len() == 0
}

// ---------------------------------------------------------------------------
// CronEntry
// ---------------------------------------------------------------------------

// CronEntry represents a scheduled cron job.
type CronEntry struct {
	CronID      string  `json:"cron_id"`
	Schedule    string  `json:"schedule"`
	Prompt      string  `json:"prompt"`
	Description *string `json:"description,omitempty"`
	Enabled     bool    `json:"enabled"`
	CreatedAt   uint64  `json:"created_at"`
	UpdatedAt   uint64  `json:"updated_at"`
	LastRunAt   *uint64 `json:"last_run_at,omitempty"`
	RunCount    uint64  `json:"run_count"`
}

func (e *CronEntry) clone() CronEntry {
	c := *e
	if e.Description != nil {
		d := *e.Description
		c.Description = &d
	}
	if e.LastRunAt != nil {
		lr := *e.LastRunAt
		c.LastRunAt = &lr
	}
	return c
}

// ---------------------------------------------------------------------------
// CronRegistry
// ---------------------------------------------------------------------------

// CronRegistry is a thread-safe in-memory cron entry registry.
type CronRegistry struct {
	mu      sync.Mutex
	entries map[string]*CronEntry
	counter uint64
}

// NewCronRegistry creates a new empty cron registry.
func NewCronRegistry() *CronRegistry {
	return &CronRegistry{
		entries: make(map[string]*CronEntry),
	}
}

// Create creates a new enabled cron entry.
func (r *CronRegistry) Create(schedule, prompt string, description *string) CronEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.counter++
	ts := nowSecs()
	cronID := fmt.Sprintf("cron_%08x_%d", ts, r.counter)

	entry := &CronEntry{
		CronID:      cronID,
		Schedule:    schedule,
		Prompt:      prompt,
		Description: description,
		Enabled:     true,
		CreatedAt:   ts,
		UpdatedAt:   ts,
		RunCount:    0,
	}
	r.entries[cronID] = entry
	return entry.clone()
}

// Get retrieves a cron entry by ID.
func (r *CronRegistry) Get(cronID string) (CronEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[cronID]
	if !ok {
		return CronEntry{}, false
	}
	return e.clone(), true
}

// List returns cron entries, optionally filtering for enabled-only.
func (r *CronRegistry) List(enabledOnly bool) []CronEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]CronEntry, 0, len(r.entries))
	for _, e := range r.entries {
		if !enabledOnly || e.Enabled {
			result = append(result, e.clone())
		}
	}
	return result
}

// Delete removes a cron entry (hard delete).
func (r *CronRegistry) Delete(cronID string) (CronEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[cronID]
	if !ok {
		return CronEntry{}, fmt.Errorf("cron not found: %s", cronID)
	}
	c := e.clone()
	delete(r.entries, cronID)
	return c, nil
}

// Disable marks a cron entry as disabled without removing it.
func (r *CronRegistry) Disable(cronID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[cronID]
	if !ok {
		return fmt.Errorf("cron not found: %s", cronID)
	}
	e.Enabled = false
	e.UpdatedAt = nowSecs()
	return nil
}

// RecordRun increments the run count and updates the last run timestamp.
func (r *CronRegistry) RecordRun(cronID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[cronID]
	if !ok {
		return fmt.Errorf("cron not found: %s", cronID)
	}
	ts := nowSecs()
	e.LastRunAt = &ts
	e.RunCount++
	e.UpdatedAt = ts
	return nil
}

// Len returns the number of cron entries.
func (r *CronRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// IsEmpty returns true if the registry has no entries.
func (r *CronRegistry) IsEmpty() bool {
	return r.Len() == 0
}
