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
    // Router kot s privzetim Scratch (~/Library/Caches/…): v domači mapi, ki jo peskovnik finalize sicer zavrne.
    let scratch = URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent("Library/Caches/minigw-test-" + UUID().uuidString)
    try FileManager.default.createSymbolicLink(at: root.appendingPathComponent("scratch"), withDestinationURL: scratch)  // pospravi defer
    return (root, client, ShareHub.Tree(repo: scratch.appendingPathComponent("repos/dev/x.git"), worktrees: scratch.appendingPathComponent("worktrees/dev/x"), sub: ""))
}

/// Odstrani fixture in Scratch v ~/Library/Caches (povezava root/scratch).
private func cleanup(_ root: URL) {
    if let s = try? FileManager.default.destinationOfSymbolicLink(atPath: root.appendingPathComponent("scratch").path) {
        try? FileManager.default.removeItem(atPath: s)
    }
    try? FileManager.default.removeItem(at: root)
}

/// minigw za teste: obdela WORKTREE-PUSH in WORKTREE-PULL kot ShareClient (MiniGW/main.swift) prek ShareHub.
private func fakeClient(_ hub: ShareHub, key: String, top: URL) -> Task<Void, Never> {
    Task {
        while !Task.isCancelled {
            guard case .some(.some(let job)) = await hub.poll(key: key, wait: 0.2) else { continue }
            var r = DAVResponse(403)
            if job.method == "WORKTREE-PUSH" {
                let have = (try? JSONSerialization.jsonObject(with: job.body) as? [String: Any])?["have"] as? [String] ?? []
                let f = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".bundle")
                // git odjemalca zunaj bazena Swift concurrency (kot minigw v svojem procesu): vzporedni testi ne čakajo nanj
                let (snap, made) = await offPool { () -> (String, Bool) in
                    let snap = Worktree.snapshot(top: top.path)!
                    return (snap, Worktree.bundle(top: top.path, snapshot: snap, have: have, to: f) == true)
                }
                if made { _ = await hub.worktreePut(key: key, part: 0, data: try! Data(contentsOf: f), device: "dev") }
                try? FileManager.default.removeItem(at: f)
                r = DAVResponse(200, [:], try! JSONSerialization.data(withJSONObject: ["snapshot": snap]))
            } else if job.method == "WORKTREE-PULL" {
                let branch = (try? JSONSerialization.jsonObject(with: job.body) as? [String: Any])?["branch"] as? String ?? ""
                let f = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".bundle")
                if let d = await hub.worktreeResult(key: key, device: "dev") { try? d.write(to: f) }
                r = DAVResponse(await offPool { Worktree.fetch(top: top.path, bundle: f, branch: branch) } == nil ? 200 : 500)
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
    defer { cleanup(root) }
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
    defer { cleanup(root) }
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
        // L2: lažni alternates in commit-graph v objects/info klona
        try file(dir, ".git/objects/info/alternates", "/etc\n\(marks.path)/objects\n")
        try file(dir, ".git/objects/info/commit-graph", "garbage")
    }
    let wt0 = tree.worktrees.appendingPathComponent("claude-\(String(Sync.folderID(session).prefix(8)))-0")
    #expect((try? String(contentsOf: wt0.appendingPathComponent(".git/objects/info/alternates"), encoding: .utf8))
            == tree.repo.appendingPathComponent("objects").path + "\n")
    #expect(!FileManager.default.fileExists(atPath: wt0.appendingPathComponent(".git/objects/info/commit-graph").path))
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
    #expect(((try? FileManager.default.contentsOfDirectory(atPath: marks.path)) ?? []).isEmpty, "\((try? FileManager.default.contentsOfDirectory(atPath: marks.path)) ?? [])")
    #expect(try Data(contentsOf: client.appendingPathComponent(".git/config")) == config)
}

@Test func worktreeAgentSandboxAndModes() throws {
    let root = URL(fileURLWithPath: "/Users/u/Library/Caches/dev.trojar.minigateway/Scratch/worktrees/dev/x/claude-1")
    let maven = root.deletingLastPathComponent().appendingPathComponent(".maven")
    let a = ClaudeCLIConfig().worktreeArgs(root: root, maven: maven, cwd: root.appendingPathComponent("sub"), allTools: ["Bash", "WebFetch"], network: false)
    let settings = try #require(a.firstIndex(of: "--settings").map { a[$0 + 1] })
    let sb = try #require((try JSONSerialization.jsonObject(with: Data(settings.utf8)) as? [String: Any])?["sandbox"] as? [String: Any])
    let fs = try #require(sb["filesystem"] as? [String: [String]])
    #expect(fs["denyRead"] == ["~"] && fs["allowWrite"]?.first == root.path && sb["allowUnsandboxedCommands"] as? Bool == false)
    #expect(!(fs["allowWrite"] ?? []).contains { $0.hasSuffix("/Library/Caches") || $0.hasSuffix("/Development") })  // ne scratch, ne dovoljene mape
    // H1: ne lastnikovi predpomnilniki gradnje (Maven, SwiftPM), nastavitve gradnje zavrnjene tudi kanonično
    #expect(!(fs["allowWrite"] ?? []).contains { $0.hasSuffix("/.m2") || $0.contains("swiftpm") } && fs["allowWrite"]?.contains(maven.path) == true)
    #expect(Set(Native.buildCacheDenies().paths).isSubset(of: Set(fs["denyWrite"] ?? [])) && fs["denyWrite"]?.contains(root.path + "/.git/commondir") == true)
    if let real = Sync.real(URL(fileURLWithPath: NSHomeDirectory() + "/.swiftpm/configuration")), real.path != NSHomeDirectory() + "/.swiftpm/configuration" {
        #expect(fs["denyWrite"]?.contains(real.path) == true)  // povezava ~/.swiftpm/configuration → ~/Library/org.swift.swiftpm/…
    }
    let env = Worktree.harnessEnv(root: root, maven: maven)
    #expect(env["GIT_DIR"] == root.path + "/.git" && env["GIT_WORK_TREE"] == root.path && env["GIT_COMMON_DIR"] == root.path + "/.git"
            && env["MAVEN_OPTS"] == "-Dmaven.repo.local=" + maven.path + " -Djava.io.tmpdir=" + root.path + "/.build/tmp")
    #expect((sb["network"] as? [String: [String]])?["allowedDomains"] == [])
    #expect(a.contains("--add-dir") && a.contains("default") && a.contains("acceptEdits"))
    #expect(Array((fs["denyWrite"] ?? []).prefix(4)) == ["config", "hooks", "info", "commondir"].map { root.path + "/.git/" + $0 })
    #expect(a[a.firstIndex(of: "--disallowedTools")! + 1].contains("Edit(/" + root.path + "/**/.git/**)"))
    #expect(Worktree.scratchIsSafe(URL(fileURLWithPath: NSHomeDirectory() + "/Library/Caches/dev.trojar.minigateway/Scratch")))
    #expect(!Worktree.scratchIsSafe(URL(fileURLWithPath: "/private/tmp/scratch")))
    #expect(Worktree.validBranch("minigw/claude-0123abcd-2") && !Worktree.validBranch("main") && !Worktree.validBranch("minigw/x/../../main"))

    #expect(SharedFolders.candidates(work: "auto", readOnly: true, smb: true, git: true) == ["direct"])
    // M2: repozitorij git nikoli v direct za pisanje
    #expect(SharedFolders.candidates(work: "auto", readOnly: false, smb: true, git: true) == ["native", "worktree", "sync"])
    #expect(SharedFolders.candidates(work: "auto", readOnly: false, smb: false, git: true) == ["worktree", "sync"])
    #expect(SharedFolders.candidates(work: "worktree", readOnly: false, smb: false, git: true) == ["worktree", "sync"])
    #expect(SharedFolders.candidates(work: "auto", readOnly: false, smb: false, git: false) == ["sync", "direct"])
    #expect(SharedFolders.candidates(work: "worktree", readOnly: false, smb: false, git: false) == ["direct"])
    #expect(SharedFolders.mode(flag: nil, saved: SharedFolders.Entry(path: "/p", work: "worktree"), readOnly: true) == ("direct", true))
    #expect(SharedFolders.mode(flag: "auto", saved: SharedFolders.Entry(path: "/p", work: "sync"), readOnly: false) == ("auto", false))
}

/// Agent si v klonu nastavi fsmonitor, filter (.gitattributes) in hook: git, ki ga poganja sam agent (claude -p, codex,
/// grok zunaj peskovnika orodij), z okoljem harnessEnv ne zažene ničesar; brez njega (kontrola) bi.
@Test func worktreeHarnessGitIgnoresTheClonesConfig() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("harness-" + UUID().uuidString)
    defer { cleanup(root) }
    let repo = root.appendingPathComponent("wt"), marks = root.appendingPathComponent("marks")
    try FileManager.default.createDirectory(at: marks, withIntermediateDirectories: true)
    try FileManager.default.createDirectory(at: repo, withIntermediateDirectories: true)
    g(repo, "init", "-q"); try file(repo, "a.txt", "a\n"); g(repo, "add", "-A"); g(repo, "commit", "-qm", "a")
    g(repo, "config", "core.fsmonitor", "touch \(marks.path)/fsmonitor; true")
    g(repo, "config", "filter.evil.clean", "touch \(marks.path)/clean; cat")
    try file(repo, ".gitattributes", "* filter=evil\n")
    try file(repo, ".git/hooks/pre-commit", "#!/bin/sh\ntouch \(marks.path)/hook\n")
    chmod(repo.appendingPathComponent(".git/hooks/pre-commit").path, 0o755)
    try file(repo, "a.txt", "changed\n")
    func agentGit(_ env: [String: String]) {
        for args in [["status", "--porcelain"], ["diff"], ["add", "-A"], ["-c", "user.name=a", "-c", "user.email=a@x", "commit", "-qm", "x"]] {
            Worktree.run(args, cwd: repo.path, env: env, safe: false)
        }
    }
    agentGit(Worktree.harnessEnv(root: repo, maven: root.appendingPathComponent("m2")))
    #expect(((try? FileManager.default.contentsOfDirectory(atPath: marks.path)) ?? []).isEmpty)
    try file(repo, "a.txt", "again\n")
    agentGit([:])  // kontrola: brez okolja se vse troje zažene
    #expect(Set((try? FileManager.default.contentsOfDirectory(atPath: marks.path)) ?? []) == ["fsmonitor", "clean", "hook"])
}

/// M4: bundle se preveri (glava) pri odjemalcu in na routerju; L5: veja, na katero je uporabnik commital, ostane njegova.
@Test func worktreeBundleHeaderAndNoForcedUpdate() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let notBundle = root.appendingPathComponent("x.bundle")
    try Data("not a bundle\n".utf8).write(to: notBundle)
    #expect(Worktree.fetch(top: client.path, bundle: notBundle, branch: "minigw/claude-0123abcd-0") == "the gateway did not send a git bundle")
    #expect(Worktree.fetch(top: client.path, bundle: client.appendingPathComponent(".git"), branch: "minigw/claude-0123abcd-0") != nil)
    #expect(Worktree.ensureMirror(tree.repo) && !Worktree.importBase(tree.repo, bundle: notBundle, snapshot: String(repeating: "a", count: 40), base: "refs/minigw/base/x"))

    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    _ = try await agentRun(hub, session) { dir in try file(dir, "one.txt", "1\n") }
    let branch = "minigw/claude-\(String(Sync.folderID(session).prefix(8)))-0"
    // uporabnik doda svoj commit na vejo agenta (brez checkouta)
    let mine = g(client, "commit-tree", g(client, "rev-parse", "\(branch)^{tree}"), "-p", branch, "-m", "mine")
    g(client, "update-ref", "refs/heads/" + branch, mine)
    _ = try await agentRun(hub, session) { dir in try file(dir, "two.txt", "2\n") }
    #expect(g(client, "rev-parse", branch) == mine)  // ni prepisana
}

/// H2: Grok s pisanjem v načinu worktree ni podprt (400 z navodilom za sync, preden se karkoli priklopi);
/// F2: Codex brez pisanja ni zavrnjen (v klonu le bere).
@Test func worktreeRefusesGrokAndReadOnlyCodex() async throws {
    var c = RouterConfig()
    c.grok.enabled = true; c.grok.write = true; c.codex.enabled = true; c.codex.write = false
    let r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let session = UUID().uuidString
    _ = await Router.shares.register(session: session, cwd: "/p", key: nil, device: nil,
                                     tree: ShareHub.Tree(repo: URL(fileURLWithPath: "/nonexistent/x.git"), worktrees: URL(fileURLWithPath: "/nonexistent/w"), sub: ""))
    for model in ["claude-grok", "claude-codex"] {
        var q = request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": session, "x-claude-mini-cwd": "/p"],
                        body: #"{"model":"\#(model)","max_tokens":5,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}"#)
        q.peer = "127.0.0.1"
        let w = FakeWriter()
        await r.handle(q, writer: w)
        if model == "claude-grok" {
            #expect(w.status == 400 && w.text.contains("rerun with minigw --minigw-work=sync"), "\(model): \(w.text)")
        } else {
            #expect(!w.text.contains("may not write"), "\(model): \(w.text)")
        }
    }
}

/// N1: agent zamenja .git/objects klona s povezavo ven: finalize ne briše in ne piše tam (in ne vrne veje).
/// Izhod gradnje (.build, target) nikoli v vejo (info/exclude zrcala).
@Test func worktreeFinalizeRefusesSymlinkedObjectsAndSkipsBuildOutput() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    let branch = "minigw/claude-\(String(Sync.folderID(session).prefix(8)))-0"
    _ = try await agentRun(hub, session) { dir in
        try file(dir, "ok.txt", "ok\n"); try file(dir, ".build/debug/app", "bin"); try file(dir, "target/app.jar", "jar")
    }
    #expect(g(client, "show", "\(branch):ok.txt") == "ok")
    #expect(!g(client, "ls-tree", "-r", "--name-only", branch).contains(".build") && !g(client, "ls-tree", "-r", "--name-only", branch).contains("target/"))

    let outside = root.appendingPathComponent("outside")
    try file(outside, "info/keep", "keep\n")
    let before = g(client, "rev-parse", branch)
    _ = try await agentRun(hub, session) { dir in
        try file(dir, "more.txt", "more\n")
        let objects = dir.appendingPathComponent(".git/objects")
        try FileManager.default.moveItem(at: objects, to: root.appendingPathComponent("moved-objects"))
        try FileManager.default.createSymbolicLink(at: objects, withDestinationURL: outside)
    }
    #expect(FileManager.default.fileExists(atPath: outside.appendingPathComponent("info/keep").path))
    #expect(!FileManager.default.fileExists(atPath: outside.appendingPathComponent("info/alternates").path))
    #expect(g(client, "rev-parse", branch) == before)
}

/// L1 (0.5.16): objects/info klona kot povezava ven: finalize odstrani le povezavo in naredi svojo mapo (deskriptor, brez sledenja).
@Test func worktreeFinalizeRemovesSymlinkedInfoOnly() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    let outside = root.appendingPathComponent("outside-info")
    try file(outside, "keep", "keep\n")
    var clone: URL?
    _ = try await agentRun(hub, session) { dir in
        clone = dir
        try file(dir, "ok.txt", "ok\n")
        let info = dir.appendingPathComponent(".git/objects/info")
        try FileManager.default.removeItem(at: info)
        try FileManager.default.createSymbolicLink(at: info, withDestinationURL: outside)
    }
    let info = try #require(clone).appendingPathComponent(".git/objects/info")
    #expect((try? String(contentsOf: outside.appendingPathComponent("keep"), encoding: .utf8)) == "keep\n")
    #expect(!FileManager.default.fileExists(atPath: outside.appendingPathComponent("alternates").path))
    let attrs = try? FileManager.default.attributesOfItem(atPath: info.path)
    #expect(attrs?[.type] as? FileAttributeType == .typeDirectory)
    #expect(g(client, "show", "minigw/claude-\(String(Sync.folderID(session).prefix(8)))-0:ok.txt") == "ok")
}

/// N2: git v finalize ne bere vsebine zunaj klona, zrcala in svoje začasne mape (domača mapa, zrcala drugih naprav).
@Test func worktreeFinalizeProfileDeniesOtherReads() throws {
    let scratch = URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent("Library/Caches/minigw-test-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: scratch) }
    let repo = scratch.appendingPathComponent("repos/dev/x.git"), other = scratch.appendingPathComponent("repos/other/y.git")
    let wt = scratch.appendingPathComponent("worktrees/dev/x/claude-1"), tmp = scratch.appendingPathComponent("repos/dev/.finalize-1")
    for d in [repo, other, wt, tmp] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    try file(other, "config", "secret of another device\n")
    try file(wt, "a.txt", "clone\n")
    let profile = try #require(Worktree.finalizeProfile(wt: wt, repo: repo, tmp: tmp, out: scratch.appendingPathComponent("repos/dev/x.git.result")))
    func cat(_ u: URL) throws -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
        p.arguments = ["-p", profile, "/bin/cat", u.path]
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try p.run(); p.waitUntilExit()
        return p.terminationStatus
    }
    #expect(try cat(wt.appendingPathComponent("a.txt")) == 0)
    #expect(try cat(other.appendingPathComponent("config")) != 0)
    #expect(try cat(URL(fileURLWithPath: NSHomeDirectory() + "/.zshrc")) != 0 || !FileManager.default.fileExists(atPath: NSHomeDirectory() + "/.zshrc"))
    #expect(try cat(URL(fileURLWithPath: "/private/etc/hosts")) != 0)  // tudi zunaj domače mape privzeto ne
    #expect(Worktree.run(["--version"], cwd: tmp.path, sandbox: profile).status == 0)  // git (Xcode) teče
}

/// N3: veja, izvlečena v povezani delovni mapi uporabnika, se ne premakne (git fetch brez sile jo zavrne).
@Test func worktreeBranchCheckedOutInLinkedWorktreeIsNotMoved() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    let branch = "minigw/claude-\(String(Sync.folderID(session).prefix(8)))-0"
    _ = try await agentRun(hub, session) { dir in try file(dir, "one.txt", "1\n") }
    let linked = root.appendingPathComponent("linked")
    g(client, "worktree", "add", "-q", linked.path, branch)
    let before = g(client, "rev-parse", branch)
    _ = try await agentRun(hub, session) { dir in try file(dir, "two.txt", "2\n") }
    #expect(g(client, "rev-parse", branch) == before)
    #expect(!FileManager.default.fileExists(atPath: linked.appendingPathComponent("two.txt").path))
}

/// H1 (0.5.16): agenti in minigw run ne pišejo v DARWIN_USER_TEMP_DIR/DARWIN_USER_CACHE_DIR ali njuno nadrejeno mapo
/// (predpomnilnik xcrun, ModuleCache); svoj TMPDIR in ModuleCache imajo v .build klona.
@Test func worktreeNoDarwinTempOrCacheIsWritable() throws {
    func confstrPath(_ name: Int32) -> String? {
        var b = [CChar](repeating: 0, count: 1024)
        guard confstr(name, &b, b.count) > 0 else { return nil }
        return Sync.real(URL(fileURLWithPath: String(cString: b)))?.path
    }
    let darwin = [confstrPath(_CS_DARWIN_USER_TEMP_DIR), confstrPath(_CS_DARWIN_USER_CACHE_DIR)].compactMap { $0 }
    #expect(darwin.count == 2)
    let root = URL(fileURLWithPath: "/Volumes/X/Scratch/worktrees/dev/x/claude-1")
    let a = ClaudeCLIConfig().worktreeArgs(root: root, maven: root, cwd: root, allTools: [], network: true)
    let settings = try #require(a.firstIndex(of: "--settings").map { a[$0 + 1] })
    let claudeWrite = (((try JSONSerialization.jsonObject(with: Data(settings.utf8)) as? [String: Any])?["sandbox"] as? [String: Any])?["filesystem"]
                       as? [String: [String]])?["allowWrite"] ?? []
    let lists = [Worktree.writable(root: root, maven: root.deletingLastPathComponent().appendingPathComponent(".maven")),
                 Native.buildCaches(), claudeWrite]
    for list in lists {
        for w in list {
            let real = Sync.real(URL(fileURLWithPath: w))?.path ?? w
            for d in darwin { #expect(!(d == real || d.hasPrefix(real + "/") || d == w || d.hasPrefix(w + "/")), "\(w) covers \(d)") }
        }
    }
    let env = Worktree.harnessEnv(root: root, maven: root)
    #expect(env["TMPDIR"] == root.path + "/.build/tmp/" && env["CLANG_MODULE_CACHE_PATH"] == root.path + "/.build/ModuleCache"
            && env["xcrun_nocache"] == "1")
    let codex = Codex.args(cwd: root, extraDirs: [], agent: true, output: root, write: true)
    #expect(codex.contains("sandbox_workspace_write.exclude_tmpdir_env_var=true"))
}

/// H1: git routerja ne gre skozi shim xcrun: podtaknjen predpomnilnik (env xcrun_db) nima učinka. Kontrola: shim
/// /usr/bin/git z istim predpomnilnikom izvede podtaknjen program.
@Test func routerGitIgnoresPoisonedXcrunCache() throws {
    #expect(!Worktree.git.hasPrefix("/usr/bin/"))
    let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("xcrun-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    // Pravi predpomnilnik za git, nato v njem pot do gita zamenjana s podtaknjenim programom enake dolžine.
    func shim(_ env: [String: String]) {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/git")
        p.arguments = ["--version"]
        p.environment = ProcessInfo.processInfo.environment.filter { !$0.key.hasPrefix("xcrun") }.merging(env) { $1 }
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try? p.run(); p.waitUntilExit()
    }
    let db = dir.appendingPathComponent("db")
    shim(["xcrun_db": db.path])
    let data = try Data(contentsOf: db)
    let text = String(decoding: data, as: UTF8.self)
    let real = try #require(text.components(separatedBy: CharacterSet(charactersIn: "\0\n|")).first { $0.hasSuffix("/usr/bin/git") && $0.hasPrefix("/") })
    let fakeDir = "/private/tmp/minigw-xcrun-" + UUID().uuidString.prefix(8)
    guard real.count > fakeDir.count + 5 else { return }  // pot Xcode prekratka za zamenjavo enake dolžine (ni preizkusa)
    let fake = fakeDir + "/" + String(repeating: "x", count: real.count - fakeDir.count - 5) + "/git"
    try FileManager.default.createDirectory(atPath: (fake as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(atPath: fakeDir) }
    let marker = fakeDir + "/PWNED"
    try Data("#!/bin/sh\ntouch \(marker)\n".utf8).write(to: URL(fileURLWithPath: fake))
    chmod(fake, 0o755)
    let poison = dir.appendingPathComponent("poison")
    var bytes = data  // zamenjava po bajtih (predpomnilnik je binaren); dolžine enake: zapis ostane veljaven
    while let r = bytes.range(of: Data(real.utf8)) { bytes.replaceSubrange(r, with: Data(fake.utf8)) }
    try bytes.write(to: poison)

    Worktree.run(["--version"], env: ["xcrun_db": poison.path])
    #expect(!FileManager.default.fileExists(atPath: marker))
    shim(["xcrun_db": poison.path])  // kontrola: shim podtaknjeni program izvede
    #expect(FileManager.default.fileExists(atPath: marker))
}

/// M1 (0.5.16): minigw run v DARWIN_USER_TEMP_DIR le ustvari nove mape TemporaryDirectory.* (in NSIRD_*) in piše v njih;
/// vnosov (TemporaryItems, obstoječe mape) ne more preimenovati, izbrisati ali zamenjati s povezavo, datotek v T ne.
/// Če bi peskovnik kaj dovolil, test to takoj popravi (vrne TemporaryItems, odstrani svoje).
@Test func runProfileProtectsDarwinTempEntries() throws {
    var b = [CChar](repeating: 0, count: 1024)
    #expect(confstr(_CS_DARWIN_USER_TEMP_DIR, &b, b.count) > 0)
    let t = try #require(Sync.real(URL(fileURLWithPath: String(cString: b)))).path
    let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("runprof-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    let profile = try #require(Native.sandboxProfile(dir: dir.path, network: true))
    func sh(_ c: String) -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
        p.arguments = ["-p", profile, "/bin/sh", "-c", c]
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try? p.run(); p.waitUntilExit()
        return p.terminationStatus
    }
    let id = UUID().uuidString, fm = FileManager.default
    let items = t + "/TemporaryItems", moved = items + "-minigw-test-" + id, mine = t + "/TemporaryDirectory.minigw-test-" + id
    let link = t + "/TemporaryDirectory.minigw-link-" + id
    defer {
        if fm.fileExists(atPath: moved) { rename(moved, items) }
        for p in [mine, mine + "-2", link, t + "/minigw-test-" + id, items + "/minigw-test-" + id] { try? fm.removeItem(atPath: p) }
        try? fm.removeItem(at: dir)
    }
    let renamed = sh("mv '\(items)' '\(moved)'")
    if fm.fileExists(atPath: moved) { rename(moved, items) }
    #expect(renamed != 0 && fm.fileExists(atPath: items))
    #expect(sh("ln -s /private/tmp '\(link)'") != 0 && (try? fm.destinationOfSymbolicLink(atPath: link)) == nil)
    #expect(sh("touch '\(t)/minigw-test-\(id)'") != 0)
    #expect(sh("touch '\(items)/minigw-test-\(id)'") != 0)
    #expect(sh("echo x >> '\(t)/xcrun_db'") != 0)
    #expect(sh("mkdir '\(mine)' && echo x > '\(mine)/f'") == 0)  // nova mapa in pisanje v njej: da
    #expect(sh("mv '\(mine)' '\(mine)-2'") != 0 && sh("rm -rf '\(mine)'") != 0 && fm.fileExists(atPath: mine))  // vnos: ne
}

/// L3 (0.5.16): objects/info, globlja od omejitve brisanja: finalize ne izbriše do konca in ne vrne veje.
@Test func worktreeFinalizeGivesUpOnTooDeepInfo() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    _ = try await agentRun(hub, session) { dir in
        try file(dir, "ok.txt", "ok\n")
        let deep = (0..<24).map { "d\($0)" }.joined(separator: "/")
        try file(dir.appendingPathComponent(".git/objects/info"), deep + "/x", "x")
    }
    #expect(g(client, "for-each-ref", "refs/heads/minigw").isEmpty)  // brez rezultata
}

/// git s stdin (mktree, hash-object) v mapi `dir`.
private func gIn(_ dir: URL, _ input: String, _ args: String...) throws -> String {
    let p = Process(), out = Pipe(), inp = Pipe()
    p.executableURL = URL(fileURLWithPath: "/usr/bin/git")
    p.arguments = ["-C", dir.path, "-c", "user.name=t", "-c", "user.email=t@x"] + args
    p.environment = ProcessInfo.processInfo.environment.merging(["xcrun_nocache": "1"]) { $1 }
    p.standardOutput = out; p.standardInput = inp; p.standardError = FileHandle.nullDevice
    try p.run()
    inp.fileHandleForWriting.write(Data(input.utf8)); try inp.fileHandleForWriting.close()
    let d = out.fileHandleForReading.readDataToEndOfFile()
    p.waitUntilExit()
    return String(decoding: d, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
}

/// Agentov commit, ki ga fsck zavrne (drevo z ».git«): brez rezultata, ref zrcala ne kaže na manjkajoče objekte, zato
/// naslednji klon in izvajanje na istem zrcalu delujeta (kot TestWorktreeFinalizeRejectedKeepsMirror v Go).
@Test func worktreeFinalizeRejectedKeepsMirrorUsable() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    _ = try await agentRun(hub, session) { dir in
        let head = g(dir, "rev-parse", "HEAD")
        let blob = try gIn(dir, "#!/bin/sh\necho pwned\n", "hash-object", "-w", "--stdin")
        let hooks = try gIn(dir, "100644 blob \(blob)\tpost-checkout\n", "mktree")
        let top = try gIn(dir, "040000 tree \(hooks)\t.git\n", "mktree")
        let c = try gIn(dir, "", "commit-tree", top, "-p", head, "-m", "dot git")
        g(dir, "update-ref", "HEAD", c)
    }
    let before = Set(g(client, "for-each-ref", "--format=%(refname)", "refs/heads/minigw").split(separator: "\n").map(String.init))
    #expect(!before.contains { !g(client, "ls-tree", "-r", "--name-only", $0).isEmpty && g(client, "ls-tree", "--name-only", $0).contains(".git") })
    let mirrorRefs = g(tree.repo, "--git-dir", tree.repo.path, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads")
    for line in mirrorRefs.split(separator: "\n") {  // vsak ref zrcala kaže na obstoječ commit
        let sha = String(line.split(separator: " ").last ?? "")
        #expect(Worktree.run(["--git-dir", tree.repo.path, "cat-file", "-e", sha + "^{commit}"], cwd: root.path, safe: false).status == 0, "\(line)")
    }
    // Novo izvajanje na istem zrcalu: klon in rezultat delujeta.
    _ = try await agentRun(hub, session) { dir in try file(dir, "after.txt", "after\n") }
    let branches = g(client, "for-each-ref", "--format=%(refname:short)", "refs/heads/minigw").split(separator: "\n").map(String.init)
    #expect(branches.contains { g(client, "show", "\($0):after.txt") == "after" }, "\(branches)")
}

/// Native z git (privzeto): agenti v klonih na svojih vejah iz posnetka mape na tem Macu (brez minigw, brez bundla); veje
/// pridejo naravnost v repozitorij uporabnika, njegove datoteke, indeks, HEAD in nastavitve ostanejo nespremenjeni.
@Test func nativeGitParallelBranchesInTheRealRepository() async throws {
    let (root, client, mirror) = try fixture()
    defer { cleanup(root) }
    try file(client, "sub/s.txt", "s\n"); g(client, "add", "sub/s.txt"); g(client, "commit", "-qm", "sub")
    try file(client, "a.txt", "one\nuncommitted\n")
    let status = g(client, "status", "--porcelain"), head = g(client, "rev-parse", "HEAD")
    let index = try Data(contentsOf: client.appendingPathComponent(".git/index"))
    // H1: nastavitve repozitorija, ki bi ob dostavi zagnale ukaz (fsmonitor, alternateRefsCommand z alternates)
    let marks = root.appendingPathComponent("marks")
    try FileManager.default.createDirectory(at: marks, withIntermediateDirectories: true)
    g(client, "config", "core.fsmonitor", "touch \(marks.path)/fsmonitor; false")
    g(client, "config", "core.alternateRefsCommand", "touch \(marks.path)/alternate; true")
    let other = root.appendingPathComponent("other.git")
    g(root, "init", "-q", "--bare", other.path)
    try file(client, ".git/objects/info/alternates", other.appendingPathComponent("objects").path + "\n")
    let config = try Data(contentsOf: client.appendingPathComponent(".git/config"))
    let dir = client.appendingPathComponent("sub")
    let (top, sub) = try #require(Router.nativeBranches(dir, inPlace: false, roots: root.path, scratch: mirror.repo))
    #expect(top.path == client.path && sub == "sub")
    let tree = ShareHub.Tree(repo: mirror.repo, worktrees: mirror.worktrees, sub: sub, local: top)
    let hub = ShareHub(), session = UUID().uuidString
    _ = try #require(await hub.register(session: session, cwd: "/Volumes/share/client/sub", key: nil, device: "dev", tree: tree))
    func mount(_ id: String) async -> URL? {
        await ShareHub.$run.withValue(id) { await ShareHub.$agent.withValue("claude") {
            await hub.mount(session: session, cwd: nil, base: "", device: "dev", roots: root.path, writes: true) } }
    }
    let (a, b) = (UUID().uuidString, UUID().uuidString)
    let (dirA, dirB) = try await (#require(mount(a)), #require(mount(b)))
    #expect(dirA != dirB && dirA.lastPathComponent == "sub")
    #expect(try String(contentsOf: dirA.deletingLastPathComponent().appendingPathComponent("a.txt"), encoding: .utf8) == "one\nuncommitted\n")
    #expect(FileManager.default.fileExists(atPath: dirB.deletingLastPathComponent().appendingPathComponent("new.txt").path))
    try file(dirA, "agentA.txt", "A\n")
    try file(dirB, "agentB.txt", "B\n")
    let short0 = String(Sync.folderID("dev|" + session).prefix(8))
    let (brA, brB) = (await hub.runBranch(a), await hub.runBranch(b))
    #expect(brA == "minigw/claude-\(short0)-0" && brB == "minigw/claude-\(short0)-1")  // veja izvajanja
    for id in [a, b] { await ShareHub.$run.withValue(id) { await hub.syncBack(session: session) }; await hub.finish(run: id) }
    #expect(try FileManager.default.contentsOfDirectory(atPath: marks.path).isEmpty)  // H1: ob posnetku in dostavi ni teklo nič
    let short = String(Sync.folderID("dev|" + session).prefix(8))  // L3: naprava in seja
    #expect(await hub.sessionMode(session, device: "dev") == ("native", false, "minigw/claude-\(short)-0"))  // zgodovina seje
    #expect(g(client, "show", "minigw/claude-\(short)-0:sub/agentA.txt") == "A")
    #expect(g(client, "show", "minigw/claude-\(short)-1:sub/agentB.txt") == "B")
    #expect(g(client, "show", "minigw/claude-\(short)-1:a.txt") == "one\nuncommitted")
    #expect(g(client, "-c", "core.fsmonitor=false", "status", "--porcelain") == status && g(client, "rev-parse", "HEAD") == head)
    #expect(try Data(contentsOf: client.appendingPathComponent(".git/index")) == index)
    #expect(try Data(contentsOf: client.appendingPathComponent(".git/config")) == config)
    #expect(!FileManager.default.fileExists(atPath: dir.appendingPathComponent("agentA.txt").path))

    // Siri (brez izvajanja): prava mapa le za branje
    #expect(await hub.mount(session: session, cwd: nil, base: "", device: "dev", roots: root.path) == dir)

    // M1: zamenjan .git/objects (povezava) → brez novega izvajanja in brez branja objektov uporabnika
    let objects = client.appendingPathComponent(".git/objects")
    try FileManager.default.moveItem(at: objects, to: client.appendingPathComponent(".git/objects.real"))
    try FileManager.default.createSymbolicLink(at: objects, withDestinationURL: client.appendingPathComponent(".git/objects.real"))
    #expect(!tree.localObjectsIntact)
    #expect(await mount(UUID().uuidString) == nil)
}

/// M2: posnetek native upošteva globalne izključitve uporabnika (core.excludesFile).
@Test func nativeSnapshotHonoursGlobalExcludes() throws {
    let (root, client, mirror) = try fixture()
    defer { cleanup(root) }
    let ignore = root.appendingPathComponent("global-ignore"), cfg = root.appendingPathComponent("gitconfig")
    try Data(".env\n".utf8).write(to: ignore)
    try Data("[core]\n\texcludesFile = \(ignore.path)\n".utf8).write(to: cfg)
    #expect(Worktree.globalExcludes(env: ["GIT_CONFIG_GLOBAL": cfg.path])?.path == ignore.path)
    try file(client, ".env", "SECRET=1\n")
    let snap = try #require(Worktree.localSnapshot(repo: mirror.repo, top: client, base: "refs/minigw/base/t", excludes: ignore))
    #expect(Worktree.run(["--git-dir", mirror.repo.path, "cat-file", "-e", snap + ":.env"]).status != 0)
    #expect(Worktree.run(["--git-dir", mirror.repo.path, "cat-file", "-e", snap + ":new.txt"]).status == 0)
}

/// Odločitev native: git → veje; native-direct, brez git, .git kot datoteka ali zunaj dovoljenih map → v mapi sami.
@Test func nativeBranchesDecisionAndOptOut() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("nb-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let repo = root.appendingPathComponent("repo"), plain = root.appendingPathComponent("plain"), linked = root.appendingPathComponent("linked")
    for d in [repo.appendingPathComponent(".git"), repo.appendingPathComponent("a/b"), plain, linked] {
        try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true)
    }
    try Data("gitdir: /elsewhere\n".utf8).write(to: linked.appendingPathComponent(".git"))
    let scratch = root.appendingPathComponent("scratch")
    #expect(Router.nativeBranches(repo.appendingPathComponent("a/b"), inPlace: false, roots: root.path, scratch: scratch)?.sub == "a/b")
    #expect(Router.nativeBranches(repo, inPlace: true, roots: root.path, scratch: scratch) == nil)
    #expect(Router.nativeBranches(plain, inPlace: false, roots: root.path, scratch: scratch) == nil)
    #expect(Router.nativeBranches(linked, inPlace: false, roots: root.path, scratch: scratch) == nil)
    #expect(Router.nativeBranches(repo, inPlace: false, roots: plain.path, scratch: scratch) == nil)
    #expect(SharedFolders.candidates(work: "native-direct", readOnly: false, smb: true, git: true) == ["native-direct", "direct"])
    #expect(SharedFolders.mode(flag: "native-direct", saved: nil, readOnly: true).work == "direct")
}

/// Mapa brez veje (native brez git, native-direct, direct): en pisoči agent naenkrat; bralci ne čakajo; po koncu prvega
/// lahko naslednji.
@Test func inPlaceWriterLock() async throws {
    let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("lock-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let hub = ShareHub(), session = UUID().uuidString
    _ = try #require(await hub.register(session: session, cwd: "/Volumes/share/x", key: nil, device: "dev", native: dir))
    let old = ShareHub.writerWait
    ShareHub.writerWait = .seconds(1)
    defer { ShareHub.writerWait = old }
    func mount(_ id: String, writes: Bool) async -> URL? {
        await ShareHub.$run.withValue(id) { await hub.mount(session: session, cwd: nil, base: "", device: "dev", roots: dir.deletingLastPathComponent().path, writes: writes) }
    }
    #expect(await mount("w1", writes: true) == dir)
    #expect(await mount("w2", writes: true) == nil)
    #expect(await hub.sessionMode(session, device: "dev") == ("native-direct", false, ""))  // v mapi sami: brez veje
    #expect(await ShareHub.$run.withValue("w2") { await hub.takeRefusal() }?.contains("Another agent is still changing /Volumes/share/x") == true)
    #expect(await mount("r1", writes: false) == dir)  // bralec ne čaka
    #expect(await mount("w1", writes: true) == dir)   // isto izvajanje znova
    await hub.finish(run: "w1")
    #expect(await mount("w2", writes: true) == dir)
    await hub.finish(run: "w2")
    #expect(await hub.takeRefusal() == nil)

    // Mapa in podmapa se prekrivata (tudi minigw run z lastnim ključem izvajanja); druga mapa ne; DAV druge naprave ne.
    let a = ShareHub.writerKey(scope: "", path: "/x/a"), ab = ShareHub.writerKey(scope: "", path: "/x/a/b")
    #expect(ShareHub.overlaps(a, ab) && ShareHub.overlaps(ab, a) && !ShareHub.overlaps(a, ShareHub.writerKey(scope: "", path: "/x/ab")))
    #expect(!ShareHub.overlaps(ShareHub.writerKey(scope: "dav:d1", path: "/x"), ShareHub.writerKey(scope: "dav:d2", path: "/x")))
    #expect(await hub.waitWriter(a, folder: "/x/a", writes: true, run: "run-1"))
    #expect(await hub.waitWriter(ab, folder: "/x/a/b", writes: true, run: "run-2") == false)
    #expect(await ShareHub.$run.withValue("run-2") { await hub.takeRefusal() }?.contains("/x/a/b") == true)
    await hub.releaseWriter(run: "run-1")
    #expect(await hub.waitWriter(ab, folder: "/x/a/b", writes: true, run: "run-2"))
    await hub.releaseWriter(run: "run-2")
}

/// Posnetek native ne zažene ničesar iz repozitorija uporabnika (filter, fsmonitor, hooki): git dela z nastavitvami zrcala.
@Test func nativeSnapshotRunsNothingFromTheUsersRepository() throws {
    let (root, client, mirror) = try fixture()
    defer { cleanup(root) }
    let marks = root.appendingPathComponent("marks")
    try FileManager.default.createDirectory(at: marks, withIntermediateDirectories: true)
    g(client, "config", "filter.evil.clean", "touch \(marks.path)/clean; cat")
    g(client, "config", "core.fsmonitor", "touch \(marks.path)/fsmonitor")
    try file(client, ".gitattributes", "* filter=evil\n")
    try file(client, ".git/hooks/post-commit", "#!/bin/sh\ntouch \(marks.path)/hook\n")
    try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: client.appendingPathComponent(".git/hooks/post-commit").path)
    try file(client, ".git/info/exclude", "secret.txt\n")
    try file(client, "secret.txt", "local only\n")
    let snap = try #require(Worktree.localSnapshot(repo: mirror.repo, top: client, base: "refs/minigw/base/t"))
    #expect(try FileManager.default.contentsOfDirectory(atPath: marks.path).isEmpty)
    let show = Worktree.run(["--git-dir", mirror.repo.path, "show", snap + ":a.txt"]).out
    #expect(show == "one\nuncommitted")
    #expect(Worktree.run(["--git-dir", mirror.repo.path, "cat-file", "-e", snap + ":secret.txt"]).status != 0)  // .git/info/exclude
}

/// I8: skrit Scratch za povezavo je skrit pod dobesedno in kanonično potjo (profil Native in Copilot).
@Test func nativeProfileHidesScratchBehindASymlink() throws {
    let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("i8-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: base) }
    let real = base.appendingPathComponent("real-scratch"), link = base.appendingPathComponent("scratch-link")
    try FileManager.default.createDirectory(at: real, withIntermediateDirectories: true)
    try FileManager.default.createSymbolicLink(at: link, withDestinationURL: real)
    let p = try #require(Native.sandboxProfile(dir: base.appendingPathComponent("proj").path, home: base.path, network: true, hidden: [link.path]))
    let hide = try #require(p.split(separator: "\n").first { $0.hasPrefix("(deny file-read-data file-write*") })
    #expect(hide.contains("\"\(link.path)\"") && hide.contains("\"\(real.path)\""))
    // L4: ugnezdeni .git: nastavitve in hooki, pa tudi .git sam (preimenovanje, zamenjava z datoteko gitdir)
    #expect(p.contains(#"(regex #"/\.git/(config|config\.worktree|hooks|info)(/|$)")"#) && p.contains(#"(regex #"/\.git$")"#))
}

/// Agent-calls na native-direct: vprašani agent dela v mapi klicatelja v njegovem izvajanju in ne čaka na zaklep, ki ga
/// drži klicatelj (sicer bi čakal nase).
@Test func nestedAgentOnNativeDirectDoesNotWait() async throws {
    let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("nested-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let hub = ShareHub(), session = UUID().uuidString
    _ = try #require(await hub.register(session: session, cwd: "/Volumes/share/x", key: nil, device: "dev", native: dir))
    let parent = "parent-run"
    let first = await ShareHub.$run.withValue(parent) {
        await hub.mount(session: session, cwd: nil, base: "", device: "dev", roots: dir.deletingLastPathComponent().path, writes: true)
    }
    #expect(first == dir)  // klicatelj drži zaklep
    var c = RouterConfig()
    c.claudeCLI.askOthers = true; c.claudeCLI.allowLocalTools = true
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let caller = request("POST", "/v1/messages", headers: ["x-claude-mini-session": session, "x-claude-mini-cwd": "/Volumes/share/x"])
    let run = try #require(await ShareHub.$run.withValue(parent) { await router.startAgentRun(caller, kind: "claude", dir: dir) })
    defer { run.stop() }
    let old = ShareHub.writerWait
    ShareHub.writerWait = .seconds(1)
    defer { ShareHub.writerWait = old }
    let start = ContinuousClock.now
    let nested = await AgentRun.$current.withValue(run) {
        await ShareHub.$run.withValue(parent) {  // syncRun: vprašani agent v izvajanju klicatelja
            await hub.mount(session: session, cwd: nil, base: "", device: "dev", roots: dir.deletingLastPathComponent().path, writes: true)
        }
    }
    #expect(nested == dir && ContinuousClock.now - start < .milliseconds(500))
    await hub.finish(run: parent)
}

/// L2: git, ki visi (tudi njegov pomožni proces), konča po časovni omejitvi.
@Test func gatewayGitTimeoutKillsTheProcessGroup() throws {
    let start = ContinuousClock.now
    let r = Worktree.run(["-c", "alias.hang=!sleep 30", "hang"], timeout: 1)
    #expect(r.status != 0 && ContinuousClock.now - start < .seconds(10))
}

/// #16: posnetek vzame nove datoteke, ne pa prezrtih (.gitignore, .git/info/exclude); #10: brez commita ga ni.
@Test func worktreeSnapshotSkipsIgnoredFilesAndNeedsACommit() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("wt-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    g(root, "init", "-q", "-b", "main")
    #expect(!Worktree.hasCommit(top: root.path) && Worktree.snapshot(top: root.path) == nil)
    try file(root, ".gitignore", "build/\n")
    g(root, "add", ".gitignore"); g(root, "commit", "-qm", "first")
    try file(root, ".git/info/exclude", ".omc/\n")
    for f in ["new.txt", "build/out.o", ".omc/state.json"] { try file(root, f, "x\n") }
    let snap = try #require(Worktree.snapshot(top: root.path))
    #expect(Worktree.hasCommit(top: root.path))
    #expect(g(root, "ls-tree", "-r", "--name-only", snap) == ".gitignore\nnew.txt")
}

/// Auto (privzeti ali izrecni) le v korenu repozitorija (kot Go repoRoot/workTop); vrstica o veji le z delom agenta
/// od posnetka (neshranjene in nesledene datoteke uporabnika niso v obsegu), kot Go TestBranchLineShowsAgentOnlyRange.
@Test func autoRepoRootAndAgentOnlyBranchRange() throws {
    let (root, client, _) = try fixture()
    defer { cleanup(root) }
    let sub = client.appendingPathComponent("sub")
    try FileManager.default.createDirectory(at: sub, withIntermediateDirectories: true)
    #expect(SharedFolders.repoRoot(client.path, client.path) && !SharedFolders.repoRoot(client.path, sub.path) && !SharedFolders.repoRoot(nil, client.path))

    let snap = try #require(Worktree.snapshot(top: client.path))  // a.txt neshranjen, new.txt nesleden
    #expect(snap != g(client, "rev-parse", "HEAD"))
    let branch = "minigw/codex-0123abcd-0"
    let blob = Worktree.run(["hash-object", "-w", "--stdin"], cwd: client.path, safe: false, input: Data("agent\n".utf8)).out
    let tree = Worktree.run(["mktree"], cwd: client.path, safe: false,
                            input: Data((g(client, "ls-tree", snap) + "\n100644 blob \(blob)\tagent.txt\n").utf8)).out
    let tip = g(client, "commit-tree", tree, "-p", snap, "-m", "agent")
    g(client, "update-ref", "refs/heads/" + branch, tip)
    try file(client, "a.txt", "later\n")
    let later = try #require(Worktree.snapshot(top: client.path))

    let base = Worktree.agentBase(top: client.path, branch: branch, sent: [snap, later])
    #expect(base == snap)
    #expect(g(client, "diff", "--name-only", "\(base)..\(branch)") == "agent.txt")
    let line = Worktree.branchLine(branch: branch, base: base)
    #expect(line.contains("git diff \(snap)..\(branch)") && line.contains("git cherry-pick \(snap)..\(branch)") && !line.contains("HEAD"))
    #expect(Worktree.agentBase(top: client.path, branch: branch, sent: [later]) == "HEAD")
    // R1: veja iz sporočila prehoda (native) le, če obstaja in je začetek njen prednik
    #expect(Worktree.delivered(root: client.path, branch: branch, base: snap))
    #expect(!Worktree.delivered(root: client.path, branch: branch, base: later) && !Worktree.delivered(root: client.path, branch: "minigw/codex-0123abcd-9", base: snap)
            && !Worktree.delivered(root: client.path, branch: "main", base: snap))
}

/// R4: agent, ki v klonu le bere (izvajanje za branje), vpraša Claude CLI: ta v načinu worktree zavrne (400), ne piše v klon.
@Test func readOnlyRunAskingClaudeInWorktreeIsRefused() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    var c = RouterConfig()
    c.claudeCLI.enabled = true; c.claudeCLI.askOthers = true; c.claudeCLI.allowLocalTools = true; c.codex.askOthers = true
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let hub = Router.shares, session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    let caller = request("POST", "/v1/messages", headers: ["x-claude-mini-session": session, "x-claude-mini-cwd": client.path])
    let id = UUID().uuidString
    let run = try #require(await ShareHub.$run.withValue(id) {
        await ShareHub.$agent.withValue("codex") { () -> AgentRun? in
            guard await hub.mount(session: session, cwd: nil, base: "http://127.0.0.1:1", device: "dev") != nil else { return nil }
            return await router.startAgentRun(caller, kind: "codex", dir: nil, readOnly: true)
        }
    })
    defer { run.stop(); Task { await hub.finish(run: id) } }
    run.started(getpid())
    var r = URLRequest(url: URL(string: "http://127.0.0.1:\(run.port)/v1/messages")!)
    r.httpMethod = "POST"
    r.setValue("Bearer " + run.token, forHTTPHeaderField: "Authorization")
    r.setValue("application/json", forHTTPHeaderField: "content-type")
    r.httpBody = Data(#"{"model":"claude-cli-haiku","max_tokens":5,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}"#.utf8)
    let (data, resp) = try await URLSession(configuration: .ephemeral).data(for: r)
    #expect((resp as? HTTPURLResponse)?.statusCode == 400 && String(decoding: data, as: UTF8.self).contains("Worktree mode cannot be read-only"),
            "\(String(decoding: data, as: UTF8.self))")
}
