package watch

import (
	"testing"
)

type testType string

func TestFake(t *testing.T) {
	f := NewFake()

	table := []struct {
		t EventType
		s testType
	}{
		{"FakeObject", testType("foo")},
	}

	// Prove that f implements Interface by phrasing this as a function.
	consumer := func(w Interface) {
		for _, expect := range table {
			got, ok := <-w.ResultChan()
			if !ok {
				t.Fatalf("closed early")
			}
			if e, a := expect.t, got.Type; e != a {
				t.Fatalf("Expected %v, got %v", e, a)
			}
			if a, ok := got.Object.(testType); !ok || a != expect.s {
				t.Fatalf("Expected %v, got %v", expect.s, a)
			}
		}
		_, stillOpen := <-w.ResultChan()
		if stillOpen {
			t.Fatal("Never stopped")
		}
	}

	sender := func() {
		f.Emit(testType("foo"))
		f.Stop()
	}

	go sender()
	consumer(f)
}
