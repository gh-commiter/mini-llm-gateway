import Foundation

// Buffer usage objects only; skip response text and tool payloads, including in SSE.
struct UsageMetrics {
    var cacheUsed = false
    var cacheRead: Int?
    var cacheWrite: Int?
    var input: Int?
    var output: Int?
    var cost: Int64?
    private var disabled = false
    private var lineStart = true
    private var previousCR = false
    private var sse = false
    private var skipLine = false
    private var prefix: [UInt8] = []
    private var stack: [String] = []
    private var key = ""
    private var word: [UInt8] = []
    private var quoted = false, escape = false, keyString = false
    private var capture: [UInt8] = []
    private var captureDepth = 0

    mutating func feed(_ data: Data) {
        for byte in data {
            if byte == 10 && previousCR { previousCR = false; continue }
            previousCR = byte == 13
            let b: UInt8 = byte == 13 ? 10 : byte
            if lineStart && (b == 13 || b == 10) {
                if b == 10 && sse {
                    // Each SSE event has its own JSON document, including after a broken frame.
                    disabled = false; quoted = false; escape = false; keyString = false
                    stack = []; word = []; capture = []; prefix = []; key = ""; captureDepth = 0
                }
                continue
            }
            if skipLine {
                if b == 10 { skipLine = false; lineStart = true }
                continue
            }
            if lineStart {
                prefix.append(b)
                if let field = ["event:", "id:", "retry:", ":"].first(where: { Array($0.utf8).starts(with: prefix) }) {
                    if prefix.count == field.utf8.count { prefix = []; skipLine = true; lineStart = false }
                    continue
                }
                if Array("data:".utf8).starts(with: prefix) {
                    if prefix.count == 5 { sse = true; prefix = []; lineStart = false }
                    continue
                }
                let p = prefix; prefix = []; lineStart = false; scan(Data(p))
            } else { scan(Data([b])) }
            if b == 10 { lineStart = true }
        }
    }

    private mutating func scan(_ data: Data) {
        guard !disabled else { return }
        for b in data {
            if captureDepth > 0 && capture.count < 65537 { capture.append(b) }
            if quoted {
                if keyString && word.count < 512 { word.append(b) }
                if escape { escape = false; continue }
                if b == 92 { escape = true; continue }
                if b == 34 {
                    quoted = false
                    if keyString, let k = try? JSONSerialization.jsonObject(with: Data(word), options: [.fragmentsAllowed]) as? String { key = k }
                    word = []
                }
                continue
            }
            switch b {
            case 34:
                quoted = true; keyString = captureDepth == 0 && !stack.isEmpty && key.isEmpty
                if keyString { word = [34] }
            case 123, 91:
                if b == 123 && key == "usage" && (stack.count == 1 || stack.count == 2 && stack[1] == "message") && captureDepth == 0 {
                    capture = [123]; captureDepth = stack.count + 1
                }
                guard stack.count < 128 else { disabled = true; stack = []; capture = []; captureDepth = 0; return }
                stack.append(key); key = ""
            case 125, 93:
                if captureDepth == stack.count && captureDepth > 0 {
                    if capture.count <= 65536 { readUsage(Data(capture)) } else { cacheUsed = true }
                    capture = []; captureDepth = 0
                }
                if !stack.isEmpty { stack.removeLast() }; key = ""
            case 44: key = ""
            default: break
            }
        }
    }

    private mutating func readUsage(_ data: Data) {
        // Decode fields separately so an invalid cost or counter cannot hide another valid metric.
        struct Input: Decodable { let input_tokens: Int? }
        struct Output: Decodable { let output_tokens: Int? }
        struct Object: Decodable {}
        struct CacheRead: Decodable { let cache_read_input_tokens: Int? }
        struct CacheWrite: Decodable { let cache_creation_input_tokens: Int? }
        if let u = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
            for key in ["cache_read_input_tokens", "cache_creation_input_tokens", "cache_creation", "server_tool_use", "prompt_tokens_details", "input_tokens_details"] {
                guard let value = u[key], !(value is NSNull) else { continue }
                if Self.zeroDimension(value) { continue }
                cacheUsed = true
            }
        }
        let decoder = JSONDecoder()
        if let n = (try? decoder.decode(CacheRead.self, from: data))?.cache_read_input_tokens, n >= 0 { cacheRead = max(cacheRead ?? 0, n) }
        if let n = (try? decoder.decode(CacheWrite.self, from: data))?.cache_creation_input_tokens, n >= 0 { cacheWrite = max(cacheWrite ?? 0, n) }
        if let n = (try? decoder.decode(Input.self, from: data))?.input_tokens, n >= 0 { input = max(input ?? 0, n) }
        if let n = (try? decoder.decode(Output.self, from: data))?.output_tokens, n >= 0 { output = max(output ?? 0, n) }
        if (try? decoder.decode(Object.self, from: data)) != nil,
           let raw = Self.costNumber(data), let nanos = Self.costNanos(raw), nanos <= Self.maxEventCostNanos {
            cost = nanos
        }
    }

    private static func zeroDimension(_ value: Any) -> Bool {
        if value is NSNull { return true }
        if let n = value as? NSNumber { return n.doubleValue == 0 }
        if let d = value as? [String: Any] { return d.values.allSatisfy { zeroDimension($0) } }
        return false
    }

    /// A reported cost above 1,000,000 USD per event is stored as unknown, so sums cannot overflow.
    static let maxEventCostNanos: Int64 = 1_000_000_000_000_000

    // Keep the number's spelling: binary floating point and Decimal can lose the rounding boundary.
    private static func costNumber(_ data: Data) -> String? {
        let bytes = Array(data)
        var depth = 0, i = 0
        var found: String?  // a repeated "cost" key: the last one wins, as in the Go gateway
        while i < bytes.count {
            let b = bytes[i]
            if b == 34 {
                let start = i
                i += 1
                while i < bytes.count {
                    if bytes[i] == 92 { i += 2; continue }
                    if bytes[i] == 34 { break }
                    i += 1
                }
                let end = min(i + 1, bytes.count)
                if depth == 1,
                   let key = try? JSONDecoder().decode(String.self, from: Data(bytes[start..<end])), key == "cost" {
                    var j = end
                    while j < bytes.count && [9, 10, 13, 32].contains(bytes[j]) { j += 1 }
                    if j < bytes.count && bytes[j] == 58 {
                        j += 1
                        while j < bytes.count && [9, 10, 13, 32].contains(bytes[j]) { j += 1 }
                        let first = j
                        while j < bytes.count && ![9, 10, 13, 32, 44, 125].contains(bytes[j]) { j += 1 }
                        found = String(decoding: bytes[first..<j], as: UTF8.self)
                    }
                }
            } else if b == 123 || b == 91 { depth += 1
            } else if b == 125 || b == 93 { depth -= 1 }
            i += 1
        }
        return found
    }

    private static func costNanos(_ raw: String) -> Int64? {
        guard raw.utf8.count <= 128 else { return nil }
        let parts = raw.lowercased().split(separator: "e", omittingEmptySubsequences: false)
        guard parts.count <= 2, let exponent = parts.count == 2 ? Int(parts[1]) : 0,
              (-400...400).contains(exponent) else { return nil }
        var mantissa = String(parts[0])
        let negative = mantissa.hasPrefix("-")
        if negative { mantissa.removeFirst() }
        let decimal = mantissa.split(separator: ".", omittingEmptySubsequences: false)
        guard decimal.count <= 2 else { return nil }
        let digits = decimal.joined()
        guard !digits.isEmpty, digits.utf8.allSatisfy({ (48...57).contains($0) }) else { return nil }
        let significant = String(digits.drop(while: { $0 == "0" }))
        if significant.isEmpty { return 0 }
        guard !negative else { return nil }
        let shift = exponent + 9 - (decimal.count == 2 ? decimal[1].count : 0)
        let end = significant.count + shift
        guard end <= 19 else { return nil }
        if end < 0 { return 0 }
        let whole = end == 0 ? "0" : String(significant.prefix(end)) + String(repeating: "0", count: max(0, shift))
        guard let n = Int64(whole) else { return nil }
        let roundUp = end < significant.count && significant[significant.index(significant.startIndex, offsetBy: end)] >= "5"
        if roundUp { return n == Int64.max ? nil : n + 1 }
        return n
    }
}


/// OpenRouter is recognised by the host of the provider base URL (openrouter.ai and subdomains), not by its configured name.
func spendingLimit(baseURL: String, body: Data) -> Bool {
    let host = URL(string: baseURL)?.host?.lowercased() ?? ""
    guard host == "openrouter.ai" || host.hasSuffix(".openrouter.ai"),
          let o = try? JSONSerialization.jsonObject(with: body) as? [String: Any],
          let e = o["error"] as? [String: Any] else { return false }
    let code = e["code"] as? String ?? ""
    let msg = (e["message"] as? String ?? "").lowercased()
    return ["spending_limit_reached", "spending_limit_exceeded"].contains(code) || msg.contains("spending limit reached") || msg.contains("spending limit exceeded") || msg.contains("key limit exceeded")
}

/// Removes the configured provider key and anything shaped like a bearer token or an sk- key from upstream text.
func redactSecrets(_ text: String, key: String) -> String {
    var s = key.isEmpty ? text : text.replacingOccurrences(of: key, with: "[redacted]")
    let re = try! NSRegularExpression(pattern: #"bearer\s+[A-Za-z0-9._~+/=-]+|sk-[A-Za-z0-9_-]{8,}"#, options: .caseInsensitive)
    s = re.stringByReplacingMatches(in: s, range: NSRange(s.startIndex..., in: s), withTemplate: "[redacted]")
    return s
}
