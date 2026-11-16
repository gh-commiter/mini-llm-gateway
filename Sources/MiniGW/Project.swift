// minigw project | note | usage: glue between the command line and RouterCore's ProjectEnv (the logic and its tests live in RouterCore).
import Foundation
import RouterCore

/// The project id the next local proxy sends with its requests (set by the run path before route; "" = none).
var proxyProject = ""

func projectCommand(_ kind: String, _ args: [String]) -> Never {
    let agent = agentConn()  // inside an agent run: the run port and its token; only note list|read work, on the run's own project
    let env = ProjectEnv(
        connect: {
            if let a = agent { return (ProjectConn(url: a.url, token: a.token), "") }
            let p = requireProfile()
            let c = route(p)
            return (ProjectConn(url: c.url, token: c.token), Projects.gatewayID(url: p.conn.url, key: p.conn.key))
        },
        state: Projects.statePath(), cwd: FileManager.default.currentDirectoryPath,
        stdin: { (try? FileHandle.standardInput.read(upToCount: ProjectEnv.maxNoteBody + 1)) ?? Data() },
        out: { try? out.write(contentsOf: Data($0.utf8)) }, err: { try? err.write(contentsOf: Data($0.utf8)) },
        agent: agent != nil, runConn: agent.map { ProjectConn(url: $0.url, token: $0.token) },
        send: { c, method, path, headers, body in
            if let a = agent {  // the run port, if need be through the sandbox's proxy
                var status = 0, data = Data()
                _ = agentHTTP(Connection(url: a.url, token: a.token), method, path, body: body, onHead: { status = $0 }) { if data.count < maxReply { data.append($0) } }
                return ProjectReply(status: status, data: data)
            }
            return ProjectHTTP.send(c, method, path, headers, body, clientInfo: clientInfo)
        })
    do {
        switch kind {
        case "kb": try env.kb(args)
        case "team": try env.team(args)
        case "note": try env.note(args)
        case "usage": try env.usage(args)
        default: try env.project(args)
        }
    } catch { fail((error as? ProjectError)?.message ?? "\(error)") }
    exit(0)
}
