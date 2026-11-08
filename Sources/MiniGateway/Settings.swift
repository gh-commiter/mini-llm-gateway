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
    @State private var base = RouterConfig()  // nastavitve, na katerih temelji osnutek (za tristransko združitev)
    @State private var keys: [String: String] = [:]
    @State private var pane: Pane? = .general
    @State private var back: [Pane] = []      // zgodovina za puščici (kot System Settings)
    @State private var forward: [Pane] = []
    @State private var navigating = false     // sprememba s puščico: ne zapiši v zgodovino
    @State private var search = ""
    @State private var claudeStatus = (ok: true, text: "")
    @State private var axTrusted = AXIsProcessTrusted()
    @State private var cliStatus = ""
    @State private var cliTools: [String] = []
    @State private var selectedRoot: String?
    @State private var removingDevice: DeviceConfig?
    @State private var clearingSessions = false
    @State private var allowNames: [String: String] = [:]  // ime čakajoče naprave, preden jo dovoliš

    static func lastSeen(_ id: String) -> String {
        Router.history.lastSeen(id).map { String(localized: "Last seen \($0.formatted(date: .abbreviated, time: .shortened))") }
            ?? String(localized: "Never connected")
    }
    @State private var codexModels: [Codex.Model] = []
    @State private var askCopilotCommands = false
    @State private var grokModels: [String] = []
    @State private var foundryModels: [Foundry.Model] = []
    @State private var foundryStatus = ""
    @State private var foundryFound: String?  // samodejno najdena pot (iskanje v .task, ne ob vsakem izrisu)

    enum Pane: Hashable {
        case general, provider(String), siri, appleFM, foundry, claudeCLI, codex, copilot, grok, devices, sessions
    }

    private struct Item: Identifiable {
        let pane: Pane
        let title: String
        let symbol: String
        let color: Color
        var about = ""
        var id: Pane { pane }
    }

    private var items: [Item] {
        [Item(pane: .general, title: String(localized: "General"), symbol: "gearshape.fill", color: .gray,
              about: String(localized: "Address, port and access token of the gateway, and the app version."))]
            + draft.providers.map { p in
                Item(pane: .provider(p.id), title: p.name,
                     symbol: p.baseURL.contains("openrouter.ai") ? "point.3.connected.trianglepath.dotted" : "server.rack",
                     color: p.baseURL.contains("openrouter.ai") ? .purple : .blue,
                     about: String(localized: "Models of an Anthropic-compatible provider, passed through to Claude Code."))
            }
            + [Item(pane: .siri, title: "Siri AI", symbol: "waveform", color: .pink,
                    about: String(localized: "The Siri AI app as a model, driven in the background.")),
               Item(pane: .appleFM, title: "Apple Foundation Models", symbol: "apple.intelligence", color: .orange,
                    about: String(localized: "Apple's on-device model: private, without a network.")),
               Item(pane: .foundry, title: "Foundry Local", symbol: "cpu", color: .blue,
                    about: String(localized: "Microsoft Foundry Local: models on this Mac, without the cloud.")),
               Item(pane: .claudeCLI, title: "Claude CLI", symbol: "terminal.fill", color: .brown,
                    about: String(localized: "Your local Claude Code as an agent model, with tools in the allowed folders.")),
               Item(pane: .codex, title: "Codex CLI", symbol: "chevron.left.forwardslash.chevron.right", color: .teal,
                    about: String(localized: "OpenAI Codex CLI as an agent model in the session's folder.")),
               Item(pane: .copilot, title: "Copilot CLI", symbol: "person.2.wave.2", color: .gray,
                    about: String(localized: "GitHub Copilot CLI as an agent model in the session's folder.")),
               Item(pane: .grok, title: "Grok CLI", symbol: "sparkle", color: .indigo,
                    about: String(localized: "xAI Grok CLI as an agent model in the session's folder.")),
               Item(pane: .devices, title: String(localized: "Devices"), symbol: "laptopcomputer.and.iphone", color: .blue,
                    about: String(localized: "Computers that may use this gateway. Compare a waiting device's ID with the one on the client before you allow it.")),
               Item(pane: .sessions, title: String(localized: "Sessions"), symbol: "clock.arrow.circlepath", color: .green,
                    about: String(localized: "Recent client sessions: folder, work mode and use per model. Prompts and answers are never stored."))]
    }

    @State private var removing = false
    static let builtIn = RouterConfig.builtInProviders  // privzeta prehoda: le izklop, ne odstranitev
    static let startURLs: Set<String> = ["http://localhost:4000", "https://openrouter.ai/api", "https://"]

    /// Izbrani prehod ima kaj nastavljenega: ključ, vklop, svoj naslov ali aliase.
    private var configured: Bool {
        guard let p = selectedProvider else { return false }
        return p.enabled || !p.aliases.isEmpty || !Self.startURLs.contains(p.baseURL)
            || !(keys[p.id] ?? "").isEmpty || state.keychain.lookup(p.id).value?.isEmpty == false
    }

    private var selectedProvider: ProviderConfig? {
        if case .provider(let id)? = pane { return draft.providers.first { $0.id == id } }
        return nil
    }

    enum GatewayKind { case litellm, openrouter, other }

    private func kind(_ p: ProviderConfig) -> GatewayKind {
        p.baseURL.contains("openrouter.ai") ? .openrouter
            : p.id == "litellm" || p.name.hasPrefix("LiteLLM") ? .litellm : .other
    }

    /// Nov prehod (več LiteLLM, OpenRouter ali drugih, vsak s svojim ključem in modeli), takoj za zadnjim iste vrste.
    private func addProvider(_ k: GatewayKind, _ base: String, _ url: String) {
        var name = base, n = 2
        while draft.providers.contains(where: { $0.name == name }) { name = "\(base) \(n)"; n += 1 }
        let p = ProviderConfig(id: "gw-" + UUID().uuidString.prefix(8).lowercased(), name: name, enabled: false,
                               baseURL: url, aliases: "", enabledModels: k == .openrouter ? [] : nil)
        let at = draft.providers.lastIndex { kind($0) == k }.map { $0 + 1 } ?? draft.providers.count
        draft.providers.insert(p, at: at)
        pane = .provider(p.id)
    }

    private func removeProvider() {
        guard let p = selectedProvider, !Self.builtIn.contains(p.id) else { return }
        draft.providers.removeAll { $0.id == p.id }
        keys[p.id] = nil
        state.keychain.delete(p.id)
        back.removeAll { $0 == .provider(p.id) }; forward.removeAll { $0 == .provider(p.id) }
        navigating = true
        pane = .general
    }

    /// Puščica: prejšnji panel iz `from`, trenutni v `to`.
    private func go(_ from: inout [Pane], _ to: inout [Pane]) {
        guard let target = from.popLast() else { return }
        if let pane { to.append(pane) }
        navigating = true
        pane = target
    }

    private struct PendingApply: Equatable { let draft: RouterConfig; let claudeOK: Bool }

    /// Uveljavi osnutek (skupaj z morebitnimi spremembami od drugod); neveljaven vklopljen claude ga zadrži.
    private func applyDraft() {
        guard loaded, draft != state.config, !(draft.claudeCLI.enabled && !claudeStatus.ok) else { return }
        // Tik pred uveljavitvijo še enkrat združi: sprememba skrbniškega API v istem koraku glavne niti ostane.
        let merged = RouterConfig.merge(draft: draft, old: base, new: state.config)
        state.apply(merged)
        base = merged
        draft = merged
    }

    var body: some View {
        NavigationSplitView {
            List(selection: $pane) {
                ForEach(items.filter { search.isEmpty || $0.title.localizedCaseInsensitiveContains(search) || $0.about.localizedCaseInsensitiveContains(search) }) { item in
                    Label { Text(verbatim: item.title) } icon: { IconTile(symbol: item.symbol, color: item.color) }
                        .tag(item.pane)
                }
            }
            .safeAreaInset(edge: .bottom) {
                HStack(spacing: 0) {
                    Menu {
                        Button("LiteLLM") { addProvider(.litellm, "LiteLLM", "http://localhost:4000") }
                        Button("OpenRouter") { addProvider(.openrouter, "OpenRouter", "https://openrouter.ai/api") }
                        Button("Other Anthropic-compatible provider") { addProvider(.other, String(localized: "Provider"), "https://") }
                    } label: { Image(systemName: "plus").frame(width: 22, height: 18) }
                        .menuIndicator(.hidden).fixedSize()
                        .help(String(localized: "Add Provider")).accessibilityLabel(String(localized: "Add Provider"))
                    Divider().frame(height: 16)
                    // Nenastavljen (pravkar dodan) prehod izgine brez vprašanja; nastavljen šele po potrditvi.
                    Button { if configured { removing = true } else { removeProvider() } } label: {
                        Image(systemName: "minus").frame(width: 22, height: 18)
                    }
                        .disabled(selectedProvider.map { Self.builtIn.contains($0.id) } ?? true)
                        .help(selectedProvider.map { Self.builtIn.contains($0.id) } == true
                              ? String(localized: "The built-in providers cannot be removed; turn them off instead.") : "")
                        .accessibilityLabel(String(localized: "Remove"))
                    Spacer()
                }
                .buttonStyle(.borderless)
                .padding(.horizontal, 12).padding(.vertical, 8)
            }
            .confirmationDialog(String(localized: "Remove \(selectedProvider?.name ?? "")?"), isPresented: $removing) {
                Button("Remove", role: .destructive) { removeProvider() }
            } message: {
                if selectedProvider?.enabled == true {
                    Text("This provider is on: Claude Code sessions that use its models stop working. Its settings and API key are deleted from this Mac.")
                } else {
                    Text("Its settings and API key are deleted from this Mac.")
                }
            }
            .frame(minWidth: 220)  // v običajnem oknu SwiftUI navigationSplitViewColumnWidth prezre
            .navigationSplitViewColumnWidth(min: 220, ideal: 230, max: 280)
            .searchable(text: $search, placement: .sidebar)
            .toolbar(removing: .sidebarToggle)  // System Settings nima gumba za stransko vrstico
        } detail: {
            detail
                .formStyle(.grouped)
                .navigationTitle(items.first { $0.pane == pane }?.title ?? "")
                .toolbar(removing: .title)
                .toolbar {
                    ToolbarItemGroup(placement: .navigation) {
                        Button { go(&back, &forward) } label: { Image(systemName: "chevron.left") }
                            .disabled(back.isEmpty).help(String(localized: "Back"))
                        Button { go(&forward, &back) } label: { Image(systemName: "chevron.right") }
                            .disabled(forward.isEmpty).help(String(localized: "Forward"))
                    }
                }
        }
        .onChange(of: pane) { old, _ in
            if navigating { navigating = false } else if let old { back.append(old); forward = [] }
        }
        .frame(minWidth: 760, idealWidth: 820, minHeight: 520, idealHeight: 640)
        .onAppear {
            if !loaded { draft = state.config; base = state.config; loaded = true; return }
            // Okno je bilo skrito, ko se je nastavitev spremenila drugje: prikaz mora spet ustrezati dejanski nastavitvi.
            draft = RouterConfig.merge(draft: draft, old: base, new: state.config)
            base = state.config
        }
        // Zaprtje okna v 0,8 s po spremembi ne sme izgubiti osnutka (task bi se preklical, prikaz pa bi kazal neuveljavljeno).
        .onDisappear { applyDraft() }
        // Spremembe od drugod (minigw router, nova naprava): tristranska združitev z osnutkom (tudi odstranitve), da
        // samodejna uveljavitev osnutka ne povozi sprememb skrbniškega API in ne zavrže tipkanja.
        .onChange(of: state.config) { old, new in
            guard loaded else { return }
            draft = RouterConfig.merge(draft: draft, old: base, new: new)
            base = new
        }
        // Samodejna uveljavitev: 0,8 s po zadnji spremembi.
        // claudeStatus.ok v id: osnutek, ki je čakal na veljaven claude, se uveljavi, ko preverjanje uspe.
        .task(id: PendingApply(draft: draft, claudeOK: claudeStatus.ok)) {
            guard loaded, draft != state.config else { return }
            try? await Task.sleep(for: .milliseconds(800))
            guard !Task.isCancelled else { return }
            applyDraft()
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

    /// Kartica na vrhu panela kot v System Settings: velika ikona, naslov, opis.
    private var header: some View {
        let item = items.first { $0.pane == pane } ?? items[0]
        return PaneHeader(symbol: item.symbol, color: item.color, title: item.title, about: item.about)
    }

    @ViewBuilder private var detail: some View {
        switch pane ?? .general {
        case .general: general
        case .provider(let id):
            if let i = draft.providers.firstIndex(where: { $0.id == id }) {
                ProviderPane(header: PaneHeader(symbol: items.first { $0.pane == pane }?.symbol ?? "server.rack",
                                                color: items.first { $0.pane == pane }?.color ?? .blue,
                                                title: draft.providers[i].name,
                                                about: items.first { $0.pane == pane }?.about ?? ""),
                             provider: $draft.providers[i], keyInput: keyBinding(id), savedKey: state.keychain.get(id) ?? "")
            }
        case .siri: siri
        case .appleFM: appleFM
        case .claudeCLI: claudeCLI
        case .codex: codex
        case .copilot: copilot
        case .grok: grok
        case .foundry: foundry
        case .devices: devices
        case .sessions: sessions
        }
    }

    private var general: some View {
        Form {
            header
            Section {
                LoginItemRow()
            }
            Section {
                TextField("Address", text: $draft.host)
                TextField("Port", value: $draft.port, format: .number.grouping(.never))
            } footer: {
                Text("Changes are applied automatically.").font(.caption).foregroundStyle(.secondary)
            }
            Section("Token") {
                TokenRow()
            }
            Section {
                Toggle("Allow this user's apps without a token", isOn: $draft.allowLocalApps)
            } footer: {
                Text("Programs of this user on this computer may use the models and agents without the token (they still send an Anthropic header, so web pages cannot). Other users, other computers, the gateway's own agents, apps in a web view and sandboxed apps (on the Mac also commands in Claude Code's sandbox) need the token. Docker and other containers, VMs and tunnels (ssh -R, ngrok…) running as you also count as this user's apps; with this on they can use agents in your allowed folders without a token.").font(.caption).foregroundStyle(.secondary)
            }
            ScratchSection(draft: $draft)
            Section {
                Toggle("Network for builds", isOn: $draft.worktreeNetwork)
                Toggle("Network for Codex builds", isOn: $draft.worktreeCodexNetwork)
                    .help(String(localized: "Codex can read all files of this Mac's user, so network for its builds is off by default."))
            } header: {
                Text("Worktree mode")
            } footer: {
                Text("Agents in worktree mode (minigw --minigw-work=worktree) work in a git clone on this Mac: Claude CLI with Bash in Claude Code's sandbox, Codex in its own sandbox (Grok is not supported yet). They write only to the clone, their own Maven repository and temporary folders. These switches let their builds download (Maven, SwiftPM); Codex can read all your files, so its network is off by default.").font(.caption).foregroundStyle(.secondary)
            }
            Section {
                TextField("Allowed commands", text: $draft.runCommands)
                ForEach($draft.devices) { $d in
                    Toggle(isOn: $d.run) { Text(verbatim: d.name) }
                    Toggle("Network", isOn: $d.runNetwork).disabled(!d.run).padding(.leading, 20)
                }
            } header: {
                Text(verbatim: "minigw run")
            } footer: {
                Label {
                    Text("minigw run is outside the agent sandbox: it runs the project's build with your full rights. Macs switched on here may run these commands in a folder they use from this Mac's SMB share (native mode). A sandbox reduces what the build can do (it writes only to that folder and the build caches, and your keys and app data are hidden), but it is not a security boundary: allow only Macs you would trust with your user account. Network: the build may download (Maven).")
                } icon: {
                    Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                }
                .font(.caption)
            }
            Section("About") {
                LabeledContent("Version") { Text(verbatim: "\(MenuView.version) (\(MenuView.build))").textSelection(.enabled) }
                LabeledContent("License") {
                    Link("MIT License", destination: URL(string: "https://github.com/gh-commiter/mini-llm-gateway/blob/main/LICENSE")!)
                }
            }
        }
    }

    private var siri: some View {
        Form {
            header
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
                    Toggle(isOn: $d.siri) { Text(verbatim: d.name) }
                }
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

    /// Naprave, enako kot podokno Devices na strani nastavitev minigw serve: dovoljenje za vsako napravo z žetonom in
    /// naslovi odjemalcev, naprave, ki čakajo na odobritev (Allow, Reject), dovoljene naprave (Copy ID, Remove).
    private var devices: some View {
        Form {
            header
            Section {
                // Le pri praznem seznamu (kot na strani nastavitev in v skrbniškem API)
                Toggle(isOn: Binding(get: { draft.allowAnyDevice && draft.devices.isEmpty }, set: { draft.allowAnyDevice = $0 })) {
                    Text("Allow any device with the token")
                    Text("Only while the list of allowed devices is empty. Otherwise each new device waits for approval.")
                }
                .disabled(!draft.devices.isEmpty)
                TextField("Allowed clients (IP; empty = all)", text: $draft.allowedClients, prompt: Text(verbatim: "192.168.1.20, 192.168.1.21"))
                Toggle("Allow unencrypted connections from other Macs", isOn: $draft.allowPlainRemote)
                    .help(String(localized: "minigw 0.4 and newer always encrypts. Turn on only for other clients on a trusted network."))
            }
            Section("Waiting for approval") {
                ForEach(state.pending) { p in
                    LabeledContent {
                        EqualWidthHStack {
                            WideButton("Allow") { state.allow(p, name: allowNames[p.id]) }.buttonStyle(.borderedProminent)
                            WideButton("Reject") { state.reject(p) }
                        }
                    } label: {
                        TextField("Name", text: Binding(get: { allowNames[p.id] ?? p.name }, set: { allowNames[p.id] = $0 }))
                            .labelsHidden()
                        Text(verbatim: p.id).font(.caption2.monospaced()).lineLimit(2).textSelection(.enabled)  // celoten ID (43 znakov) za primerjavo
                        Text(verbatim: ([p.address, p.time.formatted(date: .abbreviated, time: .shortened)] + (p.client.isEmpty ? [] : [p.client]))
                            .joined(separator: " · "))
                        if state.nameTaken(p) {
                            Text("Same name as an allowed device: compare the ID.").foregroundStyle(.orange)
                        }
                    }
                    .listRowBackground(Color.yellow.opacity(0.14))  // kot poudarjena skupina na strani nastavitev
                }
                if state.pending.isEmpty { Text("No device is waiting.").foregroundStyle(.secondary) }
            }
            Section {
                TimelineView(.periodic(from: .now, by: 15)) { _ in  // zadnji stik iz Router.deviceLog
                    ForEach($draft.devices) { $d in
                        LabeledContent {
                            EqualWidthHStack {
                                WideButton("Copy ID") {
                                    NSPasteboard.general.clearContents()
                                    NSPasteboard.general.setString(d.id, forType: .string)
                                }
                                WideButton("Remove") { removingDevice = d }
                            }
                        } label: {
                            DeviceNameField(name: $d.name)
                            Text(verbatim: d.id).font(.caption.monospaced()).lineLimit(1).truncationMode(.middle).textSelection(.enabled)
                            Text(([Self.lastSeen(d.id)] + [Router.history.client(d.id)].compactMap { $0 }).joined(separator: " · "))
                        }
                        DisclosureGroup("History") {  // priklopi naprave (kot razširjena vrstica na strani nastavitev)
                            let h = Router.history.history(d.id)
                            ForEach(Array(h.enumerated()), id: \.offset) { _, c in
                                Text(verbatim: ([c.time.formatted(date: .abbreviated, time: .shortened), c.address] + (c.client.isEmpty ? [] : [c.client]))
                                    .joined(separator: " · ")).foregroundStyle(.secondary)
                            }
                            if h.isEmpty { Text("No connections recorded.").foregroundStyle(.secondary) }
                        }
                    }
                }
                if draft.devices.isEmpty { Text("None").foregroundStyle(.secondary) }
            } header: {
                Text("Allowed devices")
            } footer: {
                Text("A removed device has to be allowed again when it connects. Siri AI and minigw run are switched on per device in their panes.")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
        .alert(Text(String(localized: "Remove \(removingDevice.map { $0.name.isEmpty ? $0.id : $0.name } ?? "")?")),
               isPresented: Binding(get: { removingDevice != nil }, set: { if !$0 { removingDevice = nil } })) {
            Button("Remove", role: .destructive) {
                if let d = removingDevice { draft.devices.removeAll { $0.id == d.id } }
                removingDevice = nil
            }
            Button("Cancel", role: .cancel) { removingDevice = nil }
        } message: {
            Text("It can no longer use this gateway until you allow it again.")
        }
    }

    /// Seje, enako kot podokno Sessions na strani nastavitev minigw serve: seznam (čas, naprava, mapa, način, modeli),
    /// razkritje pokaže števce po modelih.
    private var sessions: some View {
        Form {
            header
            Section {
                TimelineView(.periodic(from: .now, by: 15)) { _ in
                    let list = Router.history.sessions()
                    ForEach(list) { s in
                        DisclosureGroup {
                            Text(String(localized: "Started \(s.start.formatted(date: .abbreviated, time: .shortened)) · \(Int((s.last.timeIntervalSince(s.start) / 60).rounded())) min")
                                 + (s.branch.isEmpty ? "" : " · " + String(localized: "Branch \(s.branch)"))
                                 + (s.role.isEmpty ? "" : " · " + String(localized: "Role \(s.role)")))
                                .foregroundStyle(.secondary)
                            ForEach(Array(s.models.enumerated()), id: \.offset) { _, u in
                                LabeledContent {
                                    Text("\(u.requests) requests · \(u.errors) errors · \(u.refused) refused · \(Int(u.seconds.rounded())) s · tokens \(u.inputTokens) in, \(u.outputTokens) out")
                                } label: {
                                    Text(verbatim: u.model)
                                    let sub = [u.role.isEmpty ? nil : String(localized: "Role \(u.role)"), u.via.isEmpty ? nil : String(localized: "via \(u.via)"),
                                               u.readOnly ? String(localized: "read-only") : nil, u.tools.isEmpty ? nil : String(localized: "tools \(u.tools)")]
                                        .compactMap { $0 }.joined(separator: " · ")
                                    if !sub.isEmpty { Text(verbatim: sub) }
                                }
                            }
                        } label: {
                            Text(verbatim: s.last.formatted(date: .abbreviated, time: .shortened) + " · " + Self.who(s))
                            Text(verbatim: ([s.folder, s.mode + (s.readOnly ? ", " + String(localized: "read-only") : ""),
                                            Set(s.models.map(\.model)).sorted().joined(separator: ", ")].filter { !$0.isEmpty }).joined(separator: " · "))
                                .foregroundStyle(.secondary)
                        }
                    }
                    if list.isEmpty { Text("No sessions recorded.").foregroundStyle(.secondary) }
                }
            } header: {
                HStack {
                    Text("Sessions")
                    Spacer()
                    Button("Clear History") { clearingSessions = true }
                }
            } footer: {
                Text("The last 200 sessions and the last 20 connections of each allowed device are kept on this computer.")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
        .alert(Text("Clear the session history?"), isPresented: $clearingSessions) {
            Button("Clear History", role: .destructive) { Router.history.clearSessions() }
            Button("Cancel", role: .cancel) {}
        }
    }

    static func who(_ s: GatewayHistory.Session) -> String {
        guard !s.device.isEmpty else { return String(localized: "This computer") }
        let name = s.deviceName.isEmpty ? s.device : s.deviceName
        return s.deviceRemoved ? String(localized: "\(name) (removed)") : name
    }

    private var appleFM: some View {
        Form {
            header
            Section {
                Toggle("Enabled", isOn: $draft.appleFM.enabled)
            }
            Section("Instructions") {
                TextEditor(text: $draft.appleFM.instructions)
                    .frame(minHeight: 60)
            }
        }
    }

    private var foundry: some View {
        Form {
            header
            Section {
                Toggle("Enabled", isOn: $draft.foundry.enabled)
                LabeledContent("Executable (empty = auto-detect)") {
                    TextField("", text: $draft.foundry.executable, prompt: Text(verbatim: foundryFound ?? "foundry"))
                        .labelsHidden()
                }
            } footer: {
                Text(verbatim: foundryStatus).font(.caption).foregroundStyle(.secondary)
            }
            Section {
                ForEach(foundryModels, id: \.name) { m in
                    Toggle(isOn: Binding(get: { draft.foundry.isModelEnabled(m.name) },
                                         set: { draft.foundry.setModel(m.name, $0, all: foundryModels.map(\.name)) })) {
                        Text(verbatim: m.name)
                        Text(m.vision ? "Text and images" : "Text").font(.caption).foregroundStyle(.secondary)
                    }
                }
            } header: {
                HStack {
                    Text("Models")
                    Spacer()
                    EqualWidthHStack {
                        WideButton("Enable All") { draft.foundry.enabledModels = nil }
                        WideButton("Disable All") { draft.foundry.enabledModels = [] }
                    }
                    .disabled(foundryModels.isEmpty)
                }
                .controlSize(.small)
            }
            .task(id: draft.foundry.executable) {
                foundryFound = await Foundry.executable("")
                let exe = await Foundry.executable(draft.foundry.executable)
                guard exe != nil else {
                    foundryModels = []
                    foundryStatus = String(localized: "Foundry Local is not installed: brew tap microsoft/foundrylocal && brew install foundrylocal; then download a model, e.g. foundry model download phi-4-mini.")
                    return
                }
                do {
                    foundryModels = try await Router.foundry.list(exe)
                    foundryStatus = foundryModels.isEmpty ? String(localized: "No downloaded models: foundry model download <model>") : ""
                } catch {
                    foundryModels = []
                    foundryStatus = (error as? Foundry.RequestError)?.message ?? error.localizedDescription
                }
            }
            Section {
                TextEditor(text: $draft.foundry.instructions)
                    .frame(minHeight: 60)
            } header: {
                Text("Instructions")
            } footer: {
                Text("The claude-foundry models run on this Mac with Foundry Local: conversation only, no tools; images only for models that take them. Claude Code's long system prompt is not sent; these instructions are.")
                    .font(.caption).foregroundStyle(.secondary)
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
            header
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
                let path = await Codex.executable(exe)
                codexModels = await Task.detached { path.map(Codex.catalog) ?? [] }.value
            }
            foldersSection
            Section {
                Toggle("Allow writing in the session's folder", isOn: $draft.codex.write)
                Toggle("Allow network access for commands", isOn: $draft.codex.network)
                    .disabled(!draft.codex.write)
                Toggle("May ask other models", isOn: $draft.codex.askOthers)
                    .help(String(localized: "The agent may ask another model of this gateway (for example Codex) with minigw, in the same folder and with the same device's rights; one level deep, never its own kind. Codex needs “Allow network access for commands” for it."))
            } header: {
                Text("Sandbox")
            } footer: {
                Text("The claude-codex models (one per Codex model of your account) run “codex exec” as an agent in the session's folder, also in folders shared from other Macs. Its sandbox writes only to that folder and the allowed folders from Claude CLI. Devices that use Codex can read every file this user can read, and they share this Mac's Codex sessions (~/.codex). Sign in once with “codex login”.")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private var copilot: some View {
        Form {
            header
            Section {
                Toggle("Enabled", isOn: $draft.copilot.enabled)
                LabeledContent("Executable (empty = auto-detect)") {
                    TextField("", text: $draft.copilot.executable, prompt: Text(verbatim: Copilot.detected ?? "copilot"))
                        .labelsHidden()
                }
                TextField("Models (comma-separated; empty = only the default, “auto”)", text: $draft.copilot.models)
            }
            Section {
                Toggle("Allow writing in the session's folder", isOn: $draft.copilot.write)
                Toggle("Allow web access (fetch tool)", isOn: $draft.copilot.network)
                Toggle("Allow commands (in a sandbox)", isOn: Binding(get: { draft.copilot.commands },
                                                set: { on in if on { askCopilotCommands = true } else { draft.copilot.commands = false } }))
                    .disabled(!draft.copilot.write)
                SecureField(state.keychain.lookup(Copilot.tokenAccount).value?.isEmpty == false
                            ? "GitHub token for Copilot (saved; enter to replace)"
                            : "GitHub token for Copilot (fine-grained, only “Copilot Requests”)", text: keyBinding(Copilot.tokenAccount))
                Toggle("May ask other models", isOn: $draft.copilot.askOthers)
                    .help(String(localized: "The agent may ask another model of this gateway (for example Codex) with minigw, in the same folder and with the same device's rights; one level deep, never its own kind. Codex needs “Allow network access for commands” for it."))
            } header: {
                Text("Sandbox")
            } footer: {
                Text("The claude-copilot models run Copilot CLI as an agent in the session's folder, also in folders shared from other Macs, always in a sandbox with a fresh Copilot home for each request. By default it may only read the session's folder; with writing on it may edit files there. It signs in only with the token below, a fine-grained token with only the “Copilot Requests” permission, never with the GitHub CLI's sign-in, which other agents could read.")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
        .alert("Allow Copilot to run commands?", isPresented: $askCopilotCommands) {
            Button("Allow Commands", role: .destructive) { draft.copilot.commands = true }
            Button("Cancel", role: .cancel) {}
        } message: {
            Text("Copilot then runs commands as you on this Mac, in a sandbox that writes only to the session's folder and a temporary folder, and hides your keys, sign-ins and app data; commands can still read your other files and use the network. Commands can use the Copilot token to make Copilot requests on your account. Allow only if you trust the devices that use Copilot.")
        }
    }

    private var grok: some View {
        Form {
            header
            Section {
                Toggle("Enabled", isOn: $draft.grok.enabled)
                LabeledContent("Executable (empty = auto-detect)") {
                    TextField("", text: $draft.grok.executable, prompt: Text(verbatim: Grok.detected ?? "grok"))
                        .labelsHidden()
                }
            }
            Section {
                ForEach(grokModels, id: \.self) { m in
                    Toggle(isOn: Binding(get: { draft.grok.isModelEnabled(m) }, set: { draft.grok.setModel(m, $0, all: grokModels) })) {
                        Text(verbatim: m)
                    }
                }
            } header: {
                HStack {
                    Text("Models")
                    Spacer()
                    EqualWidthHStack {
                        WideButton("Enable All") { draft.grok.enabledModels = nil }
                        WideButton("Disable All") { draft.grok.enabledModels = [] }
                    }
                    .disabled(grokModels.isEmpty)
                }
                .controlSize(.small)
            }
            .task(id: draft.grok.executable) {
                let exe = draft.grok.executable
                let path = await Grok.executable(exe)
                grokModels = await Task.detached { path.map(Grok.catalog) ?? [] }.value
            }
            Section {
                Toggle("Allow writing in the session's folder", isOn: $draft.grok.write)
                Toggle("May ask other models", isOn: $draft.grok.askOthers)
                    .help(String(localized: "The agent may ask another model of this gateway (for example Codex) with minigw, in the same folder and with the same device's rights; one level deep, never its own kind. Codex needs “Allow network access for commands” for it."))
            } header: {
                Text("Sandbox")
            } footer: {
                Text("The claude-grok models run “grok -p” as an agent in the session's folder, also in folders shared from other Macs. Grok's kernel sandbox (workspace) lets it write only to that folder. Devices that use Grok can read every file this user can read, and they share this Mac's Grok sessions (~/.grok). Without writing it runs read-only. Sign in once with “grok login”.")
                    .font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private var claudeCLI: some View {
        Form {
            header
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
                Toggle("May ask other models", isOn: $draft.claudeCLI.askOthers)
                    .disabled(!draft.claudeCLI.allowLocalTools)
                    .help(String(localized: "The agent may ask another model of this gateway (for example Codex) with minigw, in the same folder and with the same device's rights; one level deep, never its own kind. Needs “Allow local tools” (Bash)."))
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
                Text("Starts Claude Code through this gateway, e.g. “minigw --model claude-apple-fm”. Also: minigw connect | status | models | disconnect. Install links the signed command into your PATH and connects it; Download saves a copy for another Mac.")
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
        let all = panel.urls.map { ($0.path as NSString).abbreviatingWithTildeInPath }.filter { !roots.contains($0) }
        let added = all.filter { !RouterConfig.overlapsScratch($0, scratch: draft.scratchURL) && !RouterConfig.overlapsCopilotCache($0) }  // H1, #26
        if added.count < all.count {
            let a = NSAlert()
            a.messageText = String(localized: "A folder that contains the scratch folder, or is inside it, cannot be an allowed folder.")
            a.informativeText = String(localized: "Agents could change the gateway's mirrors and other devices' copies there. Choose another folder, or move the scratch folder (General).")
            a.runModal()
        }
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
struct PaneHeader: View {
    let symbol: String, color: Color, title: String, about: String

    var body: some View {
        Section {
            VStack(spacing: 6) {
                Image(systemName: symbol)
                    .font(.system(size: 30, weight: .semibold))
                    .foregroundStyle(.white)
                    .frame(width: 52, height: 52)
                    .background(color.gradient, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                Text(verbatim: title).font(.title2.bold())
                Text(verbatim: about).font(.callout).foregroundStyle(.secondary).multilineTextAlignment(.center)
            }
            .frame(maxWidth: .infinity)
            .padding(.vertical, 8)
        }
    }
}

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
/// Ime dovoljene naprave: shrani se ob Return ali ko polje izgubi fokus, očiščeno kot v API (Noise.cleanName); prazno
/// ime ohrani staro (enako kot spletna stran in PATCH devices).
struct DeviceNameField: View {
    @Binding var name: String
    @State private var text = ""
    @FocusState private var focused: Bool

    var body: some View {
        TextField("Name", text: $text, prompt: Text(verbatim: "—"))
            .labelsHidden()
            .focused($focused)
            .onAppear { text = name }
            .onChange(of: name) { _, n in if !focused { text = n } }
            .onSubmit(commit)
            .onChange(of: focused) { _, f in if !f { commit() } }
    }

    private func commit() {
        let c = Noise.cleanName(text)
        if !c.isEmpty, c != name { name = c }
        text = c.isEmpty ? name : c
    }
}

struct ProviderPane: View {
    let header: PaneHeader
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
            header
            Section {
                // Brez ključa ponudnik ne more delovati → stikalo izklopljeno in onemogočeno.
                Toggle("Enabled", isOn: Binding(get: { provider.enabled && !key.isEmpty }, set: { provider.enabled = $0 }))
                    .disabled(key.isEmpty)
                    .help(key.isEmpty ? String(localized: "Enter an API key to enable") : "")
                TextField("Name", text: $provider.name)
                    .disabled(RouterConfig.builtInProviders.contains(provider.id))  // vgrajena: ime ostane (kot minigw serve)
                TextField("Base URL", text: $provider.baseURL)
                Toggle("Allow http (localhost, a private IP or a .local name only)", isOn: $provider.insecure)
                if let e = provider.urlError, !provider.baseURL.isEmpty, provider.baseURL != "https://" {
                    Text(verbatim: e).font(.caption).foregroundStyle(.red)
                }
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

/// Mapa za začasne datoteke agentov; premik prenese vsebino (npr. na hiter zunanji SSD).
struct ScratchSection: View {
    @Binding var draft: RouterConfig
    @State private var moving = false
    @State private var failure: String?

    private var shown: String { (draft.scratchURL.path as NSString).abbreviatingWithTildeInPath }

    var body: some View {
        Section {
            LabeledContent("Folder") {
                HStack {
                    if moving { ProgressView().controlSize(.small) }
                    Text(verbatim: shown).truncationMode(.middle).lineLimit(1).textSelection(.enabled)
                }
            }
            HStack {
                Spacer()
                EqualWidthHStack {
                    WideButton("Show in Finder") {
                        try? FileManager.default.createDirectory(at: draft.scratchURL, withIntermediateDirectories: true)
                        NSWorkspace.shared.activateFileViewerSelecting([draft.scratchURL])
                    }
                    WideButton("Default") { move(to: nil) }.disabled(draft.scratchFolder.isEmpty || moving)
                    WideButton("Choose…") { choose() }.disabled(moving)
                }
            }
        } header: {
            Text("Scratch folder")
        } footer: {
            Text("Temporary files of agents: conversations, outputs, attachments. Choosing another folder moves them into a “Mini Gateway Scratch” folder inside it, for example on a fast external SSD. If that disk is not connected, the default folder is used.")
                .font(.caption).foregroundStyle(.secondary)
        }
        .alert(String(localized: "The scratch folder could not be moved"), isPresented: Binding(get: { failure != nil }, set: { if !$0 { failure = nil } })) {
            Button("OK") { failure = nil }
        } message: { Text(verbatim: failure ?? "") }
    }

    private func choose() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.canCreateDirectories = true
        panel.prompt = String(localized: "Choose")
        panel.directoryURL = draft.scratchURL.deletingLastPathComponent()
        guard panel.runModal() == .OK, let url = panel.url else { return }
        move(to: url)
    }

    /// nil = privzeta mapa.
    private func move(to url: URL?) {
        // M1, L1: ista pravila kot API; shrani se kanonična pot. L3: ne med sejami minigw ali agenti.
        if let url, let why = RouterConfig.scratchProblem(url, allowedRoots: draft.claudeCLI.allowedRoots) {
            failure = String(localized: "This folder cannot hold the scratch folder: \(why).")
            return
        }
        let real = url.map { Sync.real($0) ?? $0 }
        let old = draft.scratchURL, new = real.map(RouterConfig.dedicated) ?? RouterConfig.defaultScratch
        moving = true
        Task.detached {
            if await Router.scratchInUse() {
                await MainActor.run { moving = false; failure = String(localized: "minigw sessions or agents are active. Move the scratch folder when no minigw session runs.") }
                return
            }
            let error: String? = { do { try RouterConfig.moveScratch(from: old, to: new); return nil } catch { return error.localizedDescription } }()
            await MainActor.run {
                moving = false
                if let error { failure = error } else { draft.scratchFolder = real?.path ?? "" }
            }
        }
    }
}
