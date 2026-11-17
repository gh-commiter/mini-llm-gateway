import Foundation
import Testing
@testable import RouterCore

/// An option that a command's own help text names must not be answered with "unknown option": the Mac CLI's real argument parser
/// (AdminArgs.parse and usageError, the functions RouterCommand.swift calls) gets every advertised --flag.
@Suite struct AdminArgsParity {
    static func flags(in line: String) -> [String] {
        let re = try! NSRegularExpression(pattern: #"--([a-z][a-z-]*)"#)
        return re.matches(in: line, range: NSRange(line.startIndex..., in: line)).map { String(line[Range($0.range(at: 1), in: line)!]) }
    }
    static func line(_ help: String, _ key: String) -> String { help.components(separatedBy: "\n").first { $0.contains(key) } ?? "" }

    @Test func everyAdvertisedOptionOfGwUsageIsAccepted() {
        let valueless: Set<String> = ["json", "csv", "legacy"]
        let advertised = Set(Self.flags(in: UsageReport.help))
        #expect(advertised.count >= 8)
        for f in advertised {
            let argv = ["usage", "--" + f] + (valueless.contains(f) ? [] : ["x"])
            let (pos, flags) = AdminArgs.parse(argv)
            #expect(flags[f] != nil && pos == ["usage"], "--\(f) was not parsed")
            #expect(AdminArgs.usageError(pos, flags) == nil, "--\(f) is named in the help of gw usage and answered \"unknown option\"")
        }
        let (pos, flags) = AdminArgs.parse(["usage", "--by=phase"])
        #expect(flags["by"] == "phase" && AdminArgs.usageError(pos, flags) == nil)
        // the control: an option nobody advertises is refused
        let (p2, f2) = AdminArgs.parse(["usage", "--no-such-option"])
        #expect(AdminArgs.usageError(p2, f2)?.contains("unknown option --no-such-option") == true)
    }

    @Test func everyAdvertisedOptionOfTheOtherCommandsIsParsed() {
        let valueless: Set<String> = ["json", "csv", "explain", "history", "yes", "dry-run", "with-private"]
        for (cmd, key) in [("usage-events", "gw usage-events"), ("project-sessions", "gw project-sessions")] {
            let line = Self.line(AdminCommands.help, key)
            let advertised = Set(Self.flags(in: line))
            #expect(advertised.count >= 4, "\(cmd): found \(advertised) in its help")
            for f in advertised {
                let (pos, flags) = AdminArgs.parse([cmd, "--" + f] + (valueless.contains(f) ? [] : ["x"]))
                #expect(flags[f] != nil && pos == [cmd], "\(cmd): --\(f) was not parsed")
                #expect(AdminArgs.usageError(pos, flags) == nil)
            }
        }
    }

    @Test func everyAdvertisedOptionOfProjectSessionsIsAccepted() throws {
        let line = Self.line(ProjectEnv.projectHelp, "minigw project sessions")
        let valueless: Set<String> = ["json"]
        let advertised = Set(Self.flags(in: line))
        #expect(advertised.count >= 6)
        for f in advertised {
            nonisolated(unsafe) var out = ""
            nonisolated(unsafe) var requests: [String] = []
            let env = ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: "", stdin: { Data() }, out: { out += $0 }, err: { _ in }, agent: false, send: { _, m, p, _, _ in
                requests.append(p)
                return ProjectReply(status: 200, data: Data(#"{"sessions":[]}"#.utf8))
            })
            do { try env.project(["sessions", "shop", "--" + f] + (valueless.contains(f) ? [] : (f == "days" ? ["3"] : ["x"]))) }
            catch { Issue.record("project sessions --\(f) is named in its help and was refused: \(error)") }
            #expect(requests.count == 1, "--\(f): the request was not sent")
        }
    }
}

@Suite struct ProviderStatusText {
    /// The same texts as the Go gateway's gatewayReadyText (client/provider_status_test.go).
    @Test func readyLoadingFailed() {
        #expect(AdminCommands.gatewayReadyText(ready: true, state: "ready", reason: "") == ", ready")
        #expect(AdminCommands.gatewayReadyText(ready: false, state: "loading", reason: "") == ", models loading (not loaded yet)")
        #expect(AdminCommands.gatewayReadyText(ready: false, state: "failed", reason: "cannot connect: x") == ", models failed to load: cannot connect: x (see the gateway log)")
        #expect(AdminCommands.gatewayReadyText(ready: false, state: "failed", reason: "") == ", models failed to load (see the gateway log)")
        #expect(AdminCommands.gatewayReadyText(ready: false, state: "", reason: "") == ", models not loaded (see the gateway log)")
    }
}
