import Foundation
import CoreFoundation

// The shared core validates and prices these decimal USD/token rates with exact arithmetic.
struct PriceRates: Sendable, Equatable {
    let unit = "USD/token"
    let input: String
    let output: String
    var json: [String: String] { ["unit": unit, "input": input, "output": output] }

    static func parse(_ m: [String: Any], openRouter: Bool) -> PriceRates? {
        let i = openRouter ? "prompt" : "input_cost_per_token", o = openRouter ? "completion" : "output_cost_per_token"
        func decimal(_ value: Any?) -> String? {
            if let s = value as? String { return s }
            if let n = value as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID() { return n.stringValue }
            return nil
        }
        func withinRateCeiling(_ s: String) -> Bool {
            let parts = s.lowercased().split(separator: "e", omittingEmptySubsequences: false)
            guard parts.count <= 2, let exponent = parts.count == 2 ? Int(parts[1]) : 0 else { return false }
            let mantissa = parts[0].split(separator: ".", omittingEmptySubsequences: false)
            let digits = mantissa.joined().drop(while: { $0 == "0" })
            if digits.isEmpty { return true }
            let places = digits.count + exponent - (mantissa.count == 2 ? mantissa[1].count : 0)
            return places < 7 || places == 7 && digits.first == "1" && digits.dropFirst().allSatisfy({ $0 == "0" })
        }
        func valid(_ s: String) -> Bool {
            s.utf8.count <= 128 && s.range(of: #"^[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]{1,3})?$"#, options: .regularExpression) != nil
                && withinRateCeiling(s)
        }
        guard let input = decimal(m[i]), let output = decimal(m[o]), valid(input), valid(output) else { return nil }
        for (k, v) in m where k != i && k != o && !k.contains("cache") {
            if openRouter || (k.contains("cost") || k.contains("tier") || k.contains("multiplier")) {
                if v is NSNull { continue }
                guard let n = decimal(v), valid(n), n.split(whereSeparator: { $0 == "e" || $0 == "E" })[0].allSatisfy({ $0 == "0" || $0 == "." }) else { return nil }
            }
        }
        return PriceRates(input: input, output: output)
    }
}

struct PriceSnapshot: Sendable, Equatable {
    let id: String
    let provider: String
    let upstreamModel: String
    let fetchedAt: Date
    let source: String
    let rates: PriceRates
    init(provider: String, model: String, source: String, rates: PriceRates) {
        id = "ps_" + UUID().uuidString.lowercased()
        self.provider = provider; upstreamModel = model; fetchedAt = Date(); self.source = source; self.rates = rates
    }
    var json: [String: Any] {
        let stamp = UsageEvent.iso(fetchedAt)
        return ["id": id, "provider": provider, "upstreamModel": upstreamModel, "fetchedAt": stamp,
                "effectiveDay": String(stamp.prefix(10)), "source": source, "rates": rates.json]
    }
}

/// Only complete input and output rates of the owner's own proxy count; a deployment without them has no price.
func deploymentPrices(_ data: [[String: Any]]) -> [String: PriceRates] {
    let counts = Dictionary(data.compactMap { ($0["model_name"] as? String).map { ($0, 1) } }, uniquingKeysWith: +)
    var out: [String: PriceRates] = [:]
    for m in data {
        guard let name = m["model_name"] as? String, counts[name] == 1 else { continue }
        let params = m["litellm_params"] as? [String: Any] ?? [:]
        var local: [String: Any] = [:]
        for overrides in [m["model_info"] as? [String: Any] ?? [:], params] {
            for (k, v) in overrides where !(v is NSNull) { local[k] = v }
        }
        var merged: [String: Any] = [:]
        for (k, v) in local { merged[k] = v }
        if let p = PriceRates.parse(merged, openRouter: false) { out[name] = p }
    }
    return out
}

// Preserve numeric rate spellings before Foundation can round through Double.
func priceJSON(_ data: Data) -> [String: Any]? {
    let b = Array(data)
    var out: [UInt8] = [], i = 0
    while i < b.count {
        if b[i] != 34 { out.append(b[i]); i += 1; continue }
        let start = i
        i += 1
        while i < b.count {
            if b[i] == 92 { i += 2; continue }
            if b[i] == 34 { i += 1; break }
            i += 1
        }
        guard i <= b.count else { return nil }
        out.append(contentsOf: b[start..<i])
        let key = (try? JSONDecoder().decode(String.self, from: Data(b[start..<i]))) ?? ""
        var j = i
        while j < b.count && [9, 10, 13, 32].contains(b[j]) { j += 1 }
        guard j < b.count, b[j] == 58 else { continue }
        j += 1
        while j < b.count && [9, 10, 13, 32].contains(b[j]) { j += 1 }
        let pricing = key.contains("cost") || key.contains("tier") || key.contains("multiplier") || ["prompt", "completion", "request", "image", "audio", "web_search", "internal_reasoning", "input_cache_read", "input_cache_write"].contains(key)
        if pricing && j < b.count && (b[j] == 45 || (48...57).contains(b[j])) {
            out.append(contentsOf: b[i..<j])
            var end = j
            while end < b.count && ![9, 10, 13, 32, 44, 125, 93].contains(b[end]) { end += 1 }
            out.append(34); out.append(contentsOf: b[j..<end]); out.append(34)
            i = end
        }
    }
    return (try? JSONSerialization.jsonObject(with: Data(out))) as? [String: Any]
}
