package json

import (
	"github.com/docker/docker/pkg/watch"
	"io"
	"reflect"
	"testing"
)

type WatchState struct {
	Running    bool
	Paused     bool
	Restarting bool
	Pid        int
	ExitCode   int
}

const watchStateEvent = "WatchState"

func TestDecoder(t *testing.T) {

	var eventType watch.EventType

	expect := &WatchState{
		Running:    true,
		Paused:     false,
		Restarting: false,
		Pid:        100,
		ExitCode:   0,
	}
	scheme := watch.NewScheme()
	scheme.AddKnownTypes(expect)

	eventType = watchStateEvent

	out, in := io.Pipe()
	decoder := NewDecoder(out, scheme)
	encoder := NewEncoder(in)
	go func() {
		if err := encoder.Encode(&watch.Event{eventType, expect}); err != nil {
			t.Errorf("Unexpected error %v", err)
		}
		in.Close()
	}()

	done := make(chan struct{})
	go func() {
		r, got, err := decoder.Decode()
		if err != nil {
			t.Fatalf("Unexpected error %v", err)
		}
		if e, a := eventType, r; e != a {
			t.Errorf("Expected %v, got %v", e, a)
		}
		if e, a := expect, got; !reflect.DeepEqual(e, a) {
			t.Errorf("Expected %v, got %v", e, a)
		}
		t.Logf("Exited read")
		close(done)
	}()
	<-done
    decoder.Close()
}
