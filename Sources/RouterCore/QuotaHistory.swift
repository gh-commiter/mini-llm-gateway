import Foundation

public enum QuotaHistory {
    /// One line per probe, with all its measured windows on that line.
    public static func text(_ data: Data) -> String {
        guard let r = JSONValue.parse(data)?.dict else { return "" }
        var out = ""
        for p in r["probes"]?.array ?? [] {
            let d = p.dict
            var parts = [d["probedat"]?.string ?? "", d["provider"]?.string ?? ""]
            if d["status"]?.string == "error" { parts.append(d["errorkind"]?.string ?? "") }
            else {
                for v in d["values"]?.array ?? [] {
                    let w = v.dict
                    let value: String
                    if case .num(let n)? = w["value"] { value = GoText.f(n.doubleValue) } else { value = "-" }
                    parts.append((w["name"]?.string ?? "") + " " + value + " " + (w["unit"]?.string ?? "") + " resets " + (w["resetsat"]?.string ?? "-"))
                }
            }
            out += clean(parts.joined(separator: "  ")) + "\n"
        }
        for g in r["gaps"]?.array ?? [] {
            out += "gap  " + clean(g.dict["from"]?.string ?? "") + "  " + clean(g.dict["to"]?.string ?? "") + "\n"
        }
        return out
    }
}
