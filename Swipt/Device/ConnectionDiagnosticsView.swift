import SwiftUI
import UIKit

struct ConnectionDiagnosticsView: View {
    var isSheet = false

    @Environment(\.dismiss) private var dismiss
    @State private var report = ""
    @State private var copied = false

    private let device = Platform.shared.device

    var body: some View {
        ScrollView {
            Text(verbatim: report)
                .font(.system(.caption, design: .monospaced))
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding()
        }
        .navigationTitle("Connection Diagnostics")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .cancellationAction) {
                if isSheet {
                    Button("Done") { dismiss() }
                }
            }
            ToolbarItem(placement: .primaryAction) {
                Button(copied ? "Copied" : "Copy Diagnostics") {
                    UIPasteboard.general.string = report
                    copied = true
                }
            }
            ToolbarItem(placement: .bottomBar) {
                Button("Refresh Diagnostics") {
                    report = device.connectionDiagnostics
                    copied = false
                }
            }
        }
        .onAppear { report = device.connectionDiagnostics }
    }
}
