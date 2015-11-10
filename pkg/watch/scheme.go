package watch

import (
	"fmt"
	"reflect"
)

// Scheme defines an entire encoding and decoding scheme.
type Scheme struct {
	typeMap map[string]reflect.Type
}

func NewScheme() *Scheme {
	s := &Scheme{
		typeMap: map[string]reflect.Type{},
	}
	return s
}

func (s *Scheme) AddKnownTypes(types ...interface{}) {
	for _, obj := range types {
		t := reflect.TypeOf(obj)
		if t.Kind() != reflect.Ptr {
			panic("All types must be pointers to structs.")
		}
		t = t.Elem()
		if t.Kind() != reflect.Struct {
			panic("All types must be pointers to structs.")
		}
		s.typeMap[t.Name()] = t
	}
}

func (s *Scheme) NewObject(typeName string) (interface{}, error) {
	if t, ok := s.typeMap[typeName]; ok {
		return reflect.New(t).Interface(), nil
	}
	return nil, fmt.Errorf("no type '%v'", typeName)
}
