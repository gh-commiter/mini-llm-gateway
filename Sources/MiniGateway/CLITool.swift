import Foundation
import RouterCore

/// Ukaz `minigw`: podpisan pomožni program v Contents/Helpers. Install v PATH naredi
/// povezavo nanj (posodobi se z aplikacijo) in izvede `connect`; naslov in žeton hrani ukaz v svojem Keychainu.
enum CLITool {
    static let name = "minigw"

    static var helper: URL { Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/\(name)") }

    /// Povezava v PATH na ukaz v aplikaciji, nato `minigw connect <url> --token <t>`.
    static func install(in dir: String, url: String, token: String) throws -> String {
        let link = URL(fileURLWithPath: dir).appendingPathComponent(name)
        let fm = FileManager.default
        try fm.createDirectory(atPath: dir, withIntermediateDirectories: true)
        if (try? fm.destinationOfSymbolicLink(atPath: link.path)) != nil || fm.fileExists(atPath: link.path) {
            try fm.removeItem(at: link)  // prejšnja povezava ali stara skripta
        }
        try fm.createSymbolicLink(at: link, withDestinationURL: helper)
        return try run(["connect", url, "--token", token])
    }

    /// Kopija podpisanega ukaza za drug Mac (tam: minigw connect <ta Mac> --token …).
    static func export(to file: URL) throws {
        let fm = FileManager.default
        if fm.fileExists(atPath: file.path) { try fm.removeItem(at: file) }
        try fm.copyItem(at: helper, to: file)
    }

    /// Zažene ukaz in vrne njegov izpis (ali vrže napako z izpisom).
    static func run(_ args: [String]) throws -> String {
        let p = Process()
        p.executableURL = helper
        p.arguments = args
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        try p.run()
        let text = String(decoding: pipe.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        p.waitUntilExit()  // izhod je že zaprt, proces končan
        guard p.terminationStatus == 0 else { throw NSError(domain: name, code: Int(p.terminationStatus), userInfo: [NSLocalizedDescriptionKey: text]) }
        return text
    }

    /// PATH iz uporabnikove prijavne lupine (aplikacija iz Finderja ima le sistemskega).
    static let loginPATH: [String] = {
        let shell = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
        let p = Process()
        p.executableURL = URL(fileURLWithPath: shell)
        p.arguments = ["-lic", "print -r -- \"__PATH__$PATH\""]
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        p.standardInput = FileHandle.nullDevice
        var path = ProcessInfo.processInfo.environment["PATH"] ?? ""
        if (try? p.run()) != nil {
            let text = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
            if let line = text.split(whereSeparator: \.isNewline).last(where: { $0.hasPrefix("__PATH__") }) {
                path = String(line.dropFirst("__PATH__".count))
            }
        }
        var seen = Set<String>()
        return path.split(separator: ":").map(String.init).filter { !$0.isEmpty && seen.insert($0).inserted }
    }()

    static func installedPath() -> String? {
        loginPATH.map { "\($0)/\(name)" }.first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    /// Običajno mesto za lastne ukaze, ki je v PATH in zapisljivo brez gesla. Ne katerakoli zapisljiva
    /// mapa v PATH: npr. ~/.jenv/shims ali ~/.pyenv/shims upravljajo druga orodja in jih brišejo.
    static func installDirectory() -> (path: String, inPATH: Bool) {
        let home = NSHomeDirectory()
        let preferred = ["\(home)/.local/bin", "\(home)/bin", "/opt/homebrew/bin", "/usr/local/bin"]
        if let dir = preferred.first(where: { loginPATH.contains($0) && FileManager.default.isWritableFile(atPath: $0) }) {
            return (dir, true)
        }
        return ("\(home)/.local/bin", false)  // nobeno ni v PATH: namesti sem in povej, kaj dodati
    }

    /// Naslov routerja: za ta Mac loopback, za drug Mac ime tega Maca v lokalnem omrežju.
    static func routerURL(_ config: RouterConfig, forThisMac: Bool) -> String {
        let loopback = ["127.0.0.1", "localhost", "::1"].contains(config.host)
        let host: String
        if forThisMac {
            host = loopback || config.host == "0.0.0.0" ? "127.0.0.1" : config.host
        } else {
            host = loopback || config.host == "0.0.0.0" ? (ProcessInfo.processInfo.hostName) : config.host
        }
        return "http://\(host):\(config.port)"
    }
}
