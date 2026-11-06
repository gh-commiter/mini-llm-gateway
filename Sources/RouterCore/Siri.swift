import ApplicationServices
import Foundation

public struct SiriConfig: Codable, Sendable, Equatable {
    public var enabled = false  // vklop v nastavitvah zahteva dovoljenje za Accessibility
    public var timeout = 65.0   // s, skupaj za eno vprašanje
    public var maxQueue = 4     // čakajoče zahteve; nad tem 429

    public init() {}

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = SiriConfig()
        enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? d.enabled
        timeout = try c.decodeIfPresent(Double.self, forKey: .timeout) ?? d.timeout
        maxQueue = try c.decodeIfPresent(Int.self, forKey: .maxQueue) ?? d.maxQueue
    }
}

enum SiriError: Error {
    case permission, unavailable, queueFull, timeout, uiFailed(String)

    var status: (Int, String, String) {
        switch self {
        case .permission: (503, "permission_error", "Mini Gateway needs Accessibility permission (System Settings → Privacy & Security → Accessibility)")
        case .unavailable: (503, "api_error", "Siri AI is not available (app not running or has no window)")
        case .queueFull: (429, "rate_limit_error", "Siri queue is full; try again shortly")
        case .timeout: (504, "api_error", "Siri did not finish answering in time")
        case .uiFailed(let what): (503, "api_error", "Siri UI step failed: \(what)")
        }
    }
}

enum Siri {
    static let ids: Set<String> = ["claude-siri-ai", "siri-ai"]
    private static let markerRegex = try! NSRegularExpression(pattern: "(?i)\\bend-[0-9a-f]{4}\\b")
    static let placeholders = ["looking into it", "searching for", "searching"]  // vmesna stanja, ne odgovor

    /// Besedilo odgovora brez end markerja in vsega za njim (Siri doda Applovo opozorilo).
    static func cleanReply(_ text: String) -> String {
        let r = NSRange(text.startIndex..., in: text)
        guard let m = markerRegex.firstMatch(in: text, range: r), let range = Range(m.range, in: text) else {
            return text.trimmingCharacters(in: .whitespacesAndNewlines)
        }
        return String(text[..<range.lowerBound]).trimmingCharacters(in: .whitespacesAndNewlines)
    }

    static func instruction(_ marker: String) -> String {
        "\n\n(End your answer with the tag \(marker) on the last line.)"
    }

    static func squash(_ s: String) -> String { s.split(whereSeparator: \.isWhitespace).joined(separator: " ") }

    /// Nova Siri nit dobi dosedanji pogovor kot kontekst (zadnji del, če je predolg).
    static func packed(history: [ClaudeCLI.Turn], last: String, limit: Int = 8000) -> String {
        guard !history.isEmpty else { return last }
        var past = history.map { "\($0.role == "assistant" ? "Assistant" : "User"): \($0.text)" }.joined(separator: "\n\n")
        let room = max(limit - last.count - 120, 0)
        if past.count > room { past = "…" + past.suffix(room) }
        return "Earlier conversation (for context):\n\n\(past)\n\n---\n\n\(last)"
    }
}

/// Zaporedno izvajanje Siri vprašanj (ena AX operacija naenkrat), register niti in idempotenca.
actor SiriEngine {
    static let shared = SiriEngine()

    struct Reply: Sendable {
        let text: String
        let newThread: Bool
        let markerMissing: Bool
    }

    struct ThreadRecord: Codable {
        var desc: String       // opis vrstice v seznamu pogovorov (naslov, predogled)
        var lastReply: String  // začetek zadnjega odgovora
        var used: Date
    }

    private var threads: [String: ThreadRecord]
    private var inflight: [String: Task<Reply, Error>] = [:]
    private var done: [String: (Reply, Date)] = [:]
    private var tail: Task<Void, Never>?
    private var queued = 0

    private static let file: URL = {
        let dir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Mini Gateway", isDirectory: true)
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir.appendingPathComponent("siri-threads.json")
    }()

    init() {
        threads = (try? JSONDecoder().decode([String: ThreadRecord].self, from: Data(contentsOf: Self.file))) ?? [:]
    }

    /// `context`: vsebina omenjenih datotek; gre le v poslano besedilo, ne v ključe niti (nadaljevanje pogovora ostane).
    func ask(_ turns: [ClaudeCLI.Turn], scope: String, cfg: SiriConfig, context: String = "") async throws -> Reply {
        let reqKey = ClaudeCLI.key(turns, scope: scope)
        done = done.filter { Date().timeIntervalSince($0.value.1) < 600 }
        // Claude Code ob napaki ali prekinitvi ponovi zahtevo: Siri istega sporočila ne dobi dvakrat.
        if let (reply, _) = done[reqKey] { return reply }
        if let running = inflight[reqKey] { return try await running.value }
        guard queued < cfg.maxQueue else { throw SiriError.queueFull }

        queued += 1
        let previous = tail
        let task = Task { () -> Reply in
            await previous?.value
            return try await self.run(turns, scope: scope, cfg: cfg, context: context)
        }
        tail = Task { _ = try? await task.value }
        inflight[reqKey] = task
        defer {
            queued -= 1
            inflight[reqKey] = nil
        }
        let reply = try await task.value  // prekinitev odjemalca ne prekine Siri; odgovor ostane v `done`
        done[reqKey] = (reply, Date())
        return reply
    }

    private func run(_ turns: [ClaudeCLI.Turn], scope: String, cfg: SiriConfig, context: String) async throws -> Reply {
        guard SiriAX.trusted(prompt: false) else { throw SiriError.permission }  // dialog sproži le vklop v nastavitvah
        guard let root = await SiriAX.app() else { throw SiriError.unavailable }
        let deadline = Date().addingTimeInterval(cfg.timeout)
        let history = Array(turns.dropLast())
        let last = turns.last?.text ?? ""

        var newThread = true
        if let rec = threads[ClaudeCLI.key(history, scope: scope)], await open(root, rec) {
            newThread = false
        } else {
            guard SiriAX.newChat(root) else { throw SiriError.uiFailed("new conversation") }
            for _ in 0..<12 where SiriAX.find(root, SiriAX.ID.composer) == nil || !SiriAX.transcript(root).isEmpty {
                try await Task.sleep(for: .milliseconds(250))
            }
        }

        let before = SiriAX.transcript(root).count
        let marker = "END-" + String(UUID().uuidString.prefix(4)).lowercased()
        let prompt = context + (newThread ? Siri.packed(history: history, last: last) : last) + Siri.instruction(marker)
        guard SiriAX.send(root, prompt) else { throw SiriError.uiFailed("send") }

        var seen = ""
        var stableSince = Date()
        var result: (String, Bool)?
        while Date() < deadline {
            try await Task.sleep(for: .milliseconds(250))
            let items = SiriAX.transcript(root)
            guard items.count > before, let resp = items.last, resp.role == SiriAX.ID.modelResponse else { continue }
            if resp.text.range(of: marker, options: .caseInsensitive) != nil {
                result = (Siri.cleanReply(resp.text), false)
                break
            }
            if resp.text != seen {
                seen = resp.text
                stableSince = Date()
            } else if !seen.isEmpty, !Siri.placeholders.contains(where: { seen.lowercased().hasPrefix($0) }),
                      Date().timeIntervalSince(stableSince) > 5 {
                result = (Siri.cleanReply(seen), true)  // Siri oznake ni dodala; rezerva: stabilno besedilo
                break
            }
        }
        guard let (text, missing) = result, !text.isEmpty else { throw SiriError.timeout }

        let after = turns + [ClaudeCLI.Turn(role: "assistant", text: text)]
        threads[ClaudeCLI.key(after, scope: scope)] = ThreadRecord(desc: SiriAX.selectedDesc(root) ?? "",
                                                                    lastReply: String(text.prefix(200)), used: Date())
        if threads.count > 200 {  // ponytail: obdrži zadnjih 200 niti
            for k in threads.sorted(by: { $0.value.used < $1.value.used }).prefix(threads.count - 200).map(\.key) { threads[k] = nil }
        }
        try? JSONEncoder().encode(threads).write(to: Self.file, options: .atomic)
        return Reply(text: text, newThread: newThread, markerMissing: missing)
    }

    /// Odpre pogovor niti in preveri, da je res pravi (zadnji odgovor se ujema).
    private func open(_ root: AXUIElement, _ rec: ThreadRecord) async -> Bool {
        func matches() -> Bool {
            guard let r = SiriAX.transcript(root).last(where: { $0.role == SiriAX.ID.modelResponse }) else { return false }
            return Siri.squash(Siri.cleanReply(r.text)).hasPrefix(Siri.squash(rec.lastReply).prefix(150))
        }
        if matches() { return true }
        let rows = SiriAX.rows(root)
        let probe = String(Siri.squash(rec.lastReply).prefix(30))
        guard let row = rows.first(where: { !rec.desc.isEmpty && $0.desc == rec.desc })
                ?? rows.first(where: { Siri.squash($0.desc).contains(probe) }),
              SiriAX.select(root, row.row) else { return false }
        for _ in 0..<12 {
            try? await Task.sleep(for: .milliseconds(250))
            if matches() { return true }
        }
        return false
    }
}

extension Router {
    func runSiri(_ req: HTTPRequest, body: [String: Any], rid: String, writer: ResponseWriter) async -> Int {
        let turns = ClaudeCLI.turns(body)
            .map { ClaudeCLI.Turn(role: $0.role, text: AppleFM.stripReminders($0.text)) }
            .filter { !$0.text.isEmpty }
        guard turns.last?.role == "user" else {
            return await error(400, "invalid_request_error", "Last message must be from the user", rid: rid, writer: writer)
        }
        let scope = req.header("x-claude-mini-session") ?? ""
        let clientCwd = req.header("x-claude-mini-cwd")
        return await replyWhenDone(body: body, model: "claude-siri-ai", rid: rid, writer: writer) {
            // Datoteke, omenjene v vprašanju, iz mape seje (priklopljene z drugega Maca ali dovoljene tukaj).
            var context = ""
            if let question = turns.last?.text {
                let root: URL? = await Self.shares.mount(session: scope, cwd: clientCwd, base: localBase,
                                                 wait: req.header("x-claude-mini-share") == "1")
                    ?? ClaudeCLI.resolveCwd(clientCwd, roots: config.claudeCLI.allowedRoots)
                if let root {
                    let names = await Mentions.names(in: question)
                    let files = Mentions.find(names, question: question, in: root)
                    context = Mentions.attachments(files, root: root)
                    Self.log.notice("\(rid, privacy: .public) siri mentions=\(names.count) attached=\(files.count)")
                }
            }
            let reply = try await SiriEngine.shared.ask(turns, scope: scope, cfg: config.siri, context: context)
            Self.log.notice("\(rid, privacy: .public) siri thread=\(reply.newThread ? "new" : "continued", privacy: .public) marker=\(reply.markerMissing ? "missing" : "ok", privacy: .public)")
            var headers = [("x-siri-thread", reply.newThread ? "new" : "continued")]
            if reply.markerMissing { headers.append(("x-siri-end-marker", "missing")) }
            return (reply.text, headers)
        }
    }
}
