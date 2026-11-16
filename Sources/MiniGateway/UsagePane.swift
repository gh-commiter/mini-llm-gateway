import Charts
import RouterCore
import SwiftUI

/// The Usage page of the Settings window: requests, tokens and spend per day and model, from the same answer as `minigw gw usage`
/// (the admin route /usage/events through the helper, UsageChart.load). Model and provider names are untrusted: shown as plain text only.
struct UsagePane: View {
    let header: PaneHeader
    @EnvironmentObject var state: AppState
    @AppStorage("usage.days") private var days = 30
    @AppStorage("usage.measure") private var measureRaw = UsageMeasure.tokens.rawValue
    @State private var loaded: UsageChart.Loaded?
    @State private var updated: Date?
    @State private var picked: Date?  // the day under the pointer

    private var measure: UsageMeasure { UsageMeasure(rawValue: measureRaw) ?? .tokens }

    private static let palette: [Color] = [.blue, .orange, .teal, .pink, .indigo, .green]
    private static let utcDay: DateFormatter = {
        let f = DateFormatter()
        f.calendar = Calendar(identifier: .gregorian); f.timeZone = TimeZone(identifier: "UTC"); f.locale = Locale(identifier: "en_US_POSIX")
        f.dateFormat = "yyyy-MM-dd"
        return f
    }()

    var body: some View {
        Form {
            header
            Section {
                Picker("Period", selection: $days) {
                    ForEach([7, 30, 90], id: \.self) { Text("\($0) days").tag($0) }
                }
                .pickerStyle(.segmented)
                Picker("Measure", selection: $measureRaw) {
                    ForEach(UsageMeasure.allCases, id: \.rawValue) { Text(Self.title($0)).tag($0.rawValue) }
                }
                .pickerStyle(.segmented)
            }
            switch loaded {
            case nil:
                Section { ProgressView().frame(maxWidth: .infinity) }
            case .failed(let why)?:
                Section {
                    Text(why.isEmpty ? String(localized: "The project database is not available.") : why).foregroundStyle(.secondary)
                } header: { Text("The usage could not be loaded") }
            case .ok(let chart)?:
                content(chart, chart.summary(measure))
            }
            Section {
                HStack {
                    if let updated { Text("Updated \(updated.formatted(date: .omitted, time: .shortened))").foregroundStyle(.secondary).font(.caption) }
                    Spacer()
                    Button("Refresh") { Task { await load() } }
                }
            }
        }
        .task(id: days) { await load() }  // on appearance and when the period changes; never polled
    }

    @ViewBuilder private func content(_ chart: UsageChart, _ s: UsageChart.Summary) -> some View {
        if s.isEmpty {
            Section { Text("No usage in this period.").foregroundStyle(.secondary) }
        } else {
            Section("Total") {
                Text(verbatim: Self.money(s.totalText, measure))
                    .font(.system(size: 34, weight: .semibold, design: .rounded)).monospacedDigit()
                    .accessibilityLabel(Text("Total")).accessibilityValue(Text(verbatim: Self.money(s.totalText, measure) + " " + Self.title(measure)))
            }
            Section("Daily by model") { dailyChart(s) }
            Section("Top models") {
                ForEach(Array(s.top.prefix(10).enumerated()), id: \.offset) { _, t in
                    VStack(alignment: .leading, spacing: 4) {
                        LabeledContent {
                            Text(verbatim: Self.money(t.text, measure)).monospacedDigit()
                        } label: {
                            Text(verbatim: t.model)
                            Text(verbatim: t.provider)
                        }
                        ProgressView(value: t.fraction).accessibilityHidden(true)
                    }
                    .accessibilityElement(children: .combine)
                }
            }
            if measure == .spend {
                Section {
                    if s.costUnknown > 0 {
                        Label("\(s.costUnknown) requests have no known cost and are not counted in the spend.", systemImage: "exclamationmark.triangle")
                    }
                    Text("Amounts with ~ are estimates; the others are reported by the provider.").font(.caption).foregroundStyle(.secondary)
                }
            }
            if measure == .failedTime {
                Section {
                    if s.durationUnknown > 0 {
                        Label("\(s.durationUnknown) requests have an unknown duration and are not counted.", systemImage: "exclamationmark.triangle")
                    }
                    Text("Summed agent minutes are concurrent work time, not clock time. Only requests that ended in an error count.").font(.caption).foregroundStyle(.secondary)
                }
            }
            if !chart.gaps.isEmpty {
                Section {
                    ForEach(Array(chart.gaps.enumerated()), id: \.offset) { _, g in
                        Label("\(g.count) events were lost between \(Self.stamp(g.from)) and \(Self.stamp(g.to)) (UTC).", systemImage: "exclamationmark.circle")
                    }
                }
            }
        }
    }

    private func dailyChart(_ s: UsageChart.Summary) -> some View {
        let names = s.series.map { $0 ?? String(localized: "Other") }
        let colors = s.series.indices.map { s.series[$0] == nil ? Color.gray : Self.palette[$0 % Self.palette.count] }
        let scale = measure == .spend ? 1_000_000_000.0 : measure == .failedTime ? 60_000.0 : 1  // nanodollars, milliseconds
        let day = picked.map { Self.utcDay.string(from: $0) }
        let shown = day.map { d in s.bars.filter { $0.day == d } } ?? []
        return Chart {
            ForEach(Array(s.bars.enumerated()), id: \.offset) { _, b in
                BarMark(x: .value("Day", Self.utcDay.date(from: b.day) ?? .now, unit: .day), y: .value(Self.title(measure), Double(b.value) / scale))
                    .foregroundStyle(by: .value("Model", b.model ?? String(localized: "Other")))
                    .accessibilityLabel(Text(verbatim: (b.model ?? String(localized: "Other")) + ", " + b.day))
                    .accessibilityValue(Text(verbatim: Self.money(b.text, measure)))
            }
            if let picked, !shown.isEmpty {
                RuleMark(x: .value("Day", picked, unit: .day)).foregroundStyle(.secondary.opacity(0.4))
                    .annotation(position: .top, overflowResolution: .init(x: .fit(to: .chart), y: .disabled)) {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(verbatim: day ?? "").font(.caption.bold())
                            ForEach(Array(shown.enumerated()), id: \.offset) { _, b in
                                Text(verbatim: (b.model ?? String(localized: "Other")) + ": " + Self.money(b.text, measure)).font(.caption)
                            }
                        }
                        .padding(6).background(.regularMaterial, in: RoundedRectangle(cornerRadius: 6))
                    }
            }
        }
        .chartForegroundStyleScale(domain: names, range: colors)
        .chartXSelection(value: $picked)
        .chartXAxisLabel(String(localized: "Day (UTC)"))
        .chartYAxisLabel(Self.title(measure))
        .chartYAxis {
            AxisMarks { v in
                AxisGridLine()
                AxisValueLabel { if let d = v.as(Double.self) { Text(verbatim: Self.axis(measure, d * scale)) } }
            }
        }
        .frame(height: 240)
        .accessibilityLabel(Text("Daily by model"))
    }

    private func load() async {
        let r = await UsageChart.load(core: state.core, days: days)
        loaded = r
        updated = .now
    }

    static func title(_ m: UsageMeasure) -> String {
        switch m {
        case .tokens: String(localized: "Tokens")
        case .spend: String(localized: "Spend")
        case .requests: String(localized: "Requests")
        case .failedTime: String(localized: "Failed time")
        }
    }

    /// Spend gets a dollar sign ("~" stays in front); unknown ("-") stays as it is.
    static func money(_ text: String, _ m: UsageMeasure) -> String {
        if m == .failedTime { return text + " min" }
        guard m == .spend, text != "-" else { return text }
        return text.hasPrefix("~") ? "~$" + text.dropFirst() : "$" + text
    }

    static func axis(_ m: UsageMeasure, _ v: Double) -> String {
        let n = Int64(v.rounded())
        return m == .spend ? "$" + UsageChart.axisText(.spend, n) : UsageChart.axisText(m, n)
    }

    static func stamp(_ d: Date?) -> String {
        guard let d else { return "?" }
        return d.formatted(Date.FormatStyle(date: .abbreviated, time: .shortened, timeZone: TimeZone(identifier: "UTC")!))
    }
}
