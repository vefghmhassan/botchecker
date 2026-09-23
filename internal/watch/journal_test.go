package watch_test

import (
	"fmt"
	"strings"
	"sync"
)

// journal is one ordered record of everything the fakes were asked to do.
//
// The single-property tests around it each check that some fact ended up true.
// What none of them can see is the *order* — and order is where this service
// has actually gone wrong. Configs were switched off before a replacement
// existed, which no individual assertion noticed because every fact was
// correct; it was the sequence that cost users six and a half minutes. A
// machine's address was recorded as dead after it was deleted rather than
// before, leaving a window where the pool could hand it straight back.
//
// So the fakes all write here, and a lifecycle test reads the sequence.
type journal struct {
	mu     sync.Mutex
	events []string
}

func (j *journal) add(format string, args ...any) {
	j.mu.Lock()
	j.events = append(j.events, fmt.Sprintf(format, args...))
	j.mu.Unlock()
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.events...)
}

// indexOf is the position of the first event with this prefix, or -1.
func (j *journal) indexOf(prefix string) int {
	for i, e := range j.all() {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

// count is how many events start with this prefix.
func (j *journal) count(prefix string) int {
	n := 0
	for _, e := range j.all() {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// String renders the timeline for a failure message, which is the whole point:
// when a lifecycle assertion fails, the reader needs to see what actually
// happened rather than which boolean was wrong.
func (j *journal) String() string {
	var b strings.Builder
	for i, e := range j.all() {
		fmt.Fprintf(&b, "\n  %2d. %s", i+1, e)
	}
	if b.Len() == 0 {
		return "(nothing happened)"
	}
	return b.String()
}

// firstLine trims an alert to its headline, so the timeline stays readable
// rather than carrying whole HTML messages.
func firstLine(s string) string {
	s = strings.ReplaceAll(s, "<b>", "")
	s = strings.ReplaceAll(s, "</b>", "")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
