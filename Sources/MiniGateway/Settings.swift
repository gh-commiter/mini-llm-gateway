import AppKit
import ApplicationServices
import RouterCore
import ServiceManagement
import SwiftUI

/// Nastavitve po vzoru Sistemskih nastavitev: stranska vrstica z iskanjem, desno skupine s stikali.
/// Spremembe se uveljavijo same (brez gumba Save), kot v macOS.
struct SettingsView: View {
    @EnvironmentObject var state: AppState
    @State private var draft = RouterConfig()
    @State private var loaded = false
    @State private var keys: [String: String] = [:]
    @State private var pane: Pane? = .general
    @State private var search = ""
    @State private var claudeStatus = (ok: true, text: "")
    @State private var axTrusted = AXIsProcessTrusted()
    @State private var cliStatus = ""
    @State private var cliTools: [String] = []
    @State private var selectedRoot: String?
    @State private var selectedDevice: String?
    @State private var codexModels: [Codex.Model] = []

    enum Pane: Hashable {
        case general, provider(String), siri, appleFM, claudeCLI, codex
    }

    private struct Item: Identifiable {
        let pane: Pane
        let title: String
        let symbol: String
        let color: Color
        var id: Pane { pane }
    }

    private var items: [Item] {
        [Item(pane: .general, title: String(localized: "General"), symbol: "gearshape.fill", color: .gray)]
            + draft.providers.map { p in
                Item(pane: .provider(p.id), title: p.name,
                     symbol: p.id == "openrouter" ? "point.3.connected.trianglepath.dotted" : "server.rack",
                     color: p.id == "openrouter" ? .purple : .blue)
            }
            + [Item(pane: .siri, title: "Siri AI", symbol: "waveform", color: .pink),
               Item(pane: .appleFM, title: "Apple Foundation Models", symbol: "apple.intelligence", color: .orange),
               Item(pane: .claudeCLI, title: "Claude CLI", symbol: "terminal.fill", color: .brown),
               Item(pane: .codex, title: "Codex CLI", symbol: "chevron.left.forwardslash.chevron.right", color: .teal)]
    }

    var body: some View {
        NavigationSplitView {
            List(selection: $pane) {
                ForEach(items.filter { search.isEmpty || $0.title.localizedCaseInsensitiveContains(search) }) { item in
                    Label { Text(verbatim: item.title) } icon: { IconTile(symbol: item.symbol, color: item.color) }
                        .tag(item.pane)
                }
            }
            .searchable(text: $search, placement: .sidebar)
            .navigationSplitViewColumnWidth(min: 200, ideal: 220, max: 280)
        } detail: {
            detail
                .formStyle(.grouped)
                .navigationTitle(items.first { $0.pane == pane }?.title ?? "")
        }
        .frame(minWidth: 760, idealWidth: 820, minHeight: 520, idealHeight: 640)
        .onAppear {
            if !loaded { draft = state.config; loaded = true }
        }
        // Nove naprave doda aplikacija sama (ob prvi povezavi): prenesi jih v osnutek, da jih uveljavitev ne izbriše.
        .onChange(of: state.config.devices) { _, devices in
            draft.devices += devices.filter { d in !draft.devices.contains { $0.id == d.id } }
        }
        // Samodejna uveljavitev: 0,8 s po zadnji spremembi.
        .task(id: draft) {
            guard loaded, draft != state.config else { return }
            try? await Task.sleep(for: .milliseconds(800))
            guard !Task.isCancelled, !(draft.claudeCLI.enabled && !claudeStatus.ok) else { return }
            state.apply(draft)
        }
        .task(id: keys) {
            guard keys.values.contains(where: { !$0.isEmpty }) else { return }
            try? await Task.sleep(for: .milliseconds(800))
            guard !Task.isCancelled else { return }
            // Keychain le ob spremembi: privzetega ali enakega ključa ne zapisujemo.
            let changed = keys.filter { id, k in !k.isEmpty && k != state.keychain.get(id) }
            guard !changed.isEmpty else { return }
            for (id, k) in changed { state.keychain.set(id, k) }
            state.restart()
        }
        .task {
            while !Task.isCancelled {  // dovoljenje se spreminja v Sistemskih nastavitvah
                axTrusted = AXIsProcessTrusted()
                try? await Task.sleep(for: .seconds(2))
            }
        }
        .task(id: draft.claudeCLI.executable) {
            try? await Task.sleep(for: .milliseconds(400))
            guard !Task.isCancelled else { return }
            claudeStatus = Self.check(draft.claudeCLI.executable)
        }
        .onChange(of: draft.siri.enabled) { _, on in
            // Ob vklopu preveri dovoljenje; če ga ni, macOS pokaže svoj poziv.
            if on { axTrusted = AXIsProcessTrustedWithOptions([kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary) }
        }
    }

    @ViewBuilder private var detail: some View {
        switch pane ?? .general {
        case .general: general
        case .provider(let id):
            if let i = draft.providers.firstIndex(where: { $0.id == id }) {
                ProviderPane(provider: $draft.providers[i], keyInput: keyBinding(id), savedKey: state.keychain.get(id) ?? "")
            }
        case .siri: siri
        case .appleFM: appleFM
        case .claudeCLI: claudeCLI
        case .codex: codex
        }
    }

    private var general: some View {
        Form {
            Section {
                LoginItemRow()
            }
            Section {
                TextField("Address", text: $draft.host)
                TextField("Port", value: $draft.port, format: .number.grouping(.never))
                TextField("Allowed clients (IP; empty = all)", text: $draft.allowedClients)
                Toggle("Allow unencrypted connections from other Macs", isOn: $draft.allowPlainRemote)
                    .help(String(localized: "minigw 0.4 and newer always encrypts. Turn on only for other clients on a trusted network."))
            } footer: {
                Text("Changes are applied automatically.").font(.caption).foregroundStyle(.secondary)
            }
            Section("Token") {
                TokenRow()
            }
        }
    }

    private var siri: some View {
        Form {
            Section {
                Toggle("Enabled", isOn: $draft.siri.enabled)
                TextField("Timeout (s)", value: $draft.siri.timeout, format: .number)
                    .disabled(!draft.siri.enabled)
            } footer: {
                Label {
                    Text("Siri answers with the personal context of this Mac's user (notes, documents, messages that Apple Intelligence can use). Other Macs get it only when you allow them below. Siri cannot read your project files.")
                } icon: {
                    Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                }
                .font(.caption)
            }
            Section {
                ForEach($draft.devices) { $d in
                    Toggle(isOn: $d.siri) {
                        Text(verbatim: d.name)
                        Text(state.deviceSeen[d.id].map { String(localized: "Last seen \($0.formatted(date: .omitted, time: .shortened))") }
                             ?? String(localized: "Not seen since Mini Gateway started"))
                    }
                    .contentShape(Rectangle())
                    .listRowBackground(selectedDevice == d.id ? Color.accentColor.opacity(0.2) : nil)
                    .onTapGesture { selectedDevice = selectedDevice == d.id ? nil : d.id }
                }
                HStack(spacing: 0) {
                    Button { draft.devices.removeAll { $0.id == selectedDevice }; selectedDevice = nil } label: {
                        Image(systemName: "minus").frame(width: 22, height: 18)
                    }
                    .disabled(selectedDevice == nil).accessibilityLabel("Remove")
                    Spacer()
                }
                .buttonStyle(.borderless)
            } header: {
                Text("Devices")
            } footer: {
                Text("Macs that connected with minigw (0.4.4 or newer). The switch allows Siri AI for that Mac. This Mac always has it. A removed device appears again when it connects, with Siri off.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Section {
                LabeledContent {
                    EqualWidthHStack {
                        WideButton("Open Accessibility Settings") {
                            NSWorkspace.shared.open(URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility")!)
                        }
                        if !axTrusted {
                            // macOS dovoljenje včasih upošteva šele po ponovnem zagonu (kot »Quit & Reopen« v sistemu).
                            WideButton("Quit & Reopen") { state.relaunch() }
                        }
                    }
                } label: {
                    Label {
                        Text(axTrusted ? "Accessibility: allowed" : "Accessibility: not allowed")
                    } icon: {
                        Image(systemName: axTrusted ? "checkmark.circle.fill" : "exclamationmark.triangle.fill")
                            .foregroundStyle(axTrusted ? Color.green : Color.orange)
                    }
                }
            }
        }
    }

    private var appleFM: some View {
        Form {
            Section {
                Toggle("Enabled", isOn: $draft.appleFM.enabled)
            }
            Section("Instructions") {
                TextEditor(text: $draft.appleFM.instructions)
                    .frame(minHeight: 60)
            }
        }
    }

    /// Dovoljene mape: skupne za Claude CLI in Codex.
    private var foldersSection: some View {
        Section {
            ForEach(roots, id: \.self) { root in
                Label { Text(verbatim: root) } icon: { Image(systemName: "folder") }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .contentShape(Rectangle())
                    .listRowBackground(selectedRoot == root ? Color.accentColor.opacity(0.2) : nil)
                    .onTapGesture { selectedRoot = selectedRoot == root ? nil : root }
            }
            HStack(spacing: 0) {
                Button { addRoot() } label: { Image(systemName: "plus").frame(width: 22, height: 18) }
                    .help("Add Folder…").accessibilityLabel("Add Folder…")
                Divider().frame(height: 16)
                Button { removeRoot() } label: { Image(systemName: "minus").frame(width: 22, height: 18) }
                    .disabled(selectedRoot == nil).accessibilityLabel("Remove")
                Spacer()
            }
            .buttonStyle(.borderless)
        } header: {
            Text("Allowed folders")
        } footer: {
            Text("Agents work only in these folders. Folders shared from other Macs (minigw) are included automatically.")
                .font(.caption).foregroundStyle(.secondary)
        }
    }

    private var codex: some View {
        Form {
            Section {
                Toggle("Enabled", isOn: $draft.codex.enabled)
                LabeledContent("Executable (empty = auto-detect)") {
                    TextField("", text: $draft.codex.executable, prompt: Text(verbatim: Codex.detected ?? "codex"))
                        .labelsHidden()
                }
            }
            Section {
                ForEach(codexModels, id: \.slug) { m in
                    Toggle(isOn: Binding(get: { draft.codex.isModelEnabled(m.slug) }, set: { draft.codex.setModel(m.slug, $0, all: codexModels.map(\.slug)) })) {
                        Text(verbatim: m.name)
                        Text(verbatim: m.slug + (m.window.map { " · \($0 / 1000)K" } ?? ""))
                    }
                }
            } header: {
                HStack {
                    Text("Models")
                    Spacer()
                    EqualWidthHStack {
                        WideButton("Enable All") { draft.codex.enabledModels = nil }
                        WideButton("Disable All") { draft.codex.enabledModels = [] }
                    }
                    .disabled(codexModels.isEmpty)
                }
                .controlSize(.small)
            }
            .task(id: draft.codex.executable) {
                let exe = draft.codex.executable
                codexModels = await Task.detached { Codex.executable(exe).map(Codex.catalog) ?? [] }.value
            }
            foldersSection
            Section {
                Toggle("Allow writing in the session's folder", isOn: $draft.codex.write)
                Toggle("Allow network access for commands", isOn: $draft.codex.network)
                    .disabled(!draft.codex.write)
            } header: {
                Text("Sandbox")
            } footer: {
                Text("The claude-codex models (one per Codex model of your account) run “codex exec” as an agent in the session's folder, also in folders shared from other Macs. Its sandbox writes only to that folder and the allowed folders from Claude CLI; reads are not limited. Sign in once with “codex login”.")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private var claudeCLI: some View {
        Form {
            Section {
                Toggle("Enabled", isOn: $draft.claudeCLI.enabled)
            }
            Section {
                LabeledContent("Executable (empty = auto-detect)") {
                    HStack {
                        TextField("", text: $draft.claudeCLI.executable, prompt: Text(verbatim: ClaudeCLI.detected ?? "claude"))
                            .labelsHidden()
                        Button("Choose…") { chooseClaude() }
                    }
                }
            } footer: {
                Label {
                    Text(verbatim: claudeStatus.text)
                } icon: {
                    Image(systemName: claudeStatus.ok ? "checkmark.circle.fill" : "xmark.octagon.fill")
                        .foregroundStyle(claudeStatus.ok ? Color.green : Color.red)
                }
                .font(.caption)
            }
            foldersSection
            Section {
                Toggle("Allow local tools", isOn: $draft.claudeCLI.allowLocalTools)
                    .help(String(localized: "Agents get every Claude Code tool on this Mac, but files only in the allowed folders: commands run in a sandbox that cannot read your home folder or write outside these folders."))
                if !draft.claudeCLI.allowLocalTools {
                    TextField("Tools (comma-separated; empty = none)", text: $draft.claudeCLI.tools)
                    Picker("Permission mode", selection: $draft.claudeCLI.permissionMode) {
                        ForEach(["dontAsk", "acceptEdits", "auto", "plan"], id: \.self) { Text(verbatim: $0) }
                    }
                }
                TextField("Setting sources", text: $draft.claudeCLI.settingSources)
            }
            if draft.claudeCLI.allowLocalTools {
                Section {
                    ForEach(cliTools, id: \.self) { t in
                        Toggle(isOn: Binding(get: { !draft.claudeCLI.disabledTools.contains(t) },
                                             set: { on in
                                                 draft.claudeCLI.disabledTools.removeAll { $0 == t }
                                                 if !on { draft.claudeCLI.disabledTools.append(t) }
                                             })) { Text(verbatim: t) }
                    }
                } header: {
                    HStack {
                        Text("Tools")
                        Spacer()
                        EqualWidthHStack {
                            WideButton("Enable All") { draft.claudeCLI.disabledTools = [] }
                            WideButton("Disable All") { draft.claudeCLI.disabledTools = cliTools }
                        }
                        .disabled(cliTools.isEmpty)
                    }
                    .controlSize(.small)
                }
                .task(id: draft.claudeCLI.executable) {
                    // Izjeme, ki jih ta različica Claude Code ne pozna, ostanejo v nastavitvah in so tudi prikazane.
                    let found = await ClaudeCLI.availableTools(draft.claudeCLI.executable)
                    cliTools = Array(Set(found + draft.claudeCLI.disabledTools)).sorted()
                }
            }
            Section {
                LabeledContent {
                    EqualWidthHStack {
                        WideButton("Download…") { downloadCLI() }
                        WideButton(CLITool.installedPath() == nil ? "Install" : "Reinstall") { installCLI() }
                    }
                } label: {
                    Text(verbatim: CLITool.name).font(.system(.body, design: .monospaced))
                    Text(verbatim: cliStatus.isEmpty ? (CLITool.installedPath().map { String(localized: "Installed: \($0)") } ?? String(localized: "Not installed")) : cliStatus)
                }
            } header: {
                Text("Command-line tool")
            } footer: {
                Text("Starts Claude Code through this router, e.g. “minigw --model claude-apple-fm”. Also: minigw connect | status | models | disconnect. Install links the signed command into your PATH and connects it; Download saves a copy for another Mac.")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private func installCLI() {
        let (dir, inPATH) = CLITool.installDirectory()
        do {
            _ = try CLITool.install(in: dir, url: CLITool.routerURL(state.config, forThisMac: true), token: state.token)
            let path = "\(dir)/\(CLITool.name)"
            cliStatus = inPATH ? String(localized: "Installed: \(path)") : String(localized: "Installed: \(path) – add \(dir) to your PATH")
        } catch {
            cliStatus = String(localized: "Install failed: \(error.localizedDescription)")
        }
    }

    private func downloadCLI() {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = CLITool.name
        panel.message = String(localized: "On the other Mac run: minigw connect \(CLITool.routerURL(state.config, forThisMac: false)) --token <token>")
        guard panel.runModal() == .OK, let url = panel.url else { return }
        do {
            try CLITool.export(to: url)
            cliStatus = String(localized: "Saved: \(url.path)")
        } catch {
            cliStatus = String(localized: "Save failed: \(error.localizedDescription)")
        }
    }

    /// Prazno = samodejno najdena pot; sicer ročno vpisana ali izbrana.
    static func check(_ configured: String) -> (ok: Bool, text: String) {
        guard let path = configured.isEmpty ? ClaudeCLI.detected : configured else {
            return (false, String(localized: "claude not found in PATH"))
        }
        switch ClaudeCLI.validate(path) {
        case .success(let version):
            return (true, configured.isEmpty ? String(localized: "Detected: \(path) · \(version)") : version)
        case .failure(let e):
            return (false, String(localized: "Invalid: \(e.message)"))
        }
    }

    /// Dovoljene mape (shranjene ločeno z vejico, kot doslej).
    private var roots: [String] {
        draft.claudeCLI.allowedRoots.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
    }

    private func addRoot() {
        let panel = NSOpenPanel()
        panel.canChooseFiles = false
        panel.canChooseDirectories = true
        panel.allowsMultipleSelection = true
        panel.prompt = String(localized: "Add")
        guard panel.runModal() == .OK else { return }
        let added = panel.urls.map { ($0.path as NSString).abbreviatingWithTildeInPath }.filter { !roots.contains($0) }
        draft.claudeCLI.allowedRoots = (roots + added).joined(separator: ",")
    }

    private func removeRoot() {
        draft.claudeCLI.allowedRoots = roots.filter { $0 != selectedRoot }.joined(separator: ",")
        selectedRoot = nil
    }

    private func chooseClaude() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = false
        panel.allowsMultipleSelection = false
        panel.showsHiddenFiles = true
        panel.treatsFilePackagesAsDirectories = true
        panel.prompt = String(localized: "Choose")
        panel.message = String(localized: "Select the Claude Code executable")
        if let current = ClaudeCLI.detected { panel.directoryURL = URL(fileURLWithPath: current).deletingLastPathComponent() }
        let validator = ClaudePanelValidator()
        panel.delegate = validator
        if panel.runModal() == .OK, let url = panel.url { draft.claudeCLI.executable = url.path }
        _ = validator  // delegat je weak; drži ga do konca okna
    }

    private func keyBinding(_ id: String) -> Binding<String> {
        Binding(get: { keys[id, default: ""] }, set: { keys[id] = $0 })
    }
}

/// Gumb, čigar ozadje zapolni širino, ki mu jo da EqualWidthHStack.
struct WideButton: View {
    let title: LocalizedStringKey
    let action: () -> Void

    init(_ title: LocalizedStringKey, action: @escaping () -> Void) {
        self.title = title
        self.action = action
    }

    var body: some View {
        Button(action: action) { Text(title).frame(maxWidth: .infinity) }
    }
}

/// Vrstica, v kateri so vsi elementi široki kot najširši (gumbi v isti vrstici enako široki).
struct EqualWidthHStack: Layout {
    var spacing: CGFloat = 8

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let sizes = subviews.map { $0.sizeThatFits(.unspecified) }
        let width = sizes.map(\.width).max() ?? 0
        return CGSize(width: width * CGFloat(subviews.count) + spacing * CGFloat(max(subviews.count - 1, 0)),
                      height: sizes.map(\.height).max() ?? 0)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        let width = subviews.map { $0.sizeThatFits(.unspecified).width }.max() ?? 0
        var x = bounds.minX
        for s in subviews {
            s.place(at: CGPoint(x: x, y: bounds.midY), anchor: .leading, proposal: ProposedViewSize(width: width, height: nil))
            x += width + spacing
        }
    }
}

/// Zagon ob prijavi (SMAppService; vidno tudi v Sistemskih nastavitvah → Splošno → Elementi ob prijavi).
/// Privzeto izklopljeno: vklopi ga uporabnik.
struct LoginItemRow: View {
    @State private var status = SMAppService.mainApp.status
    @State private var failure = ""

    var body: some View {
        Toggle("Open at Login", isOn: Binding(get: { status == .enabled || status == .requiresApproval }, set: set))
        if status == .requiresApproval {
            LabeledContent {
                Button("Open Login Items") { SMAppService.openSystemSettingsLoginItems() }
            } label: {
                Text("Waiting for approval in System Settings → General → Login Items.")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
        if !failure.isEmpty {
            Text(verbatim: failure).font(.caption).foregroundStyle(.orange)
        }
    }

    private func set(_ on: Bool) {
        do {
            if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
            failure = ""
        } catch {
            failure = String(localized: "Could not change: \(error.localizedDescription)")
        }
        status = SMAppService.mainApp.status
    }
}

/// Ikona v barvni zaobljeni ploščici, kot v Sistemskih nastavitvah.
struct IconTile: View {
    let symbol: String
    let color: Color

    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: 11, weight: .semibold))
            .foregroundStyle(.white)
            .frame(width: 20, height: 20)
            .background(color.gradient, in: RoundedRectangle(cornerRadius: 5, style: .continuous))
    }
}

struct TokenRow: View {
    @EnvironmentObject var state: AppState
    @State private var show = false

    var body: some View {
        LabeledContent {
            EqualWidthHStack {
                WideButton(show ? "Hide" : "Show") { show.toggle() }
                WideButton("Copy Token") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(state.connectToken, forType: .string)
                }
                WideButton("New Token") { state.regenerateToken() }
            }
        } label: {
            Text(verbatim: show ? state.token : String(repeating: "•", count: 24))
                .font(.system(.body, design: .monospaced))
                .textSelection(.enabled)
                .lineLimit(1)
                .truncationMode(.middle)
        }
    }
}

/// Ponudnik: povezava in modeli, ki jih ponuja (vsak s stikalom za objavo v /model).
struct ProviderPane: View {
    @Binding var provider: ProviderConfig
    @Binding var keyInput: String
    let savedKey: String
    @State private var models: [Discovery.Model] = []
    @State private var phase = Phase.idle
    @State private var failure = ""
    @State private var search = ""
    @State private var reload = 0

    enum Phase { case idle, loading, failed, loaded }

    private var key: String { keyInput.isEmpty ? savedKey : keyInput }

    /// Zadetki iskanja, omogočeni najprej.
    private var filtered: [Discovery.Model] {
        let hits = search.isEmpty ? models : models.filter { $0.id.localizedCaseInsensitiveContains(search) }
        return hits.filter { provider.isModelEnabled($0.id) } + hits.filter { !provider.isModelEnabled($0.id) }
    }

    var body: some View {
        Form {
            Section {
                // Brez ključa ponudnik ne more delovati → stikalo izklopljeno in onemogočeno.
                Toggle("Enabled", isOn: Binding(get: { provider.enabled && !key.isEmpty }, set: { provider.enabled = $0 }))
                    .disabled(key.isEmpty)
                    .help(key.isEmpty ? String(localized: "Enter an API key to enable") : "")
                TextField("Base URL", text: $provider.baseURL)
                SecureField(savedKey.isEmpty ? "API key"
                            : savedKey == Keychain.defaultKeys[provider.id] ? "API key (default; enter to replace)"
                            : "API key (saved; enter to replace)", text: $keyInput)
            }
            Section {
                if models.count > 5 {
                    TextField("Search models", text: $search, prompt: Text("Search models"))
                        .labelsHidden()
                }
                ForEach(filtered) { m in
                    Toggle(isOn: enabled(m.id)) {
                        Text(verbatim: m.id)
                        if let w = m.window { Text(verbatim: "\(w / 1000)K") }
                    }
                }
            } header: {
                HStack {
                    Text("Models")
                    Spacer()
                    status.font(.caption).fontWeight(.regular)
                    EqualWidthHStack {
                        WideButton("Enable All") { provider.enabledModels = nil }
                        WideButton("Disable All") { provider.enabledModels = [] }
                    }
                    .disabled(models.isEmpty)
                    Button { reload += 1 } label: { Image(systemName: "arrow.clockwise") }
                        .help("Refresh").disabled(!provider.enabled)
                }
                .controlSize(.small)
            }
            Section {
                DisclosureGroup("Aliases (advanced)") {
                    Text("One per line “alias = upstream ID, context window” (e.g. qwen = qwen/qwen3-coder, 128k). When set, these define the models instead of the table.")
                        .font(.caption).foregroundStyle(.secondary)
                    TextEditor(text: $provider.aliases)
                        .font(.system(.body, design: .monospaced))
                        .frame(minHeight: 60)
                }
            }
        }
        .task(id: "\(provider.enabled)|\(provider.baseURL)|\(key)|\(reload)") {
            guard provider.enabled, !key.isEmpty else { models = []; phase = .idle; return }
            phase = .loading
            try? await Task.sleep(for: .milliseconds(400))  // počakaj, da uporabnik neha tipkati
            guard !Task.isCancelled else { return }
            switch await Discovery.load(provider, key: key) {
            case .success(let found):
                models = found.sorted { $0.id < $1.id }
                phase = .loaded
            case .failure(let f):
                models = []
                failure = f.message
                phase = .failed
            }
        }
    }

    @ViewBuilder private var status: some View {
        switch phase {
        case .loading:
            ProgressView().controlSize(.mini)
        case .failed:
            Label { Text(verbatim: failure) } icon: { Image(systemName: "exclamationmark.triangle") }
                .foregroundStyle(.orange)
                .lineLimit(2)
                .help(failure)
        case .loaded:
            Text("\(models.filter { provider.isModelEnabled($0.id) }.count) of \(models.count) enabled")
                .foregroundStyle(.secondary)
        case .idle:
            Text("Enable the provider to load its models").foregroundStyle(.secondary)
        }
    }

    /// »Enable All« = vsi, tudi prihodnji (nil); posamezno stikalo = izrecen seznam.
    private func enabled(_ id: String) -> Binding<Bool> {
        Binding(get: { provider.isModelEnabled(id) }, set: { on in
            var list = provider.enabledModels ?? models.map(\.id)
            list.removeAll { $0 == id }
            if on { list.append(id) }
            provider.enabledModels = list
        })
    }
}
