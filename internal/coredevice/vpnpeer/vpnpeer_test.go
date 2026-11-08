package vpnpeer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type testConn struct {
	net.Conn
	local     netip.Addr
	closed    atomic.Bool
	closeDone chan struct{}
}

func (c *testConn) LocalAddr() net.Addr {
	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(c.local, 12345))
}

func (c *testConn) Close() error {
	if c.closed.CompareAndSwap(false, true) && c.closeDone != nil {
		close(c.closeDone)
	}
	return nil
}

func TestDocumentedPeerCannotLoseToFasterRoute(t *testing.T) {
	addresses := []netip.Addr{netip.MustParseAddr("198.19.0.3"), fixedAddress}
	original := slices.Clone(addresses)
	started, release := make(chan struct{}), make(chan struct{})
	conn := &testConn{local: fixedAddress}
	var otherCalls atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		peer, err := find(ctx, 54321, addresses, nil, func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" {
				t.Errorf("network %q", network)
			}
			if address != "10.7.0.1:54321" {
				otherCalls.Add(1)
				return &testConn{local: addresses[0]}, nil // wins an unrestricted TCP race
			}
			close(started)
			select {
			case <-release:
				return conn, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		if err != nil || peer.Address != fixedAddress {
			t.Errorf("peer=%+v error=%v", peer, err)
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("preferred dial did not start")
	}
	if otherCalls.Load() != 0 {
		t.Error("route raced the documented peer")
	}
	close(release)
	<-done
	if otherCalls.Load() != 0 || !conn.closed.Load() {
		t.Error("unrelated route dialed or socket leaked")
	}
	if !slices.Equal(addresses, original) {
		t.Error("caller candidate list mutated")
	}
}

func TestOtherPeerStillWorksAfterDocumentedPeerFails(t *testing.T) {
	other := netip.MustParseAddr("172.19.0.2")
	conn := &testConn{local: other}
	peer, err := find(t.Context(), 55555, []netip.Addr{other, fixedAddress}, nil, func(ctx context.Context, _, address string) (net.Conn, error) {
		if address == "10.7.0.1:55555" {
			return nil, syscall.ECONNREFUSED
		}
		if address != "172.19.0.2:55555" {
			t.Errorf("unexpected target %s", address)
		}
		return conn, nil
	})
	if err != nil || peer.Address != other || !conn.closed.Load() {
		t.Fatalf("peer=%+v error=%v closed=%v", peer, err, conn.closed.Load())
	}
}

func TestDocumentedPeerTimeoutDoesNotConsumeFallbackBudget(t *testing.T) {
	other := netip.MustParseAddr("172.19.0.2")
	peer, err := find(t.Context(), 49152, []netip.Addr{other, fixedAddress}, nil, func(ctx context.Context, _, address string) (net.Conn, error) {
		if address == "10.7.0.1:49152" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if ctx.Err() != nil {
			t.Error("fallback inherited expired stage deadline")
		}
		return &testConn{local: other}, nil
	})
	if err != nil || peer.Address != other {
		t.Fatalf("peer=%+v error=%v", peer, err)
	}
}

func TestParentDeadlineStopsBeforeFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var calls atomic.Int32
	_, err := find(ctx, 49152, []netip.Addr{netip.MustParseAddr("172.19.0.2"), fixedAddress}, nil, func(ctx context.Context, _, _ string) (net.Conn, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("error=%v calls=%d", err, calls.Load())
	}
}

func TestParentCancellationStopsBeforeFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	_, err := find(ctx, 49152, []netip.Addr{netip.MustParseAddr("172.19.0.2"), fixedAddress}, nil, func(ctx context.Context, _, _ string) (net.Conn, error) {
		calls.Add(1)
		cancel()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("error=%v calls=%d", err, calls.Load())
	}
}

func TestFailuresPreserveBothStagesAndScannerError(t *testing.T) {
	scanErr := errors.New("read routes failed")
	_, err := find(t.Context(), 49152, []netip.Addr{netip.MustParseAddr("172.19.0.2"), fixedAddress}, scanErr, func(_ context.Context, _, address string) (net.Conn, error) {
		if address == "10.7.0.1:49152" {
			return nil, syscall.ECONNREFUSED
		}
		return nil, syscall.ENETUNREACH
	})
	for _, cause := range []error{scanErr, syscall.ECONNREFUSED, syscall.ENETUNREACH} {
		if !errors.Is(err, cause) {
			t.Errorf("missing cause %v in %v", cause, err)
		}
	}
}

func TestSingleDocumentedPeerFailure(t *testing.T) {
	scanErr := errors.New("read routes failed")
	_, err := find(t.Context(), 49152, []netip.Addr{fixedAddress}, scanErr, func(context.Context, string, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED })
	if !errors.Is(err, scanErr) || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatal(err)
	}
}

func TestRouteOnlyCandidatesRetainRaceAndCloseLateSuccess(t *testing.T) {
	// Virtual-machine guests and VPNs without the conventional address retain
	// their existing candidate selection; a late TCP success still must close.
	slow, fast := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	slowStarted, slowClosed := make(chan struct{}), make(chan struct{})
	slowConn, fastConn := &testConn{local: slow, closeDone: slowClosed}, &testConn{local: fast}
	peer, err := find(t.Context(), 49152, []netip.Addr{slow, fast}, nil, func(ctx context.Context, _, address string) (net.Conn, error) {
		if address == "192.0.2.1:49152" {
			close(slowStarted)
			<-ctx.Done()
			// Close is performed by the probe after this late successful dial.
			return slowConn, nil
		}
		<-slowStarted
		return fastConn, nil
	})
	if err != nil || peer.Address != fast || !fastConn.closed.Load() {
		t.Fatalf("peer=%+v error=%v", peer, err)
	}
	select {
	case <-slowClosed:
	case <-time.After(time.Second):
		t.Error("late successful connection leaked")
	}
}

func TestNoCandidatesPreservesScannerError(t *testing.T) {
	scanErr := errors.New("read routes failed")
	_, err := find(t.Context(), 49152, nil, scanErr, nil)
	if !errors.Is(err, scanErr) {
		t.Fatal(err)
	}
}
