package json

import (
	"encoding/json"
	"io"

	"github.com/docker/docker/pkg/watch"
)

// Encoder implements the json.Encoder interface for io.Writers that
// should serialize watchEvent objects into JSON. It will encode any object
// registered in the supplied codec and return an error otherwies.
type Encoder struct {
	w       io.Writer
	encoder *json.Encoder
}

// NewEncoder creates an Encoder for the given writer
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{
		w:       w,
		encoder: json.NewEncoder(w),
	}
}

// Encode writes an event to the writer. Returns an error
// if the writer is closed or an object can't be encoded.
func (e *Encoder) Encode(event *watch.Event) error {
	return e.encoder.Encode(event)
}
