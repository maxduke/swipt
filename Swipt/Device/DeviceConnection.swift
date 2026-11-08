import Foundation
import LibSwipt
import Network
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
    private(set) var connectionErrorCode: Int32 = 0
    private(set) var tunnelFound: Bool?
    private(set) var tunnelThroughLocalNetwork = false
    private(set) var usesCellular = false

    let remotePairing = RemotePairingBrowser()
    let pairingSession: PairingSession
    @PerceptionIgnored private let device: LibSwiptDevice
    @PerceptionIgnored private var probeTask: Task<Void, Never>?
    @PerceptionIgnored private var pathMonitor: NWPathMonitor?
    @PerceptionIgnored private var probeClients = 0
    @PerceptionIgnored private var isBrowsing = false

    private var autoUnlock: AutoUnlockSettings { .shared }
    private var pairing: PairingStore { .shared }

    init(_ device: LibSwiptDevice, events: DeviceEvents) {
        self.device = device
        pairingSession = PairingSession(device: device)
        remotePairing.onPortChange = { [weak self] port in
            self?.device.setRemotePairingPort(Int32(port))
            self?.probe()
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
        remotePairing.start()
        probeClients += 1
        guard pathMonitor == nil else { return }
        let monitor = NWPathMonitor()
        monitor.pathUpdateHandler = { [weak self] path in
            let cellular = path.usesInterfaceType(.cellular)
            Task { @MainActor in
                guard let self else { return }
                if self.usesCellular != cellular {
                    self.usesCellular = cellular
                }
                self.probe()
            }
        }
        monitor.start(queue: .main)
        pathMonitor = monitor
    }

    private func probe() {
        guard pathMonitor != nil else { return }
        probeTask?.cancel()
        probeTask = Task {
            var reportedFailure: String?
            while !Task.isCancelled {
                let found: Bool
                let throughLocalNetwork: Bool
                if phase == .preparing || phase == .running {
                    found = true
                    throughLocalNetwork = device.loopbackAddress()?.localNetwork ?? false
                } else {
                    let port = remotePairing.port ?? 0
                    let probe = UncheckedSendable(value: LibSwiptNewLoopbackProbe(Int32(port))!)
                    let result = await withTaskCancellationHandler {
                        do {
                            let localNetwork = try await blocking { try probe.value.run().localNetwork }
                            return (found: true, localNetwork: localNetwork, failure: String?.none)
                        } catch {
                            return (found: false, localNetwork: false, failure: error.localizedDescription)
                        }
                    } onCancel: {
                        probe.value.cancel()
                    }
                    guard !Task.isCancelled else { return }
                    found = result.found
                    throughLocalNetwork = result.localNetwork
                    if let failure = result.failure, failure != reportedFailure {
                        Self.logger.notice("Look for the tunnel: \(failure, privacy: .public)")
                    }
                    reportedFailure = result.failure
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
        remotePairing.stop()
        probeClients = max(0, probeClients - 1)
        guard probeClients == 0 else { return }
        pathMonitor?.cancel()
        pathMonitor = nil
        probeTask?.cancel()
        probeTask = nil
    }

    var connectionDiagnostics: String {
        let version = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "?"
        let build = Bundle.main.infoDictionary?["CFBundleVersion"] as? String ?? "?"
        let vpnStatus = tunnelFound.map { $0 ? "reachable" : "unreachable" } ?? "unknown"
        return """
        Swipt \(version) (\(build))
        OS: \(ProcessInfo.processInfo.operatingSystemVersionString)
        Policy: documented peer first; original one-second stage deadlines
        Foreground VPN result: \(vpnStatus)
        Current Bonjour remote-pairing port: \(remotePairing.port ?? 49152) (\(remotePairing.port == nil ? "fallback" : "discovered"))
        Native connection phase: \(phase); error code: \(connectionErrorCode)
        Pairing record present: \(pairing.isPaired)
        TCP connectivity alone does not establish pairing or a developer tunnel.

        Latest actual native connection attempt:
        \(device.connectionDiagnostics())
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
