package handlers

import "net/http"

// StatusRecorder is a transparent http.ResponseWriter wrapper. Captures
// the first WriteHeader status code, cumulative bytes written, and the
// first non-nil Write error. Pass-through only — no behavior change. On
// implicit-WriteHeader (Write without prior WriteHeader) sets HeaderStatus
// to 200 to mirror net/http's contract.
//
// Exported for use by both internal/handlers (call.go logCallDone) and
// internal/handlers/dispatchers (widgets.go logWidgetDone, Q-5XX-DIAG
// 0.25.324). All fields are read by deferred logging hooks AFTER the
// handler completes — no mutex needed since the handler goroutine owns
// the wrapper for its lifetime.
type StatusRecorder struct {
	http.ResponseWriter
	HeaderStatus int
	BytesWritten int64
	WriteErr     error
	wroteHeader  bool
}

// NewStatusRecorder returns a fresh recorder wrapping w. Always allocates;
// the wrapper itself is cheap (one struct + one bool) and is collected
// when the handler goroutine returns.
func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w}
}

func (s *StatusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.HeaderStatus = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *StatusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		// Mirror net/http's implicit 200-on-first-Write contract.
		s.HeaderStatus = http.StatusOK
		s.wroteHeader = true
	}
	n, err := s.ResponseWriter.Write(b)
	s.BytesWritten += int64(n)
	if err != nil && s.WriteErr == nil {
		s.WriteErr = err
	}
	return n, err
}
