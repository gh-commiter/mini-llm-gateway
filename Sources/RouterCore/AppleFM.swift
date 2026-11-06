import Foundation
import FoundationModels

/// Ponudnik Apple Foundation Models (na napravi, ~8K kontekst, brez orodij).
public struct AppleFMConfig: Codable, Sendable, Equatable {
    public var enabled = true
    public var instructions = "You are a helpful assistant running on-device. Answer concisely."

    public init() {}

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = AppleFMConfig()
        enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? d.enabled
        instructions = try c.decodeIfPresent(String.self, forKey: .instructions) ?? d.instructions
    }
}

enum AppleFM {
    static let ids: Set<String> = ["claude-apple-fm", "apple-fm"]
    static let responseReserve = 1024  // tokeni, ki ostanejo za odgovor

    private static let reminder = try! NSRegularExpression(pattern: "<system-reminder>[\\s\\S]*?</system-reminder>")

    private static let lineNumbers = try! NSRegularExpression(pattern: "(?m)^ *\\d+\\t")
    private static let readPath = try! NSRegularExpression(pattern: "\"file_path\"\\s*:\\s*\"([^\"]+)\"")

    /// Claude Code v uporabniška sporočila vstavlja dolge <system-reminder> bloke (CLAUDE.md, okolje …),
    /// ki bi sami napolnili 8K kontekst. Izjema: datoteke, omenjene z @ (Claude Code jih pošlje kot rezultat
    /// orodja Read) — njihova vsebina ostane, brez številk vrstic, z imenom datoteke.
    static func stripReminders(_ s: String) -> String {
        var out = s
        for m in reminder.matches(in: s, range: NSRange(s.startIndex..., in: s)).reversed() {
            guard let r = Range(m.range, in: out) else { continue }
            let block = String(out[r])
            var keep = ""
            if block.contains("Called the Read tool with the following input:"),
               let p = readPath.firstMatch(in: block, range: NSRange(block.startIndex..., in: block)),
               let pr = Range(p.range(at: 1), in: block) {
                keep = "Attached file \(block[pr]):"
            } else if let start = block.range(of: "Result of calling the Read tool:") {
                let body = String(block[start.upperBound...].dropLast("</system-reminder>".count))
                let text = lineNumbers.stringByReplacingMatches(in: body, range: NSRange(body.startIndex..., in: body), withTemplate: "")
                keep = "```\n" + text.trimmingCharacters(in: .whitespacesAndNewlines) + "\n```"
            }
            out.replaceSubrange(r, with: keep)
        }
        return out.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// Najnovejše izmenjave, ki skupaj z navodili in zadnjim sporočilom ostanejo v proračunu.
    static func fit(_ turns: [ClaudeCLI.Turn], instructions: String, budget: Int,
                    count: (String) async -> Int) async -> (history: [ClaudeCLI.Turn], last: String, truncated: Bool) {
        var last = turns.last?.text ?? ""
        var used = await count(instructions)
        let lastCount = await count(last)
        var truncated = false
        if used + lastCount > budget {
            // ponytail: obdrži konec sporočila (vprašanje je običajno zadaj); razumnejši povzetek, če bo treba
            let keep = max(0, Int(Double(last.count) * Double(max(budget - used, 0)) / Double(max(lastCount, 1)) * 0.9))
            last = String(last.suffix(keep))
            truncated = true
        }
        used += await count(last)

        var history: [ClaudeCLI.Turn] = []
        for t in turns.dropLast().reversed() {
            let n = await count(t.text)
            if used + n > budget { truncated = true; break }
            used += n
            history.insert(t, at: 0)
        }
        while history.first?.role == "assistant" { history.removeFirst() }  // prepis se začne z uporabnikom
        return (history, last, truncated)
    }
}

extension Router {
    func runFM(_ req: HTTPRequest, body: [String: Any], rid: String, writer: ResponseWriter) async -> Int {
        let model = SystemLanguageModel.default
        guard case .available = model.availability else {
            return await error(503, "api_error", "Apple Foundation Models unavailable: \(model.availability)", rid: rid, writer: writer)
        }
        let turns = ClaudeCLI.turns(body)
            .map { ClaudeCLI.Turn(role: $0.role, text: AppleFM.stripReminders($0.text)) }
            .filter { !$0.text.isEmpty }
        guard turns.last?.role == "user" else {
            return await error(400, "invalid_request_error", "Last message must be from the user", rid: rid, writer: writer)
        }
        let instructions = config.appleFM.instructions
        let fit = await AppleFM.fit(turns, instructions: instructions, budget: model.contextSize - AppleFM.responseReserve) { s in
            (try? await model.tokenCount(for: s)) ?? s.utf8.count / 3
        }

        var entries: [Transcript.Entry] = [.instructions(Transcript.Instructions(segments: [.text(.init(content: instructions))], toolDefinitions: []))]
        for t in fit.history {
            let seg: [Transcript.Segment] = [.text(.init(content: t.text))]
            entries.append(t.role == "user" ? .prompt(Transcript.Prompt(segments: seg)) : .response(Transcript.Response(assetIDs: [], segments: seg)))
        }
        let session = LanguageModelSession(model: model, tools: [], transcript: Transcript(entries: entries))

        let stream = body["stream"] as? Bool == true
        var t = StreamTranslator(model: body["model"] as? String ?? "claude-apple-fm")
        var headers: [(String, String)] = [("request-id", rid)]
        if fit.truncated { headers.append(("x-context-truncated", "true")) }

        var started = false
        do {
            if stream {
                var seen = ""
                for try await snap in session.streamResponse(to: fit.last) {
                    if !started {
                        try await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "text/event-stream"), ("Cache-Control", "no-cache")] + headers)
                                               + t.start().reduce(Data(), +))
                        started = true
                    }
                    let content = snap.content
                    guard content.count > seen.count, content.hasPrefix(seen) else { seen = content; continue }
                    let d = String(content.dropFirst(seen.count))
                    seen = content
                    try await writer.write(t.emit(d))
                }
                if !started { try await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "text/event-stream")] + headers) + t.start().reduce(Data(), +)) }
                t.usage = ["output_tokens": (try? await model.tokenCount(for: t.text)) ?? 0]
                try await writer.write(t.finish().reduce(Data(), +))
                return 200
            }
            let r = try await session.respond(to: fit.last)
            _ = t.emit(r.content)
            t.usage = ["output_tokens": (try? await model.tokenCount(for: r.content)) ?? 0]
            return await json(200, t.message(), rid: rid, writer: writer)
        } catch {
            Self.log.error("\(rid, privacy: .public) apple-fm failed: \(String(describing: error), privacy: .public)")
            let (status, type) = Self.fmStatus(error)
            if started {
                try? await writer.write(StreamTranslator.sse("error", ["type": "error", "error": ["type": type, "message": "Apple FM: \(error)"]]))
                return status
            }
            return await self.error(status, type, "Apple FM: \(error)", rid: rid, writer: writer)
        }
    }

    static func fmStatus(_ error: Error) -> (Int, String) {
        guard let e = error as? LanguageModelError else { return (500, "api_error") }
        switch e {
        case .contextSizeExceeded, .guardrailViolation, .refusal, .unsupportedCapability, .unsupportedTranscriptContent,
             .unsupportedGenerationGuide, .unsupportedLanguageOrLocale:
            return (400, "invalid_request_error")
        case .rateLimited: return (429, "rate_limit_error")
        case .timeout: return (504, "api_error")
        @unknown default: return (500, "api_error")
        }
    }
}
