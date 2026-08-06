package annotationhost

import (
	"strings"
	"sync"
)

type boundedBuffer struct {
	mu        sync.Mutex
	maximum   int
	content   []byte
	truncated bool
}

func newBoundedBuffer(maximum int) *boundedBuffer {
	return &boundedBuffer{maximum: maximum}
}

func (buffer *boundedBuffer) Write(content []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	original := len(content)
	remaining := buffer.maximum - len(buffer.content)
	if remaining > 0 {
		if len(content) > remaining {
			content = content[:remaining]
		}
		buffer.content = append(buffer.content, content...)
	}
	if original > remaining {
		buffer.truncated = true
	}
	return original, nil
}

func (buffer *boundedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	value := string(buffer.content)
	if buffer.truncated {
		value += "\n[stderr truncated]"
	}
	return strings.TrimSpace(value)
}
