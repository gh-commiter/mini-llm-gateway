import Foundation

// minigw gw usage: the provider/model report of GET /usage/events (totals per provider and model, per day, a total line), as text,
// CSV or the unchanged JSON. A port of client/serve_usage_report.go; client/testdata/usage-report.json is asserted here
// (UsageReportTests) and in client/admin_parity_test.go.

public enum UsageReport {
    public static let help = """
        minigw gw usage [--days N] [--provider P] [--model M] [--sort failed] [--json|--csv]   requests, tokens, cost and failed agent time per provider, model and day (UTC)
          minigw gw usage --legacy [--days N] [--device ID] [--folder DIR]   counters per folder, day (UTC) and agent (clear: sessions clear)
        """

    /// Known reported and estimated sums; `unknown` counts events without cost.
    struct Sum {
        var events: Int64 = 0, failed: Int64 = 0, tin: Int64 = 0, tout: Int64 = 0, unknown: Int64 = 0, nanos: Int64 = 0
        var cin: Int64 = 0, cout: Int64 = 0  // input tokens read from / written to the provider's prompt cache
        var errors: Int64 = 0, durationMs: Int64 = 0, failedMs: Int64 = 0, unknownMs: Int64 = 0
        var known = false
        var estimated: Int64 = 0, reported: Int64 = 0
        var estimateKnown = false, reportedKnown = false

        mutating func add(_ r: JDict) {
            let events = r["events"]?.int ?? 0, nocost = r["eventswithoutcost"]?.int ?? 0
            self.events += events
            failed += r["failedevents"]?.int ?? 0
            tin += r["tokensin"]?.int ?? 0
            tout += r["tokensout"]?.int ?? 0
            cin += r["cachereadtokens"]?.int ?? 0
            cout += r["cachewritetokens"]?.int ?? 0
            unknown += nocost
            errors += r["errorevents"]?.int ?? 0
            durationMs += r["durationms"]?.int ?? 0
            failedMs += r["faileddurationms"]?.int ?? 0
            unknownMs += r["unknowndurationevents"]?.int ?? 0
            if let c = r["estimatedcostusd"]?.string, let n = UsageReport.parseNanos(c) { estimated += n; nanos += n; known = true; estimateKnown = true }
            guard let c = r["reportedcostusd"]?.string else { return }
            if let n = UsageReport.parseNanos(c) { nanos += n; reported += n; known = true; reportedKnown = true } else { unknown += events - nocost }  // malformed: not a known cost
        }

        /// ", 7 errors, 58.0 failed agent minutes" for the total line; "" when no event failed with an error.
        var failedSummary: String {
            guard errors > 0 else { return "" }
            return ", \(errors) errors" + (failedMs > 0 ? ", " + UsageReport.minutes(failedMs) + " failed agent minutes" : "") + (unknownMs > 0 ? " (\(unknownMs) events of unknown duration)" : "")
        }

        var cost: String {
            guard known else { return "-" }
            var c = UsageReport.usd(reported)
            if estimateKnown { c = "~" + UsageReport.usd(estimated); if reportedKnown { c = UsageReport.usd(reported) + " + " + c } }
            return unknown > 0 ? "\(c) (+\(unknown) unknown)" : c
        }
    }

    /// A decimal USD string ("0.006000000") as nanodollars; more than 9 decimals are cut off.
    static func parseNanos(_ s: String) -> Int64? {
        let parts = s.split(separator: ".", maxSplits: 1, omittingEmptySubsequences: false).map(String.init)
        let whole = parts[0], frac = parts.count > 1 ? parts[1] : ""
        guard !whole.isEmpty, whole.count <= 12, (whole + frac).utf8.allSatisfy({ $0 >= 0x30 && $0 <= 0x39 }) else { return nil }
        guard let w = Int64(whole), let f = Int64(String((frac + "000000000").prefix(9))) else { return nil }
        return w * 1_000_000_000 + f
    }

    /// Two decimals, four below one cent.
    static func usd(_ nanos: Int64) -> String {
        if nanos == 0 { return "0.00" }
        if nanos < 100_000 { return "<0.0001" }
        if nanos < 10_000_000 { return String(format: "%lld.%04lld", (nanos + 50_000) / 1_000_000_000, (nanos + 50_000) % 1_000_000_000 / 100_000) }
        let c = (nanos + 5_000_000) / 10_000_000
        return String(format: "%lld.%02lld", c / 100, c % 100)
    }

    /// Milliseconds as minutes with one decimal: 3480000 -> "58.0".
    static func minutes(_ ms: Int64) -> String {
        let t = (ms + 3000) / 6000  // tenths of a minute, rounded
        return "\(t / 10).\(t % 10)"
    }

    /// 914, 1.2K, 914K, 46.1M, 3B.
    static func human(_ n: Int64) -> String {
        if n < 1000 { return String(n) }
        for (div, unit) in [(Int64(1_000), "K"), (1_000_000, "M"), (1_000_000_000, "B")] {
            let t = (n * 10 + div / 2) / div  // tenths
            if t >= 10000 && unit != "B" { continue }
            return t >= 1000 || t % 10 == 0 ? "\(t / 10)\(unit)" : "\(t / 10).\(t % 10)\(unit)"
        }
        return ""
    }

    /// Bytewise order, as Go compares strings.
    static func before(_ a: String, _ b: String) -> Bool { a.utf8.lexicographicallyPrecedes(b.utf8) }

    static func oneLine(_ s: String) -> String { s.isEmpty ? s : ProjectText.oneLine(s) }

    /// Columns two spaces apart; `right` columns are right-aligned; the last column is not padded.
    static func table(_ header: [String], _ rows: [[String]], _ right: [Bool]) -> String {
        let all = [header] + rows
        var w = [Int](repeating: 0, count: header.count)
        for r in all { for (i, c) in r.enumerated() { w[i] = max(w[i], c.unicodeScalars.count) } }
        var out = ""
        for r in all {
            for (i, c) in r.enumerated() {
                let pad = String(repeating: " ", count: w[i] - c.unicodeScalars.count)
                if i == r.count - 1 { out += c } else if right[i] { out += pad + c + "  " } else { out += c + pad + "  " }
            }
            out += "\n"
        }
        return out
    }

    static func rows(_ data: Data) -> (days: [JDict], gaps: [JDict])? {
        guard case .obj? = JSONValue.parse(data) else { return nil }
        let d = JSONValue.parse(data)!.dict
        return ((d["days"]?.array ?? []).map(\.dict), (d["gaps"]?.array ?? []).map(\.dict))
    }

    static func text(_ rows: [JDict], _ gaps: [JDict], sortFailed: Bool = false) -> String {
        if rows.isEmpty { return "no events\n" }
        var total = Sum()
        var groups: [[String]: Sum] = [:], days: [String: Sum] = [:]
        for r in rows {
            let k = [r["provider"]?.string ?? "", r["model"]?.string ?? ""], day = r["day"]?.string ?? ""
            groups[k, default: Sum()].add(r)
            days[day, default: Sum()].add(r)
            total.add(r)
        }
        let keys = groups.keys.sorted { x, y in
            let a = groups[x]!, b = groups[y]!
            if sortFailed && a.failedMs != b.failedMs { return a.failedMs > b.failedMs }
            if a.known != b.known { return a.known }
            if a.nanos != b.nanos { return a.nanos > b.nanos }
            if a.events != b.events { return a.events > b.events }
            return before(x[0] + "\u{0}" + x[1], y[0] + "\u{0}" + y[1])
        }
        let withFailedTime = total.failedMs > 0  // the column appears when any model has failed agent minutes
        let withCache = total.cin > 0  // the column appears when any model read a prompt cache
        let mrows = keys.map { k -> [String] in
            let s = groups[k]!
            var row = [oneLine(k[0]), oneLine(k[1]), String(s.events), String(s.failed)]
            if withFailedTime { row.append(s.failedMs > 0 ? minutes(s.failedMs) : "-") }
            row.append(human(s.tin))
            if withCache { row.append(human(s.cin)) }
            return row + [human(s.tout), s.cost]
        }
        let drows = days.keys.sorted(by: before).map { d -> [String] in
            let s = days[d]!
            var row = [oneLine(d), String(s.events), human(s.tin + s.tout)]
            if withCache { row.append(human(s.cin)) }
            return row + [s.cost]
        }
        var head = ["provider", "model", "requests", "failed"], right = [false, false, true, true]
        if withFailedTime { head.append("failed min"); right.append(true) }
        head.append("tokens in"); right.append(true)
        if withCache { head.append("cached in"); right.append(true) }
        head.append(contentsOf: ["tokens out", "cost USD"]); right.append(contentsOf: [true, false])
        var dhead = ["day", "requests", "tokens in+out"], dright = [false, true, true]
        if withCache { dhead.append("cached in"); dright.append(true) }
        dhead.append("cost USD"); dright.append(false)
        var out = table(head, mrows, right)
            + "\n" + table(dhead, drows, dright)
            + "\ntotal: \(total.events) requests (\(total.failed) failed), \(human(total.tin)) tokens in, "
        if withCache { out += "\(human(total.cin)) cached in, " }
        out += "\(human(total.tout)) tokens out, cost USD \(total.cost)\(total.failedSummary)\n"
        let utc = TimeZone(identifier: "UTC")!
        for g in gaps {
            out += "\(g["count"]?.int ?? 0) events lost between \(ProjectText.stamp(g["from"]?.string.flatMap(ProjectText.parseTime), utc)) and \(ProjectText.stamp(g["to"]?.string.flatMap(ProjectText.parseTime), utc)) (UTC)\n"
        }
        return out
    }

    /// Commas, quotes and newlines are quoted; a text cell that starts like a formula gets a leading apostrophe.
    static func cell(_ s: String, text: Bool = true) -> String {
        var s = s
        if text, let f = s.utf8.first, "=+-@".utf8.contains(f) { s = "'" + s }
        if s.contains(where: { ",\"\n\r\r\n".contains($0) }) { s = "\"" + s.replacingOccurrences(of: "\"", with: "\"\"") + "\"" }
        return s
    }

    /// One row per day, provider and model (the users and projects of the API rows are added up).
    static func csv(_ rows: [JDict]) -> String {
        var sums: [[String]: Sum] = [:]
        for r in rows { sums[[r["day"]?.string ?? "", r["provider"]?.string ?? "", r["model"]?.string ?? ""], default: Sum()].add(r) }
        var out = "day,provider,model,requests,failed,tokens_in,tokens_out,reported_cost_usd,estimated_cost_usd,events_without_cost,errors,duration_ms,failed_duration_ms,unknown_duration_events,cache_read_tokens,cache_write_tokens\n"
        for k in sums.keys.sorted(by: { before($0.joined(separator: "\u{0}"), $1.joined(separator: "\u{0}")) }) {
            let s = sums[k]!
            let cost = s.reportedKnown ? String(format: "%lld.%09lld", s.reported / 1_000_000_000, s.reported % 1_000_000_000) : ""
            let estimated = s.estimateKnown ? String(format: "%lld.%09lld", s.estimated / 1_000_000_000, s.estimated % 1_000_000_000) : ""
            out += "\(cell(k[0])),\(cell(k[1])),\(cell(k[2])),\(s.events),\(s.failed),\(s.tin),\(s.tout),\(cost),\(estimated),\(s.unknown),\(s.errors),\(s.durationMs),\(s.failedMs),\(s.unknownMs),\(s.cin),\(s.cout)\n"
        }
        return out
    }
}
