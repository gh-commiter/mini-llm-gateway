import Foundation
import Testing
@testable import RouterCore

private let sessionRepo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
private func sessionFixture() throws -> [String: Any] {
    try JSONSerialization.jsonObject(with: Data(contentsOf: sessionRepo.appendingPathComponent("client/testdata/project-sessions.json"))) as! [String: Any]
}

@Suite struct SessionHistoryTests {
    @Test func sharedTextAndJSON() throws {
        let page = try sessionFixture()
        let want = try String(contentsOf: sessionRepo.appendingPathComponent("client/testdata/project-sessions.txt"), encoding: .utf8)
        #expect(SessionHistory.text(page) == want)
        let json = try SessionHistory.json(page)
        #expect(!json.contains("prompt") && !json.contains("folder") && json.contains("1.234567891"))
    }

    @Test func pagesAndRepeatedCursor() throws {
        var calls = 0
        let page = try SessionHistory.collect { after in
            calls += 1
            if after.isEmpty { return ["sessions": [["id": "a"]], "next": "cursor"] }
            #expect(after == "cursor")
            return ["sessions": [["id": "b"]]]
        }
        #expect(calls == 2 && (page["sessions"] as? [[String: Any]])?.count == 2)
        #expect(throws: ProjectError.self) { try SessionHistory.collect { _ in ["sessions": [], "next": "same"] } }
    }

    @Test func memberAndOwnerCommands() throws {
        var output = "", paths: [String] = []
        let fixture = try sessionFixture()
        let env = ProjectEnv(connect: { (ProjectConn(url: "http://test", token: "tok"), "gw") }, state: "/unused", cwd: "/unused", stdin: { Data() }, out: { output += $0 }, err: { _ in }, agent: false, send: { _, _, path, _, _ in
            paths.append(path)
            return ProjectReply(status: 200, data: try! JSONSerialization.data(withJSONObject: fixture))
        })
        try env.project(["sessions", "7", "--model", "a", "--device", "d"])
        #expect(paths.first?.contains("/projects/7/sessions?") == true)
        #expect(paths.first?.contains("model=a") == true && paths.first?.contains("device=d") == true)
        #expect(output == SessionHistory.text(fixture))
        env.agent = true
        #expect(throws: ProjectError.self) { try env.project(["sessions", "7"]) }
        paths = []; output = ""
        let admin = AdminCommands(call: { _, path, _ in
            paths.append(path)
            if path == "/projects" { return (200, Data(#"{"projects":[{"id":7,"slug":"p"}]}"#.utf8)) }
            return (200, try! JSONSerialization.data(withJSONObject: fixture))
        }, out: { output += $0 })
        try admin.run(["project-sessions"], AdminOpts(project: "p", model: "a", device: "d"))
        #expect(paths.last?.hasPrefix("/usage/sessions?") == true && output == SessionHistory.text(fixture))
    }

    @Test func deviceNamesUseCurrentRegistryOnly() throws {
        var c = RouterConfig()
        c.devices = [DeviceConfig(id: "d", name: "Renamed")]
        let app = FakeApp(c)
        let response = CoreResponse(status: 200, body: try JSONSerialization.data(withJSONObject: sessionFixture()))
        let named = app.router().sessionNames(response)
        #expect(String(decoding: named.body, as: UTF8.self).contains("Renamed"))
        app.config.devices = []
        let missing = app.router().sessionNames(response)
        let row = JSONValue.parse(missing.body)?.dict["sessions"]?.array.first?.dict
        #expect(row?["deviceName"]?.string == "d" && row?["userId"]?.int == 3)
    }
}
