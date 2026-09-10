package cursor

import (
	"context"
	"fmt"
	"sync"
)

// RunRegistry cancels and drains native agent runs when sessions close or policy changes.
type RunRegistry struct {
	mu       sync.Mutex
	closed   bool
	draining int
	runs     map[string]*activeRun
}

type activeRun struct {
	session string
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewRunRegistry() *RunRegistry { return &RunRegistry{runs: make(map[string]*activeRun)} }

func (r *RunRegistry) Start(parent context.Context, owner, session string) (context.Context, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.draining > 0 {
		return nil, nil, fmt.Errorf("Cursor executor is closed")
	}
	if _, exists := r.runs[owner]; exists {
		return nil, nil, fmt.Errorf("Cursor session already has an active run")
	}
	ctx, cancel := context.WithCancel(parent)
	run := &activeRun{session: session, cancel: cancel, done: make(chan struct{})}
	r.runs[owner] = run
	var once sync.Once
	finish := func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.runs, owner)
			close(run.done)
			r.mu.Unlock()
		})
	}
	return ctx, finish, nil
}

func (r *RunRegistry) CloseSession(session string) { r.close(session, false, false) }
func (r *RunRegistry) CancelAll()                  { r.close("", true, false) }
func (r *RunRegistry) Close()                      { r.close("", true, true) }

func (r *RunRegistry) close(session string, all, permanent bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if permanent {
		r.closed = true
	}
	r.draining++
	runs := make([]*activeRun, 0, len(r.runs))
	for _, run := range r.runs {
		if all || run.session == session {
			run.cancel()
			runs = append(runs, run)
		}
	}
	r.mu.Unlock()
	for _, run := range runs {
		<-run.done
	}
	r.mu.Lock()
	r.draining--
	r.mu.Unlock()
}
