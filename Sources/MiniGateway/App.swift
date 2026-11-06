import CryptoKit
import AppKit
import ApplicationServices
import os
import RouterCore
import Security
import SwiftUI

// Izvorni jezik vmesnika je angleščina; prevodi so v Resources/Localizable.xcstrings.

@main
struct MiniGatewayApp: App {
    @NSApplicationDelegateAdaptor private var delegate: AppDelegate
    @StateObject private var state = AppState()

    var body: some Scene {
        MenuBarExtra("Mini Gateway", systemImage: state.running ? "arrow.triangle.branch" : "exclamationmark.triangle") {
            MenuView().environmentObject(state)
        }
        Settings {
            SettingsView().environmentObject(state)
        }
        .windowResizability(.contentMinSize)
        Window("Activity", id: "activity") {
            ActivityView().environmentObject(state)
        }
    }
}

@MainActor
final class AppState: ObservableObject {
    @Published var config = RouterConfig.load()
    @Published var status = String(localized: "Stopped")
    @Published var running = false
    @Published var logs: [RequestLog] = []
    @Published var deviceSeen: [String: Date] = [:]  // zadnja zahteva naprave (le v pomnilniku)
    let keychain = Keychain()
    private var server: RouterServer?
    private let log = Logger(subsystem: "dev.trojar.minigateway", category: "app")

    init() {
        config.save()  // starejši JSON blob v preferences → berljiva struktura
        start()
    }

    var token: String {
        if let t = keychain.get("inbound-token"), !t.isEmpty { return t }
        let t = Self.newToken()
        keychain.set("inbound-token", t)
        return t
    }

    static func newToken() -> String {
        var bytes = [UInt8](repeating: 0, count: 32)
        _ = SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes)
        return "sk-siri-" + bytes.map { String(format: "%02x", $0) }.joined()
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
        })
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

struct MenuView: View {
    @EnvironmentObject var state: AppState
    @Environment(\.openWindow) private var openWindow
    @Environment(\.openSettings) private var openSettings

    var body: some View {
        Text(verbatim: state.status)
        Divider()
        if state.logs.isEmpty {
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
            openSettings()
            bringToFront("Settings")
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

