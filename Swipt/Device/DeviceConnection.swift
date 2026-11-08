import Foundation
import LibSwipt
import Perception
import os

@Perceptible
final class DeviceConnection {
    nonisolated private static let logger = Logger(subsystem: Bundle.main.bundleIdentifier ?? "Swipt", category: "device")

    enum Phase: Equatable {
        case idle
        case connecting
        case preparing
        case running
        case failed

        var isConnected: Bool {
            switch self {
            case .idle, .failed: false
            case .connecting, .preparing, .running: true
            }
        }
    }

    private(set) var phase: Phase = .idle
    private(set) var tunnelFound: Bool?
    private(set) var tunnelThroughLocalNetwork = false
    private(set) var probeInProgress = false
    private(set) var probeMessage = String(localized: "Checking developer-service connectivity…")
    private(set) var probeDiagnostics = "No completed foreground TCP probe."
    private(set) var probeContext = ""
    private(set) var connectionErrorCode: Int32 = 0

    let remotePairing = RemotePairingBrowser()
    let pairingSession: PairingSession
    @PerceptionIgnored private let device: LibSwiptDevice
    @PerceptionIgnored private var probeTask: Task<Void, Never>?
    @PerceptionIgnored private var probeClients = 0
    @PerceptionIgnored private var probeGeneration = UUID()
    @PerceptionIgnored private var isBrowsing = false

    private var autoUnlock: AutoUnlockSettings { .shared }
    private var pairing: PairingStore { .shared }

    init(_ device: LibSwiptDevice, events: DeviceEvents) {
        self.device = device
        pairingSession = PairingSession(device: device)
        remotePairing.onPortChange = { [weak self] port in
            self?.device.setRemotePairingPort(Int32(port))
            self?.restartProbing()
        }
        events.connect { [weak self] change in
            DispatchQueue.main.async { self?.handle(change) }
        }
    }

    var unavailableReason: String? {
        if !pairing.isPaired {
            return String(localized: "Pair this device in Swipt first.")
        }
        if autoUnlock.problem != nil {
            return String(localized: "Check Settings › Automatic Unlock.")
        }
        return nil
    }

    func start() {
        observePairing()
        observeAvailability()
        observeSettings()
    }

    func clearFailure() {
        if phase == .failed {
            phase = .idle
        }
    }

    func setBrowsing(_ browsing: Bool) {
        guard browsing != isBrowsing else { return }
        isBrowsing = browsing
        if browsing {
            remotePairing.start()
        } else {
            remotePairing.stop()
        }
    }

    func startProbing() {
        probeClients += 1
        remotePairing.start()
        launchProbeTask()
    }

    func retryProbing() {
        remotePairing.retryDiscovery()
        restartProbing()
    }

    private func restartProbing() {
        guard probeClients > 0 else { return }
        let previous = probeTask
        previous?.cancel()
        probeTask = nil
        launchProbeTask(after: previous)
    }

    private func launchProbeTask(after previous: Task<Void, Never>? = nil) {
        guard probeTask == nil else { return }
        let generation = UUID()
        probeGeneration = generation
        probeTask = Task {
            await previous?.value
            guard !Task.isCancelled else { return }
            probeInProgress = false
            var reportedFailure: String?
            while !Task.isCancelled {
                let found: Bool
                let throughLocalNetwork: Bool
                if phase == .preparing || phase == .running {
                    found = true
                    throughLocalNetwork = device.loopbackAddress()?.localNetwork ?? false
                    probeMessage = String(localized: "Paired developer tunnel reached. Developer services are connected or preparing.")
                    reportedFailure = nil
                } else {
                    let port = remotePairing.port ?? 0
                    let context = "Foreground probe\n" + remotePairing.diagnostics
                    probeInProgress = true
                    let probe = UncheckedSendable(value: device.newLoopbackProbe(Int32(port))!)
                    let result = await withTaskCancellationHandler {
                        do {
                            let localNetwork = try await blocking { try probe.value.run().localNetwork }
                            return (found: true, localNetwork: localNetwork)
                        } catch {
                            return (found: false, localNetwork: false)
                        }
                    } onCancel: {
                        probe.value.cancel()
                    }
                    // Keep the completed attempt even if it was cancelled on
                    // backgrounding, but never replace a newer retry's report.
                    guard probeGeneration == generation else { return }
                    probeDiagnostics = probe.value.diagnostics()
                    probeContext = context
                    probeInProgress = false
                    guard !Task.isCancelled else { return }
                    found = result.found
                    if found {
                        probeMessage = String(localized: "Developer-service TCP port reached. Pairing and tunnel setup are checked separately.")
                    } else if remotePairing.localNetworkDenied {
                        probeMessage = String(localized: "Local Network access is blocked. Allow Swipt in Settings › Privacy & Security › Local Network, then retry.")
                    } else {
                        probeMessage = String(localized: "Swipt couldn’t reach the developer service on the selected port. Check that your VPN is active, then retry or copy diagnostics.")
                    }
                    throughLocalNetwork = result.localNetwork
                    let failure = found ? nil : probeMessage
                    if let failure, failure != reportedFailure {
                        Self.logger.notice("Look for the tunnel: \(failure, privacy: .public)")
                    }
                    reportedFailure = failure
                }
                if tunnelFound != found {
                    tunnelFound = found
                }
                if tunnelThroughLocalNetwork != throughLocalNetwork {
                    tunnelThroughLocalNetwork = throughLocalNetwork
                }
                try? await Task.sleep(seconds: 2)
            }
        }
    }

    func stopProbing() {
        probeClients = max(0, probeClients - 1)
        remotePairing.stop()
        guard probeClients == 0 else { return }
        probeTask?.cancel()
        probeTask = nil
    }

    var diagnostics: String {
        let version = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "?"
        let build = Bundle.main.infoDictionary?["CFBundleVersion"] as? String ?? "?"
        let vpnStatus = tunnelFound.map { $0 ? "reachable" : "unreachable" } ?? "unknown"
        return """
        Swipt \(version) (\(build))
        OS: \(ProcessInfo.processInfo.operatingSystemVersionString)
        Foreground probing requested: \(probeClients > 0); probe in progress: \(probeInProgress)
        VPN setup result: \(vpnStatus)
        Pairing record present: \(pairing.isPaired); Automatic Unlock decided: \(autoUnlock.isDecided)
        Connection phase: \(phase); error code: \(connectionErrorCode)
        TCP reachability does not verify the VPN peer's identity or establish a paired tunnel.

        Latest foreground attempt context:
        \(probeContext)
        \(probeDiagnostics)

        Current discovery (may differ from the last attempt):
        \(remotePairing.diagnostics)

        Latest device-connection TCP probe (separate from foreground checks):
        \(device.probeDiagnostics())
        """
    }

    private func observePairing() {
        let record = withPerceptionTracking {
            pairing.record
        } onChange: { [weak self] in
            DispatchQueue.main.async {
                self?.remotePairing.refresh()
                self?.clearFailure()
                self?.observePairing()
            }
        }
        configure { try $0.setPairingRecord(record ?? Data()) }
    }

    private func observeAvailability() {
        let message = withPerceptionTracking {
            unavailableReason ?? ""
        } onChange: { [weak self] in
            DispatchQueue.main.async { self?.observeAvailability() }
        }
        configure { $0.setUnavailable(message) }
    }

    private var settings: LibSwiptDeviceSettings {
        let settings = LibSwiptDeviceSettings()
        settings.autoUnlockPasscode = autoUnlock.isEnabled && autoUnlock.problem == nil ? autoUnlock.passcode : ""
        settings.autoUnlockPasscodeType = autoUnlock.passcodeType.rawValue
        return settings
    }

    private func observeSettings() {
        let settings = withPerceptionTracking {
            self.settings
        } onChange: { [weak self] in
            DispatchQueue.main.async { self?.observeSettings() }
        }
        let box = UncheckedSendable(value: settings)
        configure { try $0.update(box.value) }
    }

    private func configure(_ change: @escaping @Sendable (LibSwiptDevice) throws -> Void) {
        let box = UncheckedSendable(value: device)
        Platform.queue.async {
            do {
                try change(box.value)
            } catch {
                Self.logger.error("Configure the device: \(error, privacy: .public)")
            }
        }
    }

    private func handle(_ change: DeviceStateChange) {
        // Export only the classified code, never arbitrary pairing messages.
        connectionErrorCode = change.errorCode
        switch change.state {
        case LibSwiptDeviceConnecting:
            phase = .connecting
        case LibSwiptDevicePreparing:
            phase = .preparing
        case LibSwiptDeviceConnected:
            phase = .running
        case LibSwiptDeviceFailed:
            phase = .failed
            if change.errorCode == LibSwiptDeviceErrorPairingRejected {
                pairing.clear()
            }
        default:
            phase = change.errorCode == LibSwiptDeviceErrorNone ? .idle : .failed
        }
    }
}
