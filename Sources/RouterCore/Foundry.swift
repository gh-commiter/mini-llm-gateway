import Foundation

/// Ponudnik Foundry Local (Microsoft): modeli na tem Macu prek lokalnega strežnika, združljivega z OpenAI, poleg
/// Apple Foundation Models; kot `minigw serve` (client/foundry.go). `claude-foundry-<model>`, le pogovor.
/// Vrata iz `foundry server status` (le številka; gostitelj je vedno 127.0.0.1), preverjena z GET /openai/status;
/// brez posrednika in preusmeritev, z omejenim časom in velikostjo odgovora.
public struct FoundryConfig: Codable, Sendable, Equatable {
    public var enabled = false
    public var executable = ""            // prazno = samodejno iskanje
    public var enabledModels: [String]?   // nil = vsi preneseni modeli
    public var instructions = AppleFMConfig().instructions
    public init() {}

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = FoundryConfig()
        enabled = try c.decodeIfPresent(Bool.self, forKey: .enabled) ?? d.enabled
        executable = try c.decodeIfPresent(String.self, forKey: .executable) ?? d.executable
        enabledModels = try c.decodeIfPresent([String].self, forKey: .enabledModels)
        instructions = try c.decodeIfPresent(String.self, forKey: .instructions) ?? d.instructions
    }

    public func isModelEnabled(_ m: String) -> Bool { enabledModels?.contains(m) ?? true }

    public mutating func setModel(_ m: String, _ on: Bool, all: [String]) {
        var list = enabledModels ?? all
        list.removeAll { $0 == m }
        if on { list.append(m) }
        enabledModels = Set(list) == Set(all) ? nil : list
    }
}

public enum Foundry {
    static let prefix = "claude-foundry-"
    static let maxBody = 8 << 20
    static let install = "Foundry Local is not installed: brew tap microsoft/foundrylocal && brew install foundrylocal"
    nonisolated(unsafe) static var timeout: TimeInterval = 600  // testi: krajši rok

    public struct Model: Sendable, Equatable { public let name: String, vision: Bool }

    public static func executable(_ configured: String) async -> String? {
        configured.isEmpty ? await offPool { ClaudeCLI.locate("foundry") }
            : [(configured as NSString).expandingTildeInPath].first { FileManager.default.isExecutableFile(atPath: $0) }
    }

    /// Vrata iz izpisa CLI (le lokalni URL).
    static func port(_ out: String) -> Int? {
        let rx = try! NSRegularExpression(pattern: #"https?://(?:localhost|127\.0\.0\.1|\[::1\]):(\d{2,5})"#)
        guard let m = rx.firstMatch(in: out, range: NSRange(out.startIndex..., in: out)), let r = Range(m.range(at: 1), in: out),
              let p = Int(out[r]), (1...65535).contains(p) else { return nil }
        return p
    }

    /// Imena modelov: seznam nizov ali objektov z name/id (oblika ni dokumentirana).
    static func names(_ data: Data) -> [String] {
        let obj = try? JSONSerialization.jsonObject(with: data)
        let list = (obj as? [Any]) ?? ((obj as? [String: Any]).map { ($0["data"] as? [Any] ?? []) + ($0["models"] as? [Any] ?? []) } ?? [])
        return list.compactMap { x in
            if let s = x as? String { return s }
            guard let d = x as? [String: Any] else { return nil }
            return ["name", "id", "modelId"].lazy.compactMap { d[$0] as? String }.first { !$0.isEmpty }
        }
    }

    static func stop(_ reason: String?) -> String { reason == "length" ? "max_tokens" : "end_turn" }

    public struct RequestError: Error { public let message: String }

    /// Anthropic Messages → OpenAI chat completions (kot foundryRequest v Go): le pogovor, navodila iz nastavitev.
    static func request(_ body: [String: Any], model: String, instructions: String, vision: Bool, stream: Bool) throws -> [String: Any] {
        if let tc = body["tool_choice"] as? [String: Any], let t = tc["type"] as? String, t == "any" || t == "tool" {
            throw RequestError(message: "Foundry Local models here take no tools: tool_choice \(t) cannot be met")
        }
        var turns = ClaudeCLI.turns(body).map { ClaudeCLI.Turn(role: $0.role, text: AppleFM.stripReminders($0.text)) }.filter { !$0.text.isEmpty }
        guard turns.last?.role == "user" else { throw RequestError(message: "Last message must be from the user") }
        while turns.count > 1, turns.first?.role == "assistant" { turns.removeFirst() }
        var msgs: [[String: Any]] = [["role": "system", "content": instructions]]
        for (i, t) in turns.enumerated() {
            var content: Any = t.text
            if i == turns.count - 1 {
                let imgs = lastImages(body)
                if !imgs.isEmpty {
                    content = vision ? [["type": "text", "text": t.text]] + imgs.map { ["type": "image_url", "image_url": ["url": $0]] as [String: Any] }
                        : t.text + "\n\n[\(imgs.count) image(s) left out: this model reads text only]"
                }
            }
            msgs.append(["role": t.role, "content": content])
        }
        var out: [String: Any] = ["model": model, "messages": msgs, "stream": stream]
        if let n = body["max_tokens"] as? Int, n > 0 { out["max_completion_tokens"] = min(n, 32768) }
        for k in ["temperature", "top_p"] { if let v = body[k] as? Double { out[k] = v } }
        if let s = body["stop_sequences"] as? [String], !s.isEmpty { out["stop"] = s }
        if stream { out["stream_options"] = ["include_usage": true] }
        return out
    }

    static func lastImages(_ body: [String: Any]) -> [String] {
        guard let last = (body["messages"] as? [[String: Any]])?.last, let blocks = last["content"] as? [[String: Any]] else { return [] }
        return blocks.compactMap { b in
            guard b["type"] as? String == "image", let src = b["source"] as? [String: Any], src["type"] as? String == "base64",
                  let mt = src["media_type"] as? String, mt.hasPrefix("image/"), !mt.contains(";"), !mt.contains(","),
                  let data = src["data"] as? String, Data(base64Encoded: data) != nil else { return nil }
            return "data:\(mt);base64,\(data)"
        }
    }

    /// Le 127.0.0.1, brez posrednika in brez preusmeritev.
    final class NoRedirect: NSObject, URLSessionTaskDelegate {
        func urlSession(_ s: URLSession, task: URLSessionTask, willPerformHTTPRedirection r: HTTPURLResponse, newRequest: URLRequest) async -> URLRequest? { nil }
    }
    nonisolated(unsafe) static var session: URLSession = {  // testi: URLProtocol
        let c = URLSessionConfiguration.ephemeral
        c.connectionProxyDictionary = [:]
        c.timeoutIntervalForRequest = 300
        return URLSession(configuration: c, delegate: NoRedirect(), delegateQueue: nil)
    }()

    static func get(_ url: URL, timeout: TimeInterval) async throws -> Data {
        var r = URLRequest(url: url)
        r.timeoutInterval = timeout
        let (d, resp) = try await session.data(for: r)
        guard (resp as? HTTPURLResponse)?.statusCode == 200 else { throw RequestError(message: "HTTP \((resp as? HTTPURLResponse)?.statusCode ?? 0)") }
        guard d.count <= maxBody else { throw RequestError(message: "answer too large") }
        return d
    }

    /// CLI z rokom (status, start).
    /// F1: »start« brez cevi (strežnik v ozadju bi jo držal odprto); drugi ukazi berejo izpis le do roka, tudi če cev
    /// drži otrok. Vrne izpis (največ 64 KB); pri »start« prazno ob uspehu, »failed« sicer.
    static func run(_ exe: String, _ args: [String], timeout: TimeInterval) -> String {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        p.standardInput = FileHandle.nullDevice
        let exited = DispatchSemaphore(value: 0)
        p.terminationHandler = { _ in exited.signal() }
        if args.last == "start" {
            p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
            guard (try? p.run()) != nil else { return "failed" }
            if exited.wait(timeout: .now() + timeout) == .timedOut { p.terminate(); return "failed" }
            return p.terminationStatus == 0 ? "" : "failed"
        }
        final class Box: @unchecked Sendable { let lock = NSLock(); var data = Data() }
        let out = Pipe(), box = Box(), eof = DispatchSemaphore(value: 0)
        p.standardOutput = out; p.standardError = out
        out.fileHandleForReading.readabilityHandler = { h in
            let d = h.availableData
            if d.isEmpty { h.readabilityHandler = nil; eof.signal(); return }
            box.lock.withLock { if box.data.count < 64 << 10 { box.data.append(d) } }
        }
        guard (try? p.run()) != nil else { out.fileHandleForReading.readabilityHandler = nil; return "" }
        let deadline = DispatchTime.now() + timeout
        if eof.wait(timeout: deadline) == .timedOut || exited.wait(timeout: deadline) == .timedOut {
            out.fileHandleForReading.readabilityHandler = nil
            if p.isRunning { p.terminate() }
        }
        return box.lock.withLock { String(decoding: box.data.prefix(64 << 10), as: UTF8.self) }
    }

    /// F7: ime gre v pot /openai/load/<ime>: brez »/« in »..«.
    static func validModel(_ m: String) -> Bool { Router.validModel(m) && !m.contains("/") && !m.contains("..") }

    /// F5: število žetonov le v 0…1e9.
    static func count(_ v: Any?) -> Int? { (v as? Int).flatMap { (0...1_000_000_000).contains($0) ? $0 : nil } }
}

/// Najdena vrata, modeli in naloženi modeli (predpomnjeno).
public actor FoundryHub {
    private var base: URL?
    private var checked = Date.distantPast
    private var models: [Foundry.Model]?
    private var modelsAt = Date.distantPast
    private var loaded: [String: Date] = [:]
    private(set) var lastError = ""
    nonisolated(unsafe) static var command: @Sendable (String, [String], TimeInterval) -> String = Foundry.run  // testi

    var running: Bool { base != nil }

    private var retryAt = Date.distantPast  // F6: po neuspehu 30 s brez novega iskanja ali zagona
    nonisolated(unsafe) static var listenerIsOurs: @Sendable (UInt16) -> Bool = { PeerProcess.listenerIsOurs(port: $0) }  // testi

    func endpoint(_ exe: String?) async throws -> URL {
        if let base, Date().timeIntervalSince(checked) < 60 { return base }
        if Date() < retryAt { throw Foundry.RequestError(message: lastError) }
        func fail(_ m: String) -> Foundry.RequestError { base = nil; lastError = m; retryAt = Date().addingTimeInterval(30); return Foundry.RequestError(message: m) }
        guard let exe else { throw fail(Foundry.install) }
        var foreign = false
        func find() async -> URL? {
            for sub in ["server", "service"] {
                let out = await offPool { Self.command(exe, [sub, "status"], 20) }
                guard let p = Foundry.port(out), let u = URL(string: "http://127.0.0.1:\(p)") else { continue }
                guard let d = try? await Foundry.get(u.appendingPathComponent("openai/status"), timeout: 5),
                      (try? JSONSerialization.jsonObject(with: d) as? [String: Any])?["Endpoints"] is [Any] else { continue }
                guard Self.listenerIsOurs(UInt16(p)) else { foreign = true; continue }  // F2: vrata drugega uporabnika
                return u
            }
            return nil
        }
        var u = await find()
        if u == nil, !foreign {
            for sub in ["server", "service"] {  // F6: »service« le, če »server« ni uspel
                _ = await offPool { Self.command(exe, [sub, "start"], 120) }
                if let found = await find() { u = found; break }
            }
        }
        guard let u else {
            throw fail(foreign ? "Foundry Local's port belongs to another user's process: not used" : "Foundry Local does not answer (foundry server status)")
        }
        base = u; checked = Date(); lastError = ""; retryAt = .distantPast
        return u
    }

    public func list(_ exe: String?) async throws -> [Foundry.Model] {
        let u = try await endpoint(exe)
        if let models, Date().timeIntervalSince(modelsAt) < 300 { return models }
        let names = Foundry.names(try await Foundry.get(u.appendingPathComponent("openai/models"), timeout: 30))
        var vision: [String: Bool] = [:]
        if let d = try? await Foundry.get(u.appendingPathComponent("foundry/list"), timeout: 30),
           let cat = try? JSONSerialization.jsonObject(with: d) as? [[String: Any]] {
            for m in cat {
                let info = [m["inputModalities"], m["task"], m["modalities"]].map { "\($0 ?? "")" }.joined().lowercased()
                if let n = m["name"] as? String { vision[n] = info.contains("image") }
            }
        }
        let out = names.filter(Foundry.validModel).map { Foundry.Model(name: $0, vision: vision[$0] ?? false) }
        models = out; modelsAt = Date()
        return out
    }

    func needsLoad(_ m: String) -> Bool { Date().timeIntervalSince(loaded[m] ?? .distantPast) > 60 }
    func didLoad(_ m: String) { loaded[m] = Date() }
    func forget() { base = nil; models = nil }
}

extension Router {
    public static let foundry = FoundryHub()

    func foundryModelList(created: String) async -> [[String: Any]] {
        guard config.foundry.enabled, let ms = try? await Self.foundry.list(await Foundry.executable(config.foundry.executable)) else { return [] }
        return ms.filter { config.foundry.isModelEnabled($0.name) }.map { m in
            ["type": "model", "id": Foundry.prefix + m.name, "display_name": "\(m.name) · Foundry Local",
             "description": "Foundry Local · on this Mac · no tools", "created_at": created]
        }
    }

    func runFoundry(_ req: HTTPRequest, body: [String: Any], model: String, rid: String, writer: ResponseWriter) async -> Int {
        let fc = config.foundry, exe = await Foundry.executable(fc.executable)
        let models: [Foundry.Model]
        do { models = try await Self.foundry.list(exe) } catch { return await self.error(503, "api_error", "\(error)", rid: rid, writer: writer) }
        guard let m = models.first(where: { $0.name == model }), fc.isModelEnabled(model) else {
            return await error(404, "not_found_error", "Unknown model: \(Foundry.prefix)\(model)", rid: rid, writer: writer)
        }
        let stream = body["stream"] as? Bool == true
        let oreq: [String: Any]
        do { oreq = try Foundry.request(body, model: model, instructions: fc.instructions, vision: m.vision, stream: stream) } catch {
            return await self.error(400, "invalid_request_error", (error as? Foundry.RequestError)?.message ?? "\(error)", rid: rid, writer: writer)
        }
        let deadline = Date().addingTimeInterval(Foundry.timeout)
        do {
            let base = try await Self.foundry.endpoint(exe)
            if await Self.foundry.needsLoad(model) {
                _ = try await Foundry.get(base.appendingPathComponent("openai/load").appendingPathComponent(model), timeout: Foundry.timeout)
                await Self.foundry.didLoad(model)
            }
            var r = URLRequest(url: base.appendingPathComponent("v1/chat/completions"))
            r.httpMethod = "POST"
            r.setValue("application/json", forHTTPHeaderField: "Content-Type")
            r.httpBody = try JSONSerialization.data(withJSONObject: oreq)
            r.timeoutInterval = Foundry.timeout
            var t = StreamTranslator(model: Foundry.prefix + model)
            func usage(_ u: [String: Any]?) {
                guard let u else { return }
                if let n = Foundry.count(u["prompt_tokens"]) { t.usage["input_tokens"] = n }  // F5
                if let n = Foundry.count(u["completion_tokens"]) { t.usage["output_tokens"] = n }
            }
            if !stream {
                let (d, resp) = try await Foundry.session.data(for: r)
                guard (resp as? HTTPURLResponse)?.statusCode == 200, d.count <= Foundry.maxBody,
                      let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any],
                      let c = (o["choices"] as? [[String: Any]])?.first else {
                    Self.log.error("\(rid, privacy: .public) foundry: HTTP \((resp as? HTTPURLResponse)?.statusCode ?? 0)")
                    return await error(502, "api_error", "Foundry Local gave no answer (see the log)", rid: rid, writer: writer)
                }
                _ = t.emit((c["message"] as? [String: Any])?["content"] as? String ?? "")
                t.stop = Foundry.stop(c["finish_reason"] as? String)
                usage(o["usage"] as? [String: Any])
                return await json(200, t.message(), rid: rid, writer: writer)
            }
            let (bytes, resp) = try await Foundry.session.bytes(for: r)
            guard (resp as? HTTPURLResponse)?.statusCode == 200 else {
                return await error(502, "api_error", "Foundry Local answered HTTP \((resp as? HTTPURLResponse)?.statusCode ?? 0)", rid: rid, writer: writer)
            }
            try await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "text/event-stream"), ("Cache-Control", "no-cache"), ("request-id", rid)])
                                   + t.start().reduce(Data(), +))
            var total = 0
            for try await line in bytes.lines {
                total += line.utf8.count
                guard total <= Foundry.maxBody, line.utf8.count <= 1 << 20, Date() < deadline else {  // F4: tudi vrstica do 1 MB
                    try await writer.write(StreamTranslator.sse("error", ["type": "error", "error": ["type": "api_error", "message": "Foundry Local: answer too large or too slow"]]))
                    return 502
                }
                guard line.hasPrefix("data:") else { continue }
                let data = line.dropFirst(5).trimmingCharacters(in: .whitespaces)
                if data == "[DONE]" { break }
                guard let o = try? JSONSerialization.jsonObject(with: Data(data.utf8)) as? [String: Any] else { continue }
                usage(o["usage"] as? [String: Any])
                for c in o["choices"] as? [[String: Any]] ?? [] {
                    if let s = (c["delta"] as? [String: Any])?["content"] as? String, !s.isEmpty { try await writer.write(t.emit(s)) }
                    if let f = c["finish_reason"] as? String { t.stop = Foundry.stop(f) }
                }
            }
            if (t.usage["output_tokens"] as? Int ?? 0) == 0 { t.usage["output_tokens"] = t.text.utf8.count / 4 }
            try await writer.write(t.finish().reduce(Data(), +))
            return 200
        } catch {
            await Self.foundry.forget()
            Self.log.error("\(rid, privacy: .public) foundry failed: \(String(describing: error), privacy: .public)")
            return await self.error(502, "api_error", "Foundry Local: \((error as? Foundry.RequestError)?.message ?? error.localizedDescription)", rid: rid, writer: writer)
        }
    }
}
