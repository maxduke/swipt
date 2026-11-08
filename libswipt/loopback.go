package libSwipt

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/swipt-automation/swipt/internal/coredevice/vpnpeer"
)

const defaultRemotePairingPort = 49152

type LoopbackAddress struct {
	LocalNetwork bool
}

func newLoopbackAddress(peer vpnpeer.Peer) *LoopbackAddress {
	return &LoopbackAddress{LocalNetwork: peer.LocalNetwork}
}

type LoopbackProbe struct {
	ctx     context.Context
	cancel  context.CancelFunc
	port    uint16
	service string
	report  atomic.Pointer[vpnpeer.Report]
}

func NewLoopbackProbe(remotePairingPort int32) *LoopbackProbe {
	port := remotePairingPortOrDefault(remotePairingPort)
	ctx, cancel := context.WithCancel(context.Background())
	return &LoopbackProbe{ctx: ctx, cancel: cancel, port: port, service: "remote pairing"}
}

func (p *LoopbackProbe) Run() (*LoopbackAddress, error) {
	defer p.cancel()
	peer, report, err := vpnpeer.FindWithDiagnostics(p.ctx, p.port, true)
	report.Service = p.service
	p.report.Store(&report)
	if p.ctx.Err() != nil {
		return nil, p.ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return newLoopbackAddress(peer), nil
}

func (p *LoopbackProbe) Cancel() {
	p.cancel()
}

// Diagnostics is safe to export: the probe never handles pairing data.
func (p *LoopbackProbe) Diagnostics() string {
	return probeDiagnostics(p.report.Load())
}

func probeDiagnostics(report *vpnpeer.Report) string {
	if report == nil {
		return "No completed TCP probe."
	}
	// Retain only the latest attempt and bound exported route metadata.
	snapshot := *report
	snapshot.Candidates = snapshot.Candidates[:min(128, len(snapshot.Candidates))]
	data, _ := json.MarshalIndent(snapshot, "", "  ")
	return string(data)
}

func remotePairingPortOrDefault(port int32) uint16 {
	if port <= 0 || port > 65535 {
		return defaultRemotePairingPort
	}
	return uint16(port)
}
