package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Sentinel errors
// ---------------------------------------------------------------------------

// ErrNotFound is returned when a task is not found.
var ErrNotFound = errors.New("task not found")

// ErrTerminalState is returned when a task is in a terminal state and cannot
// be modified.
var ErrTerminalState = errors.New("task is in terminal state")

// ---------------------------------------------------------------------------
// TaskStatus
// ---------------------------------------------------------------------------

// TaskStatus represents the lifecycle state of a task.
type TaskStatus int

const (
	StatusCreated TaskStatus = iota
	StatusRunning
	StatusCompleted
	StatusFailed
	StatusStopped
)

var statusStrings = [...]string{
	"created",
	"running",
	"completed",
	"failed",
	"stopped",
}

func (s TaskStatus) String() string {
	if int(s) < len(statusStrings) {
		return statusStrings[s]
	}
	return "unknown"
}

func (s TaskStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

func (s *TaskStatus) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	for i, name := range statusStrings {
		if name == str {
			*s = TaskStatus(i)
			return nil
		}
	}
	return fmt.Errorf("unknown task status: %q", str)
}

// IsTerminal returns true if the status is a terminal state.
func (s TaskStatus) IsTerminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusStopped
}

// ParseStatusAlias resolves a status name in either vocabulary: the native
// execution names (created, running, completed, failed, stopped) or Claude
// Code's task-graph work-item names (pending→created, in_progress→running,
// completed, deleted is handled by the caller as a removal).
func ParseStatusAlias(s string) (TaskStatus, bool) {
	switch s {
	case "pending":
		return StatusCreated, true
	case "in_progress":
		return StatusRunning, true
	}
	for i, name := range statusStrings {
		if name == s {
			return TaskStatus(i), true
		}
	}
	return 0, false
}

// WorkStatus renders the status in the task-graph work-item vocabulary
// (pending / in_progress / completed / failed / stopped).
func (s TaskStatus) WorkStatus() string {
	switch s {
	case StatusCreated:
		return "pending"
	case StatusRunning:
		return "in_progress"
	}
	return s.String()
}

// ---------------------------------------------------------------------------
// TaskPacket
// ---------------------------------------------------------------------------

// TaskPacket is a structured task specification.
type TaskPacket struct {
	Objective         string   `json:"objective"`
	Scope             string   `json:"scope"`
	Repo              string   `json:"repo"`
	BranchPolicy      string   `json:"branch_policy"`
	AcceptanceTests   []string `json:"acceptance_tests"`
	CommitPolicy      string   `json:"commit_policy"`
	ReportingContract string   `json:"reporting_contract"`
	EscalationPolicy  string   `json:"escalation_policy"`
}

// TaskPacketValidationError accumulates validation errors.
type TaskPacketValidationError struct {
	Errors []string
}

func (e *TaskPacketValidationError) Error() string {
	return strings.Join(e.Errors, "; ")
}

// ValidatePacket validates a TaskPacket and returns an error if any fields are empty.
func ValidatePacket(p TaskPacket) (*TaskPacket, error) {
	var errs []string

	validateRequired := func(field, value string) {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, field+" must not be empty")
		}
	}

	validateRequired("objective", p.Objective)
	validateRequired("scope", p.Scope)
	validateRequired("repo", p.Repo)
	validateRequired("branch_policy", p.BranchPolicy)
	validateRequired("commit_policy", p.CommitPolicy)
	validateRequired("reporting_contract", p.ReportingContract)
	validateRequired("escalation_policy", p.EscalationPolicy)

	for i, test := range p.AcceptanceTests {
		if strings.TrimSpace(test) == "" {
			errs = append(errs, fmt.Sprintf("acceptance_tests contains an empty value at index %d", i))
		}
	}

	if len(errs) > 0 {
		return nil, &TaskPacketValidationError{Errors: errs}
	}
	return &p, nil
}

// ---------------------------------------------------------------------------
// Task
// ---------------------------------------------------------------------------

// Task represents a sub-agent task. It doubles as a work item in the
// task graph (Claude Code 2.1.172 vocabulary): Subject/ActiveForm/Owner
// describe the item, Blocks/BlockedBy wire dependency edges, and the
// read-time Blocked/WorkStatus fields render the graph state.
type Task struct {
	TaskID      string        `json:"task_id"`
	Prompt      string        `json:"prompt"`
	Subject     string        `json:"subject,omitempty"`
	Description *string       `json:"description,omitempty"`
	ActiveForm  string        `json:"active_form,omitempty"`
	Owner       string        `json:"owner,omitempty"`
	TaskPacket  *TaskPacket   `json:"task_packet,omitempty"`
	Status      TaskStatus    `json:"status"`
	CreatedAt   uint64        `json:"created_at"`
	UpdatedAt   uint64        `json:"updated_at"`
	Messages    []TaskMessage `json:"messages"`
	Output      string        `json:"output"`
	TeamID      *string       `json:"team_id,omitempty"`

	// Blocks / BlockedBy are dependency edges by task id, maintained
	// reciprocally by the registry (A blocks B ⟺ B blocked-by A).
	Blocks    []string `json:"blocks,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`

	// Blocked and WorkStatus are derived at read time (Get/List): Blocked is
	// true while any BlockedBy task exists and is not completed; WorkStatus
	// is the Status in work-item vocabulary (pending/in_progress/completed).
	Blocked    bool   `json:"blocked,omitempty"`
	WorkStatus string `json:"work_status,omitempty"`
}

// clone returns a deep copy of the task, including the Messages slice.
func (t *Task) clone() Task {
	c := *t
	if t.Messages != nil {
		c.Messages = make([]TaskMessage, len(t.Messages))
		copy(c.Messages, t.Messages)
	}
	if t.Description != nil {
		d := *t.Description
		c.Description = &d
	}
	if t.TeamID != nil {
		tid := *t.TeamID
		c.TeamID = &tid
	}
	if t.TaskPacket != nil {
		p := *t.TaskPacket
		if t.TaskPacket.AcceptanceTests != nil {
			p.AcceptanceTests = make([]string, len(t.TaskPacket.AcceptanceTests))
			copy(p.AcceptanceTests, t.TaskPacket.AcceptanceTests)
		}
		c.TaskPacket = &p
	}
	if t.Blocks != nil {
		c.Blocks = append([]string(nil), t.Blocks...)
	}
	if t.BlockedBy != nil {
		c.BlockedBy = append([]string(nil), t.BlockedBy...)
	}
	c.WorkStatus = t.Status.WorkStatus()
	return c
}

// TaskMessage is a message associated with a task.
type TaskMessage struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp uint64 `json:"timestamp"`
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// Registry is a thread-safe in-memory task registry.
type Registry struct {
	mu      sync.Mutex
	tasks   map[string]*Task
	counter uint64
}

// NewRegistry creates a new empty task registry.
func NewRegistry() *Registry {
	return &Registry{
		tasks: make(map[string]*Task),
	}
}

func nowSecs() uint64 {
	return uint64(time.Now().Unix())
}

// Create creates a new task with the given prompt and optional description.
func (r *Registry) Create(prompt string, description *string) Task {
	return r.createTask(prompt, description, nil)
}

// CreateFromPacket creates a new task from a validated TaskPacket.
func (r *Registry) CreateFromPacket(packet TaskPacket) (Task, error) {
	validated, err := ValidatePacket(packet)
	if err != nil {
		return Task{}, err
	}
	desc := validated.Scope
	return r.createTask(validated.Objective, &desc, validated), nil
}

func (r *Registry) createTask(prompt string, description *string, packet *TaskPacket) Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createLocked(prompt, description, packet).clone()
}

// createLocked allocates and registers a task. Caller must hold r.mu.
func (r *Registry) createLocked(prompt string, description *string, packet *TaskPacket) *Task {
	r.counter++
	ts := nowSecs()
	taskID := fmt.Sprintf("task_%08x_%d", ts, r.counter)

	t := &Task{
		TaskID:      taskID,
		Prompt:      prompt,
		Description: description,
		TaskPacket:  packet,
		Status:      StatusCreated,
		CreatedAt:   ts,
		UpdatedAt:   ts,
		Messages:    []TaskMessage{},
		Output:      "",
	}
	r.tasks[taskID] = t
	return t
}

// ---------------------------------------------------------------------------
// Task graph (work items with dependencies)
// ---------------------------------------------------------------------------

// TaskSpec describes a task at creation in the task-graph vocabulary.
// Prompt and Subject are aliases: either may be provided, the other is
// seeded from it so both the execution path (Prompt) and the work-item
// rendering (Subject) stay populated.
type TaskSpec struct {
	Prompt      string
	Subject     string
	Description *string
	ActiveForm  string
	Owner       string
	Blocks      []string
	BlockedBy   []string
}

// TaskFieldUpdate mutates work-item fields on an existing task. Nil
// pointers leave the field untouched. AddBlocks/AddBlockedBy append
// dependency edges (reciprocally). Deleted removes the task from the
// graph, cleaning its edges from every other task.
type TaskFieldUpdate struct {
	Subject      *string
	Description  *string
	ActiveForm   *string
	Owner        *string
	Status       *TaskStatus
	AddBlocks    []string
	AddBlockedBy []string
	Deleted      bool
}

// CreateWithSpec creates a task carrying the work-item fields and wires the
// dependency edges reciprocally. Referencing an unknown task id in
// Blocks/BlockedBy is an explicit error.
func (r *Registry) CreateWithSpec(spec TaskSpec) (Task, error) {
	prompt := strings.TrimSpace(spec.Prompt)
	subject := strings.TrimSpace(spec.Subject)
	if prompt == "" {
		prompt = subject
	}
	if subject == "" {
		subject = prompt
	}
	if prompt == "" {
		return Task{}, fmt.Errorf("task: 'prompt' or 'subject' is required")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, id := range spec.Blocks {
		if _, ok := r.tasks[id]; !ok {
			return Task{}, fmt.Errorf("%w: %s (referenced in blocks)", ErrNotFound, id)
		}
	}
	for _, id := range spec.BlockedBy {
		if _, ok := r.tasks[id]; !ok {
			return Task{}, fmt.Errorf("%w: %s (referenced in blocked_by)", ErrNotFound, id)
		}
	}

	t := r.createLocked(prompt, spec.Description, nil)
	t.Subject = subject
	t.ActiveForm = strings.TrimSpace(spec.ActiveForm)
	t.Owner = strings.TrimSpace(spec.Owner)
	r.addEdgesLocked(t, spec.Blocks, spec.BlockedBy)

	c := t.clone()
	c.Blocked = r.blockedLocked(t)
	return c, nil
}

// SetFields applies a work-item field update. Deleted removes the task and
// returns its last snapshot.
func (r *Registry) SetFields(taskID string, u TaskFieldUpdate) (Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, err := r.getByID(taskID)
	if err != nil {
		return Task{}, err
	}

	if u.Deleted {
		c := t.clone()
		r.removeLocked(taskID)
		return c, nil
	}

	for _, id := range u.AddBlocks {
		if _, ok := r.tasks[id]; !ok {
			return Task{}, fmt.Errorf("%w: %s (referenced in add_blocks)", ErrNotFound, id)
		}
	}
	for _, id := range u.AddBlockedBy {
		if _, ok := r.tasks[id]; !ok {
			return Task{}, fmt.Errorf("%w: %s (referenced in add_blocked_by)", ErrNotFound, id)
		}
	}

	if u.Subject != nil {
		t.Subject = strings.TrimSpace(*u.Subject)
	}
	if u.Description != nil {
		t.Description = u.Description
	}
	if u.ActiveForm != nil {
		t.ActiveForm = strings.TrimSpace(*u.ActiveForm)
	}
	if u.Owner != nil {
		t.Owner = strings.TrimSpace(*u.Owner)
	}
	if u.Status != nil {
		t.Status = *u.Status
	}
	r.addEdgesLocked(t, u.AddBlocks, u.AddBlockedBy)
	t.UpdatedAt = nowSecs()

	c := t.clone()
	c.Blocked = r.blockedLocked(t)
	return c, nil
}

// addEdgesLocked wires blocks/blocked-by edges reciprocally, skipping
// duplicates and self-references. Caller must hold r.mu and have validated
// that every referenced id exists.
func (r *Registry) addEdgesLocked(t *Task, blocks, blockedBy []string) {
	for _, id := range blocks {
		if id == t.TaskID {
			continue
		}
		t.Blocks = appendUnique(t.Blocks, id)
		other := r.tasks[id]
		other.BlockedBy = appendUnique(other.BlockedBy, t.TaskID)
		other.UpdatedAt = nowSecs()
	}
	for _, id := range blockedBy {
		if id == t.TaskID {
			continue
		}
		t.BlockedBy = appendUnique(t.BlockedBy, id)
		other := r.tasks[id]
		other.Blocks = appendUnique(other.Blocks, t.TaskID)
		other.UpdatedAt = nowSecs()
	}
}

// blockedLocked reports whether any dependency of t is still incomplete.
// Edges to since-removed tasks are ignored. Caller must hold r.mu.
func (r *Registry) blockedLocked(t *Task) bool {
	for _, id := range t.BlockedBy {
		if dep, ok := r.tasks[id]; ok && dep.Status != StatusCompleted {
			return true
		}
	}
	return false
}

// removeLocked deletes a task and strips its id from every other task's
// edges. Caller must hold r.mu.
func (r *Registry) removeLocked(taskID string) {
	delete(r.tasks, taskID)
	for _, other := range r.tasks {
		other.Blocks = removeString(other.Blocks, taskID)
		other.BlockedBy = removeString(other.BlockedBy, taskID)
	}
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func removeString(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Get retrieves a task by ID. Returns (Task, true) if found.
func (r *Registry) Get(taskID string) (Task, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.tasks[taskID]
	if !ok {
		return Task{}, false
	}
	c := t.clone()
	c.Blocked = r.blockedLocked(t)
	return c, true
}

// List returns all tasks, optionally filtered by status.
func (r *Registry) List(statusFilter *TaskStatus) []Task {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		if statusFilter == nil || t.Status == *statusFilter {
			c := t.clone()
			c.Blocked = r.blockedLocked(t)
			result = append(result, c)
		}
	}
	return result
}

// getByID looks up a task by ID. Caller must hold r.mu.
func (r *Registry) getByID(taskID string) (*Task, error) {
	t, ok := r.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, taskID)
	}
	return t, nil
}

// getNonTerminal looks up a task by ID and rejects terminal-state mutations.
// Only Stop uses this — matching Rust, where only stop() guards terminal state.
// Caller must hold r.mu.
func (r *Registry) getNonTerminal(taskID string) (*Task, error) {
	t, err := r.getByID(taskID)
	if err != nil {
		return nil, err
	}
	if t.Status.IsTerminal() {
		return nil, fmt.Errorf("%w: task %s is already in terminal state: %s", ErrTerminalState, taskID, t.Status)
	}
	return t, nil
}

// Stop marks a task as stopped. Returns ErrTerminalState if already terminal.
func (r *Registry) Stop(taskID string) (Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, err := r.getNonTerminal(taskID)
	if err != nil {
		return Task{}, err
	}
	t.Status = StatusStopped
	t.UpdatedAt = nowSecs()
	return t.clone(), nil
}

// Update adds a user message to the task.
// Matching Rust: update() does not guard against terminal states.
func (r *Registry) Update(taskID string, message string) (Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, err := r.getByID(taskID)
	if err != nil {
		return Task{}, err
	}
	t.Messages = append(t.Messages, TaskMessage{
		Role:      "user",
		Content:   message,
		Timestamp: nowSecs(),
	})
	t.UpdatedAt = nowSecs()
	return t.clone(), nil
}

// Output returns the accumulated output for a task.
func (r *Registry) Output(taskID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.tasks[taskID]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNotFound, taskID)
	}
	return t.Output, nil
}

// AppendOutput appends text to the task's output.
func (r *Registry) AppendOutput(taskID string, output string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.tasks[taskID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, taskID)
	}
	t.Output += output
	t.UpdatedAt = nowSecs()
	return nil
}

// SetStatus updates the task's status unconditionally.
// Matching Rust: set_status() does not guard against terminal states,
// allowing recovery-driven status changes on completed/failed tasks.
func (r *Registry) SetStatus(taskID string, status TaskStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, err := r.getByID(taskID)
	if err != nil {
		return err
	}
	t.Status = status
	t.UpdatedAt = nowSecs()
	return nil
}

// AssignTeam associates a task with a team.
func (r *Registry) AssignTeam(taskID string, teamID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.tasks[taskID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, taskID)
	}
	t.TeamID = &teamID
	t.UpdatedAt = nowSecs()
	return nil
}

// Remove hard-deletes a task, stripping its dependency edges from every
// remaining task. Returns the removed task if it existed.
func (r *Registry) Remove(taskID string) *Task {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.tasks[taskID]
	if !ok {
		return nil
	}
	c := t.clone()
	r.removeLocked(taskID)
	return &c
}

// Len returns the number of tasks in the registry.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tasks)
}

// IsEmpty returns true if the registry has no tasks.
func (r *Registry) IsEmpty() bool {
	return r.Len() == 0
}
