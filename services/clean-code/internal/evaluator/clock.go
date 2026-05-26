package evaluator

import "time"

// nowUnixNano is an indirected wall-clock read so tests can
// inject a fixed clock via [NewGateWithEngine].
func nowUnixNano() int64 {
	return time.Now().UTC().UnixNano()
}
