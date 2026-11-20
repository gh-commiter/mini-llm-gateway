// minigw project | note | usage: glue between the command line and RouterCore's ProjectEnv (the logic and its tests live in RouterCore).
import Foundation
import RouterCore

/// The project id the next local proxy sends with its requests (set by the run path before route; "" = none).
var proxyProject = ""

func projectCommand(_ kind: String, _ args: [String]) -> Never {
    let agent = agentConn()  // inside an agent run: the run port and its token; KB reads use the run's scope
    let runMode = ProcessInfo.processInfo.environment["MINIGW_AGENT_URL"] != nil || ProcessInfo.processInfo.environment["MINIGW_AGENT_AUTH_FILE"] != nil
    let env = ProjectEnv(
        connect: {
            if let a = agent { return (ProjectConn(url: a.url, token: a.token), "") }
            let p = requireProfile()
            let c = route(p)
            return (ProjectConn(url: c.url, token: c.token), Projects.gatewayID(url: p.conn.url, key: p.conn.key))
        },
        state: Projects.statePath(), cwd: FileManager.default.currentDirectoryPath,
        stdin: {  // until the end of input or one byte over the limit (a pipe may deliver in pieces)
            var all = Data()
            while all.count <= ProjectEnv.maxNoteBody, let piece = try? FileHandle.standardInput.read(upToCount: ProjectEnv.maxNoteBody + 1 - all.count), !piece.isEmpty { all += piece }
            return all
        },
        out: { try? out.write(contentsOf: Data($0.utf8)) }, err: { try? err.write(contentsOf: Data($0.utf8)) },
        agent: runMode, runConn: agent.map { ProjectConn(url: $0.url, token: $0.token) },
        send: ProjectEnv.olderRouteHint { c, method, path, headers, body in
            if let a = agent {  // the run port, if need be through the sandbox's proxy
                var status = 0, data = Data()
                _ = agentHTTP(Connection(url: a.url, token: a.token), method, path, body: body, onHead: { status = $0 }) { if data.count < maxReply { data.append($0) } }
                return ProjectReply(status: status, data: data)
            }
            return ProjectHTTP.send(c, method, path, headers, body, clientInfo: clientInfo)
        })
    env.gateway = { loadProfiles().first.map { Projects.gatewayID(url: $0.conn.url, key: $0.conn.key) } ?? "" }  // kb hooks: no profile is not an error
    env.hookConn = {  // the session start: the default profile's connection, never an exit
        guard agent == nil, let p = loadProfiles().first, case .success(let c) = routeResult(p) else { return nil }
        return ProjectConn(url: c.url, token: c.token)
    }
    do {
        switch kind {
        case "kb":
            if args.first == "mcp" {
                let executable = Bundle.main.executableURL?.path ?? URL(fileURLWithPath: CommandLine.arguments[0]).standardizedFileURL.path
                try env.kbMCP(Array(args.dropFirst()), executable: executable,
                              read: { let d = try FileHandle.standardInput.read(upToCount: 4096); return d?.isEmpty == true ? nil : d },
                              write: { try FileHandle.standardOutput.write(contentsOf: $0) })
            } else { try env.kb(args) }
        case "team": try env.team(args)
        case "note": try env.note(args)
        case "usage": try env.usage(args)
        case "running": try env.running(args)
        default: try env.project(args)
        }
    } catch {
        var message = (error as? ProjectError)?.message ?? "\(error)"
        if let e = error as? ProjectError, agent == nil, let p = loadProfiles().first {
            message = ProjectError.hint(message, status: e.gatewayStatus, profile: p.name, url: p.conn.url, profiles: profileNames().count)
        }
        fail(message)
    }
    exit(0)
}
