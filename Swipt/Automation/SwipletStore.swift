import Foundation
import LibSwipt
import Perception
import UIKit
internal import SwiptHost
import UniformTypeIdentifiers
import os

extension UTType {
    nonisolated static let swiplet = UTType(exportedAs: "io.nekohasekai.swipt.swiplet", conformingTo: .zip)
}

@Perceptible
final class SwipletStore {
    static let shared = SwipletStore()
    nonisolated static let directory = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("Swiplets", isDirectory: true)
    nonisolated static let caches = AppDirectory.caches.appendingPathComponent("Swiplets", isDirectory: true)

    private(set) var ids: [String] = []
    private(set) var directories: [String: URL] = [:]
    private(set) var icons: [String: SwipletIcon] = [:]
    private(set) var titles: [String: String] = [:]
    private(set) var versions: [String: String] = [:]
    private(set) var interfaces: [String: SwipletEntry] = [:]
    private(set) var actions: [String: [SwipletAction]] = [:]
    private(set) var withServices: Set<String> = []
    private(set) var services: [String: SwipletService] = [:]
    private(set) var updatable: Set<String> = []
    private(set) var outdated: Set<String> = []
    private(set) var updates: [String: SwipletUpdate] = [:]
    private(set) var updating: Set<String> = []
    private(set) var repositories: [Repository] = []
    @PerceptionIgnored private var manifests: [String: LibSwiptSwipletManifest] = [:]
    @PerceptionIgnored private var repositoriesChecked: [String: Date] = [:]
    private var checked: [String: Date] = [:]
    @PerceptionIgnored private var incompatibleUpdates: [String: String] = [:]

    private init() {
        var stored: [Repository] = []
        do {
            stored = try SwipletDatabase.repositories()
        } catch {
            AppDirectory.logger.error("load repositories: \(error, privacy: .public)")
        }
        for repository in stored where repository.isOfficial && repository.url != Repository.official.url {
            removeRepository(repository.url)
        }
        repositories = [stored.first { $0.url == Repository.official.url } ?? .official] + stored.filter { !$0.isOfficial }
    }

    func reload() {
        let swiplets: [LibSwiptSwipletInfo]
        do {
            swiplets = try Platform.shared.swiplets()
        } catch {
            ErrorAlert.shared.report(String(localized: "Couldn’t Load the Swiplets"), error: error)
            return
        }
        manifests = Dictionary(uniqueKeysWithValues: swiplets.compactMap { swiplet in swiplet.manifest.map { (swiplet.id_, $0) } })
        directories = Dictionary(uniqueKeysWithValues: swiplets.map { ($0.id_, URL(fileURLWithPath: $0.directory, isDirectory: true)) })
        titles = manifests.mapValues { manifest in
            let title = Self.preferredName(manifest)
            return title.isEmpty ? manifest.id_ : title
        }
        versions = manifests.mapValues(\.version)
        outdated = Set(swiplets.filter(\.needsUpdate).map(\.id_))
        do {
            let stored = try SwipletDatabase.swipletIDs()
            let added = swiplets.map(\.id_).filter { !stored.contains($0) }
            ids = stored.filter { directories[$0] != nil } + added.sorted { (titles[$0] ?? $0).localizedStandardCompare(titles[$1] ?? $1) == .orderedAscending }
            if ids != stored {
                try SwipletDatabase.setSwipletIDs(ids)
            }
        } catch {
            AppDirectory.logger.error("load the swiplet order: \(error, privacy: .public)")
        }
        icons = directories.mapValues { SwipletIcon.load(from: $0) }
        interfaces = directories.filter { !outdated.contains($0.key) }.compactMapValues { directory in
            let entry = directory
                .appendingPathComponent(LibSwiptSwipletSourceFolder, isDirectory: true)
                .appendingPathComponent(LibSwiptSwipletInterfaceFile)
            guard let modified = try? entry.resourceValues(forKeys: [.contentModificationDateKey]).contentModificationDate else { return nil }
            return SwipletEntry(url: entry, modified: modified)
        }
        actions = manifests.mapValues { manifest in
            (0..<manifest.actionCount()).map { index in
                SwipletAction(
                    name: manifest.actionName(index), title: manifest.actionTitle(index), description: manifest.actionDescription(index),
                    shortcuts: manifest.actionExposed(index, caller: LibSwiptCallerShortcuts))
            }
        }
        withServices = Set(manifests.filter { $0.value.service && !outdated.contains($0.key) }.keys)
        services = Dictionary(uniqueKeysWithValues: withServices.map { ($0, Platform.shared.serviceState(of: $0)) })
        updatable = Set(manifests.filter { !$0.value.version.isEmpty && !$0.value.updateURL.isEmpty }.keys)
    }

    func reorder(_ ids: [String]) {
        self.ids = ids
        do {
            try SwipletDatabase.setSwipletIDs(ids)
        } catch {
            AppDirectory.logger.error("save the swiplet order: \(error, privacy: .public)")
        }
    }

    var hasEnabledServices: Bool {
        services.values.contains(where: \.enabled)
    }

    func service(of id: String) -> SwipletService {
        services[id] ?? SwipletService()
    }

    func dataDirectory(of id: String) -> URL? {
        directories[id]?.appendingPathComponent(LibSwiptSwipletDataFolder, isDirectory: true)
    }

    func assetsDirectory(of id: String) -> URL? {
        directories[id]?.appendingPathComponent(LibSwiptSwipletAssetsFolder, isDirectory: true)
    }

    nonisolated static func cacheDirectory(of id: String) -> URL {
        caches.appendingPathComponent(id, isDirectory: true)
    }

    func clearData(of id: String) async throws {
        try await Platform.shared.clearSwipletData(id)
    }

    func serviceChanged(_ id: String, to service: SwipletService) {
        services[id] = service
        SwiptHostModule.send("service", to: id, [
            "enabled": service.enabled, "status": service.status.rawValue, "error": service.error,
            "since": service.since.map { $0.timeIntervalSince1970 * 1000 } ?? 0,
        ])
    }

    func setServiceEnabled(_ id: String, _ enabled: Bool) async throws {
        try await Platform.shared.setServiceEnabled(id, enabled)
    }

    @available(iOS 17.4, *)
    func importSwiplet(from picked: URL) async throws {
        defer { reload() }
        let id = try await Platform.shared.installSwiplet(picked: picked)
        try SwipletDatabase.setETag("", of: id)
    }

    @available(iOS 17.4, *)
    func importSwiplet(downloadingFrom link: String) async throws {
        let link = link.trimmingCharacters(in: .whitespacesAndNewlines)
        let download = try await Self.download(link)
        defer {
            Self.discard(download.file)
            reload()
        }
        let id = try await Platform.shared.installSwiplet(download.file)
        try SwipletDatabase.setETag(download.updateURL == link ? download.etag : "", of: id)
    }

    @available(iOS 17.4, *)
    func open(_ picked: URL) async throws -> SwipletPreview {
        let file = try Self.temporaryFile()
        do {
            try await Task.detached(priority: .userInitiated) {
                try picked.withPickedFile { try FileManager.default.copyItem(at: $0, to: file) }
            }.value
            guard let manifest = try Self.manifest(of: file) else { throw MessageError(String(localized: "The file is not a swiplet.")) }
            let title = Self.preferredName(manifest)
            return SwipletPreview(
                file: file, id: manifest.id_, title: title.isEmpty ? manifest.id_ : title, version: manifest.version,
                icon: SwipletIcon(color: manifest.color, symbol: manifest.symbol))
        } catch {
            Self.discard(file)
            throw error
        }
    }

    @available(iOS 17.4, *)
    func importSwiplet(_ preview: SwipletPreview) async throws {
        defer {
            Self.discard(preview.file)
            reload()
        }
        let id = try await Platform.shared.installSwiplet(preview.file)
        try SwipletDatabase.setETag("", of: id)
    }

    func discard(_ preview: SwipletPreview) {
        Self.discard(preview.file)
    }

    func repository(_ url: String) -> Repository? {
        repositories.first { $0.url == url }
    }

    @available(iOS 17.4, *)
    func addRepository(_ link: String) async throws {
        let link = link.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !repositories.contains(where: { $0.url == link }), !Repository.isOfficial(link) else {
            throw MessageError(String(localized: "The repository is already added."))
        }
        let fetched = try await Self.fetch(link, etag: "", source: true)
        let index = try JSONDecoder().decode(SourceIndex.self, from: fetched.data)
        let image = await Self.image(index.image, relativeTo: link)
        let repository = Repository(
            url: link, name: index.name ?? Self.repositoryName(of: link), localizedNames: index.localizedNames, icon: index.icon, image: image,
            etag: fetched.etag, swiplets: index.swiplets)
        try SwipletDatabase.save(repository)
        repositories.append(repository)
        repositoriesChecked[link] = .now
    }

    func removeRepository(_ url: String) {
        repositories.removeAll { $0.url == url }
        repositoriesChecked[url] = nil
        do {
            try SwipletDatabase.removeRepository(url)
        } catch {
            AppDirectory.logger.error("remove repository \(url, privacy: .public): \(error, privacy: .public)")
        }
    }

    @available(iOS 17.4, *)
    func refreshRepository(_ url: String, force: Bool) async throws {
        guard let index = repositories.firstIndex(where: { $0.url == url }) else { return }
        if !force, let last = repositoriesChecked[url], Date.now.timeIntervalSince(last) < Self.updateInterval {
            return
        }
        let fetched = try await Self.fetch(url, etag: repositories[index].etag, source: true)
        repositoriesChecked[url] = .now
        guard !fetched.notModified else { return }
        let listed = try JSONDecoder().decode(SourceIndex.self, from: fetched.data)
        let image = await Self.image(listed.image, relativeTo: url)
        guard let current = repositories.firstIndex(where: { $0.url == url }) else { return }
        let repository = Repository(
            url: url, name: listed.name ?? Self.repositoryName(of: url), localizedNames: listed.localizedNames, icon: listed.icon, image: image,
            etag: fetched.etag, swiplets: listed.swiplets)
        try SwipletDatabase.save(repository)
        repositories[current] = repository
    }

    func hasNewer(_ swiplet: RepositorySwiplet) -> Bool {
        guard let local = manifests[swiplet.id] else { return false }
        if swiplet.versionCode > 0, local.versionCode > 0 {
            return swiplet.versionCode > local.versionCode
        }
        return !swiplet.version.isEmpty && swiplet.version != local.version
    }

    @available(iOS 17.4, *)
    func install(_ swiplet: RepositorySwiplet, from repository: Repository) async throws {
        updating.insert(swiplet.id)
        defer {
            updating.remove(swiplet.id)
            reload()
        }
        guard let link = URL(string: swiplet.url, relativeTo: URL(string: repository.url))?.absoluteString else {
            throw MessageError(String(localized: "The repository has no URL for the swiplet."))
        }
        let download = try await Self.download(link, sha256: swiplet.sha256)
        defer { Self.discard(download.file) }
        guard download.id == swiplet.id else { throw MessageError(String(localized: "The repository sent a different swiplet.")) }
        let id = try await Platform.shared.installSwiplet(download.file)
        try SwipletDatabase.setETag(download.etag, of: id)
        forgetUpdate(of: id)
    }

    @available(iOS 17.4, *)
    func checkForUpdates(of ids: [String]) async {
        for id in ids where updatable.contains(id) {
            if let last = checked[id], Date.now.timeIntervalSince(last) < Self.updateInterval {
                continue
            }
            _ = try? await checkForUpdate(of: id)
        }
    }

    @available(iOS 17.4, *)
    @discardableResult
    func checkForUpdate(of id: String) async throws -> Bool {
        checked[id] = .now
        guard let local = manifests[id], !local.version.isEmpty, !local.updateURL.isEmpty else { return false }
        let incompatible = incompatibleUpdates[id]
        let download = try await Self.download(
            local.updateURL, etag: incompatible ?? SwipletDatabase.etag(of: id), version: local.version, versionCode: local.versionCode)
        guard Self.isNewer(download, than: local) else {
            Self.discard(download.file)
            if !download.etag.isEmpty, download.etag != incompatible {
                try SwipletDatabase.setETag(download.etag, of: id)
            }
            return false
        }
        guard download.id == id else {
            Self.discard(download.file)
            throw MessageError(String(localized: "The update is a different swiplet."))
        }
        do {
            try await Platform.shared.checkSwipletInstall(download.file)
        } catch is SwiptOutdatedError, is IOSOutdatedError {
            Self.discard(download.file)
            incompatibleUpdates[id] = download.etag
            return false
        }
        incompatibleUpdates[id] = nil
        if let previous = updates[id] {
            Self.discard(previous.file)
        }
        updates[id] = SwipletUpdate(id: id, file: download.file, version: download.version, etag: download.etag)
        return true
    }

    @available(iOS 17.4, *)
    func applyUpdates(_ ids: [String]) async throws {
        let ids = ids.filter { !updating.contains($0) }
        updating.formUnion(ids)
        var failure: (any Error)?
        for id in ids {
            defer { updating.remove(id) }
            guard let update = updates[id] else { continue }
            do {
                _ = try await Platform.shared.installSwiplet(update.file)
                try SwipletDatabase.setETag(update.etag, of: id)
                Self.discard(update.file)
                updates[id] = nil
                reload()
            } catch {
                failure = failure ?? error
            }
        }
        if let failure {
            throw failure
        }
    }

    nonisolated private static let updateInterval: TimeInterval = 60 * 60

    nonisolated private static func repositoryName(of link: String) -> String {
        guard let components = URLComponents(string: link), let host = components.host else { return link }
        let path = (components.path as NSString).deletingLastPathComponent
        return path == "/" || path.isEmpty ? host : host + path
    }

    nonisolated private static func image(_ link: String?, relativeTo base: String) async -> Data? {
        guard let link, let url = URL(string: link, relativeTo: URL(string: base))?.absoluteString else { return nil }
        return try? await fetch(url, etag: "", source: false).data
    }

    nonisolated private static func fetch(_ link: String, etag: String, source: Bool) async throws -> Fetched {
        let downloader = UncheckedSendable(value: LibSwiptNewSwipletDownloader()!)
        return try await withTaskCancellationHandler {
            try await blocking {
                let fetched = downloader.value.fetch(link, etag: etag, asSource: source)!
                switch fetched.errorCode {
                case LibSwiptSwipletErrorNone:
                    return Fetched(etag: fetched.entityTag, notModified: fetched.notModified, data: fetched.data ?? Data())
                case LibSwiptSwipletErrorTimeout:
                    throw MessageError(String(localized: "The connection timed out."))
                default:
                    throw MessageError(fetched.errorMessage)
                }
            }
        } onCancel: {
            downloader.value.cancel()
        }
    }

    nonisolated private static func isNewer(_ download: SwipletDownload, than local: LibSwiptSwipletManifest) -> Bool {
        if download.versionCode > 0, local.versionCode > 0 {
            return download.versionCode > local.versionCode
        }
        return !download.version.isEmpty && download.version != local.version
    }

    nonisolated private static func download(_ link: String, etag: String = "", version: String = "", versionCode: Int64 = 0, sha256: String = "") async throws -> SwipletDownload {
        let file = try temporaryFile()
        let downloader = UncheckedSendable(value: LibSwiptNewSwipletDownloader()!)
        do {
            return try await withTaskCancellationHandler {
                try await blocking {
                    let download = downloader.value.download(link, destination: file.path, etag: etag, version: version, versionCode: versionCode, checksum: sha256)!
                    switch download.errorCode {
                    case LibSwiptSwipletErrorNone:
                        let declared = FileManager.default.fileExists(atPath: file.path) ? (try? manifest(of: file))?.updateURL ?? "" : ""
                        return SwipletDownload(
                            file: file, url: download.url, etag: download.entityTag, id: download.id_, version: download.version,
                            versionCode: download.versionCode, updateURL: declared)
                    case LibSwiptSwipletErrorTimeout:
                        throw MessageError(String(localized: "The connection timed out."))
                    default:
                        throw MessageError(download.errorMessage)
                    }
                }
            } onCancel: {
                downloader.value.cancel()
            }
        } catch {
            discard(file)
            throw error
        }
    }

    nonisolated private static func manifest(of file: URL) throws -> LibSwiptSwipletManifest? {
        var error: NSError?
        let manifest = LibSwiptReadSwipletManifest(file.path, &error)
        if let error { throw error }
        return manifest
    }

    nonisolated private static func discard(_ file: URL) {
        try? FileManager.default.removeItem(at: file.deletingLastPathComponent())
    }

    nonisolated private static func temporaryFile() throws -> URL {
        let folder = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        return folder.appendingPathComponent("swiplet")
    }

    @available(iOS 17.4, *)
    func export(_ id: String) async throws -> URL {
        let folder = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        let file = folder.appendingPathComponent(id).appendingPathExtension(for: .swiplet)
        try await Platform.shared.exportSwiplet(id, to: file)
        return file
    }

    nonisolated static func preferredName(_ name: String, localized: [String: String]) -> String {
        let languages = ["en"] + localized.keys.filter { $0 != "en" }.sorted()
        let preferred = Bundle.preferredLocalizations(from: languages, forPreferences: Locale.preferredLanguages).first ?? "en"
        return localized[preferred] ?? name
    }

    nonisolated private static func preferredName(_ names: LibSwiptSwipletManifest) -> String {
        var localized: [String: String] = [:]
        var name = ""
        for index in 0..<names.len() {
            let language = names.language(index)
            if language.isEmpty {
                name = names.name(index)
            } else {
                localized[language] = names.name(index)
            }
        }
        return preferredName(name, localized: localized)
    }

    private func forgetUpdate(of id: String) {
        if let update = updates.removeValue(forKey: id) {
            Self.discard(update.file)
        }
        checked[id] = nil
    }

    func remove(_ id: String) async throws {
        defer { reload() }
        forgetUpdate(of: id)
        try await Platform.shared.removeSwiplet(id)
    }
}

nonisolated struct SwipletEntry: Hashable, Sendable {
    let url: URL
    let modified: Date
}

nonisolated struct SwipletAction: Hashable, Sendable {
    let name: String
    let title: String
    let description: String
    let shortcuts: Bool
}

nonisolated struct SwipletUpdate: Sendable {
    let id: String
    let file: URL
    let version: String
    let etag: String
}

nonisolated struct SwipletPreview: Identifiable, Sendable {
    let file: URL
    let id: String
    let title: String
    let version: String
    let icon: SwipletIcon
}

nonisolated struct SwiptOutdatedError: LocalizedError, RecoverableError {
    var errorDescription: String? {
        String(localized: "This swiplet requires a newer version of Swipt.")
    }

    var recoveryOptions: [String] {
        [String(localized: "Learn More")]
    }

    func attemptRecovery(optionIndex: Int) -> Bool {
        Task { @MainActor in
            _ = await UIApplication.shared.open(URL(string: LibSwiptUpdateGuide)!)
        }
        return true
    }
}

nonisolated struct IOSOutdatedError: LocalizedError {
    let version: String
    let isPad: Bool

    var errorDescription: String? {
        isPad
            ? String(localized: "This swiplet requires iPadOS \(version) or later.")
            : String(localized: "This swiplet requires iOS \(version) or later.")
    }
}

nonisolated struct Repository: Identifiable, Sendable {
    private static let stableURL = "https://github.com/swipt-automation/official-swiplets/releases/latest/download/swiplets.json"
    private static let nightlyURL = "https://github.com/swipt-automation/official-swiplets/releases/download/nightly/swiplets.json"

    static let official = Repository(
        url: Bundle.main.object(forInfoDictionaryKey: "SwiptChannel") as? String == "stable" ? stableURL : nightlyURL,
        name: String(localized: "Official Swiplets"), localizedNames: nil, icon: RepositorySwiplet.Icon(color: nil, symbol: "checkmark.seal.fill"),
        image: nil, etag: "", swiplets: [])

    let url: String
    let name: String
    let localizedNames: [String: String]?
    let icon: RepositorySwiplet.Icon?
    let image: Data?
    let etag: String
    let swiplets: [RepositorySwiplet]

    var id: String { url }

    var isOfficial: Bool { Self.isOfficial(url) }

    static func isOfficial(_ url: String) -> Bool {
        url == stableURL || url == nightlyURL
    }

    var title: String {
        SwipletStore.preferredName(name, localized: localizedNames ?? [:])
    }

    var swipletIcon: SwipletIcon {
        var shown = icon.map { SwipletIcon(color: $0.color, symbol: $0.symbol) } ?? .default
        shown.image = image.flatMap(UIImage.init(data:))
        return shown
    }
}

nonisolated struct RepositorySwiplet: Decodable, Identifiable, Sendable {
    struct Icon: Decodable, Sendable {
        let color: String?
        let symbol: String
    }

    let id: String
    let name: String
    let localizedNames: [String: String]
    let description: String
    let version: String
    let versionCode: Int64
    let icon: Icon?
    let url: String
    let sha256: String

    private enum CodingKeys: String, CodingKey {
        case id, name, localizedNames, description, version, versionCode, icon, url, sha256
    }

    var title: String {
        SwipletStore.preferredName(name, localized: localizedNames)
    }

    var swipletIcon: SwipletIcon {
        icon.map { SwipletIcon(color: $0.color, symbol: $0.symbol) } ?? .default
    }

    init(
        id: String, name: String, localizedNames: [String: String], description: String, version: String, versionCode: Int64, icon: Icon?,
        url: String, sha256: String
    ) {
        self.id = id
        self.name = name
        self.localizedNames = localizedNames
        self.description = description
        self.version = version
        self.versionCode = versionCode
        self.icon = icon
        self.url = url
        self.sha256 = sha256
    }

    init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        id = try container.decode(String.self, forKey: .id)
        name = try container.decodeIfPresent(String.self, forKey: .name) ?? id
        localizedNames = try container.decodeIfPresent([String: String].self, forKey: .localizedNames) ?? [:]
        description = try container.decodeIfPresent(String.self, forKey: .description) ?? ""
        version = try container.decodeIfPresent(String.self, forKey: .version) ?? ""
        versionCode = try container.decodeIfPresent(Int64.self, forKey: .versionCode) ?? 0
        icon = try? container.decodeIfPresent(Icon.self, forKey: .icon)
        url = try container.decodeIfPresent(String.self, forKey: .url) ?? ""
        sha256 = try container.decodeIfPresent(String.self, forKey: .sha256) ?? ""
    }
}

nonisolated private struct SourceIndex: Decodable {
    let name: String?
    let localizedNames: [String: String]?
    let icon: RepositorySwiplet.Icon?
    let image: String?
    let swiplets: [RepositorySwiplet]
}

nonisolated private struct Fetched: Sendable {
    let etag: String
    let notModified: Bool
    let data: Data
}

nonisolated private struct SwipletDownload: Sendable {
    let file: URL
    let url: String
    let etag: String
    let id: String
    let version: String
    let versionCode: Int64
    let updateURL: String
}
