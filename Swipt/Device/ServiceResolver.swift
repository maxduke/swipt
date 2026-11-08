import Foundation
import dnssd
import os

final class ServiceResolver {
    nonisolated private static let logger = Logger(subsystem: Bundle.main.bundleIdentifier ?? "Swipt", category: "discovery")

    nonisolated struct Failure: LocalizedError {
        let code: DNSServiceErrorType

        var errorDescription: String? {
            "Bonjour: \(NSError(domain: NSOSStatusErrorDomain, code: Int(code)).localizedDescription)"
        }
    }

    private let name: String
    private let type: String
    private let resolved: (String, Int) -> Void
    private let addressesChanged: (Set<Data>) -> Void
    private let failure: ((DNSServiceErrorType) -> Void)?
    private var cancelled = false
    private var reference: DNSServiceRef?
    private var addressReference: DNSServiceRef?
    private var addressHost: String?
    private var addresses: Set<Data> = []

    init(name: String, type: String, resolved: @escaping (String, Int) -> Void, addresses: @escaping (Set<Data>) -> Void, failure: ((DNSServiceErrorType) -> Void)? = nil) {
        self.name = name
        self.type = type
        self.resolved = resolved
        addressesChanged = addresses
        self.failure = failure
    }

    func start() {
        guard !cancelled, reference == nil else { return }
        let context = Unmanaged.passUnretained(self).toOpaque()
        var serviceReference: DNSServiceRef?
        let error = DNSServiceResolve(
            &serviceReference, 0, 0, name, type, "local.",
            { _, _, _, errorCode, _, hostTarget, port, _, _, context in
                guard let context else { return }
                let resolver = Unmanaged<ServiceResolver>.fromOpaque(context).takeUnretainedValue()
                MainActor.assumeIsolated {
                    guard errorCode == kDNSServiceErr_NoError, let hostTarget else {
                        resolver.fail(errorCode == kDNSServiceErr_NoError ? DNSServiceErrorType(kDNSServiceErr_BadParam) : errorCode)
                        return
                    }
                    let qualifiedHostName = String(cString: hostTarget)
                    var hostName = qualifiedHostName
                    if hostName.hasSuffix(".") { hostName.removeLast() }
                    let hostPort = Int(UInt16(bigEndian: port))
                    resolver.lookUpAddresses(of: qualifiedHostName)
                    resolver.resolved(hostName, hostPort)
                }
            }, context)
        guard error == kDNSServiceErr_NoError, let serviceReference else {
            fail(error == kDNSServiceErr_NoError ? DNSServiceErrorType(kDNSServiceErr_BadReference) : error)
            return
        }
        reference = serviceReference
        let dispatchError = DNSServiceSetDispatchQueue(serviceReference, .main)
        if dispatchError != kDNSServiceErr_NoError {
            fail(dispatchError)
        }
    }

    private func lookUpAddresses(of host: String) {
        guard addressHost != host else { return }
        if let addressReference {
            DNSServiceRefDeallocate(addressReference)
        }
        addressReference = nil
        addressHost = host
        addresses = []
        let context = Unmanaged.passUnretained(self).toOpaque()
        var serviceReference: DNSServiceRef?
        let error = DNSServiceGetAddrInfo(
            &serviceReference, 0, 0, DNSServiceProtocol(kDNSServiceProtocol_IPv4 | kDNSServiceProtocol_IPv6), host,
            { _, flags, _, errorCode, _, address, _, context in
                guard let context else { return }
                let resolver = Unmanaged<ServiceResolver>.fromOpaque(context).takeUnretainedValue()
                guard errorCode == kDNSServiceErr_NoError else {
                    ServiceResolver.logger.error("Address query failed: \(errorCode)")
                    MainActor.assumeIsolated {
                        if let reference = resolver.addressReference { DNSServiceRefDeallocate(reference) }
                        resolver.addressReference = nil
                        resolver.addressHost = nil
                        resolver.addresses = []
                        resolver.addressesChanged([])
                    }
                    return
                }
                guard let address, let raw = ServiceResolver.rawAddress(address) else { return }
                let added = flags & DNSServiceFlags(kDNSServiceFlagsAdd) != 0
                let moreComing = flags & DNSServiceFlags(kDNSServiceFlagsMoreComing) != 0
                MainActor.assumeIsolated {
                    if added {
                        resolver.addresses.insert(raw)
                    } else {
                        resolver.addresses.remove(raw)
                    }
                    if !moreComing {
                        resolver.addressesChanged(resolver.addresses)
                    }
                }
            }, context)
        guard error == kDNSServiceErr_NoError, let serviceReference else {
            Self.logger.error("Address query failed: \(error)")
            return
        }
        addressReference = serviceReference
        let dispatchError = DNSServiceSetDispatchQueue(serviceReference, .main)
        if dispatchError != kDNSServiceErr_NoError {
            Self.logger.error("Address query dispatch failed: \(dispatchError)")
            DNSServiceRefDeallocate(serviceReference)
            addressReference = nil
        }
    }

    nonisolated static func rawAddress(_ address: UnsafePointer<sockaddr>) -> Data? {
        switch Int32(address.pointee.sa_family) {
        case AF_INET:
            return address.withMemoryRebound(to: sockaddr_in.self, capacity: 1) { pointer in
                withUnsafeBytes(of: pointer.pointee.sin_addr) { Data($0) }
            }
        case AF_INET6:
            return address.withMemoryRebound(to: sockaddr_in6.self, capacity: 1) { pointer in
                withUnsafeBytes(of: pointer.pointee.sin6_addr) { Data($0) }
            }
        default:
            return nil
        }
    }

    private func fail(_ code: DNSServiceErrorType) {
        guard !cancelled else { return }
        let error = Failure(code: code)
        Self.logger.error("Service resolution failed: \(error, privacy: .public)")
        cancel()
        failure?(code)
    }

    func cancel() {
        cancelled = true
        if let reference {
            DNSServiceRefDeallocate(reference)
        }
        reference = nil
        if let addressReference {
            DNSServiceRefDeallocate(addressReference)
        }
        addressReference = nil
    }
}
