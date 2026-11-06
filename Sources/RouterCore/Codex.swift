import Foundation

/// Ponudnik `codex exec` (OpenAI Codex CLI): agent v mapi seje, kot `claude -p`.
public struct CodexConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""  // prazno = samodejno iskanje
    public init() {}

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = CodexConfig()
        enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? d.enabled
        executable = try c.decodeIfPresent(String.self, forKey: .executable) ?? d.executable
    }
}

public enum Codex {
    /// Claude Code pokaže le modele z »claude« v imenu.
    static let ids: Set<String> = ["claude-codex"]
    public static let detected: String? = ClaudeCLI.locate("codex")

    static func executable(_ configured: String) -> String? {
        configured.isEmpty ? detected : [(configured as NSString).expandingTildeInPath].first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    /// Argumenti: agent piše le v mapi seje in dovoljenih mapah (sandbox Codex), omrežje za gradnjo dovoljeno.
    /// Brez orodij (naslovi, povzetki) le branje v začasni mapi.
    static func args(cwd: URL, extraDirs: [String], agent: Bool, output: URL) -> [String] {
        ["exec", "--skip-git-repo-check", "--ephemeral", "--color", "never", "-C", cwd.path, "-o", output.path]
            + (agent ? ["--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true"]
                       + extraDirs.flatMap { ["--add-dir", $0] }
                     : ["--sandbox", "read-only"])
            + ["-"]  // navodila prek stdin (brez omejitve dolžine argumentov)
    }

    struct Failure: Error { let message: String }

    final class Handle: @unchecked Sendable { let p = Process() }

    /// Zažene codex in vrne zadnje sporočilo agenta; prekinitev odjemalca ustavi codex.
    static func run(_ exe: String, args: [String], cwd: URL, prompt: String, output: URL) async throws -> String {
        let h = Handle()
        return try await withTaskCancellationHandler {
            try await Task.detached { () throws -> String in
                let p = h.p
                p.executableURL = URL(fileURLWithPath: exe)
                p.arguments = args
                p.currentDirectoryURL = cwd
                let input = Pipe(), errors = Pipe()
                p.standardInput = input
                p.standardOutput = FileHandle.nullDevice
                p.standardError = errors
                try p.run()
                input.fileHandleForWriting.write(Data(prompt.utf8))
                try? input.fileHandleForWriting.close()
                let stderr = errors.fileHandleForReading.readDataToEndOfFile()
                p.waitUntilExit()
                let text = (try? String(contentsOf: output, encoding: .utf8))?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                guard p.terminationStatus == 0, !text.isEmpty else {
                    let tail = String(decoding: stderr.suffix(400), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                    throw Failure(message: "codex exited with \(p.terminationStatus)" + (tail.isEmpty ? "" : ": \(tail)"))
                }
                return text
            }.value
        } onCancel: { if h.p.isRunning { h.p.terminate() } }
    }
}

extension Router {
    func runCodex(_ req: HTTPRequest, body: [String: Any], rid: String, writer: ResponseWriter) async -> Int {
        guard let exe = Codex.executable(config.codex.executable) else {
            return await error(503, "api_error", "Codex CLI not found (Mini Gateway → Settings → Claude CLI → Codex)", rid: rid, writer: writer)
        }
        let all = ClaudeCLI.turns(body)
        guard let last = all.last, last.role == "user" else {
            return await error(400, "invalid_request_error", "Last message must be from the user", rid: rid, writer: writer)
        }
        let agent = (body["tools"] as? [Any])?.isEmpty == false
        var cwd = FileManager.default.temporaryDirectory.appendingPathComponent("minigateway-chat", isDirectory: true)
        var extra: [String] = []
        var prompt = ClaudeCLI.prompt(history: all.dropLast(), last: last)
        if agent {
            let clientCwd = req.header("x-claude-mini-cwd")
            let shared = await Self.shares.mount(session: req.header("x-claude-mini-session") ?? "", cwd: clientCwd, base: localBase)
            guard let dir = shared ?? ClaudeCLI.resolveCwd(clientCwd, roots: config.claudeCLI.allowedRoots) else {
                return await error(400, "invalid_request_error",
                                   "cwd_unavailable: folder \(clientCwd ?? "(none)") does not exist on this Mac or is outside the allowed folders",
                                   rid: rid, writer: writer)
            }
            cwd = dir
            extra = config.claudeCLI.allowedRoots.split(separator: ",").map { ClaudeCLI.expand($0.trimmingCharacters(in: .whitespaces)) }
                .filter { $0 != "/" && !$0.isEmpty && $0 != dir.path && FileManager.default.fileExists(atPath: $0) }
            if let shared, let clientCwd {
                prompt = "The user's folder \(clientCwd) (on their Mac) is mounted here as \(shared.path), your working directory. Map paths under \(clientCwd) to it.\n\n" + prompt
            }
        } else {
            try? FileManager.default.createDirectory(at: cwd, withIntermediateDirectories: true)
        }
        let output = FileManager.default.temporaryDirectory.appendingPathComponent("codex-\(rid).txt")
        defer { try? FileManager.default.removeItem(at: output) }
        let args = Codex.args(cwd: cwd, extraDirs: extra, agent: agent, output: output)
        return await replyWhenDone(body: body, model: "claude-codex", rid: rid, writer: writer) {
            (try await Codex.run(exe, args: args, cwd: cwd, prompt: prompt, output: output), [])
        }
    }

    /// Ponudnik brez pretoka (Siri, Codex): pri pretoku glave in message_start takoj, med čakanjem ping, nato cel odgovor.
    func replyWhenDone(body: [String: Any], model: String, rid: String, writer: ResponseWriter,
                       work: () async throws -> (String, [(String, String)])) async -> Int {
        let stream = body["stream"] as? Bool == true
        var t = StreamTranslator(model: body["model"] as? String ?? model)
        var pinger: Task<Void, Never>?
        do {
            if stream {
                try await writer.write(HTTPResponse.head(status: 200, headers: [
                    ("Content-Type", "text/event-stream"), ("Cache-Control", "no-cache"), ("request-id", rid),
                ]) + t.start().reduce(Data(), +))
                pinger = Task {
                    while !Task.isCancelled {
                        try? await Task.sleep(for: .seconds(10))
                        if Task.isCancelled { break }
                        try? await writer.write(StreamTranslator.sse("ping", ["type": "ping"]))
                    }
                }
            }
            let (text, extra) = try await work()
            pinger?.cancel()
            let delta = t.emit(text)
            t.usage = ["output_tokens": text.utf8.count / 4]
            if stream {
                try await writer.write(delta + t.finish().reduce(Data(), +))
                return 200
            }
            let data = (try? JSONSerialization.data(withJSONObject: t.message(), options: [.withoutEscapingSlashes])) ?? Data()
            try? await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "application/json"), ("request-id", rid)] + extra,
                                                      contentLength: data.count) + data)
            return 200
        } catch {
            pinger?.cancel()
            let (status, type, message) = (error as? SiriError)?.status
                ?? (error as? Codex.Failure).map { (502, "api_error", $0.message) } ?? (500, "api_error", "\(error)")
            Self.log.error("\(rid, privacy: .public) \(model, privacy: .public) failed: \(message, privacy: .public)")
            if stream {
                try? await writer.write(StreamTranslator.sse("error", ["type": "error", "error": ["type": type, "message": message]]))
                return status
            }
            return await self.error(status, type, message, rid: rid, writer: writer)
        }
    }
}
