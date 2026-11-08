# Loopback VPN detection and on-device diagnostics

This change addresses source-proven detection weaknesses. It does not establish
which weakness caused the reported Quantumult X failure, nor whether this build
works on the reporting iPhone. Quantumult X's packet-reflection implementation
has not been inspected. Its configuration need not change.

## Existing call paths and evidence

- `HomeView` starts `DeviceConnection.startProbing()` when its scene becomes
  active, and stops probing when inactive. A probe is repeated after a two-second
  pause unless a paired connection is already preparing/running.
- `SetupView.isNeeded` reads `tunnelFound`; a failed foreground probe puts a
  paired device on the loopback setup screen. Independently,
  `DeviceProblem.noTunnel` renders as “Waiting for the VPN” for native connection
  errors. These two sources of state must not be conflated.
- Before this change the foreground probe called `LibSwiptNewLoopbackProbe`
  using `RemotePairingBrowser.port`, or zero. Go converted zero to `49152`.
  Native device connections separately chose lockdown's `62078` for imported
  lockdown records, or the selected remote-pairing port for remote records.
- The Darwin scanner enumerates up IPv4-bearing `utun` interfaces, route
  gateways and destinations, and point-to-point peers. Interfaces are sorted
  by name. It also collects local addresses from **all** interfaces.
- The old candidate builder appended `10.7.0.1` through the same filter used for
  inferred addresses. Consequently it could remove this explicit fallback if
  the address appeared on an interface. Whether QX does so on this iPhone is
  unverified. The new report's `address_on_local_interface` supplies evidence.
- Previously all candidates raced under one shared one-second deadline. Array
  order provided no priority: a different reachable address could win before
  `10.7.0.1`. This was a selection risk, not proof of the reported failure (a
  raw TCP winner alone normally makes the foreground check succeed).
- Bonjour selects `_remotepairing._tcp` services matching the paired identity's
  authTag or an address on this device, then sorts service names. Previously a
  disappearing/unmatched service or browser stop did not clear the selected
  port. This could leave a stale port; actual involvement here is unverified.
- Local Network permission is per app. Swipt already declares its Bonjour types
  and usage description. The old browser ignored `.waiting` and pre-ready
  `.failed` errors. Successful dials in another app do not prove Swipt's
  permission, route, or selected port. Apple documents Bonjour policy denial
  (`kDNSServiceErr_PolicyDenied`) in
  [TN3179](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy).
- A later pairing/tunnel dial error could be classified as `ProblemNoTunnel`
  even after the initial TCP probe succeeded. It is now a connection failure,
  while specific pairing rejection and developer-mode errors retain their
  existing classifications. Pairing verification and TLS are unchanged.

## Follow-up device evidence and setup navigation

The first QX device report from this build reached `10.7.0.1:49152` in 3 ms,
selected that address, and did not attempt any inferred routes. The explicit
peer was not listed on a local interface in that attempt. This establishes TCP
reachability inside Swipt on that run; it does not establish the cause of the
original unmodified build's failure or authenticate the paired tunnel.

The user then reported that the VPN screen remained visible until leaving and
re-entering Swipt, after which it worked. `LoopbackSetupView` observed its status
text inside `WithPerceptionTracking`, but evaluated the navigation condition
outside that scope and only handled subsequent changes. Perception's
[pinned documentation](https://github.com/pointfreeco/swift-perception/blob/597afd46249f4a71885b95d26b26cc5b893daf32/README.md)
requires perceptible state reads to be tracked. The entire page now tracks the
success condition and handles an already-successful result on appearance.
Advancement is guarded to happen once. Diagnostics also expose the current VPN
setup result and the pairing/Automatic Unlock setup flags, without their contents.

The native connection is acquired by device operations (and the startup
accessibility task when enabled). An idle phase with no native probe can reflect
no active demand or an unmet availability condition; it is not itself a pairing
failure. Next acceptance: launch with QX active and verify that a successful probe
advances without another app restart, then perform a device operation. Repeat
with LocalDevVPN. The navigation repair still requires physical-device acceptance.

## Changed behavior

1. Test the documented `10.7.0.1` peer explicitly, even if listed on an interface.
   Give it a complete attempt before trying inferred peers. Virtual-machine
   guest handling retains its existing separate address selection.
2. If it fails, race all route-derived peers as before, preserving support for
   other peer addresses. Each stage gets 1.5 seconds, with no retry inside an
   attempt. All successful sockets close and outstanding dials are cancelled
   and drained before return. A late successful dial cannot leak a connection.
3. Give the optional local-network classification a separate 300 ms budget and
   report its outcome separately. Normal network dialing is bounded to at most
   two 1.5-second stages plus classification; interface/route enumeration is a
   separate synchronous OS operation included in reported elapsed time.
4. Clear a Bonjour-selected port when no matching service remains or browsing
   stops/fails/waits. Zero means `49152`; invalid numeric ports also fall back
   instead of wrapping to a different port. Keep using current discovered
   remote-pairing ports. Do not try `62078` as a universal substitute.
5. Foreground probes now use `Device.NewLoopbackProbe`, which selects lockdown
   only for an imported lockdown record, matching the native connection path.
6. Retry restarts discovery and the foreground probe. A port change supersedes
   the previous foreground probe; its completion cannot overwrite newer state.

The diagnostics sheet is a copyable snapshot. It contains the latest actual
foreground attempt, its discovery context, current discovery status, and the
latest native device-connection probe/stage. Each probe includes timestamps,
actual target service/port, candidate provenance, local source address, TCP
outcome/error, elapsed time, timeout/cancellation, selected address, and overall
reason. `tcp_connected` proves only TCP connectivity; it does not authenticate
the peer or establish a paired developer tunnel.

Only the latest reports are retained in memory; there is no history file.
Export is bounded to 128 candidate rows per report, with `candidate_count`
reporting the full number. Discovery exports status/counts rather than TXT
records or service names. No pairing records, authTags, passwords, tokens,
certificates, or protocol payloads enter these reports. “No policy denial
observed” and Bonjour `.ready` do not prove Local Network access is granted.

## iPhone validation: QX versus LocalDevVPN

1. Install a build containing both the Swift changes and regenerated LibSwipt.
   Keep the existing pairing record. Use the same Wi-Fi connection for both
   tests and keep Swipt in the foreground while an attempt runs.
2. Enable QX with the existing `reflected_routes = 10.7.0.1/32`. Return to
   Swipt. If iOS asks for Local Network access, allow it. On the loopback setup
   screen, tap **Retry**, wait about six seconds, then **Diagnostics** →
   **Refresh Diagnostics** → **Copy Diagnostics**. Paste into Notes and label
   it “QX”. If setup advances, use **Settings → VPN Diagnostics** instead.
3. Inspect `port` and `target_service` in the completed probe, not just the
   current Bonjour port (which may have changed). For remote pairing the port
   should come from current matching discovery, or be the `49152` fallback.
   For lockdown records, `62078` is expected. Check that `10.7.0.1` was tried
   and whether it connected, failed with an OS error, timed out, or was cancelled.
4. If TCP succeeds, attempt a normal developer operation in Swipt. Copy a new
   report afterward. Compare `selected_address` and the native connection
   stage: “paired tunnel connected” verifies more than a raw successful dial;
   “pairing/tunnel setup failed” calls for investigation beyond VPN detection.
5. Switch to LocalDevVPN, return to Swipt, and repeat under the same Wi-Fi and
   pairing state. Save a separate “LocalDevVPN” report, including after a normal
   developer operation. If available, repeat for StosVPN and a known-working
   sing-box profile. These apps have not been exercised by host unit tests.
6. Switch back to QX and repeat to check discovery/port changes and foreground
   cancellation. For a negative control disable the VPN: the report should
   normally show failure, but any TCP success must be treated as evidence about
   actual routing rather than automatically rejected. Temporarily disabling
   Swipt's Local Network permission can test the denial message; restore the
   permission afterward. No resetting pairing or Developer Mode is required.

If no current matching Bonjour service is available and `49152` is closed, the
report cannot identify an unknown dynamic port by itself. It deliberately does
not scan ports or attempt pairing against arbitrary services. A TCP success on
an inferred peer still requires normal pairing authentication. A service that
continues advertising an obsolete port can only be identified from device
reports; clearing vanished services does not guarantee every advertised port
is live.

## Host verification

Run with the repository's Go 1.26.8 toolchain:

```sh
go test -race -count=10 -timeout=60s ./internal/coredevice/vpnpeer
go test -race -timeout=60s libswipt/loopback.go libswipt/loopback_test.go
go vet ./internal/coredevice/vpnpeer
go vet libswipt/loopback.go libswipt/loopback_test.go
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./internal/coredevice/vpnpeer
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 libswipt/loopback.go libswipt/loopback_test.go
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go test -c -o /tmp/swipt-vpnpeer-darwin.test ./internal/coredevice/vpnpeer
```

The bridge tests are run as an explicit file set on Linux because the rest of
`libswipt` imports Apple-only automation, graphics, and JavaScriptCore packages.
Full `go test ./libswipt ./internal/device` cannot build on this Linux host;
`internal/rfb` also lacks its platform `ConnectionInfo` definition here. These
are build-environment limits, not passing integration tests.

The probe tests cover explicit-address filtering and priority, faster unrelated
candidates, route fallback and late successful dials, scan failure, all failures,
independent stage budgets, caller deadlines/cancellation, and reachable peers
behind blackholed routes. Bridge tests cover valid/dynamic/invalid port handling,
retained cancellation diagnostics, and bounded export without snapshot mutation.

Go formatting, those race tests, vet and Staticcheck checks, Darwin probe cross-compilation,
and a syntax parse of the changed Swift files pass. No Xcode, iOS SDK, or iPhone
is attached to this host. Swift type checking, LibSwipt framework generation,
iOS builds, Local Network prompt behavior, and actual QX/LocalDevVPN pairing and
tunnel acceptance remain unverified.
