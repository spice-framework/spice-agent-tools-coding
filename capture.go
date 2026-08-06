package coding

import (
	"bytes"
	"math"
	"sync"
)

type captureBudget struct {
	mutex     sync.Mutex
	remaining int64
	truncated bool
}

type boundedCapture struct {
	budget   *captureBudget
	content  bytes.Buffer
	observed int64
}

func newCapturePair(maximum int64) (*boundedCapture, *boundedCapture) {
	budget := &captureBudget{remaining: maximum}
	return &boundedCapture{budget: budget}, &boundedCapture{budget: budget}
}

func (capture *boundedCapture) Write(value []byte) (int, error) {
	capture.budget.mutex.Lock()
	defer capture.budget.mutex.Unlock()
	capture.observed = saturatingAdd(capture.observed, int64(len(value)))
	accepted := min(int64(len(value)), capture.budget.remaining)
	if accepted > 0 {
		_, _ = capture.content.Write(value[:accepted])
		capture.budget.remaining -= accepted
	}
	if accepted < int64(len(value)) {
		capture.budget.truncated = true
	}
	return len(value), nil
}

func (capture *boundedCapture) snapshot() ([]byte, int64, bool) {
	capture.budget.mutex.Lock()
	defer capture.budget.mutex.Unlock()
	return bytes.Clone(capture.content.Bytes()), capture.observed, capture.budget.truncated
}

func saturatingAdd(left, right int64) int64 {
	if right > math.MaxInt64-left {
		return math.MaxInt64
	}
	return left + right
}
