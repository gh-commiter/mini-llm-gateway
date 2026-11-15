import Foundation
import Testing
@testable import RouterCore

// The Usage page's numbers: grouped from the same rows as `minigw gw usage`. The fixture client/testdata/usage-report.json supplies
// real gateway answers; the eight-model answer is built here.

private let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()

private func fixtureBody(_ name: String) throws -> Data {
    let fx = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/usage-report.json"))) as! [String: Any]
    let c = (fx["cases"] as! [[String: Any]]).first { $0["name"] as? String == name }!
    return Data(((c["responses"] as! [[String: Any]])[0]["body"] as! String).utf8)
}

private func row(_ day: String, _ model: String, events: Int, tin: Int, tout: Int, reported: String? = nil, estimated: String? = nil, nocost: Int = 0, provider: String = "p") -> String {
    func q(_ s: String?) -> String { s.map { "\"\($0)\"" } ?? "null" }
    return "{\"day\":\"\(day)\",\"provider\":\"\(provider)\",\"model\":\"\(model)\",\"events\":\(events),\"failedEvents\":0,\"tokensIn\":\(tin),\"tokensOut\":\(tout),\"reportedCostUsd\":\(q(reported)),\"estimatedCostUsd\":\(q(estimated)),\"eventsWithoutCost\":\(nocost)}"
}

private func body(_ rows: [String], gaps: String = "") -> Data { Data("{\"days\":[\(rows.joined(separator: ","))],\"gaps\":[\(gaps)],\"timezone\":\"UTC\"}".utf8) }

@Suite struct UsageChartTests {
    // eight models m1..m8 (m1 largest), two days, an extra provider row for m1
    private let eight: UsageChart = {
        var rows: [String] = []
        for i in 1...8 {
            rows.append(row("2026-11-14", "m\(i)", events: 10 * (9 - i), tin: 1000 * (9 - i), tout: 100, reported: "0.0\(9 - i)0000000"))
            rows.append(row("2026-11-15", "m\(i)", events: 1, tin: 10, tout: 1, reported: "0.010000000"))
        }
        rows.append(row("2026-11-15", "m1", events: 5, tin: 500, tout: 0, reported: "0.500000000", provider: "other"))
        return UsageChart(body: body(rows))!
    }()

    @Test func totalsEqualTheSumOfTheRows() {
        let t = eight.summary(.tokens)
        let events = (1...8).reduce(0) { $0 + 10 * (9 - $1) + 1 } + 5
        #expect(eight.summary(.requests).total == Int64(events))
        #expect(t.total == t.bars.reduce(0) { $0 + $1.value })
        #expect(t.total == t.top.reduce(0) { $0 + $1.value })
        let s = eight.summary(.spend)
        #expect(s.total == s.bars.reduce(0) { $0 + $1.value })
        #expect(s.total == s.top.reduce(0) { $0 + $1.value })
        #expect(s.totalText == "0.94")  // 0.08+...+0.01 = 0.36, day two 8 * 0.01, the "other" provider 0.50
    }

    @Test func topSixAndOther() {
        let t = eight.summary(.tokens)
        #expect(t.series == ["m1", "m2", "m3", "m4", "m5", "m6", nil])
        #expect(t.bars.filter { $0.model == nil }.count == 2)  // m7 and m8 on two days
        #expect(Set(t.bars.map(\.day)) == ["2026-11-14", "2026-11-15"])
        #expect(t.bars.map(\.day) == t.bars.map(\.day).sorted())
        let few = UsageChart(body: body([row("2026-11-15", "a", events: 1, tin: 1, tout: 1)]))!.summary(.tokens)
        #expect(few.series == ["a"])  // no empty "Other"
    }

    @Test func topModelsSortedByTheMeasure() {
        let t = eight.summary(.requests)
        #expect(t.top.count == 9)  // eight models, m1 under two providers
        #expect(t.top[0].model == "m1" && t.top[0].value == 81 && t.top[0].fraction == 1)
        #expect(t.top.map(\.value) == t.top.map(\.value).sorted(by: >))
        #expect(t.top.allSatisfy { $0.fraction >= 0 && $0.fraction <= 1 })
        #expect(eight.summary(.spend).top[0].model == "m1")  // the "other" provider row, 0.50, is the largest pair
    }

    @Test func unknownCostIsNeverZero() throws {
        let c = UsageChart(body: try fixtureBody("unknown cost only"))!
        let s = c.summary(.spend)
        #expect(s.totalText == "-" && s.total == 0)
        #expect(s.costUnknown > 0 && s.top.allSatisfy { $0.text == "-" } && s.bars.isEmpty)
        #expect(c.summary(.tokens).costUnknown == 0)  // the note belongs to spend only
        #expect(c.summary(.requests).total > 0)
    }

    @Test func estimatesAreMarked() throws {
        let e = UsageChart(body: try fixtureBody("estimate text"))!.summary(.spend)
        #expect(e.totalText == "~0.42" && e.top[0].text == "~0.42" && e.bars[0].text == "~0.42" && e.costUnknown == 1)
        let m = UsageChart(body: try fixtureBody("mixed costs"))!.summary(.spend)
        #expect(m.totalText == "~0.43" && m.costUnknown == 1)  // reported 0.01 + estimated 0.42
        let r = eight.summary(.spend)
        #expect(!r.totalText.hasPrefix("~") && r.bars.allSatisfy { !$0.text.hasPrefix("~") })
    }

    @Test func humanNumbersAndEmptyAndGaps() throws {
        let big = UsageChart(body: body([row("2026-11-15", "a", events: 1, tin: 46_000_000, tout: 100_000)]))!.summary(.tokens)
        #expect(big.totalText == "46.1M")
        let empty = try UsageChart(body: fixtureBody("empty"))!.summary(.tokens)
        #expect(empty.isEmpty && empty.bars.isEmpty && empty.top.isEmpty && empty.totalText == "0")
        let g = UsageChart(body: body([], gaps: "{\"count\":4,\"from\":\"2026-11-14T10:00:00Z\",\"to\":\"2026-11-14T11:00:00Z\"}"))!
        #expect(g.gaps.count == 1 && g.gaps[0].count == 4 && g.gaps[0].from != nil && g.gaps[0].to != nil)
        #expect(UsageChart(body: Data("[1]".utf8)) == nil)
    }
}
