package application

import (
	"errors"
	"fmt"
	"sync"
)

// ErrInterrupted identifies an explicit request to stop the whole run.
var ErrInterrupted = errors.New("agent run interrupted")

// InterruptPersistenceError reports that the interrupted state was not saved.
type InterruptPersistenceError struct{ Err error }

func (e *InterruptPersistenceError) Error() string {
	return fmt.Sprintf("save interrupted run: %v", e.Err)
}
func (e *InterruptPersistenceError) Unwrap() error { return e.Err }

// Interrupt coordinates an explicit root-run interruption across entry points.
type Interrupt struct {
	mu        sync.Mutex
	done      chan struct{}
	requested bool
	finished  bool
}

func NewInterrupt() *Interrupt { return &Interrupt{done: make(chan struct{})} }

// Interrupt is safe to call repeatedly and from multiple goroutines.
func (i *Interrupt) Interrupt() { i.TryInterrupt() }

// TryInterrupt atomically coordinates delivery with execution finalization.
func (i *Interrupt) TryInterrupt() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.finished {
		return "terminal"
	}
	if i.requested {
		return "already_requested"
	}
	i.requested = true
	close(i.done)
	return "accepted"
}

// Finish closes the acceptance window before the terminal checkpoint is written.
func (i *Interrupt) Finish() { i.mu.Lock(); i.finished = true; i.mu.Unlock() }
