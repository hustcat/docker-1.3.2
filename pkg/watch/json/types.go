package json

import (
	"encoding/json"
	"github.com/docker/docker/pkg/watch"
)

type watchEvent struct {
	Type watch.EventType

	Object json.RawMessage
}
