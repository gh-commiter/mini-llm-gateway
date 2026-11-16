import Foundation

// The Usage page of the Settings window: GET /usage/events (the answer `minigw gw usage` formats) as chart data. The rows are read
// and summed by UsageReport (Sum, human, usd), so the app and the command show the same numbers; this file only groups them.
// Pure and without a view, so RouterCoreTests can assert it.

public enum UsageMeasure: String, CaseIterable, Sendable { case tokens, spend, requests, failedTime }

public struct UsageChart: @unchecked Sendable {  // rows are immutable
    /// One bar segment: a UTC day ("2026-11-15") and one model (nil = "Other").
    public struct Bar: Sendable, Equatable {
        public let day: String
        public let model: String?
        public let value: Int64    // tokens, requests or nanodollars; 0 = nothing known
        public let text: String    // "914K", "12", "~0.42"
    }

    public struct Top: Sendable, Equatable {
        public let provider: String
        public let model: String
        public let value: Int64
        public let text: String
        public let fraction: Double  // value / the largest value, 0...1
    }

    public struct Gap: Sendable, Equatable {
        public let count: Int64
        public let from: Date?
        public let to: Date?
    }

    public struct Summary: Sendable, Equatable {
        public let total: Int64
        public let totalText: String  // spend: "-" when no cost is known at all
        public let series: [String?]  // the models of the bars, largest first, nil (Other) last
        public let bars: [Bar]        // by day, then series order; zero segments left out
        public let top: [Top]
        public let costUnknown: Int64 // events without a known cost (spend only; 0 otherwise)
        public let durationUnknown: Int64 // events without a known duration (failed time only; 0 otherwise)
        public let isEmpty: Bool
    }

    /// Models shown in the chart before the rest is "Other".
    public static let seriesLimit = 6

    private let rows: [JDict]
    public let gaps: [Gap]

    /// nil: the body is not the answer of /usage/events.
    public init?(body: Data) {
        guard let r = UsageReport.rows(body) else { return nil }
        rows = r.days
        gaps = r.gaps.map { Gap(count: $0["count"]?.int ?? 0, from: $0["from"]?.string.flatMap(ProjectText.parseTime), to: $0["to"]?.string.flatMap(ProjectText.parseTime)) }
    }

    public func summary(_ measure: UsageMeasure) -> Summary {
        var all = UsageReport.Sum()
        var perDayModel: [[String]: UsageReport.Sum] = [:], perModel: [String: UsageReport.Sum] = [:], perPair: [[String]: UsageReport.Sum] = [:]
        for r in rows {
            let day = r["day"]?.string ?? "", provider = r["provider"]?.string ?? "", model = r["model"]?.string ?? ""
            all.add(r)
            perDayModel[[day, model], default: UsageReport.Sum()].add(r)
            perModel[model, default: UsageReport.Sum()].add(r)
            perPair[[provider, model], default: UsageReport.Sum()].add(r)
        }
        func value(_ s: UsageReport.Sum) -> Int64 {
            switch measure {
            case .tokens: s.tin + s.tout
            case .requests: s.events
            case .spend: s.nanos
            case .failedTime: s.failedMs  // milliseconds of events that ended in error
            }
        }
        func text(_ s: UsageReport.Sum) -> String {
            switch measure {
            case .tokens: UsageReport.human(s.tin + s.tout)
            case .requests: UsageReport.human(s.events)
            case .spend: s.known ? (s.estimateKnown ? "~" : "") + UsageReport.usd(s.nanos) : "-"  // unknown cost is not 0.00
            case .failedTime: UsageReport.minutes(s.failedMs)
            }
        }
        func order<K>(_ k: [(K, UsageReport.Sum)], _ name: (K) -> String) -> [(K, UsageReport.Sum)] {
            k.sorted { a, b in value(a.1) != value(b.1) ? value(a.1) > value(b.1) : UsageReport.before(name(a.0), name(b.0)) }
        }

        let ranked = order(Array(perModel), { $0 }).map(\.0)
        let shown = Set(ranked.prefix(Self.seriesLimit))
        var cells: [[String?]: UsageReport.Sum] = [:]  // [day, model or nil]
        for (k, s) in perDayModel {
            let m: String? = shown.contains(k[1]) ? k[1] : nil
            let key: [String?] = [k[0], m]
            cells[key, default: UsageReport.Sum()].merge(s)
        }
        let series: [String?] = Array(ranked.prefix(Self.seriesLimit)) + (ranked.count > Self.seriesLimit ? [nil] : [])
        let bars = cells.keys.sorted { a, b in
            a[0]! != b[0]! ? UsageReport.before(a[0]!, b[0]!) : series.firstIndex(of: a[1])! < series.firstIndex(of: b[1])!
        }.compactMap { k -> Bar? in
            let s = cells[k]!
            return value(s) > 0 ? Bar(day: k[0]!, model: k[1], value: value(s), text: text(s)) : nil
        }
        let pairs = order(Array(perPair), { $0.joined(separator: "\u{0}") })
        let largest = pairs.first.map { value($0.1) } ?? 0
        let top = pairs.map { Top(provider: $0.0[0], model: $0.0[1], value: value($0.1), text: text($0.1), fraction: largest > 0 ? Double(value($0.1)) / Double(largest) : 0) }
        return Summary(total: value(all), totalText: text(all), series: series, bars: bars, top: top,
                       costUnknown: measure == .spend ? all.unknown : 0, durationUnknown: measure == .failedTime ? all.unknownMs : 0, isEmpty: rows.isEmpty)
    }

    /// A value of the selected measure as an axis label ("46.1M", "12", "0.42"; spend in USD, no sign).
    public static func axisText(_ m: UsageMeasure, _ v: Int64) -> String { m == .spend ? UsageReport.usd(v) : m == .failedTime ? UsageReport.minutes(v) : UsageReport.human(v) }

    /// The call of the page: the admin route through the helper, as the gateway's own admin API does; no second store.
    public enum Loaded: Sendable { case ok(UsageChart), failed(String) }

    public static func load(core: CoreHelper, days: Int) async -> Loaded {
        let r = await core.call(context: .admin, request: CoreRequest(method: "GET", path: "/usage/events", query: "days=\(days)"))
        if r == .unavailable { return .failed("") }  // the view says "not available"
        guard r.status == 200, let chart = UsageChart(body: r.body) else {
            return .failed(JSONValue.parse(r.body)?.dict["error"]?.dict["message"]?.string ?? "HTTP \(r.status)")
        }
        return .ok(chart)
    }
}

extension UsageReport.Sum {
    mutating func merge(_ o: UsageReport.Sum) {
        events += o.events; failed += o.failed; tin += o.tin; tout += o.tout; unknown += o.unknown; nanos += o.nanos
        estimated += o.estimated; reported += o.reported
        errors += o.errors; durationMs += o.durationMs; failedMs += o.failedMs; unknownMs += o.unknownMs
        known = known || o.known; estimateKnown = estimateKnown || o.estimateKnown; reportedKnown = reportedKnown || o.reportedKnown
    }
}
