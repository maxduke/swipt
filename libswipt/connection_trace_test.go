package libSwipt

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"syscall"
	"testing"
)

func TestTraceRecordsActualTargetAndWithholdsProtocolPayload(t *testing.T) {
	trace := newConnectionTrace(54321, "remote_pairing")
	trace.selectPeer(netip.MustParseAddr("10.7.0.1"), false)
	trace.advance("tunnel_tcp", 60023)
	trace.advance("tunnel_tls", 60023)
	trace.finish("pairing_or_tunnel_failed", 100, errors.New("PRIVATE_PAIRING_PAYLOAD"))
	output := trace.json()
	if strings.Contains(output, "PRIVATE_PAIRING_PAYLOAD") {
		t.Fatal("protocol payload leaked")
	}
	var report connectionTraceSnapshot
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	if report.Address != "10.7.0.1" || report.PairingPort != 54321 || report.Outcome != "pairing_or_tunnel_failed" || report.Ended.IsZero() || report.FailureClass != "protocol_or_setup_error_details_withheld" {
		t.Fatalf("%+v", report)
	}
	if last := report.Steps[len(report.Steps)-1]; last.Stage != "tunnel_tls" || last.Port != 60023 {
		t.Fatalf("%+v", last)
	}
}

func TestTraceSnapshotConcurrentCopyIsBounded(t *testing.T) {
	trace := newConnectionTrace(49152, "remote_pairing")
	done, copied := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(copied)
		for {
			var report connectionTraceSnapshot
			if err := json.Unmarshal([]byte(trace.json()), &report); err != nil {
				t.Error(err)
			}
			if len(report.Steps) > 16 {
				t.Errorf("unbounded steps: %d", len(report.Steps))
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	for range 1000 {
		trace.advance("pair_verify", 49152)
	}
	trace.finish("paired_tunnel_connected", 0, nil)
	close(done)
	<-copied
}

func TestWrappedNetworkFailureDoesNotExportOtherErrorContents(t *testing.T) {
	trace := newConnectionTrace(49152, "remote_pairing")
	trace.finish("probe_failed", 1, fmt.Errorf("PRIVATE_PAIRING_PAYLOAD: %w", syscall.ECONNREFUSED))
	if report := trace.json(); strings.Contains(report, "PRIVATE_PAIRING_PAYLOAD") || !strings.Contains(report, "connection refused") {
		t.Fatal(report)
	}
}
