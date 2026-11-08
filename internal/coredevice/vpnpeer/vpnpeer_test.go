package vpnpeer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestSelectCandidates(t *testing.T) {
	tunnels := []tunnel{{
		name:         "utun4",
		gateways:     []netip.Addr{addr("192.0.2.1"), fixedAddress, addr("0.0.0.0"), {}},
		destinations: []netip.Addr{addr("192.0.2.1"), addr("192.0.2.2"), addr("127.0.0.1"), addr("224.0.0.1"), addr("255.255.255.255")},
		peers:        []netip.Addr{addr("192.0.2.3"), addr("192.0.2.4")},
	}}
	// In particular, listing the reflected peer on an interface must not
	// suppress it. Other interface-local addresses remain excluded.
	got := selectCandidates(tunnels, []netip.Addr{fixedAddress, addr("192.0.2.4")})
	want := []netip.Addr{fixedAddress, addr("192.0.2.1"), addr("192.0.2.2"), addr("192.0.2.3")}
	if len(got) != len(want) {
		t.Fatalf("candidates: %+v", got)
	}
	for i, c := range got {
		if c.address != want[i] || c.preferred != (i == 0) {
			t.Errorf("candidate %d: %+v", i, c)
		}
	}
	if got[1].source != "utun4 route gateway" || got[2].source != "utun4 route destination" || got[3].source != "utun4 point-to-point peer" {
		t.Errorf("missing provenance: %+v", got)
	}
	if !got[0].onInterface {
		t.Error("explicit peer's local-interface evidence was lost")
	}
}

func TestSelectCandidatesWithoutInterfaces(t *testing.T) {
	got := selectCandidates(nil, nil)
	if len(got) != 1 || got[0].address != fixedAddress || !got[0].preferred {
		t.Fatalf("%+v", got)
	}
}

type probeConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *probeConn) Close() error { c.closed.Store(true); return nil }
func (c *probeConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 20), Port: 12345}
}

func testCandidates() []candidate {
	return selectCandidates([]tunnel{{name: "utun0", gateways: []netip.Addr{addr("192.0.2.1"), addr("192.0.2.2")}}}, nil)
}

func TestPreferredPeerCannotLoseToFasterRoute(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var fallbackCalls atomic.Int32
	conn := new(probeConn)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		peer, report, err := find(ctx, 54321, testCandidates(), nil, time.Second, func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "10.7.0.1:54321" {
				fallbackCalls.Add(1)
				return new(probeConn), nil // would win an unrestricted TCP race
			}
			close(started)
			select {
			case <-release:
				return conn, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		if err != nil || peer.Address != fixedAddress || report.SelectedAddress != "10.7.0.1" {
			t.Errorf("%+v, %+v, %v", peer, report, err)
		}
		if report.Candidates[0].Outcome != "tcp_connected" || report.Candidates[1].Outcome != "not_attempted" {
			t.Errorf("%+v", report)
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("preferred dial did not start")
	}
	if fallbackCalls.Load() != 0 {
		t.Error("inferred route raced preferred peer")
	}
	close(release)
	<-done
	if !conn.closed.Load() || fallbackCalls.Load() != 0 {
		t.Error("connection leaked or unrelated peer dialed")
	}
}

func TestFallbackRaceCancelsAndClosesLateSuccess(t *testing.T) {
	slowStarted := make(chan struct{})
	slow, fast := new(probeConn), new(probeConn)
	peer, report, err := find(t.Context(), 55555, testCandidates(), nil, time.Second, func(ctx context.Context, _, address string) (net.Conn, error) {
		switch address {
		case "10.7.0.1:55555":
			return nil, syscall.ECONNREFUSED
		case "192.0.2.1:55555":
			close(slowStarted)
			<-ctx.Done()
			return slow, nil // a dial may succeed concurrently with cancellation
		default:
			<-slowStarted
			return fast, nil
		}
	})
	if err != nil || peer.Address != addr("192.0.2.2") || report.Port != 55555 {
		t.Fatalf("%+v, %+v, %v", peer, report, err)
	}
	if !slow.closed.Load() || !fast.closed.Load() {
		t.Error("successful connection leaked")
	}
	if report.Candidates[0].Outcome != "failed" || report.Candidates[1].Outcome != "tcp_connected" {
		t.Errorf("%+v", report)
	}
}

func TestPreferredTimeoutLeavesFallbackItsOwnBudget(t *testing.T) {
	conn := new(probeConn)
	peer, report, err := find(t.Context(), 49152, testCandidates()[:2], nil, 20*time.Millisecond, func(ctx context.Context, _, address string) (net.Conn, error) {
		if address == "10.7.0.1:49152" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if ctx.Err() != nil {
			t.Error("fallback inherited expired preferred deadline")
		}
		return conn, nil
	})
	if err != nil || peer.Address != addr("192.0.2.1") || report.Candidates[0].Outcome != "timeout" || !conn.closed.Load() {
		t.Fatalf("%+v, %+v, %v", peer, report, err)
	}
}

func TestAllCandidatesFail(t *testing.T) {
	scanErr := errors.New("routing table unavailable")
	peer, report, err := find(t.Context(), 54321, testCandidates(), scanErr, time.Second, func(context.Context, string, string) (net.Conn, error) {
		return nil, syscall.ECONNREFUSED
	})
	if peer.Address.IsValid() || !errors.Is(err, syscall.ECONNREFUSED) || !errors.Is(err, scanErr) || report.Outcome != "failed" || report.ScanError != scanErr.Error() {
		t.Fatalf("%+v, %+v, %v", peer, report, err)
	}
	for _, c := range report.Candidates {
		if c.Outcome != "failed" || c.Error == "" || c.Source == "" {
			t.Errorf("%+v", c)
		}
	}
}

func TestScanFailureDoesNotSuppressKnownPeer(t *testing.T) {
	scanErr := errors.New("routing table unavailable")
	conn := new(probeConn)
	peer, report, err := find(t.Context(), 49152, selectCandidates(nil, nil), scanErr, time.Second, func(context.Context, string, string) (net.Conn, error) { return conn, nil })
	if err != nil || peer.Address != fixedAddress || report.ScanError != scanErr.Error() || !conn.closed.Load() {
		t.Fatalf("%+v, %+v, %v", peer, report, err)
	}
}

func TestTimeoutWaitsForAllDialsToExit(t *testing.T) {
	addresses := testCandidates()
	for i := byte(3); i < 40; i++ {
		addresses = append(addresses, candidate{address: netip.AddrFrom4([4]byte{192, 0, 2, i}), source: "test route"})
	}
	var active atomic.Int32
	began := time.Now()
	_, report, err := find(t.Context(), 49152, addresses, nil, 20*time.Millisecond, func(ctx context.Context, _, _ string) (net.Conn, error) {
		active.Add(1)
		defer active.Add(-1)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || report.Outcome != "timeout" {
		t.Fatalf("%+v, %v", report, err)
	}
	if active.Load() != 0 {
		t.Fatalf("%d dials still running", active.Load())
	}

	if time.Since(began) > time.Second {
		t.Error("probe exceeded bounded stages")
	}
}

func TestParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, report, err := find(ctx, 49152, testCandidates(), nil, time.Second, func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		if !errors.Is(err, context.Canceled) || report.Outcome != "cancelled" || report.Candidates[0].Outcome != "cancelled" {
			t.Errorf("%+v, %v", report, err)
		}
	}()
	<-started
	cancel()
	<-done
}

func TestAlreadyCancelledDoesNotDial(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, report, err := find(ctx, 49152, testCandidates(), nil, time.Second, func(context.Context, string, string) (net.Conn, error) {
		t.Error("dial after cancellation")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) || report.Outcome != "cancelled" {
		t.Fatalf("%+v, %v", report, err)
	}
}

func TestParentDeadlineBoundsAllStages(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, report, err := find(ctx, 49152, testCandidates(), nil, time.Second, func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() })
	if !errors.Is(err, context.DeadlineExceeded) || report.Outcome != "timeout" || report.Candidates[1].Outcome != "not_attempted" {
		t.Fatalf("%+v, %v", report, err)
	}
}

func TestNoCandidates(t *testing.T) {
	_, report, err := find(t.Context(), 49152, nil, nil, time.Second, nil)
	if err == nil || report.Outcome != "failed" || report.Reason != "No VPN address to try." {
		t.Fatalf("%+v, %v", report, err)
	}
}

func TestExpiredStageDoesNotDial(t *testing.T) {
	_, report, err := find(t.Context(), 49152, testCandidates(), nil, 0, func(context.Context, string, string) (net.Conn, error) {
		t.Error("dial started after stage deadline")
		return nil, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || report.Outcome != "timeout" {
		t.Fatalf("%+v, %v", report, err)
	}
}

func TestLateRouteIsNotStarvedByBlackholedRoutes(t *testing.T) {
	addresses := testCandidates()
	for i := byte(3); i <= 100; i++ {
		addresses = append(addresses, candidate{address: netip.AddrFrom4([4]byte{192, 0, 2, i}), source: "test route"})
	}
	peer, report, err := find(t.Context(), 49152, addresses, nil, time.Second, func(ctx context.Context, _, target string) (net.Conn, error) {
		if target == "10.7.0.1:49152" {
			return nil, syscall.ECONNREFUSED
		}
		if target == "192.0.2.100:49152" {
			return new(probeConn), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil || peer.Address != addr("192.0.2.100") || report.Outcome != "tcp_connected" {
		t.Fatalf("%+v, %+v, %v", peer, report, err)
	}
}
