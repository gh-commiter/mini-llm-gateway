import CryptoKit
import AppKit
import ApplicationServices
import os
import RouterCore
import Security
import ServiceManagement
import SwiftUI

// Izvorni jezik vmesnika je angleščina; prevodi so v Resources/Localizable.xcstrings.

@main
struct MiniGatewayApp: App {
    @NSApplicationDelegateAdaptor private var delegate: AppDelegate
    @StateObject private var state = AppState()

    var body: some Scene {
        MenuBarExtra {
            MenuView().environmentObject(state)
        } label: {
            Image(nsImage: menuBarImage(listening: state.running, active: state.inflight.count))
        }
        // Običajno okno (ne scena Settings, ki sredini orodno vrstico): puščici in naslov levo kot v System Settings.
        Window("Settings", id: "settings") {
            SettingsView().environmentObject(state)
        }
        .windowResizability(.contentMinSize)
        .defaultPosition(.top)  // kot okno nastavitev; nato si macOS zapomni položaj
        .defaultSize(width: 980, height: 680)
        .windowToolbarStyle(.unified)
        .commands { CommandGroup(replacing: .appSettings) { SettingsMenuItem() } }
        Window("Activity", id: "activity") {
            ActivityView().environmentObject(state)
        }
    }
}

/// Ikona menijske vrstice: znak routerja, ob tekočih zahtevah še kovanček s številom (številka sredinsko).
/// Ena slika (template), ker menijska vrstica sestavljenih oznak SwiftUI ne izriše zanesljivo.
@MainActor
func menuBarImage(listening: Bool, active: Int) -> NSImage {
    let cfg = NSImage.SymbolConfiguration(pointSize: 14, weight: .regular)
    let icon = NSImage(systemSymbolName: listening ? "arrow.triangle.branch" : "exclamationmark.triangle",
                       accessibilityDescription: "Mini Gateway")?.withSymbolConfiguration(cfg) ?? NSImage()
    guard active > 0 else { icon.isTemplate = true; return icon }
    let coin = NSImage(systemSymbolName: active <= 50 ? "\(active).circle.fill" : "ellipsis.circle.fill",
                       accessibilityDescription: nil)?.withSymbolConfiguration(cfg) ?? NSImage()
    let gap: CGFloat = 3, h = max(icon.size.height, coin.size.height)
    let image = NSImage(size: NSSize(width: icon.size.width + gap + coin.size.width, height: h), flipped: false) { _ in
        icon.draw(in: NSRect(x: 0, y: (h - icon.size.height) / 2, width: icon.size.width, height: icon.size.height))
        coin.draw(in: NSRect(x: icon.size.width + gap, y: (h - coin.size.height) / 2, width: coin.size.width, height: coin.size.height))
        return true
    }
    image.isTemplate = true
    image.accessibilityDescription = "Mini Gateway, \(active) active"
    return image
}

@MainActor
final class AppState: ObservableObject {
    @Published var config = RouterConfig.load()
    @Published var status = String(localized: "Stopped")
    @Published var running = false
    /// Zgodovina (Activity): le metapodatki zahtev, nikoli vsebina; ohrani se ob ponovnem zagonu.
    @Published var logs: [RequestLog] = AppState.loadLogs() {
        didSet { try? JSONEncoder().encode(logs).write(to: Self.logsFile, options: .atomic) }
    }
    static let logsFile = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("Mini Gateway/activity.json")
    static func loadLogs() -> [RequestLog] {
        (try? JSONDecoder().decode([RequestLog].self, from: Data(contentsOf: logsFile))) ?? []
    }
    @Published var deviceSeen: [String: Date] = [:]  // zadnja zahteva naprave (le v pomnilniku)

    /// Tekoče zahteve modelom (najnovejša zgoraj), z zadnjim korakom agenta.
    struct Running: Identifiable { let id: String; let time: Date; let model: String; var step: String? }
    @Published var inflight: [Running] = []
    let keychain = Keychain()
    private var server: RouterServer?
    private let log = Logger(subsystem: "dev.trojar.minigateway", category: "app")

    init() {
        config.save()  // starejši JSON blob v preferences → berljiva struktura
        ShareHub.cleanupStale()
        // Kloni prejšnjega zagona: le imena klonov (<lastnik>-<16 hex>[-ro].<n>), nikoli cela mapa.
        let runs = config.scratch("sync").appendingPathComponent("runs")
        for n in (try? FileManager.default.contentsOfDirectory(atPath: runs.path)) ?? []
        where n.range(of: #"^.+-[0-9a-f]{16}(-ro)?\.[0-9]+$"#, options: .regularExpression) != nil {
            try? FileManager.default.removeItem(at: runs.appendingPathComponent(n))
        }
        start()
    }

    var token: String {
        if let t = keychain.get("inbound-token"), !t.isEmpty { return t }
        let t = Self.newToken()
        keychain.set("inbound-token", t)
        return t
    }

    /// 32 naključnih bajtov (CryptoKit; brez neprevrjenega rezultata SecRandomCopyBytes).
    static func random32() -> Data { SymmetricKey(size: .bits256).withUnsafeBytes { Data($0) } }

    static func newToken() -> String {
        "sk-siri-" + random32().map { String(format: "%02x", $0) }.joined()
    }

    /// Stalni ključ routerja za šifrirane povezave (Noise); ustvari ga aplikacija ob prvem zagonu.
    /// Nov ključ le, ko ga v Keychainu res ni; ob drugi napaki (zaklenjen keychain) brez šifriranih povezav,
    /// sicer bi zamenjava ključa odklopila vse odjemalce.
    lazy var routerKey: Curve25519.KeyAgreement.PrivateKey? = {
        let (v, st) = keychain.lookup("router-key")
        if st == errSecSuccess, let b = v.flatMap({ Data(base64Encoded: $0) }),
           let k = try? Curve25519.KeyAgreement.PrivateKey(rawRepresentation: b) { return k }
        guard st == errSecItemNotFound else {
            log.error("router key unavailable (\(st)); encrypted connections off")
            return nil
        }
        let k = Noise.newKey()
        keychain.set("router-key", k.rawRepresentation.base64EncodedString())
        return k
    }()

    /// Kar kopira »Copy Token«: žeton in javni ključ routerja (minigw z drugega Maca z njim preveri router).
    var connectToken: String { token + (routerKey.map { "#" + Noise.base64url($0.publicKey.rawRepresentation) } ?? "") }

    /// Skrivnost skrbniškega API (docs/admin-api.md) v Keychainu aplikacije, ne v datoteki: agenti Codex in Grok
    /// berejo datoteke povsod, Keychaina ne. Po žici gre le dokaz MGW-Admin, nikoli ona.
    lazy var adminSecret: String = {
        if let o = adminItem.lookup("admin").value.flatMap({ try? JSONSerialization.jsonObject(with: Data($0.utf8)) as? [String: Any] }),
           let s = o["secret"] as? String, s.count >= 43 { return s }
        if let v = keychain.lookup("admin-secret").value, v.count >= 43 { return v }  // razvojna različica 3c
        return Noise.base64url(Self.random32())
    }()

    /// Vnos »admin« le v prijavnem keychainu (glej Keychain.loginOnly).
    let adminItem = Keychain(loginOnly: true)

    /// Vnos »admin« v Keychainu aplikacije: skrivnost, vrata in javni ključ routerja. minigw router iz njega izve, s kom
    /// govori (ne iz datoteke ali spremenljivk okolja, ki bi jih lahko podtaknil drug proces). Zapis le ob spremembi.
    private func writeAdminItem() {
        guard let k = routerKey, let d = try? JSONSerialization.data(withJSONObject: [
            "secret": adminSecret, "port": Int(config.port), "key": Noise.base64url(k.publicKey.rawRepresentation),
        ], options: [.sortedKeys]) else { return }
        let s = String(decoding: d, as: UTF8.self)
        if adminItem.lookup("admin").value != s { adminItem.set("admin", s) }
        if keychain.lookup("admin-secret").status == errSecSuccess { keychain.delete("admin-secret") }
        try? FileManager.default.removeItem(at: Self.logsFile.deletingLastPathComponent().appendingPathComponent("router.json"))
    }

    /// Skrbniški API spreminja iste nastavitve kot okno Settings (apply: shrani in znova zažene router).
    private func adminHooks() -> AdminHooks {
        AdminHooks(secret: adminSecret, connect: connectToken, version: MenuView.version,
                   installed: { SMAppService.mainApp.status == .enabled },
                   commit: { [self] f, keys in  // AppState živi ves čas aplikacije
                       try await MainActor.run {
                           var c = self.config
                           try f(&c)
                           for (account, key) in keys {  // ključi le po uspešni spremembi, pred ponovnim zagonom
                               if let key { self.keychain.set(account, key) } else { self.keychain.delete(account) }
                           }
                           self.apply(c)
                           return c
                       }
                   },
                   rotateToken: { [self] in
                       await MainActor.run {
                           self.regenerateToken()
                           return self.connectToken
                       }
                   },
                   peerCheck: { PeerProcess.refusal(peerPort: $0, routerPort: $1) })
    }

    func regenerateToken() {
        keychain.set("inbound-token", Self.newToken())
        restart()
    }

    func start() {
        let router = Router(config: config, token: token, secrets: keychain, onLog: { [weak self] entry in
            Task { @MainActor in
                guard let self else { return }
                self.logs = Array(([entry] + self.logs).prefix(500))
            }
        }, onDevice: { [weak self] device in
            Task { @MainActor in self?.seen(device) }
        }, onActivity: { [weak self] event in
            Task { @MainActor in
                guard let self else { return }
                switch event {
                case .started(let id, let time, let model): self.inflight.insert(Running(id: id, time: time, model: model), at: 0)
                case .step(let id, let text): if let i = self.inflight.firstIndex(where: { $0.id == id }) { self.inflight[i].step = text }
                case .finished(let id): self.inflight.removeAll { $0.id == id }
                }
            }
        }, admin: adminHooks())
        writeAdminItem()
        let s = RouterServer(router: router, key: routerKey)
        s.onState = { [weak self] state in
            Task { @MainActor in self?.show(state) }
        }
        do {
            try s.start()
            server = s
        } catch {
            log.error("start failed: \(error, privacy: .public)")
            show(.failed(error.localizedDescription))
        }
    }

    private func show(_ state: ServerState) {
        switch state {
        case .listening(let host, let port):
            running = true
            status = String(localized: "Listening on \(host):\(String(port))")  // port brez ločila tisočic
        case .failed(let message):
            running = false
            status = String(localized: "Error: \(message)")
        case .stopped:
            running = false
            status = String(localized: "Stopped")
        }
    }

    /// Končaj in znova odpri aplikacijo (nova instanca se zažene, ko se ta konča).
    func relaunch() {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/sh")
        p.arguments = ["-c", "while kill -0 \(ProcessInfo.processInfo.processIdentifier) 2>/dev/null; do sleep 0.2; done; /usr/bin/open \"$0\"",
                       Bundle.main.bundlePath]
        try? p.run()
        AppDelegate.quitRequested = true
        NSApp.terminate(nil)
    }

    func restart() {
        server?.stop()
        server = nil
        start()
    }

    /// Nova naprava se doda v nastavitve brez ponovnega zagona (Siri zanjo ostane izklopljena, dokler je ne vklopiš).
    private func seen(_ d: Noise.Device) {
        if deviceSeen[d.id].map({ Date().timeIntervalSince($0) > 30 }) ?? true { deviceSeen[d.id] = Date() }
        guard !config.devices.contains(where: { $0.id == d.id }) else { return }
        config.devices.append(DeviceConfig(id: d.id, name: d.name))
        config.save()
    }

    func apply(_ c: RouterConfig) {
        config = c
        c.save()
        restart()
    }
}

/// ⌘, v meniju aplikacije (prej ga je dala scena Settings).
struct SettingsMenuItem: View {
    @Environment(\.openWindow) private var openWindow
    var body: some View {
        Button("Settings…") { openWindow(id: "settings"); bringToFront("settings") }.keyboardShortcut(",")
    }
}

struct MenuView: View {
    static let version = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "?"
    static let build = Bundle.main.object(forInfoDictionaryKey: "MGCommit") as? String ?? "?"  // commit (build.sh)
    @EnvironmentObject var state: AppState
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Text(verbatim: state.status)
        Text("Version \(Self.version) (\(Self.build))")
        Divider()
        ForEach(state.inflight) { r in
            Text(verbatim: "⏳ \(r.model)  \(Duration.seconds(Int(Date().timeIntervalSince(r.time))).formatted(.time(pattern: .minuteSecond)))"
                 + (r.step.map { "  · \($0.prefix(60))" } ?? ""))
        }
        if state.logs.isEmpty && state.inflight.isEmpty {
            Text("No requests")
        } else {
            ForEach(state.logs.prefix(10)) { l in
                Text(verbatim: "\(l.time.formatted(date: .omitted, time: .standard))  \(l.status)  \(l.model ?? l.path)  \(l.ms) ms")
            }
        }
        Button("Activity…") {
            openWindow(id: "activity")
            bringToFront("activity")
        }
        Divider()
        Button("Copy Token") {
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(state.connectToken, forType: .string)
        }
        Button("Settings…") {
            openWindow(id: "settings")
            bringToFront("settings")
        }
        Button("Quit") {
            AppDelegate.quitRequested = true
            NSApp.terminate(nil)
        }
    }
}

struct ActivityView: View {
    @EnvironmentObject var state: AppState
    @State private var filter = ""

    private var rows: [RequestLog] {
        guard !filter.isEmpty else { return state.logs }
        return state.logs.filter {
            [$0.model, $0.provider, $0.method, $0.path, String($0.status)].compactMap { $0 }
                .joined(separator: " ").localizedCaseInsensitiveContains(filter)
        }
    }

    var body: some View {
        VStack(spacing: 0) {
            if !state.inflight.isEmpty {
                // Tekoče zahteve: čas se osveži vsako sekundo, korak agenta sproti.
                TimelineView(.periodic(from: .now, by: 1)) { ctx in
                    VStack(alignment: .leading, spacing: 4) {
                        ForEach(state.inflight) { r in
                            HStack(spacing: 8) {
                                ProgressView().controlSize(.small)
                                Text(verbatim: r.model).fontWeight(.medium)
                                Text(verbatim: Duration.seconds(Int(ctx.date.timeIntervalSince(r.time))).formatted(.time(pattern: .minuteSecond)))
                                    .monospacedDigit().foregroundStyle(.secondary)
                                Text(verbatim: r.step ?? String(localized: "Working…")).lineLimit(1).truncationMode(.middle)
                                    .foregroundStyle(.secondary)
                            }
                        }
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(10)
                }
                Divider()
            }
            table
        }
    }

    private var table: some View {
        Table(rows) {
            TableColumn("Time") { Text($0.time.formatted(date: .abbreviated, time: .standard)) }
            TableColumn("Status") { Text(verbatim: String($0.status)).foregroundStyle($0.status < 400 ? Color.primary : Color.red) }
            TableColumn("Model") { Text(verbatim: $0.model ?? "–") }
            TableColumn("Provider") { Text(verbatim: $0.provider ?? "–") }
            TableColumn("Path") { Text(verbatim: "\($0.method) \($0.path)") }
            TableColumn("Duration") { Text(verbatim: "\($0.ms) ms") }
        }
        .searchable(text: $filter)
        .toolbar { Button("Clear") { state.logs = [] } }
        .frame(minWidth: 760, minHeight: 360)
    }
}

/// Okno za izbiro ne sprejme datoteke, ki ni Claude Code (sporočilo pokaže sam NSOpenPanel).
final class ClaudePanelValidator: NSObject, NSOpenSavePanelDelegate {
    func panel(_ sender: Any, validate url: URL) throws {
        if case .failure(let e) = ClaudeCLI.validate(url.path) {
            throw NSError(domain: "MiniGateway", code: 1, userInfo: [NSLocalizedDescriptionKey: String(localized: "Invalid: \(e.message)")])
        }
    }
}

/// Aplikacija živi le v menijski vrstici (.accessory), zato je macOS ne aktivira: okno bi bilo
/// spredaj, a neaktivno, s sivimi kontrolniki. Dokler je odprto okno Settings ali Activity,
/// je aplikacija navadna (.regular, z Dockom); ko se zadnje zapre, se vrne v menijsko vrstico.
@MainActor
func bringToFront(_ match: String) {
    NSApp.setActivationPolicy(.regular)
    Task { @MainActor in
        for _ in 0..<20 {  // SwiftUI okno ustvari z zamikom
            if let w = appWindows().first(where: { $0.identifier?.rawValue.localizedCaseInsensitiveContains(match) == true }), w.isVisible {
                NSApp.activate()
                w.makeKeyAndOrderFront(nil)
                w.orderFrontRegardless()
                returnToMenuBarWhenClosed(w)
                return
            }
            try? await Task.sleep(for: .milliseconds(50))
        }
    }
}

/// Cmd+Q na oknu Settings/Activity zapre le okna (router teče naprej v menijski vrstici);
/// aplikacijo ustavi samo »Quit« v meniju menijske vrstice ali odjava/izklop sistema.
final class AppDelegate: NSObject, NSApplicationDelegate {
    @MainActor static var quitRequested = false

    /// Posnetki zaslona za README: le ob zagonu z MG_SNAPSHOT_DIR; obvestilo »dev.trojar.minigateway.snapshot«
    /// z imenom zapiše vidno okno nastavitev v to mapo (lastno okno, brez dovoljenja za snemanje zaslona).
    @MainActor
    func applicationDidFinishLaunching(_ notification: Notification) {
        guard let dir = ProcessInfo.processInfo.environment["MG_SNAPSHOT_DIR"] else { return }
        DistributedNotificationCenter.default().addObserver(forName: .init("dev.trojar.minigateway.snapshot"),
                                                            object: nil, queue: .main) { n in
            let name = String((n.object as? String ?? "settings").filter { $0.isLetter || $0.isNumber || $0 == "-" }.prefix(40))
            MainActor.assumeIsolated {
                guard let w = NSApp.windows.first(where: { $0.isVisible && $0.styleMask.contains(.titled) }),
                      let view = w.contentView?.superview ?? w.contentView,
                      let rep = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { return }
                view.cacheDisplay(in: view.bounds, to: rep)
                try? rep.representation(using: .png, properties: [:])?
                    .write(to: URL(fileURLWithPath: dir).appendingPathComponent(name + ".png"))
            }
        }
    }

    @MainActor
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        // Quit Apple event (odjava, Dock, osascript) = pravi izhod; Cmd+Q iz menija nima Apple eventa.
        let event = NSAppleEventManager.shared().currentAppleEvent
        let systemQuit = event?.eventClass == AEEventClass(kCoreEventClass) && event?.eventID == AEEventID(kAEQuitApplication)
        if Self.quitRequested || systemQuit || !appWindows().contains(where: \.isVisible) { return .terminateNow }
        appWindows().forEach { $0.close() }
        NSApp.setActivationPolicy(.accessory)
        return .terminateCancel
    }
}

/// Okni Settings in Activity (ne meni v menijski vrstici).
@MainActor
func appWindows() -> [NSWindow] {
    NSApp.windows.filter { w in ["Settings", "activity"].contains { w.identifier?.rawValue.localizedCaseInsensitiveContains($0) == true } }
}

@MainActor private var watchedWindows = Set<ObjectIdentifier>()

@MainActor
private func returnToMenuBarWhenClosed(_ w: NSWindow) {
    guard watchedWindows.insert(ObjectIdentifier(w)).inserted else { return }
    NotificationCenter.default.addObserver(forName: NSWindow.willCloseNotification, object: w, queue: .main) { _ in
        DispatchQueue.main.async {  // po zaprtju je okno že nevidno
            MainActor.assumeIsolated {
                if !appWindows().contains(where: \.isVisible) { NSApp.setActivationPolicy(.accessory) }
            }
        }
    }
}

