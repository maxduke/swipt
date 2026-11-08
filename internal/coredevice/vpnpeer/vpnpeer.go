package vpnpeer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

// LocalDevVPN defaults its peer address to 10.7.0.1/32; StosVPN defaults
// tunnelFakeIp to 10.7.0.1 (LocalDevVPN af3fd69,
// LocalDevVPN/Constants.swift:16; StosVPN 010bc14,
// TunnelProv/PacketTunnelProvider.swift:12).
var fixedAddress = netip.AddrFrom4([4]byte{10, 7, 0, 1})

const (
	answerLimit       = 1500 * time.Millisecond
	localNetworkLimit = 300 * time.Millisecond
)

type tunnel struct {
	name         string
	gateways     []netip.Addr
	destinations []netip.Addr
	peers        []netip.Addr
}

type candidate struct {
	address     netip.Addr
	source      string
	preferred   bool
	onInterface bool
}

func candidates(allowLocal bool) ([]candidate, error) {
	if allowLocal {
		if addresses := guestAddresses(); len(addresses) > 0 {
			var result []candidate
			for _, address := range addresses {
				result = append(result, candidate{address: address, source: "virtual machine interface", onInterface: true})
			}
			return result, nil
		}
	}
	tunnels, local, err := scan()
	return selectCandidates(tunnels, local), err
}

func selectCandidates(tunnels []tunnel, local []netip.Addr) []candidate {
	// A reflected peer can also appear as an interface's own address. Test the
	// documented peer explicitly before inferred routes can win. The local
	// address filter still applies to inferred peers.
	result := []candidate{{address: fixedAddress, source: "documented loopback VPN peer", preferred: true, onInterface: slices.Contains(local, fixedAddress)}}
	seen := map[netip.Addr]bool{fixedAddress: true}
	add := func(address netip.Addr, source string) {
		address = address.Unmap()
		if !address.IsValid() || address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() ||
			address == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || slices.Contains(local, address) || seen[address] {
			return
		}
		seen[address] = true
		result = append(result, candidate{address: address, source: source})
	}
	// Preserve the scanner's interface order, then gateways, destinations, peers.
	for _, tunnel := range tunnels {
		for i, list := range [][]netip.Addr{tunnel.gateways, tunnel.destinations, tunnel.peers} {
			for _, address := range list {
				add(address, tunnel.name+" "+[]string{"route gateway", "route destination", "point-to-point peer"}[i])
			}
		}
	}
	return result
}

type Peer struct {
	Address      netip.Addr
	LocalNetwork bool
	localAddress netip.Addr
}

func Find(ctx context.Context, port uint16, allowLocal bool) (Peer, error) {
	peer, _, err := FindWithDiagnostics(ctx, port, allowLocal)
	return peer, err
}

// Report contains network probe metadata only, never pairing records or keys.
type Report struct {
	Started           time.Time         `json:"started"`
	Port              uint16            `json:"port"`
	Service           string            `json:"target_service,omitempty"`
	StageTimeoutMS    int64             `json:"stage_timeout_ms"`
	ElapsedMS         int64             `json:"elapsed_ms"`
	Outcome           string            `json:"outcome"`
	Reason            string            `json:"reason"`
	ScanError         string            `json:"scan_error,omitempty"`
	Candidates        []CandidateReport `json:"candidates"`
	CandidateCount    int               `json:"candidate_count"`
	SelectedAddress   string            `json:"selected_address,omitempty"`
	LocalNetwork      bool              `json:"local_network"`
	LocalNetworkCheck string            `json:"local_network_check,omitempty"`
}

type CandidateReport struct {
	Address      string `json:"address"`
	Source       string `json:"source"`
	Outcome      string `json:"outcome"`
	ElapsedMS    int64  `json:"elapsed_ms"`
	Error        string `json:"error,omitempty"`
	LocalAddress string `json:"local_address,omitempty"`
	OnInterface  bool   `json:"address_on_local_interface"`
}

func FindWithDiagnostics(ctx context.Context, port uint16, allowLocal bool) (Peer, Report, error) {
	started := time.Now()
	addresses, scanErr := candidates(allowLocal)
	var dialer net.Dialer
	peer, report, err := find(ctx, port, addresses, scanErr, answerLimit, dialer.DialContext)
	if err == nil {
		// Classification is separate from reachability, with its own short
		// budget. A nearly expired TCP probe must not suppress this check.
		localCtx, cancel := context.WithTimeout(ctx, localNetworkLimit)
		defer cancel()
		peer.LocalNetwork = peer.Address != peer.localAddress &&
			(onLocalNetwork(peer.localAddress) || loopsThroughLocalNetwork(localCtx, peer.Address))
		report.LocalNetwork = peer.LocalNetwork
		report.LocalNetworkCheck = "completed"
		if localCtx.Err() != nil {
			report.LocalNetworkCheck = localCtx.Err().Error()
		}
	}
	if ctx.Err() != nil {
		err = ctx.Err()
		report.Outcome = outcome(err)
		report.Reason = err.Error()
	}
	report.Started = started
	report.ElapsedMS = time.Since(started).Milliseconds()
	return peer, report, err
}

type dialContext func(context.Context, string, string) (net.Conn, error)

// find gives preferred peers a complete attempt before inferred candidates can
// win. Each stage is bounded; workers finish and close successful sockets before
// returning, including when cancellation races a successful dial.
func find(ctx context.Context, port uint16, addresses []candidate, scanErr error, timeout time.Duration, dial dialContext) (Peer, Report, error) {
	started := time.Now()
	report := Report{Started: started, Port: port, StageTimeoutMS: timeout.Milliseconds(), CandidateCount: len(addresses)}
	if scanErr != nil {
		report.ScanError = scanErr.Error()
	}
	for _, c := range addresses {
		report.Candidates = append(report.Candidates, CandidateReport{Address: c.address.String(), Source: c.source, Outcome: "not_attempted", OnInterface: c.onInterface})
	}
	var errs []error
	selected := -1
	for start := 0; start < len(addresses) && selected < 0 && ctx.Err() == nil; {
		end := start + 1
		if !addresses[start].preferred {
			for end < len(addresses) && !addresses[end].preferred {
				end++
			}
		}
		var stageErrs []error
		selected, stageErrs = probeStage(ctx, port, addresses, start, end, timeout, dial, report.Candidates)
		errs = append(errs, stageErrs...)
		start = end
	}
	report.ElapsedMS = time.Since(started).Milliseconds()
	if ctx.Err() != nil {
		report.Outcome = outcome(ctx.Err())
		report.Reason = ctx.Err().Error()
		return Peer{}, report, ctx.Err()
	}
	if selected >= 0 {
		report.Outcome = "tcp_connected"
		report.Reason = "TCP connectivity only; pairing and tunnel setup are separate."
		report.SelectedAddress = addresses[selected].address.String()
		local, _ := netip.ParseAddr(report.Candidates[selected].LocalAddress)
		return Peer{Address: addresses[selected].address, localAddress: local}, report, nil
	}
	report.Outcome = "failed"
	report.Reason = "No candidate accepted a TCP connection on the selected port."
	cause := errors.Join(append(errs, scanErr)...)
	if errors.Is(cause, context.DeadlineExceeded) || slices.ContainsFunc(report.Candidates, func(c CandidateReport) bool { return c.Outcome == "timeout" }) {
		report.Outcome = "timeout"
		report.Reason = "No candidate connected; one or more TCP attempts timed out."
	}
	if len(addresses) == 0 {
		report.Reason = "No VPN address to try."
	}
	err := fmt.Errorf("vpnpeer: %s port %d", report.Reason, port)
	if cause != nil {
		err = fmt.Errorf("%w: %w", err, cause)
	}
	return Peer{}, report, err
}

func probeStage(parent context.Context, port uint16, addresses []candidate, start, end int, timeout time.Duration, dial dialContext, reports []CandidateReport) (int, []error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	type result struct {
		index  int
		report CandidateReport
		err    error
	}
	results := make(chan result, end-start)
	// Race all inferred peers as before. A worker pool here could starve a
	// reachable peer behind several blackholed routes for the entire deadline.
	for i := start; i < end; i++ {
		go func() {
			r := reports[i]
			if ctx.Err() != nil {
				r.Error = ctx.Err().Error()
				results <- result{index: i, report: r}
				return
			}
			began := time.Now()
			conn, err := dial(ctx, "tcp", netip.AddrPortFrom(addresses[i].address, port).String())
			if conn != nil {
				r.LocalAddress = M.AddrPortFromNet(conn.LocalAddr()).Addr().Unmap().String()
				_ = conn.Close()
			}
			r.ElapsedMS = time.Since(began).Milliseconds()
			r.Outcome = "tcp_connected"
			if err != nil {
				r.Outcome = outcome(err)
				r.Error = err.Error()
			}
			results <- result{index: i, report: r, err: err}
		}()
	}

	selected := -1
	var errs []error
	for range end - start {
		r := <-results
		reports[r.index] = r.report
		if r.report.Outcome == "tcp_connected" && selected < 0 {
			selected = r.index
			cancel()
		}
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	if selected < 0 && ctx.Err() != nil {
		errs = append(errs, ctx.Err())
	}
	return selected, errs
}

func outcome(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if timeout, ok := errors.AsType[net.Error](err); ok && timeout.Timeout() {
		return "timeout"
	}
	return "failed"
}

func loopsThroughLocalNetwork(ctx context.Context, address netip.Addr) bool {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", ":0")
	if err != nil {
		return false
	}
	defer listener.Close()
	accepted := make(chan netip.Addr, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- M.AddrPortFromNet(conn.LocalAddr()).Addr().Unmap()
		_ = conn.Close()
	}()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(address, M.AddrPortFromNet(listener.Addr()).Port()).String())
	if err != nil {
		return false
	}
	defer conn.Close()
	select {
	case local := <-accepted:
		return onLocalNetwork(local)
	case <-ctx.Done():
		return false
	}
}
