package libSwipt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"syscall"
	"time"
)

// Diagnostic builds retain network metadata, never arbitrary protocol errors
// (which can include decrypted replies, identifiers or authentication material).
type connectionTrace struct {
	current atomic.Pointer[connectionTraceSnapshot]
}

type connectionTraceSnapshot struct {
	Started      time.Time             `json:"started"`
	Ended        time.Time             `json:"ended,omitzero"`
	Service      string                `json:"service"`
	PairingPort  uint16                `json:"pairing_port"`
	Address      string                `json:"selected_address,omitempty"`
	LocalNetwork bool                  `json:"local_network"`
	Outcome      string                `json:"outcome"`
	ErrorCode    int32                 `json:"error_code"`
	FailureClass string                `json:"failure_class,omitempty"`
	Steps        []connectionTraceStep `json:"steps"`
	ElapsedMS    int64                 `json:"elapsed_ms"`
}

type connectionTraceStep struct {
	Stage     string `json:"stage"`
	Port      uint16 `json:"port,omitempty"`
	StartedMS int64  `json:"started_ms"`
}

func newConnectionTrace(port uint16, service string) *connectionTrace {
	trace := new(connectionTrace)
	trace.current.Store(&connectionTraceSnapshot{Started: time.Now(), Service: service, PairingPort: port, Outcome: "in_progress"})
	trace.advance("select_peer", port)
	return trace
}

// A connection attempt writes its trace synchronously. Readers see immutable
// snapshots, including when copying diagnostics during a blocked handshake.
func (t *connectionTrace) advance(stage string, port uint16) {
	next := *t.current.Load()
	next.Steps = append(slices.Clone(next.Steps), connectionTraceStep{Stage: stage, Port: port, StartedMS: time.Since(next.Started).Milliseconds()})
	next.Steps = next.Steps[max(0, len(next.Steps)-16):]
	t.current.Store(&next)
}

func (t *connectionTrace) selectPeer(address netip.Addr, localNetwork bool) {
	next := *t.current.Load()
	next.Address, next.LocalNetwork = address.String(), localNetwork
	t.current.Store(&next)
}

func (t *connectionTrace) finish(outcome string, code int32, err error) {
	next := *t.current.Load()
	next.Ended, next.Outcome, next.ErrorCode = time.Now(), outcome, code
	if err != nil {
		next.FailureClass = connectionFailureClass(err)
	}
	t.current.Store(&next)
}

func (t *connectionTrace) json() string {
	snapshot := *t.current.Load()
	end := snapshot.Ended
	if end.IsZero() {
		end = time.Now()
	}
	snapshot.ElapsedMS = end.Sub(snapshot.Started).Milliseconds()
	data, _ := json.MarshalIndent(snapshot, "", "  ")
	return string(data)
}

func connectionFailureClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.EOF):
		return "end_of_stream"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_end_of_stream"
	}
	for _, errno := range []syscall.Errno{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ETIMEDOUT, syscall.ENETDOWN, syscall.EHOSTDOWN, syscall.EPIPE} {
		if errors.Is(err, errno) {
			return errno.Error()
		}
	}
	if networkError, ok := errors.AsType[net.Error](err); ok && networkError.Timeout() {
		return "timeout"
	}
	return "protocol_or_setup_error_details_withheld"
}
