import SwiftUI
#if os(macOS)
import AppKit
#endif

struct SettingsView: View {
    private static let sourceCode = URL(string: "https://github.com/swipt-automation/swipt")!
    @State private var showsVPNDiagnostics = false

    var body: some View {
        Form {
            Section {
                NavigationLink {
                    AppSettingsView()
                } label: {
                    Label("App", systemImage: "app")
                }
                #if os(iOS)
                if Platform.isSupported {
                    NavigationLink {
                        BackgroundView()
                    } label: {
                        Label("Background Service", systemImage: "arrow.clockwise.circle")
                    }
                    NavigationLink {
                        AutoUnlockView()
                    } label: {
                        Label("Automatic Unlock", systemImage: "lock.open")
                    }
                    Button {
                        showsVPNDiagnostics = true
                    } label: {
                        Label("VPN Diagnostics", systemImage: "network")
                    }
                }
                #endif
            }
            Section("About") {
                Link(destination: Self.sourceCode) {
                    Label("Source Code", systemImage: "chevron.left.forwardslash.chevron.right")
                }
                .formRowButtonStyleCompat()
            }
        }
        #if os(macOS)
        .formStyle(.grouped)
        .unboundedFormWidth()
        #endif
        .navigationTitle("Settings")
        #if os(iOS)
        .sheet(isPresented: $showsVPNDiagnostics) {
            LoopbackDiagnosticsView()
        }
        #endif
    }
}

private struct AppSettingsView: View {
    @State private var language = AppLanguage.selected
    @State private var cacheSize: Int64?
    @State private var alert: AlertState?

    var body: some View {
        Form {
            Section {
                Picker(selection: $language) {
                    Text(verbatim: AppLanguage.systemDefaultName(in: language)).tag(String?.none)
                    ForEach(AppLanguage.available, id: \.self) { code in
                        Text(verbatim: AppLanguage.name(of: code)).tag(String?.some(code))
                    }
                } label: {
                    Label("Language", systemImage: "globe")
                }
                .onChangeCompat(of: language) { language in
                    AppLanguage.select(language)
                    #if os(macOS)
                    let actions = [
                        AlertState.Action(label: String(localized: "Later"), role: .cancel) {},
                        AlertState.Action(label: String(localized: "Restart"), role: nil, isDefault: true, perform: relaunch),
                    ]
                    #else
                    let actions = [AlertState.Action(label: String(localized: "OK"), role: .cancel, isDefault: true) {}]
                    #endif
                    alert = AlertState(
                        title: String(localized: "Restart Required"),
                        message: String(localized: "The new language takes effect after you restart the app."),
                        actions: actions
                    )
                }
            }
            Section("Cache") {
                LabeledContentCompat {
                    if let cacheSize {
                        Text(cacheSize.formatted(.byteCount(style: .file)))
                    }
                } label: {
                    Label("Cache Size", systemImage: "internaldrive")
                }
                if let cacheSize, cacheSize > 0 {
                    Button(role: .destructive) {
                        Task { await clearCache() }
                    } label: {
                        Label {
                            Text("Clear Cache")
                        } icon: {
                            Image(systemName: "trash")
                                .foregroundStyle(.red)
                        }
                    }
                    .formRowButtonStyleCompat()
                }
            }
            #if os(iOS)
            Section("Tips") {
                Button {} label: {
                    Label("Show Tips", systemImage: "lightbulb")
                }
                .disabled(true)
            }
            #endif
        }
        #if os(macOS)
        .formStyle(.grouped)
        .unboundedFormWidth()
        #else
        .navigationBarTitleDisplayMode(.inline)
        #endif
        .navigationTitle("App")
        .alert($alert)
        .task {
            cacheSize = await Task.detached(priority: .utility) { AppDirectory.size(of: AppDirectory.caches) }.value
        }
    }

    private func clearCache() async {
        cacheSize = nil
        do {
            try await Task.detached(priority: .userInitiated) { try AppDirectory.clearCaches() }.value
        } catch {
            alert = AlertState(String(localized: "Couldn’t Clear the Cache"), error: error)
        }
        cacheSize = await Task.detached(priority: .utility) { AppDirectory.size(of: AppDirectory.caches) }.value
    }

    #if os(macOS)
    private func relaunch() {
        Task {
            do {
                let configuration = NSWorkspace.OpenConfiguration()
                configuration.createsNewApplicationInstance = true
                _ = try await NSWorkspace.shared.openApplication(at: Bundle.main.bundleURL, configuration: configuration)
                NSApp.terminate(nil)
            } catch {
                alert = AlertState(String(localized: "Couldn’t Restart the App"), error: error)
            }
        }
    }
    #endif
}
