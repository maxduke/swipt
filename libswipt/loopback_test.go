package libSwipt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/swipt-automation/swipt/internal/coredevice/vpnpeer"
)

func TestRemotePairingPort(t *testing.T) {
	for _, test := range []struct {
		input int32
		want  uint16
	}{
		{0, 49152},
		{-1, 49152},
		{65536, 49152},
		{114688, 49152},
		{54321, 54321},
		{65535, 65535},
	} {
		t.Run(fmt.Sprint(test.input), func(t *testing.T) {
			probe := NewLoopbackProbe(test.input)
			defer probe.Cancel()
			if probe.port != test.want {
				t.Fatalf("port=%d, want %d", probe.port, test.want)
			}
		})
	}
}

func TestCancelledProbeRetainsActualDiagnostics(t *testing.T) {
	probe := NewLoopbackProbe(54321)
	probe.Cancel()
	if _, err := probe.Run(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	var report vpnpeer.Report
	if err := json.Unmarshal([]byte(probe.Diagnostics()), &report); err != nil {
		t.Fatal(err)
	}
	if report.Port != 54321 || report.Outcome != "cancelled" || report.Service != "remote pairing" || report.Started.IsZero() {
		t.Fatalf("%+v", report)
	}
}

func TestDiagnosticExportIsBoundedAndDoesNotMutateSnapshot(t *testing.T) {
	report := vpnpeer.Report{Candidates: make([]vpnpeer.CandidateReport, 300), CandidateCount: 300}
	var exported vpnpeer.Report
	if err := json.Unmarshal([]byte(probeDiagnostics(&report)), &exported); err != nil {
		t.Fatal(err)
	}
	if len(exported.Candidates) != 128 || exported.CandidateCount != 300 || len(report.Candidates) != 300 {
		t.Fatalf("exported=%d total=%d original=%d", len(exported.Candidates), exported.CandidateCount, len(report.Candidates))
	}
}
