package daemon

import (
	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/pkg/watch"
	"sync"
	"time"
)

const WatchStateEvent = "WatchState"

type StateWatcher struct {
	result  chan watch.Event
	stopped bool
	sync.Mutex
}

func NewStateWatcher() *StateWatcher {
	return &StateWatcher{
		result:  make(chan watch.Event),
		stopped: false,
	}
}

func (sw *StateWatcher) ResultChan() <-chan watch.Event {
	sw.Lock()
	r := sw.result
	sw.Unlock()
	return r
}

// Emit sends a watch event.
func (sw *StateWatcher) Emit(obj watch.Object) {
	sw.Lock()
	defer sw.Unlock()
	if !sw.stopped {
		sw.result <- watch.Event{WatchStateEvent, obj}
	} else {
		log.Debugf("watching channel is stopped, ignore event %v", obj)
	}
}

func (sw *StateWatcher) Stop() {
	sw.Lock()
	defer sw.Unlock()
	log.Debugf("stopping watching channel")
	if !sw.stopped {
		close(sw.result)
		sw.stopped = true
	}
}

func (sw *StateWatcher) Start() {
	sw.Lock()
	defer sw.Unlock()
	if sw.stopped {
		sw.result = make(chan watch.Event)
		sw.stopped = false
	}
}

type WatchState struct {
	Running    bool
	Paused     bool
	Restarting bool
	Pid        int
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
}

func (s *State) setByWatchState(ws *WatchState) {
	s.Lock()
	s.Running = ws.Running
	s.Paused = ws.Paused
	s.Restarting = ws.Restarting
	s.Pid = ws.Pid
	s.ExitCode = ws.ExitCode
	s.StartedAt = ws.StartedAt
	s.FinishedAt = ws.FinishedAt
	s.Unlock()
}

func (s *State) ToWatchState() *WatchState {
	s.Lock()
	defer s.Unlock()

	return &WatchState{
		Running:    s.Running,
		Paused:     s.Paused,
		Restarting: s.Restarting,
		Pid:        s.Pid,
		ExitCode:   s.ExitCode,
		StartedAt:  s.StartedAt,
		FinishedAt: s.FinishedAt,
	}
}
