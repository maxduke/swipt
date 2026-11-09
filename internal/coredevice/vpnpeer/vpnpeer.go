package vpnpeer

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	F "github.com/sagernet/sing/common/format"
	M "github.com/sagernet/sing/common/metadata"
)

// LocalDevVPN defaults its peer address to 10.7.0.1/32; StosVPN defaults
// tunnelFakeIp to 10.7.0.1 (LocalDevVPN af3fd69,
// LocalDevVPN/Constants.swift:16; StosVPN 010bc14,
// TunnelProv/PacketTunnelProvider.swift:12).
var fixedAddress = netip.AddrFrom4([4]byte{10, 7, 0, 1})

const answerLimit = time.Second

type tunnel struct {
	name         string
	gateways     []netip.Addr
	destinations []netip.Addr
	peers        []netip.Addr
}

// SideStore minimuxer takes utun interfaces that have IPv4 addresses, sorted
// by name, then tries route gateways, route destinations and point-to-point
// peers, in that order (minimuxer 12be70d,
// Sources/Services/DeviceConnectionManager.swift:206-214, 260-283).
func candidates(allowLocal bool) ([]netip.Addr, error) {
	if allowLocal {
		if addresses := guestAddresses(); len(addresses) > 0 {
			return addresses, nil
		}
	}
	tunnels, local, err := scan()
	var result []netip.Addr
	add := func(address netip.Addr) {
		if address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() ||
			address == netip.AddrFrom4([4]byte{255, 255, 255, 255}) ||
			slices.Contains(local, address) || slices.Contains(result, address) {
			return
		}
		result = append(result, address)
	}
	for _, tunnel := range tunnels {
		for _, list := range [][]netip.Addr{tunnel.gateways, tunnel.destinations, tunnel.peers} {
			for _, address := range list {
				add(address)
			}
		}
	}
	add(fixedAddress)
	return result, err
}

type Peer struct {
	Address      netip.Addr
	LocalNetwork bool
}

func Find(ctx context.Context, port uint16, allowLocal bool) (Peer, error) {
	addresses, scanErr := candidates(allowLocal)
	var dialer net.Dialer
	return find(ctx, port, addresses, scanErr, dialer.DialContext)
}

type dialContext func(context.Context, string, string) (net.Conn, error)

func find(ctx context.Context, port uint16, addresses []netip.Addr, scanErr error, dial dialContext) (Peer, error) {
	// Route-derived addresses are heuristics: the first TCP connection need not
	// reach the reflected peer. Try the documented peer before racing alternatives.
	if index := slices.Index(addresses, fixedAddress); index >= 0 {
		peer, err := probe(ctx, port, []netip.Addr{fixedAddress}, nil, dial)
		if err == nil {
			return peer, nil
		}
		if ctx.Err() != nil {
			return Peer{}, ctx.Err()
		}
		if len(addresses) == 1 {
			return Peer{}, E.Errors(err, scanErr)
		}
		scanErr = E.Errors(scanErr, err)
		addresses = slices.Delete(slices.Clone(addresses), index, index+1)
	}
	return probe(ctx, port, addresses, scanErr, dial)
}

func probe(ctx context.Context, port uint16, addresses []netip.Addr, scanErr error, dial dialContext) (Peer, error) {
	if len(addresses) == 0 {
		return Peer{}, E.Errors(E.New("vpnpeer: no VPN address to try"), scanErr)
	}
	ctx, cancel := context.WithTimeout(ctx, answerLimit)
	defer cancel()
	type result struct {
		index int
		local netip.Addr
		err   error
	}
	results := make(chan result, len(addresses))
	for index, address := range addresses {
		go func() {
			conn, err := dial(ctx, "tcp", netip.AddrPortFrom(address, port).String())
			var local netip.Addr
			if err == nil {
				local = M.AddrPortFromNet(conn.LocalAddr()).Addr().Unmap()
				_ = conn.Close()
			}
			results <- result{index: index, local: local, err: err}
		}()
	}
	errs := make([]error, len(addresses))
	for range addresses {
		answer := <-results
		if answer.err == nil {
			address := addresses[answer.index]
			return Peer{Address: address, LocalNetwork: address != answer.local && (onLocalNetwork(answer.local) || loopsThroughLocalNetwork(ctx, address))}, nil
		}
		errs[answer.index] = answer.err
	}
	return Peer{}, E.Cause(E.Errors(append(errs, scanErr)...), "none of the VPN addresses ", strings.Join(F.MapToString(addresses), ", "), " answers on port ", port)
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
