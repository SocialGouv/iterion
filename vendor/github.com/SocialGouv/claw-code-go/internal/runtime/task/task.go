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

// Task represents a sub-agent task.
type Task struct {
	TaskID      string        `json:"task_id"`
	Prompt      string        `json:"prompt"`
	Description *string       `json:"description,omitempty"`
	TaskPacket  *TaskPacket   `json:"task_packet,omitempty"`
	Status      TaskStatus    `json:"status"`
	CreatedAt   uint64        `json:"created_at"`
	UpdatedAt   uint64        `json:"updated_at"`
	Messages    []TaskMessage `json:"messages"`
	Output      string        `json:"output"`
	TeamID      *string       `json:"team_id,omitempty"`
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
	return t.clone()
}

// Get retrieves a task by ID. Returns (Task, true) if found.
func (r *Registry) Get(taskID string) (Task, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.tasks[taskID]
	if !ok {
		return Task{}, false
	}
	return t.clone(), true
}

// List returns all tasks, optionally filtered by status.
func (r *Registry) List(statusFilter *TaskStatus) []Task {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		if statusFilter == nil || t.Status == *statusFilter {
			result = append(result, t.clone())
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

// Remove hard-deletes a task. Returns the removed task if it existed.
func (r *Registry) Remove(taskID string) *Task {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.tasks[taskID]
	if !ok {
		return nil
	}
	delete(r.tasks, taskID)
	c := t.clone()
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
