package model

import (
	"fmt"
	"maps"
	"strconv"
	"sync/atomic"
	"time"
)

// Event model for installer steps and progress.

type StepID string

var stepCounter atomic.Uint64

// NewStepID returns a compact unique step id like "step-1".
func NewStepID() StepID {
	n := stepCounter.Add(1)
	return StepID("step-" + strconv.FormatUint(n, 10))
}

// StepStatus is the state of a step.
type StepStatus string

const (
	StepStatusPending   StepStatus = "pending"
	StepStatusRunning   StepStatus = "running"
	StepStatusSuccess   StepStatus = "success"
	StepStatusFailed    StepStatus = "failed"
	StepStatusSkipped   StepStatus = "skipped"
	StepStatusCancelled StepStatus = "cancelled"
)

// StepKind categorizes a step (e.g., "files", "dnf", "flatpak").
type StepKind string

// StepStart signals a step has begun.
type StepStart struct {
	ID         StepID
	ParentID   StepID // empty for top-level steps
	Name       string
	Kind       StepKind
	StartedAt  time.Time
	TotalBytes int64             // -1 if unknown
	Meta       map[string]string // optional metadata
}

// StepProgress reports intermediate progress for a step.
type StepProgress struct {
	ID        StepID
	Completed int64
	Total     int64 // -1 if unknown
	UpdatedAt time.Time
	Message   string
}

// StepEnd signals a step has completed.
type StepEnd struct {
	ID        StepID
	Status    StepStatus
	Err       error  `json:"-"`
	ErrString string // textual form of Err
	StartedAt time.Time
	EndedAt   time.Time
}

// Duration returns a non-zero duration when EndedAt is after StartedAt.
func (se StepEnd) Duration() time.Duration {
	if se.EndedAt.After(se.StartedAt) {
		return se.EndedAt.Sub(se.StartedAt)
	}
	return 0
}

// StepEvent is a marker for any step-related event.
type StepEvent any

// StepNode holds the current state of a step for tree rendering.
type StepNode struct {
	ID       StepID
	ParentID StepID
	Name     string
	Kind     StepKind

	Status    StepStatus
	StartedAt time.Time
	EndedAt   time.Time

	Completed int64
	Total     int64

	Meta     map[string]string
	Children []StepID
}

// UpdateFromStart initializes/updates the node from a StepStart.
func (n *StepNode) UpdateFromStart(s StepStart) {
	n.ID = s.ID
	n.ParentID = s.ParentID
	n.Name = s.Name
	n.Kind = s.Kind
	n.StartedAt = s.StartedAt

	if s.TotalBytes > 0 {
		n.Total = s.TotalBytes
	} else {
		n.Total = -1
	}

	if n.Meta == nil && s.Meta != nil {
		n.Meta = make(map[string]string, len(s.Meta))
		maps.Copy(n.Meta, s.Meta)
	}

	n.Status = StepStatusRunning
}

// UpdateFromProgress applies a progress update to the node.
func (n *StepNode) UpdateFromProgress(p StepProgress) {
	// Caller must ensure IDs match.
	n.Completed = p.Completed
	if p.Total > 0 {
		n.Total = p.Total
	}
}

// UpdateFromEnd finalizes the node from a StepEnd.
func (n *StepNode) UpdateFromEnd(e StepEnd) {
	n.Status = e.Status
	n.EndedAt = e.EndedAt
	if !e.StartedAt.IsZero() && n.StartedAt.IsZero() {
		n.StartedAt = e.StartedAt
	}
	if e.Err != nil {
		n.Meta = ensureMeta(n.Meta)
		n.Meta["error"] = e.Err.Error()
	}
}

// ensureMeta lazily allocates a metadata map.
func ensureMeta(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// String returns a compact node summary.
func (n StepNode) String() string {
	var dur string
	if !n.StartedAt.IsZero() && !n.EndedAt.IsZero() {
		dur = fmt.Sprintf(" (%s)", n.EndedAt.Sub(n.StartedAt).Round(time.Second))
	}
	return fmt.Sprintf("%s %s%s [%s] %d/%d", n.ID, n.Name, dur, n.Status, n.Completed, n.Total)
}

// StepEmitter receives structured step events.
type StepEmitter interface {
	Start(s StepStart)
	Progress(p StepProgress)
	End(e StepEnd)
}

// NoopStepEmitter is a safe default that does nothing.
type NoopStepEmitter struct{}

func (NoopStepEmitter) Start(s StepStart)       {}
func (NoopStepEmitter) Progress(p StepProgress) {}
func (NoopStepEmitter) End(e StepEnd)           {}
