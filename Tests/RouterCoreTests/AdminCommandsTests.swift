import Foundation
import Testing
@testable import RouterCore

// The Mac client's owner commands (minigw gw user|bind|project|member|grant|core|usage-events|quota), proven against the Go client: the
// shared fixture client/testdata/admin-commands.json is asserted here and in client/admin_parity_test.go. Everything is fake: the
// gateway's answers are the fixture's, nothing touches the app, the keychain or a real folder.

private let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()

private func fixtureJSON(_ name: String) throws -> [String: Any] {
    try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/" + name))) as! [String: Any]
}

@Suite struct AdminCommandParity {
    @Test func everyCommandMatchesTheGoOutput() throws {
        let cases = try fixtureJSON("admin-commands.json")["cases"] as! [[String: Any]]
        #expect(cases.count >= 60)
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
            cmd.zone = TimeZone(identifier: "UTC")!
            let o = c["opts"] as! [String: String]
            var exit: Int32 = 0, errText = ""
            do {
                try cmd.run(c["args"] as! [String], AdminOpts(days: o["days"] ?? "7", user: o["user"] ?? "", project: o["project"] ?? "",
                                                              model: o["model"] ?? "", claudeProbe: o["claudeProbe"] ?? "", team: o["team"] ?? "",
                                                              phase: o["phase"] ?? "", activity: o["activity"] ?? "", task: o["task"] ?? ""))
            } catch let e as AdminCLIError { exit = e.code; errText = "minigw: " + e.message + "\n" }
            let want = c["want"] as! [String: Any]
            #expect(outText == want["out"] as! String, "\(name): stdout")
            #expect(errText == want["err"] as! String, "\(name): stderr")
            #expect(Int(exit) == want["exit"] as! Int, "\(name): exit")
            #expect(requests == want["requests"] as! [[String]], "\(name): requests")
        }
    }

    @Test func theDefaultBackupNameIsAUTCStamp() {
        nonisolated(unsafe) var body = ""
        let cmd = AdminCommands(call: { _, _, b in body = String(decoding: b ?? Data(), as: UTF8.self); return (200, Data(#"{"file":"/x/core.db"}"#.utf8)) }, out: { _ in })
        cmd.now = { Date(timeIntervalSince1970: 1_700_000_000) }
        try? cmd.run(["core", "backup"], AdminOpts())
        #expect(body == #"{"name":"core-20231114T221320Z.db"}"#)
    }

    @Test func goFloatsAndMaps() {
        #expect(GoText.g(1_000_000) == "1e+06")
        #expect(GoText.g(123456) == "123456")
        #expect(GoText.g(1234567.5) == "1.2345675e+06")
        #expect(GoText.g(2.5) == "2.5")
        #expect(GoText.g(0.0001) == "0.0001")
        #expect(GoText.g(0.00001) == "1e-05")
        #expect(GoText.f(1e21) == "1000000000000000000000")
        #expect(GoText.f(0.5) == "0.5")
        #expect(GoText.f(100) == "100")
        #expect(GoText.v(["b": 1, "a": [true, "x"]] as [String: Any]) == "map[a:[true x] b:1]")
    }

    @Test func aDeviceIDIsTheKeyEncodedAsGoChecksIt() {
        #expect(AdminCommands.validDeviceID(String(repeating: "A", count: 43)))
        #expect(!AdminCommands.validDeviceID(String(repeating: "A", count: 42) + "B"))  // spare bits set
        #expect(!AdminCommands.validDeviceID("short"))
    }
}

@Suite struct CoreRestoreOffline {
    @Test func refusedWhileTheAppRuns() {
        let why = CoreRestore.check(appRunning: true)
        #expect(why?.contains("quit the app first") == true && why?.contains("Nothing was changed") == true)
        #expect(CoreRestore.check(appRunning: false) == nil)
    }

    @Test func theHelperIsStartedInRestoreModeOnly() {
        let a = CoreRestore.argv(binary: "/h/minigw-core", store: "/s", file: "b.db", yes: true)
        #expect(a == ["/h/minigw-core", "restore", "-store", "/s", "-yes", "--", "b.db"])  // never the socket mode: no -store before "restore"
        #expect(CoreRestore.argv(binary: "/h/minigw-core", store: "/s", file: "-x", yes: false) == ["/h/minigw-core", "restore", "-store", "/s", "--", "-x"])
    }
}
