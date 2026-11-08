import Foundation
import RouterCore

// minigw v agentu prehoda (MINIGW_AGENT_URL, MINIGW_AGENT_AUTH; AgentRun.swift): -p pošlje vprašanje naravnost na vrata
// izvajanja in izpiše odgovor, brez vgnezdenega Claude Code. Kot agentask.go.

/// HTTP/1.0 na vrata izvajanja: neposredno ali prek posrednika sandboxa Claude Code (HTTP_PROXY), ki je tam edina pot do
/// loopbacka (URLSession posrednika iz okolja ne upošteva). `onHead` dobi status, `onData` telo po kosih; false = ni povezave.
func agentHTTP(_ c: Connection, _ method: String, _ path: String, body: Data? = nil,
               onHead: (Int) -> Void = { _ in }, onData: (Data) -> Void) -> Bool {
    guard let target = URLComponents(string: c.url + path), let th = target.host, let tp = target.port else { return false }
    let env = ProcessInfo.processInfo.environment
    let proxy = (env["HTTP_PROXY"] ?? env["http_proxy"]).flatMap { $0.isEmpty ? nil : URLComponents(string: $0) }
    let host = proxy?.host ?? th, port = proxy?.port ?? tp
    var hints = addrinfo()
    hints.ai_socktype = SOCK_STREAM
    var res: UnsafeMutablePointer<addrinfo>?
    guard getaddrinfo(host, String(port), &hints, &res) == 0 else { return false }
    defer { freeaddrinfo(res) }
    var fd: Int32 = -1
    var ai = res
    while let a = ai {
        fd = socket(a.pointee.ai_family, a.pointee.ai_socktype, a.pointee.ai_protocol)
        if fd >= 0, connect(fd, a.pointee.ai_addr, a.pointee.ai_addrlen) == 0 { break }
        if fd >= 0 { close(fd); fd = -1 }
        ai = a.pointee.ai_next
    }
    guard fd >= 0 else { return false }
    defer { close(fd) }
    var head = "\(method) \(proxy == nil ? path : c.url + path) HTTP/1.0\r\nHost: \(th):\(tp)\r\nAuthorization: Bearer \(c.token)\r\n"
        + "anthropic-version: 2023-06-01\r\nContent-Type: application/json\r\nContent-Length: \(body?.count ?? 0)\r\n"
    if let p = proxy, let u = p.user {
        head += "Proxy-Authorization: Basic \(Data((u + ":" + (p.password ?? "")).utf8).base64EncodedString())\r\n"
    }
    var out = Data((head + "\r\n").utf8) + (body ?? Data())
    while !out.isEmpty {
        let n = out.withUnsafeBytes { write(fd, $0.baseAddress, $0.count) }
        guard n > 0 else { return false }
        out = out.dropFirst(n)
    }
    var buf = [UInt8](repeating: 0, count: 64 << 10)
    var pending = Data(), headDone = false
    while true {
        let n = read(fd, &buf, buf.count)
        guard n > 0 else { break }
        if headDone { onData(Data(buf[0..<n])); continue }
        pending.append(contentsOf: buf[0..<n])
        guard let sep = pending.range(of: Data("\r\n\r\n".utf8)) else { continue }
        let status = String(decoding: pending[..<sep.lowerBound], as: UTF8.self).split(separator: " ").dropFirst().first.flatMap { Int($0) } ?? 0
        onHead(status)
        headDone = true
        if sep.upperBound < pending.endIndex { onData(pending[sep.upperBound...]) }
    }
    return headDone
}

/// Telo zahteve; modeli agentov (claude-cli, codex …) delajo v mapi le z orodji, zato eno navidezno.
func agentBody(_ model: String, _ question: String) -> Data {
    var body: [String: Any] = ["model": model, "max_tokens": 32000, "stream": true, "messages": [["role": "user", "content": question]]]
    if Profiles.isAgentModel(model) {
        body["tools"] = [["name": "none", "description": "Not used.", "input_schema": ["type": "object"]]]
    }
    return (try? JSONSerialization.data(withJSONObject: body)) ?? Data()
}

/// minigw -p --model <id> "vprašanje" (ali vprašanje na stdin); besedilo odgovora sproti na stdout.
func agentAsk(_ c: Connection, _ args: [String]) -> Never {
    var model = "", words: [String] = []
    var i = 0
    while i < args.count {
        let a = args[i]
        switch true {
        case a == "-p" || a == "--print": break
        case a == "--model" && i + 1 < args.count: model = args[i + 1]; i += 1
        case a.hasPrefix("--model="): model = String(a.dropFirst(8))
        case a.hasPrefix("-"): fail("inside a gateway agent, minigw -p takes only --model <id> and the question, not \(a)")
        default: words.append(a)
        }
        i += 1
    }
    guard !model.isEmpty else { fail("usage: minigw -p --model <id> \"question\"   (minigw models lists the models)") }
    var q = words.joined(separator: " ")
    if q.isEmpty { q = String(decoding: FileHandle.standardInput.readDataToEndOfFile(), as: UTF8.self) }
    var status = 0, errBody = Data(), line = Data(), failure: String?
    let ok = agentHTTP(c, "POST", "/v1/messages", body: agentBody(model, q), onHead: { status = $0 }) { chunk in
        guard status == 200 else { if errBody.count < 1 << 20 { errBody.append(chunk) }; return }
        line.append(chunk)
        while let nl = line.firstIndex(of: 10) {
            let l = String(decoding: line[line.startIndex..<nl], as: UTF8.self)
            line = Data(line[(nl + 1)...])
            guard l.hasPrefix("data: "), let e = try? JSONSerialization.jsonObject(with: Data(l.dropFirst(6).utf8)) as? [String: Any] else { continue }
            if e["type"] as? String == "content_block_delta", let d = e["delta"] as? [String: Any], d["type"] as? String == "text_delta",
               let t = d["text"] as? String {
                FileHandle.standardOutput.write(Data(t.utf8))
            } else if e["type"] as? String == "error" {
                failure = (e["error"] as? [String: Any])?["message"] as? String ?? "error"
            }
        }
    }
    guard ok else { fail("cannot reach the gateway (\(c.url))") }
    guard status == 200 else {
        let m = ((try? JSONSerialization.jsonObject(with: errBody)) as? [String: Any]).flatMap { ($0["error"] as? [String: Any])?["message"] as? String }
        fail("HTTP \(status): \(m ?? String(decoding: errBody.prefix(300), as: UTF8.self))")
    }
    print("")
    if let failure { fail(failure) }
    exit(0)
}
