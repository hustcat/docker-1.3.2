package watch

import (
	"sync"
)

// Interface can be implemented by anything that knows how to watch and report changes.
type Interface interface {
	// Stops watching. Will close the channel returned by ResultChan(). Releases
	// any resources used by the watch.
	Stop()

	// Returns a chan which will receive all the events. If an error occurs
	// or Stop() is called, this channel will be closed, in which case the
	// watch should be completely cleaned up.
	ResultChan() <-chan Event
}

type Object interface{}

// EventType defines the possible types of events.
type EventType string

// Event represents a single event to a watched resource.
type Event struct {
	Type EventType
	Object
}

// FakeWatcher lets you test anything that consumes a watch.Interface; threadsafe.
type FakeWatcher struct {
	result  chan Event
	Stopped bool
	sync.Mutex
}

func NewFake() *FakeWatcher {
	return &FakeWatcher{
		result: make(chan Event),
	}
}

// Stop implements Interface.Stop().
func (f *FakeWatcher) Stop() {
	f.Lock()
	defer f.Unlock()
	if !f.Stopped {
		close(f.result)
		f.Stopped = true
	}
}

func (f *FakeWatcher) ResultChan() <-chan Event {
	return f.result
}

// Emit sends a event.
func (f *FakeWatcher) Emit(obj Object) {
	f.result <- Event{"FakeObject", obj}
}
