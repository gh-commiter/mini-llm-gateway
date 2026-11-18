import Foundation
import Testing
@testable import RouterCore

// minigw gw usage (the provider/model report), proven against the Go client: the shared fixture client/testdata/usage-report.json
// is asserted here and in client/admin_parity_test.go (TestUsageReportParity). The gateway's answers are the fixture's.

private let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()

@Suite struct UsageReportParity {
    @Test func everyCaseMatchesTheGoOutput() throws {
        let fx = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/usage-report.json"))) as! [String: Any]
        let cases = fx["cases"] as! [[String: Any]]
        #expect(cases.count >= 10)
        for c in cases {
            let name = c["name"] as! String
            let responses = c["responses"] as! [[String: Any]]
            nonisolated(unsafe) var requests: [[String]] = []
            nonisolated(unsafe) var outText = ""
            let cmd = AdminCommands(call: { method, path, body in
                requests.append([method, path, body.map { String(decoding: $0, as: UTF8.self) } ?? ""])
                guard let r = responses.first(where: { ($0["match"] as! String) == method + " " + path }) else {
                    return (599, Data(#"{"error":{"message":"no such response in the fixture"}}"#.utf8))
                }
                return (r["status"] as! Int, Data((r["body"] as! String).utf8))
            }, out: { outText += $0 })
            let o = c["opts"] as! [String: Any]
            var exit: Int32 = 0, errText = ""
            do {
                try cmd.run(c["args"] as! [String], AdminOpts(days: o["days"] as? String ?? "7", model: o["model"] as? String ?? "",
                                                              provider: o["provider"] as? String ?? "", device: o["device"] as? String ?? "",
                                                              folder: o["folder"] as? String ?? "", sort: o["sort"] as? String ?? "", json: o["json"] as? Bool ?? false,
                                                              csv: o["csv"] as? Bool ?? false, by: o["by"] as? String ?? "", phase: o["phase"] as? String ?? "",
                                                              activity: o["activity"] as? String ?? "", task: o["task"] as? String ?? "", labelSource: o["labelSource"] as? String ?? ""))
            } catch let e as AdminCLIError { exit = e.code; errText = "minigw: " + e.message + "\n" }
            let want = c["want"] as! [String: Any]
            #expect(outText == want["out"] as! String, "\(name): stdout")
            #expect(errText == want["err"] as! String, "\(name): stderr")
            #expect(Int(exit) == want["exit"] as! Int, "\(name): exit")
            #expect(requests == want["requests"] as! [[String]], "\(name): requests")
        }
    }

    @Test func humanNumbers() {
        #expect([0, 999, 1000, 1234, 914_000, 999_999, 46_100_000, 3_000_000_000].map(UsageReport.human) == ["0", "999", "1K", "1.2K", "914K", "1M", "46.1M", "3B"])
    }
}
