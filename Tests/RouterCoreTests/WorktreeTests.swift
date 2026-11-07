import Foundation
import Testing
@testable import RouterCore

/// git za teste (kot uporabnik: z nastavitvami repozitorija).
@discardableResult
private func g(_ dir: URL, _ args: String...) -> String {
    Worktree.run(["-c", "user.name=t", "-c", "user.email=t@x"] + args, cwd: dir.path, safe: false).out
}

private func file(_ dir: URL, _ path: String, _ text: String) throws {
    let u = dir.appendingPathComponent(path)
    try FileManager.default.createDirectory(at: u.deletingLastPathComponent(), withIntermediateDirectories: true)
    try Data(text.utf8).write(to: u)
}

/// Repozitorij odjemalca z commitom, spremembo in novo datoteko (neshranjeno) ter router (zrcalo, kloni).
private func fixture() throws -> (root: URL, client: URL, tree: ShareHub.Tree) {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("wt-" + UUID().uuidString)
    let client = root.appendingPathComponent("client")
    try FileManager.default.createDirectory(at: client, withIntermediateDirectories: true)
    g(client, "init", "-q", "-b", "main")
    try file(client, "a.txt", "one\n")
    g(client, "add", "a.txt"); g(client, "commit", "-qm", "first")
    try file(client, "a.txt", "one\nuncommitted\n")
    try file(client, "new.txt", "untracked\n")
    return (root, client, ShareHub.Tree(repo: root.appendingPathComponent("repos/dev/x.git"), worktrees: root.appendingPathComponent("worktrees/dev/x"), sub: ""))
}

/// minigw za teste: obdela WORKTREE-PUSH in WORKTREE-PULL kot ShareClient (MiniGW/main.swift) prek ShareHub.
private func fakeClient(_ hub: ShareHub, key: String, top: URL) -> Task<Void, Never> {
    Task {
        while !Task.isCancelled {
            guard case .some(.some(let job)) = await hub.poll(key: key, wait: 0.2) else { continue }
            var r = DAVResponse(403)
            if job.method == "WORKTREE-PUSH" {
                let have = (try? JSONSerialization.jsonObject(with: job.body) as? [String: Any])?["have"] as? [String] ?? []
                let snap = Worktree.snapshot(top: top.path)!
                let f = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".bundle")
                if Worktree.bundle(top: top.path, snapshot: snap, have: have, to: f) == true {
                    _ = await hub.worktreePut(key: key, part: 0, data: try! Data(contentsOf: f), device: "dev")
                }
                try? FileManager.default.removeItem(at: f)
                r = DAVResponse(200, [:], try! JSONSerialization.data(withJSONObject: ["snapshot": snap]))
            } else if job.method == "WORKTREE-PULL" {
                let branch = (try? JSONSerialization.jsonObject(with: job.body) as? [String: Any])?["branch"] as? String ?? ""
                let f = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".bundle")
                if let d = await hub.worktreeResult(key: key, device: "dev") { try? d.write(to: f) }
                r = DAVResponse(Worktree.fetch(top: top.path, bundle: f, branch: branch) == nil ? 200 : 500)
                try? FileManager.default.removeItem(at: f)
            }
            _ = await hub.reply(key: key, DAVReply(id: job.id, response: r))
        }
    }
}

/// Začetek izvajanja agenta (mount v nalogi izvajanja); `work` v klonu, nato syncBack.
private func agentRun(_ hub: ShareHub, _ session: String, agent: String = "claude", id: String = UUID().uuidString,
                      work: (URL) throws -> Void) async throws -> String {
    try await ShareHub.$run.withValue(id) {
        try await ShareHub.$agent.withValue(agent) {
            let dir = try #require(await hub.mount(session: session, cwd: nil, base: "http://127.0.0.1:1", device: "dev"))
            try work(dir)
            await hub.syncBack(session: session)
        }
    }
    return id
}

@Test func worktreeRoundTripParallelBranchesAndUserFilesUntouched() async throws {
    let (root, client, tree) = try fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let status = g(client, "status", "--porcelain"), head = g(client, "rev-parse", "HEAD")
    let index = try Data(contentsOf: client.appendingPathComponent(".git/index"))
    let config = try Data(contentsOf: client.appendingPathComponent(".git/config"))
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }

    // Dve vzporedni izvajanji: dve mesti, dve veji; obe začneta iz stanja odjemalca (tudi neshranjenega).
    let (a, b) = (UUID().uuidString, UUID().uuidString)
    let (dirA, dirB) = try await (ShareHub.$run.withValue(a) { try await ShareHub.$agent.withValue("claude") { try #require(await hub.mount(session: session, cwd: nil, base: "", device: "dev")) } },
                                  ShareHub.$run.withValue(b) { try await ShareHub.$agent.withValue("claude") { try #require(await hub.mount(session: session, cwd: nil, base: "", device: "dev")) } })
    let aText = try String(contentsOf: dirA.appendingPathComponent("a.txt"), encoding: .utf8)
    #expect(dirA != dirB && aText == "one\nuncommitted\n")
    #expect(FileManager.default.fileExists(atPath: dirB.appendingPathComponent("new.txt").path))
    try file(dirA, "agentA.txt", "A\n"); g(dirA, "add", "agentA.txt"); g(dirA, "commit", "-qm", "agent A")
    try file(dirA, "leftover.txt", "not committed by the agent\n")  // router ga shrani sam
    try file(dirB, "agentB.txt", "B\n")
    for id in [a, b] { await ShareHub.$run.withValue(id) { await hub.syncBack(session: session) }; await hub.finish(run: id) }

    let short = String(Sync.folderID(session).prefix(8))
    let (brA, brB) = ("minigw/claude-\(short)-0", "minigw/claude-\(short)-1")
    #expect(g(client, "show", "\(brA):agentA.txt") == "A" && g(client, "show", "\(brA):leftover.txt") == "not committed by the agent")
    #expect(g(client, "show", "\(brB):agentB.txt") == "B" && g(client, "show", "\(brB)~1:new.txt") == "untracked")
    #expect(g(client, "log", "--format=%s", brA).contains("minigw: your uncommitted changes"))
    // Uporabnikove datoteke, indeks, HEAD in nastavitve nespremenjeni; nič izpuščenih refov ali FETCH_HEAD.
    #expect(g(client, "status", "--porcelain") == status && g(client, "rev-parse", "HEAD") == head)
    #expect(try Data(contentsOf: client.appendingPathComponent(".git/index")) == index)
    #expect(try Data(contentsOf: client.appendingPathComponent(".git/config")) == config)
    #expect(g(client, "for-each-ref", "--format=%(refname)", "refs/minigw").isEmpty)
    #expect(!FileManager.default.fileExists(atPath: client.appendingPathComponent(".git/FETCH_HEAD").path))

    // Naslednja zahteva iste seje nadaljuje na veji -0 (isti klon, brez novega prenosa).
    _ = try await agentRun(hub, session) { dir in
        #expect(dir == dirA)
        try file(dir, "second.txt", "2\n")
    }
    #expect(g(client, "show", "\(brA):second.txt") == "2")
}

@Test func worktreeMaliciousAgentCannotRunAnythingOnTheClientOrRouter() async throws {
    let (root, client, tree) = try fixture()
    defer { try? FileManager.default.removeItem(at: root) }
    let marks = root.appendingPathComponent("marks")
    try FileManager.default.createDirectory(at: marks, withIntermediateDirectories: true)
    // Uporabnik ima filter, ki bi ga .gitattributes v veji lahko sprožil, in svoje hooke (prevzem jih ne sme zagnati).
    g(client, "config", "filter.evil.smudge", "touch \(marks.path)/client-smudge; cat")
    g(client, "config", "filter.evil.clean", "touch \(marks.path)/client-clean; cat")
    let hooks = client.appendingPathComponent(".git/hooks")
    for h in ["reference-transaction", "post-checkout", "post-merge", "post-receive"] {
        try file(hooks, h, "#!/bin/sh\ntouch \(marks.path)/hook-\(h)\n")
        chmod(hooks.appendingPathComponent(h).path, 0o755)
    }
    let config = try Data(contentsOf: client.appendingPathComponent(".git/config"))
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }

    _ = try await agentRun(hub, session) { dir in
        try file(dir, ".gitattributes", "* filter=evil\n")
        try file(dir, ".gitmodules", "[submodule \"x\"]\n\tpath = x\n\turl = file://\(marks.path)/evil-sub\n")
        g(dir, "add", "-A"); g(dir, "commit", "-qm", "attributes and submodules")
        // Agentov .git: nastavitve in hooki, ki bi jih git routerja ali odjemalca zagnal, če bi jih bral.
        let cfg = dir.appendingPathComponent(".git/config")
        try (try String(contentsOf: cfg, encoding: .utf8) + "[core]\n\tfsmonitor = touch \(marks.path)/router-fsmonitor\n"
             + "[filter \"evil\"]\n\tclean = touch \(marks.path)/router-clean; cat\n").write(to: cfg, atomically: true, encoding: .utf8)
        try file(dir, ".git/hooks/post-commit", "#!/bin/sh\ntouch \(marks.path)/router-hook\n")
        chmod(dir.appendingPathComponent(".git/hooks/post-commit").path, 0o755)
        try file(dir, "leftover.txt", "x\n")  // router ga shrani z »git add« pod nastavitvami zrcala
    }
    let short = String(Sync.folderID(session).prefix(8)), branch = "minigw/claude-\(short)-0"
    #expect(g(client, "show", "\(branch):.gitattributes") == "* filter=evil")  // vsebina pride, izvede se nič
    #expect(g(client, "show", "\(branch):leftover.txt") == "x")
    #expect(((try? FileManager.default.contentsOfDirectory(atPath: marks.path)) ?? []).isEmpty, "\((try? FileManager.default.contentsOfDirectory(atPath: marks.path)) ?? [])")
    #expect(try Data(contentsOf: client.appendingPathComponent(".git/config")) == config)
    #expect(!FileManager.default.fileExists(atPath: client.appendingPathComponent("x").path))

    // Drevo z vnosom ».git« (git ga sam ne doda, agent ga sestavi z mktree): fsck pri odjemalcu ga zavrne.
    _ = try await agentRun(hub, session) { dir in
        let blob = g(dir, "hash-object", "-w", "--stdin")  // prazen
        let evil = Worktree.run(["mktree"], cwd: dir.path, safe: false, input: Data("100644 blob \(blob)\thooks-post-checkout\n".utf8)).out
        let top = Worktree.run(["mktree"], cwd: dir.path, safe: false, input: Data("040000 tree \(evil)\t.git\n".utf8)).out
        let c = g(dir, "commit-tree", top, "-p", "HEAD", "-m", "dot git")
        g(dir, "update-ref", "HEAD", c)
    }
    #expect(!g(client, "ls-tree", "-r", "--name-only", branch).contains(".git/"))  // veja ni posodobljena
    #expect(((try? FileManager.default.contentsOfDirectory(atPath: marks.path)) ?? []).isEmpty)
    #expect(try Data(contentsOf: client.appendingPathComponent(".git/config")) == config)
}

@Test func worktreeAgentSandboxAndModes() throws {
    let root = URL(fileURLWithPath: "/Users/u/Library/Caches/dev.trojar.minigateway/Scratch/worktrees/dev/x/claude-1")
    let a = ClaudeCLIConfig().worktreeArgs(root: root, cwd: root.appendingPathComponent("sub"), allTools: ["Bash", "WebFetch"], network: false)
    let settings = try #require(a.firstIndex(of: "--settings").map { a[$0 + 1] })
    let sb = try #require((try JSONSerialization.jsonObject(with: Data(settings.utf8)) as? [String: Any])?["sandbox"] as? [String: Any])
    let fs = try #require(sb["filesystem"] as? [String: [String]])
    #expect(fs["denyRead"] == ["~"] && fs["allowWrite"]?.first == root.path && sb["allowUnsandboxedCommands"] as? Bool == false)
    #expect(!(fs["allowWrite"] ?? []).contains { $0.hasSuffix("/Library/Caches") || $0.hasSuffix("/Development") })  // ne scratch, ne dovoljene mape
    #expect((sb["network"] as? [String: [String]])?["allowedDomains"] == [])
    #expect(a.contains("--add-dir") && a.contains("default") && a.contains("acceptEdits"))
    #expect(Worktree.scratchIsSafe(URL(fileURLWithPath: NSHomeDirectory() + "/Library/Caches/dev.trojar.minigateway/Scratch")))
    #expect(!Worktree.scratchIsSafe(URL(fileURLWithPath: "/private/tmp/scratch")) && !Worktree.scratchIsSafe(URL(fileURLWithPath: NSHomeDirectory() + "/.m2/s")))
    #expect(Worktree.validBranch("minigw/claude-0123abcd-2") && !Worktree.validBranch("main") && !Worktree.validBranch("minigw/x/../../main"))

    #expect(SharedFolders.candidates(work: "auto", readOnly: true, smb: true, git: true) == ["direct"])
    #expect(SharedFolders.candidates(work: "auto", readOnly: false, smb: true, git: true) == ["native", "worktree", "sync", "direct"])
    #expect(SharedFolders.candidates(work: "auto", readOnly: false, smb: false, git: true) == ["worktree", "sync", "direct"])
    #expect(SharedFolders.candidates(work: "auto", readOnly: false, smb: false, git: false) == ["sync", "direct"])
    #expect(SharedFolders.candidates(work: "worktree", readOnly: false, smb: false, git: false) == ["direct"])
    #expect(SharedFolders.mode(flag: nil, saved: SharedFolders.Entry(path: "/p", work: "worktree"), readOnly: true) == ("direct", true))
    #expect(SharedFolders.mode(flag: "auto", saved: SharedFolders.Entry(path: "/p", work: "sync"), readOnly: false) == ("auto", false))
}
