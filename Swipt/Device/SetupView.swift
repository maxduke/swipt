import Network
import Perception
import SwiftUI
import UIKit

enum SetupStep {
    case loopback
    case autoUnlock
}

struct SetupView: View {
    static var isNeeded: Bool {
        !PairingStore.shared.isPaired || Platform.shared.device.tunnelFound == false || !AutoUnlockSettings.shared.isDecided
    }

    @State private var path: [SetupStep] = []

    private let pairing = PairingStore.shared
    private let device = Platform.shared.device

    var body: some View {
        WithPerceptionTracking {
            NavigationStack(path: $path) {
                PairingSetupView { path.append(.loopback) }
                    .navigationDestination(for: SetupStep.self) { step in
                        Group {
                            switch step {
                            case .loopback:
                                LoopbackSetupView { path.append(.autoUnlock) }
                            case .autoUnlock:
                                AutoUnlockSetupView {}
                            }
                        }
                        .navigationBarBackButtonHidden(true)
                    }
            }
            .onAppear {
                if pairing.isPaired {
                    path = device.tunnelFound == false ? [.loopback] : [.autoUnlock]
                }
            }
            .onChangeCompat(of: pairing.isPaired) { paired in
                if !paired {
                    path = []
                }
            }
        }
    }
}

struct AutomatedSetupView: View {
    static let isRequested = ProcessInfo.processInfo.arguments.contains("--setup")

    let finished: () -> Void

    var body: some View {
        SetupPage("gearshape.2", Text("Setting Up Swipt")) {
            ProgressView()
                .controlSize(.large)
        } actions: {
        }
        .task {
            Task {
                await SetupPermissions.request()
                finished()
            }
            do {
                try await Task.sleep(nanoseconds: 60_000_000_000)
                finished()
            } catch {}
        }
    }
}

struct LoopbackSetupView: View {
    let next: () -> Void

    private static let localDevVPN = URL(string: "localdevvpn://")!
    private static let localDevVPNStore = URL(string: "https://apps.apple.com/app/id6755608044")!
    private static let singBox = URL(string: "sing-box://")!
    private static let singBoxStore = URL(string: "https://apps.apple.com/app/id6785326793")!
    private static let profile = Bundle.main.url(forResource: "LocalDevVPN", withExtension: "bpf")!

    @Environment(\.scenePhase) private var scenePhase
    @State private var hasLocalDevVPN = false
    @State private var hasSingBox = false
    @State private var usesCellular = false
    @State private var pathMonitor: NWPathMonitor?
    @State private var showsDiagnostics = false
    @State private var hasAdvanced = false

    private let device = Platform.shared.device

    var body: some View {
        WithPerceptionTracking {
            SetupPage(
                usesCellular ? "antenna.radiowaves.left.and.right.slash" : "network",
                usesCellular ? Text("Unsupported Network") : Text("Loopback VPN"),
                description: usesCellular
                    ? Text("Swipt only stops working on cellular data, due to Apple’s restrictions on developer services.")
                    : Text("Swipt needs a loopback VPN to connect to developer services. LocalDevVPN or sing-box is recommended.")
            ) {
                Text(device.probeMessage)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                if !usesCellular {
                    VStack(spacing: 24) {
                        Divider()
                        VStack(spacing: 12) {
                            if hasLocalDevVPN {
                                SetupPrimaryButton(title: Text("Open LocalDevVPN")) { UIApplication.shared.open(Self.localDevVPN) }
                            } else {
                                SetupPrimaryButton(title: Text("Get LocalDevVPN")) { UIApplication.shared.open(Self.localDevVPNStore) }
                            }
                        }
                        Divider()
                        VStack(spacing: 12) {
                            if hasSingBox {
                                SetupPrimaryButton(title: Text("Open sing-box")) { UIApplication.shared.open(Self.singBox) }
                                SetupSecondaryButton(title: Text("Import example profile into sing-box"), action: shareProfile)
                            } else {
                                SetupPrimaryButton(title: Text("Get sing-box")) { UIApplication.shared.open(Self.singBoxStore) }
                            }
                        }
                    }
                    .frame(maxWidth: 480)
                    .padding(.top, 8)
                }
            } actions: {
                HStack {
                    Button("Retry") { device.retryProbing() }
                    Spacer()
                    Button("Diagnostics") { showsDiagnostics = true }
                }
            }
            .sheet(isPresented: $showsDiagnostics) {
                LoopbackDiagnosticsView()
            }
            .onChangeCompat(of: scenePhase == .active, initial: true) { active in
                guard active else { return }
                hasLocalDevVPN = UIApplication.shared.canOpenURL(Self.localDevVPN)
                hasSingBox = UIApplication.shared.canOpenURL(Self.singBox)
            }
            .onChangeCompat(of: device.tunnelFound == true, initial: true) { found in
                guard found, !hasAdvanced else { return }
                hasAdvanced = true
                next()
            }
            .onAppear {
                let monitor = NWPathMonitor()
                monitor.pathUpdateHandler = { path in
                    let cellular = path.usesInterfaceType(.cellular)
                    Task { @MainActor in usesCellular = cellular }
                }
                monitor.start(queue: .main)
                pathMonitor = monitor
            }
            .onDisappear {
                pathMonitor?.cancel()
                pathMonitor = nil
            }
        }
    }

    private func shareProfile() {
        guard let top = topViewController() else { return }
        let controller = UIActivityViewController(activityItems: [Self.profile], applicationActivities: nil)
        if let popover = controller.popoverPresentationController {
            popover.sourceView = top.view
            popover.sourceRect = CGRect(origin: CGPoint(x: top.view.bounds.midX, y: top.view.bounds.midY), size: .zero)
            popover.permittedArrowDirections = []
        }
        top.present(controller, animated: true)
    }
}

struct LoopbackDiagnosticsView: View {
    @Environment(\.dismiss) private var dismiss
    @State private var report = ""
    @State private var copied = false

    private let device = Platform.shared.device

    var body: some View {
        NavigationStackCompat {
            ScrollView {
                Text(verbatim: report)
                    .font(.system(.caption, design: .monospaced))
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding()
            }
            .navigationTitle("VPN Diagnostics")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Done") { dismiss() }
                }
                ToolbarItem(placement: .primaryAction) {
                    Button(copied ? "Copied" : "Copy Diagnostics") {
                        UIPasteboard.general.string = report
                        copied = true
                    }
                }
                ToolbarItem(placement: .bottomBar) {
                    Button("Refresh Diagnostics") {
                        report = device.diagnostics
                        copied = false
                    }
                }
            }
        }
        .onAppear { report = device.diagnostics }
    }
}
