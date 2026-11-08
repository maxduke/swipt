import Foundation
import LibSwipt
import Network
import dnssd

// The _remotepairing._tcp authTag authenticates the service identifier with the pairing record’s altIRK
// (idevice d32c818, idevice/src/remote_pairing/peer_device.rs:33-45, idevice/src/remote_pairing/peer_device.rs:129-140).
final class RemotePairingBrowser {
    static let serviceType = "_remotepairing._tcp"

    private(set) var port: Int?
    private(set) var status = "Not browsing"
    private(set) var localNetworkDenied = false
    var onPortChange: ((Int) -> Void)?

    var diagnostics: String {
        "Bonjour: \(status)\nLocal Network policy denial observed: \(localNetworkDenied)\nServices: \(services.count); resolved: \(services.values.filter { $0.port != nil }.count)\nPort source: \(port == nil ? "fallback (no current matching service)" : "current matching Bonjour service")\nSelected remote-pairing port: \(port ?? 49152)\n\(resolutionStatus)"
    }

    private var browser: NWBrowser?
    private var resolvers: [String: ServiceResolver] = [:]
    private var services: [String: Service] = [:]
    private var activeClients = 0
    private var restart: Task<Void, Never>?
    private var resolutionStatus = "No resolver error observed"

    private struct Service {
        var txt = NWTXTRecord()
        var port: Int?
        var addresses: Set<Data> = []
    }

    func start() {
        activeClients += 1
        guard browser == nil, restart == nil else { return }
        browse()
    }

    func stop() {
        activeClients = max(0, activeClients - 1)
        guard activeClients == 0 else { return }
        restart?.cancel()
        restart = nil
        cancelBrowsing()
    }

    func retryDiscovery() {
        guard activeClients > 0 else { return }
        restart?.cancel()
        restart = nil
        cancelBrowsing()
        browse()
    }

    private func browse() {
        status = "Starting"
        localNetworkDenied = false
        resolutionStatus = "No resolver error observed"
        let browser = NWBrowser(for: .bonjourWithTXTRecord(type: Self.serviceType, domain: "local."), using: .tcp)
        browser.stateUpdateHandler = { [weak self, weak browser] state in
            let source = browser
            Task { @MainActor in
                guard let self, self.isCurrent(source) else { return }
                self.handle(state)
            }
        }
        browser.browseResultsChangedHandler = { [weak self, weak browser] results, _ in
            var found: [String: NWTXTRecord] = [:]
            for result in results {
                guard case let .service(name, _, _, _) = result.endpoint else { continue }
                found[name] = if case let .bonjour(txt) = result.metadata { txt } else { NWTXTRecord() }
            }
            let source = browser
            Task { @MainActor in
                guard let self, self.isCurrent(source) else { return }
                self.update(found)
            }
        }
        browser.start(queue: .main)
        self.browser = browser
    }

    private func isCurrent(_ source: NWBrowser?) -> Bool {
        source != nil && source === browser
    }

    private func cancelBrowsing() {
        browser?.cancel()
        browser = nil
        resolvers.values.forEach { $0.cancel() }
        resolvers.removeAll()
        services.removeAll()
        status = "Stopped"
        setPort(nil)
    }

    private func handle(_ state: NWBrowser.State) {
        switch state {
        case .ready:
            status = "Ready (does not establish TCP reachability)"
            localNetworkDenied = false
        case .waiting(let error):
            status = "Waiting: \(error)"
            localNetworkDenied = Self.isPolicyDenied(error)
            setPort(nil)
        case .failed(let error):
            let failure = "Failed: \(error)"
            localNetworkDenied = Self.isPolicyDenied(error)
            retry()
            status = failure
        default:
            break
        }
    }

    private static func isPolicyDenied(_ error: NWError) -> Bool {
        if case .dns(let code) = error {
            return code == kDNSServiceErr_PolicyDenied
        }
        return false
    }

    private func retry() {
        cancelBrowsing()
        restart = Task {
            try? await Task.sleep(seconds: 2)
            guard !Task.isCancelled else { return }
            restart = nil
            browse()
        }
    }

    private func update(_ found: [String: NWTXTRecord]) {
        for name in resolvers.keys where found[name] == nil {
            resolvers[name]?.cancel()
            resolvers[name] = nil
            services[name] = nil
        }
        for (name, txt) in found {
            services[name, default: Service()].txt = txt
            guard resolvers[name] == nil else { continue }
            let resolver = ServiceResolver(name: name, type: Self.serviceType) { [weak self] _, port in
                self?.services[name]?.port = port
                self?.refresh()
            } addresses: { [weak self] addresses in
                self?.services[name]?.addresses = addresses
                self?.refresh()
            } failure: { [weak self] code in
                self?.resolutionStatus = "Service resolution error: \(code)"
                self?.localNetworkDenied = code == kDNSServiceErr_PolicyDenied
                self?.services[name]?.port = nil
                self?.refresh()
            }
            resolvers[name] = resolver
            resolver.start()
        }
        refresh()
    }

    func refresh() {
        let localAddresses = Self.localAddresses()
        let found = services
            .filter { _, service in
                service.port.map { (1...65535).contains($0) } == true
                    && (Self.isPairedDevice(service.txt) || service.addresses.contains { localAddresses.contains(Self.comparable($0)) })
            }
            .min { $0.key < $1.key }?.value.port
        setPort(found)
    }

    private func setPort(_ found: Int?) {
        guard found != port else { return }
        port = found
        onPortChange?(found ?? 0)
    }

    private static func isPairedDevice(_ txt: NWTXTRecord) -> Bool {
        let pairing = PairingStore.shared
        guard pairing.device?.canMatchService == true, let record = pairing.record,
            let identifier = txt["identifier"], let authTag = txt["authTag"]
        else { return false }
        return LibSwiptPairingRecordMatchesService(record, identifier, authTag)
    }

    private static func localAddresses() -> Set<Data> {
        var list: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&list) == 0, let first = list else { return [] }
        defer { freeifaddrs(list) }
        var result: Set<Data> = []
        for entry in sequence(first: first, next: { $0.pointee.ifa_next }) {
            guard Int32(entry.pointee.ifa_flags) & IFF_UP != 0, let address = entry.pointee.ifa_addr,
                let raw = ServiceResolver.rawAddress(UnsafePointer(address))
            else { continue }
            result.insert(comparable(raw))
        }
        return result
    }

    private static func comparable(_ address: Data) -> Data {
        guard address.count == 16, address[0] == 0xfe, address[1] & 0xc0 == 0x80 else { return address }
        var address = address
        address[2] = 0
        address[3] = 0
        return address
    }
}
