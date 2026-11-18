import Foundation

// minigw gw usage: the provider/model report of GET /usage/events (totals per provider and model, per day, a total line), as text,
// CSV or the unchanged JSON. A port of client/serve_usage_report.go; client/testdata/usage-report.json is asserted here
// (UsageReportTests) and in client/admin_parity_test.go.

public enum UsageReport {
    public static let help = """
        minigw gw usage [--days N] [--provider P] [--model M] [--phase P] [--activity A] [--task T] [--label-source given|branch|model] [--sort failed] [--json|--csv]   requests, tokens, cost and failed agent time per provider, model and day (UTC)
          minigw gw usage --by phase|activity|task|project|model[,...] [--days N] [--phase P] [--activity A] [--task T] [--label-source given|branch|model] [--json]   the same numbers per phase, activity, task, project or model (one or two levels; at most 50 groups and an (other) row); unlabelled usage is its own row; --json gives these groups, not the raw rows
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

        mutating func fold(_ o: Sum) {
            events += o.events; failed += o.failed; tin += o.tin; tout += o.tout; unknown += o.unknown; nanos += o.nanos; cin += o.cin; cout += o.cout
            errors += o.errors; durationMs += o.durationMs; failedMs += o.failedMs; unknownMs += o.unknownMs
            known = known || o.known; estimated += o.estimated; reported += o.reported
            estimateKnown = estimateKnown || o.estimateKnown; reportedKnown = reportedKnown || o.reportedKnown
        }

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

    // MARK: --by

    static let byDims = ["phase", "activity", "task", "project", "model"]
    static let unlabelled = "(unlabelled)"

    /// --by: one or two different dimensions, comma separated.
    static func parseBy(_ v: String) -> [String]? {
        let parts = v.split(separator: ",", omittingEmptySubsequences: false).map(String.init)
        guard (1...2).contains(parts.count), parts.allSatisfy(byDims.contains), parts.count == 1 || parts[0] != parts[1] else { return nil }
        return parts
    }
    /// Does --by need the rows split by label (labels=1)?
    static func byLabels(_ by: [String]) -> Bool { by.contains { ["phase", "activity", "task"].contains($0) } }

    /// The value of one dimension of a row, with the source of a label: the display text adds " (branch)" to a task taken from
    /// the branch and " (model)" or " (internal)" to a phase or activity chosen by a model or the gateway; raw is the value alone
    /// ("(unlabelled)" for no label). Of a phase or activity only those two sources show (a given one is the plain value).
    static func byValue(_ r: JDict, _ dim: String, _ projects: [Int64: String]) -> (display: String, raw: String, source: String) {
        var v = "", source = ""
        switch dim {
        case "phase": v = r["phase"]?.string ?? ""; source = modelSource(r["phasesource"]?.string)
        case "activity": v = r["activity"]?.string ?? ""; source = modelSource(r["activitysource"]?.string)
        case "task": v = r["task"]?.string ?? ""; source = r["tasksource"]?.string ?? ""
        case "model": let m = r["model"]?.string ?? ""; return (m, m, "")
        default:
            let p = r["project"]?.int ?? 0
            let n = p == 0 ? "(no project)" : (projects[p].flatMap { $0.isEmpty ? nil : $0 } ?? String(p))
            return (n, n, "")
        }
        if v.isEmpty { return (unlabelled, unlabelled, "") }
        return ["branch", "model", "internal"].contains(source) ? (v + " (" + source + ")", v, source) : (v, v, source)
    }

    static func modelSource(_ s: String?) -> String { s == "model" || s == "internal" ? s! : "" }

    /// At most this many groups are shown; the rest is one "(other)" group, so shares add up.
    static let maxByGroups = 50

    struct ByGroup { var vals: [String]; var raw: [String]; var src: [String]; var sum: Sum }

    /// The rows summed per one or two dimensions (groups ordered by the first dimension's cost, the unlabelled one last), at most
    /// maxByGroups groups and an "(other)" group for the rest; with the total of all rows. A port of usageByGroups.
    static func byGroups(_ rows: [JDict], _ by: [String], _ projects: [Int64: String]) -> (groups: [ByGroup], total: Sum) {
        var byKey: [[String]: ByGroup] = [:]
        var total = Sum()
        for r in rows {
            var g = ByGroup(vals: [], raw: [], src: [], sum: Sum())
            for d in by { let v = byValue(r, d, projects); g.vals.append(v.display); g.raw.append(v.raw); g.src.append(v.source) }
            if byKey[g.vals] == nil { byKey[g.vals] = g }
            byKey[g.vals]!.sum.add(r)
            total.add(r)
        }
        var first: [String: (events: Int64, nanos: Int64)] = [:]
        for (k, g) in byKey { first[k[0], default: (0, 0)].events += g.sum.events; first[k[0], default: (0, 0)].nanos += g.sum.nanos }
        let keys = byKey.keys.sorted { a, b in
            if a[0] != b[0] {
                let x = first[a[0]]!, y = first[b[0]]!
                if (a[0] == unlabelled) != (b[0] == unlabelled) { return b[0] == unlabelled }
                if x.nanos != y.nanos { return x.nanos > y.nanos }
                if x.events != y.events { return x.events > y.events }
                return before(a[0], b[0])
            }
            let x = byKey[a]!.sum, y = byKey[b]!.sum
            if x.nanos != y.nanos { return x.nanos > y.nanos }
            if x.events != y.events { return x.events > y.events }
            return before(a.count > 1 ? a[1] : "", b.count > 1 ? b[1] : "")
        }
        var groups: [ByGroup] = []
        var other: ByGroup?
        for (i, k) in keys.enumerated() {
            if i < maxByGroups { groups.append(byKey[k]!); continue }
            if other == nil {
                let blank = [String](repeating: "", count: by.count)
                other = ByGroup(vals: ["(other)"] + blank.dropFirst(), raw: ["(other)"] + blank.dropFirst(), src: blank, sum: Sum())
            }
            other!.sum.fold(byKey[k]!.sum)
        }
        if let other { groups.append(other) }
        return (groups, total)
    }

    static func percent(_ events: Int64, _ all: Int64) -> Int64 { all == 0 ? 0 : (events * 100 + all / 2) / all }

    /// The usage numbers per one or two dimensions, a share column and a total line; truncated says the API cut the report.
    static func byText(_ rows: [JDict], _ gaps: [JDict], _ by: [String], _ projects: [Int64: String], truncated: Bool = false) -> String {
        if rows.isEmpty { return "no events\n" }
        let (groups, total) = byGroups(rows, by, projects)
        let withFailedTime = total.failedMs > 0, withCache = total.cin > 0
        var head = by, right = [Bool](repeating: false, count: by.count)
        head += ["requests", "share", "failed"]; right += [true, true, true]
        if withFailedTime { head.append("failed min"); right.append(true) }
        head.append("tokens in"); right.append(true)
        if withCache { head.append("cached in"); right.append(true) }
        head += ["tokens out", "cost USD"]; right += [true, false]
        let table = groups.map { g -> [String] in
            let s = g.sum
            var row = [oneLine(g.vals[0])]
            if by.count > 1 { row.append(oneLine(g.vals[1])) }
            row += [String(s.events), "\(percent(s.events, total.events))%", String(s.failed)]
            if withFailedTime { row.append(s.failedMs > 0 ? minutes(s.failedMs) : "-") }
            row.append(human(s.tin))
            if withCache { row.append(human(s.cin)) }
            return row + [human(s.tout), s.cost]
        }
        var out = Self.table(head, table, right) + "\ntotal: \(total.events) requests (\(total.failed) failed), \(human(total.tin)) tokens in, "
        if withCache { out += "\(human(total.cin)) cached in, " }
        out += "\(human(total.tout)) tokens out, cost USD \(total.cost)\(total.failedSummary)\n"
        if truncated { out += "the report was cut: only the newest 5000 rows were returned; narrow it with --days or a filter\n" }
        let utc = TimeZone(identifier: "UTC")!
        for g in gaps {
            out += "\(g["count"]?.int ?? 0) events lost between \(ProjectText.stamp(g["from"]?.string.flatMap(ProjectText.parseTime), utc)) and \(ProjectText.stamp(g["to"]?.string.flatMap(ProjectText.parseTime), utc)) (UTC)\n"
        }
        return out
    }

    /// The same groups and numbers as the table, as JSON (keys sorted); costs are decimal USD strings or null.
    static func byJSON(_ rows: [JDict], _ gaps: [JDict], _ by: [String], _ projects: [Int64: String], truncated: Bool = false) -> String {
        let (groups, total) = byGroups(rows, by, projects)
        func nanos(_ known: Bool, _ n: Int64) -> Any { known ? String(format: "%lld.%09lld", n / 1_000_000_000, n % 1_000_000_000) : NSNull() }
        func numbers(_ s: Sum) -> [String: Any] {
            ["requests": s.events, "share": percent(s.events, total.events), "failed": s.failed, "errors": s.errors, "tokensIn": s.tin, "tokensOut": s.tout,
             "cacheReadTokens": s.cin, "failedDurationMs": s.failedMs, "eventsWithoutCost": s.unknown,
             "reportedCostUsd": nanos(s.reportedKnown, s.reported), "estimatedCostUsd": nanos(s.estimateKnown, s.estimated)]
        }
        let utc = TimeZone(identifier: "UTC")!
        let iso = ISO8601DateFormatter(); iso.timeZone = utc
        let gs: [[String: Any]] = groups.map { g in
            var m = numbers(g.sum)
            for (i, d) in by.enumerated() { m[d] = g.raw[i]; if !g.src[i].isEmpty { m[d + "Source"] = g.src[i] } }
            return m
        }
        let gl: [[String: Any]] = gaps.map { g in
            ["count": g["count"]?.int ?? 0, "from": g["from"]?.string.flatMap(ProjectText.parseTime).map { iso.string(from: $0) } ?? "", "to": g["to"]?.string.flatMap(ProjectText.parseTime).map { iso.string(from: $0) } ?? ""]
        }
        var out: [String: Any] = ["by": by, "groups": gs, "total": numbers(total), "gaps": gl, "timezone": "UTC"]
        if truncated { out["truncated"] = true }
        let data = (try? JSONSerialization.data(withJSONObject: out, options: [.sortedKeys, .withoutEscapingSlashes])) ?? Data()
        return String(decoding: data, as: UTF8.self)
    }
}
