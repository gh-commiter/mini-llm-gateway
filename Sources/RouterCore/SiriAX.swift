import AppKit
import ApplicationServices

/// AX nadzor aplikacije Siri AI (com.apple.campo). Vsi selektorji so AXIdentifier (neodvisni od jezika),
/// preverjeni v fazi 0 (macOS 27.0.1, Siri 1.0). Ob posodobitvi Siri popraviti samo tukaj.
enum SiriAX {
    static let bundleID = "com.apple.campo"
    static let appPath = "/System/Applications/Siri AI.app"

    enum ID {
        static let chatList = "chatListView"
        static let listItem = "listItemView"
        static let historyItem = "chatHistoryItemTitle"
        static let session = "chatSessionView"
        static let userPrompt = "userPrompt"
        static let modelResponse = "modelResponse"
        static let newChat = "newChatButton"
        static let composer = "promptViewTextField"
        static let attach = "addAttachmentButton"
        static let openPanel = "open-panel"
        static let ok = "OKButton"
        static let cancel = "CancelButton"
    }

    struct Item: Equatable {
        let role: String  // userPrompt / modelResponse
        let text: String
    }

    static func trusted(prompt: Bool) -> Bool {
        AXIsProcessTrustedWithOptions([kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: prompt] as CFDictionary)
    }

    static func isRunning() -> Bool {
        !NSRunningApplication.runningApplications(withBundleIdentifier: bundleID).isEmpty
    }

    /// Koren AX drevesa aplikacije z oknom. `open -j -a` (ne -g: brez okna Siri nima polja za vnos);
    /// zagon ne krade fokusa, aplikacijo, ki jo zažene router, takoj skrije (skrita okna AX še vidi, Siri dela naprej).
    /// Okna na drugem namizju (Space) AX ne vidi: Siri ne sme biti dodeljena drugemu namizju.
    static func app() async -> AXUIElement? {
        for attempt in 0..<2 {
            if let a = NSRunningApplication.runningApplications(withBundleIdentifier: bundleID).first {
                let root = AXUIElementCreateApplication(a.processIdentifier)
                if !windows(root).isEmpty { return root }
            }
            if attempt == 0 {
                let p = Process()
                p.executableURL = URL(fileURLWithPath: "/usr/bin/open")
                p.arguments = ["-j", "-a", appPath]
                try? p.run()
                for _ in 0..<20 {
                    try? await Task.sleep(for: .milliseconds(250))
                    if let a = NSRunningApplication.runningApplications(withBundleIdentifier: bundleID).first {
                        let root = AXUIElementCreateApplication(a.processIdentifier)
                        if !windows(root).isEmpty {
                            AXUIElementSetAttributeValue(root, kAXHiddenAttribute as CFString, kCFBooleanTrue)
                            break
                        }
                    }
                }
            }
        }
        return nil
    }

    // MARK: branje

    static func attr(_ el: AXUIElement, _ name: String) -> AnyObject? {
        var v: AnyObject?
        return AXUIElementCopyAttributeValue(el, name as CFString, &v) == .success ? v : nil
    }

    static func children(_ el: AXUIElement) -> [AXUIElement] { (attr(el, "AXChildren") as? [AXUIElement]) ?? [] }
    static func identifier(_ el: AXUIElement) -> String? { attr(el, "AXIdentifier") as? String }
    static func windows(_ root: AXUIElement) -> [AXUIElement] { (attr(root, "AXWindows") as? [AXUIElement]) ?? [] }

    static func find(_ el: AXUIElement, _ prefix: String, depth: Int = 0) -> AXUIElement? {
        if identifier(el)?.hasPrefix(prefix) == true { return el }
        guard depth < 40 else { return nil }
        for c in children(el) { if let f = find(c, prefix, depth: depth + 1) { return f } }
        return nil
    }

    /// Sporočila odprtega pogovora po vrsti.
    static func transcript(_ root: AXUIElement) -> [Item] {
        guard let conv = find(root, ID.session) else { return [] }
        var out: [Item] = []
        func walk(_ el: AXUIElement, _ depth: Int) {
            guard depth < 30 else { return }
            if (attr(el, "AXRole") as? String) == "AXTextArea" {
                let group = children(el).first { identifier($0) == ID.userPrompt || identifier($0)?.hasPrefix(ID.modelResponse) == true }
                let role = group.flatMap(identifier) ?? "?"
                var text = ((attr(el, "AXValue") as? String) ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
                if role.hasPrefix(ID.modelResponse), let group {
                    // AXTextArea.value ne vsebuje vedno vseh odstavkov (end marker zna biti ločen odstavek).
                    let paragraphs = children(group)
                        .filter { identifier($0)?.hasPrefix(ID.modelResponse) == true }
                        .compactMap { attr($0, "AXValue") as? String }
                    if !paragraphs.isEmpty { text = paragraphs.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines) }
                }
                out.append(Item(role: role.hasPrefix(ID.modelResponse) ? ID.modelResponse : role, text: text))
                return
            }
            children(el).forEach { walk($0, depth + 1) }
        }
        walk(conv, 0)
        return out
    }

    /// Vrstice seznama pogovorov z opisom »naslov, predogled zadnjega odgovora«.
    static func rows(_ root: AXUIElement) -> [(row: AXUIElement, desc: String)] {
        guard let list = find(root, ID.chatList) else { return [] }
        return ((attr(list, "AXRows") as? [AXUIElement]) ?? []).compactMap { row in
            guard let item = find(row, ID.historyItem), let d = attr(item, "AXDescription") as? String else { return nil }
            return (row, d)
        }
    }

    static func selectedDesc(_ root: AXUIElement) -> String? {
        guard let list = find(root, ID.chatList),
              let row = (attr(list, "AXSelectedRows") as? [AXUIElement])?.first,
              let item = find(row, ID.historyItem) else { return nil }
        return attr(item, "AXDescription") as? String
    }

    // MARK: dejanja (brez kraje fokusa)

    /// `AXPress` na elementu pogovora ga ne odpre; deluje izbira vrstice.
    static func select(_ root: AXUIElement, _ row: AXUIElement) -> Bool {
        guard let list = find(root, ID.chatList) else { return false }
        return AXUIElementSetAttributeValue(list, "AXSelectedRows" as CFString, [row] as CFArray) == .success
    }

    static func newChat(_ root: AXUIElement) -> Bool {
        guard let b = find(root, ID.newChat) else { return false }
        return AXUIElementPerformAction(b, kAXPressAction as CFString) == .success
    }

    static func parent(_ el: AXUIElement) -> AXUIElement? { attr(el, "AXParent").map { $0 as! AXUIElement } }

    static func first(_ el: AXUIElement, depth: Int = 0, where f: (AXUIElement) -> Bool) -> AXUIElement? {
        if f(el) { return el }
        guard depth < 40 else { return nil }
        for c in children(el) { if let x = first(c, depth: depth + 1, where: f) { return x } }
        return nil
    }

    static func poll<T>(_ seconds: Double, _ f: () -> T?) async -> T? {
        for _ in 0..<Int(seconds * 4) {
            if let v = f() { return v }
            try? await Task.sleep(for: .milliseconds(250))
        }
        return f()
    }

    /// Priloži datoteko iz `url.deletingLastPathComponent()` prek gumba za priponke → »Files« (Siri sama bere slike in PDF).
    /// Okno odpre zadnjo mapo (NSNavLastRootDirectory), zato jo nastavimo na mapo priponk; vrstico izberemo z AXSelected.
    static func attach(_ root: AXUIElement, _ url: URL) async -> Bool {
        CFPreferencesSetAppValue("NSNavLastRootDirectory" as CFString, url.deletingLastPathComponent().path as CFString, bundleID as CFString)
        CFPreferencesAppSynchronize(bundleID as CFString)
        // Menijska vrstica (Conversation → Attach File…) je brez fokusa onemogočena; meni gumba + se včasih
        // ne odpre v prvo, zato pritisk ponovimo.
        guard let button = find(root, ID.attach) else { return false }
        var item: AXUIElement?
        for _ in 0..<3 where item == nil {
            AXUIElementPerformAction(button, kAXPressAction as CFString)
            item = await poll(1.5) {
                let items = (first(button) { attr($0, "AXRole") as? String == "AXMenu" }).map(children) ?? []
                let titled = items.filter { !((attr($0, "AXTitle") as? String) ?? "").isEmpty }
                return titled.first { attr($0, "AXTitle") as? String == "Files" } ?? titled.last  // Photos, Files
            }
        }
        guard let item, AXUIElementPerformAction(item, kAXPressAction as CFString) == .success,
              let panel = await poll(5, { find(root, ID.openPanel) }) else { return false }
        func close(_ id: String) -> Bool {
            find(panel, id).map { AXUIElementPerformAction($0, kAXPressAction as CFString) == .success } ?? false
        }
        guard let cell = await poll(4, { first(panel) { attr($0, "AXRole") as? String == "AXTextField" && attr($0, "AXValue") as? String == url.lastPathComponent } }) else {
            _ = close(ID.cancel); return false
        }
        // Pogled seznama: vrstica (AXRow) z AXSelected. Pogled stolpcev: AXGroup v AXList; nastavitvi vrneta napako,
        // izbira pa obvelja (zato šteje omogočen gumb Open).
        var row: AXUIElement? = cell
        while let r = row, attr(r, "AXRole") as? String != "AXRow" { row = parent(r) }
        if let row {
            AXUIElementSetAttributeValue(row, kAXSelectedAttribute as CFString, kCFBooleanTrue)
        } else if let group = parent(cell), let list = parent(group) {
            AXUIElementSetAttributeValue(group, kAXSelectedAttribute as CFString, kCFBooleanTrue)
            AXUIElementSetAttributeValue(list, "AXSelectedChildren" as CFString, [group] as CFArray)
        }
        guard await poll(2, { find(panel, ID.ok).flatMap { attr($0, "AXEnabled") as? Bool == true ? true : nil } }) != nil,
              close(ID.ok) else { _ = close(ID.cancel); return false }
        return await poll(3, { find(root, ID.openPanel) == nil ? true : nil }) != nil
    }

    /// Vpiše in pošlje; uspeh šele, ko se polje izprazni (Siri potrditev včasih prezre, besedilo pa ostane v polju).
    static func send(_ root: AXUIElement, _ text: String) -> Bool {
        guard let field = find(root, ID.composer),
              AXUIElementSetAttributeValue(field, kAXValueAttribute as CFString, text as CFString) == .success else { return false }
        func pending() -> Bool { !((attr(field, kAXValueAttribute) as? String) ?? "").isEmpty }
        for _ in 0..<10 {  // ~25 s: Siri ne sprejme novega sporočila, dokler še piše prejšnji odgovor
            usleep(300_000)
            AXUIElementSetAttributeValue(field, kAXFocusedAttribute as CFString, kCFBooleanTrue)
            guard AXUIElementPerformAction(field, "AXConfirm" as CFString) == .success else { continue }
            for _ in 0..<8 where pending() { usleep(250_000) }
            if !pending() { return true }
        }
        AXUIElementSetAttributeValue(field, kAXValueAttribute as CFString, "" as CFString)  // ne pusti napol poslanega
        return false
    }
}
