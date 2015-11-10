package json

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/docker/docker/pkg/watch"
)

// Decoder implements the watch.Decoder interface for io.ReadClosers that
// have contents which consist of a series of watchEvent objects encoded via JSON.
// It will decode any object registered in the supplied codec.
type Decoder struct {
	r       io.ReadCloser
	decoder *json.Decoder
	s       *watch.Scheme
}

// NewDecoder creates an Decoder for the given writer and codec.
func NewDecoder(r io.ReadCloser, s *watch.Scheme) *Decoder {
	return &Decoder{
		r:       r,
		decoder: json.NewDecoder(r),
		s:       s,
	}
}

// Decode blocks until it can return the next object in the writer. Returns an error
// if the writer is closed or an object can't be decoded.
func (d *Decoder) Decode() (watch.EventType, watch.Object, error) {
	var got watchEvent
	if err := d.decoder.Decode(&got); err != nil {
		return "", nil, err
	}

	obj, err := d.s.NewObject(string(got.Type))
	if err != nil {
		return "", nil, err
	}

	err = json.Unmarshal(got.Object, obj)
	if err != nil {
		return "", nil, fmt.Errorf("unable to decode watch event: %v", err)
	}
	return got.Type, obj, nil
}

// Close closes the underlying r.
func (d *Decoder) Close() {
	d.r.Close()
}
