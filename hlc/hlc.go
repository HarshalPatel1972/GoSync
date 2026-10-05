// Package hlc implements a Hybrid Logical Clock.
//
// An HLC timestamp combines wall-clock milliseconds with a logical counter and
// a node ID. Timestamps are totally ordered, never go backwards on a node, and
// always move ahead of any timestamp the node has observed. That gives GoSync
// last-writer-wins semantics that respect causality: a write made after seeing
// another write is guaranteed to win over it.
//
// Timestamps are encoded as fixed-width strings so that lexical order equals
// timestamp order, which lets every store (SQL, IndexedDB, memory) compare them
// without decoding.
package hlc

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	wallDigits    = 15 // milliseconds since epoch; good until year 33658
	counterDigits = 6
	// MaxNodeLen bounds the node ID so encoded timestamps stay small.
	MaxNodeLen = 64
)

// Timestamp is a decoded HLC value.
type Timestamp struct {
	Wall    int64 // unix milliseconds
	Counter int32
	Node    string
}

// String encodes the timestamp as "<wall>-<counter>-<node>" with zero padding.
func (t Timestamp) String() string {
	return fmt.Sprintf("%0*d-%0*d-%s", wallDigits, t.Wall, counterDigits, t.Counter, t.Node)
}

// Compare returns -1, 0 or 1. The node ID breaks ties between equal clocks.
func (t Timestamp) Compare(o Timestamp) int {
	switch {
	case t.Wall != o.Wall:
		return cmpInt(t.Wall, o.Wall)
	case t.Counter != o.Counter:
		return cmpInt(int64(t.Counter), int64(o.Counter))
	default:
		return strings.Compare(t.Node, o.Node)
	}
}

func cmpInt(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

var errFormat = errors.New("hlc: malformed timestamp")

// Parse decodes a timestamp produced by Timestamp.String.
func Parse(s string) (Timestamp, error) {
	if len(s) < wallDigits+counterDigits+3 || s[wallDigits] != '-' || s[wallDigits+counterDigits+1] != '-' {
		return Timestamp{}, errFormat
	}
	wall, err := strconv.ParseInt(s[:wallDigits], 10, 64)
	if err != nil || wall < 0 {
		return Timestamp{}, errFormat
	}
	counter, err := strconv.ParseInt(s[wallDigits+1:wallDigits+1+counterDigits], 10, 32)
	if err != nil || counter < 0 {
		return Timestamp{}, errFormat
	}
	node := s[wallDigits+counterDigits+2:]
	if err := ValidateNode(node); err != nil {
		return Timestamp{}, err
	}
	return Timestamp{Wall: wall, Counter: int32(counter), Node: node}, nil
}

// ValidateNode checks that a node ID is non-empty, bounded and printable ASCII
// without the separator, so encoded timestamps stay unambiguous.
func ValidateNode(node string) error {
	if node == "" || len(node) > MaxNodeLen {
		return fmt.Errorf("hlc: node id must be 1-%d bytes", MaxNodeLen)
	}
	for i := 0; i < len(node); i++ {
		c := node[i]
		if c <= ' ' || c > '~' {
			return errors.New("hlc: node id must be printable ASCII without spaces")
		}
	}
	return nil
}

// Clock generates monotonically increasing timestamps for one node.
// It is safe for concurrent use.
type Clock struct {
	mu   sync.Mutex
	node string
	now  func() int64
	last Timestamp
}

// NewClock returns a clock for node. now returns unix milliseconds; nil uses
// the system clock.
func NewClock(node string, now func() int64) (*Clock, error) {
	if err := ValidateNode(node); err != nil {
		return nil, err
	}
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	return &Clock{node: node, now: now}, nil
}

// Now returns a timestamp greater than every timestamp previously returned or
// observed by this clock.
func (c *Clock) Now() Timestamp {
	c.mu.Lock()
	defer c.mu.Unlock()
	wall := c.now()
	if wall > c.last.Wall {
		c.last = Timestamp{Wall: wall, Node: c.node}
	} else {
		c.last = Timestamp{Wall: c.last.Wall, Counter: c.last.Counter + 1, Node: c.node}
	}
	return c.last
}

// Observe advances the clock past a remote timestamp so that later local
// writes are ordered after it.
func (c *Clock) Observe(remote Timestamp) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if remote.Wall > c.last.Wall || (remote.Wall == c.last.Wall && remote.Counter > c.last.Counter) {
		c.last = Timestamp{Wall: remote.Wall, Counter: remote.Counter, Node: c.node}
	}
}
