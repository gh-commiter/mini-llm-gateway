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
    #expect(((try? FileManager.default.contentsOfDirectory(atPath: marks.path)) ?? []).isEmpty)
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
            && env["MAVEN_OPTS"] == "-Dmaven.repo.local=" + maven.path)
    #expect((sb["network"] as? [String: [String]])?["allowedDomains"] == [])
    #expect(a.contains("--add-dir") && a.contains("default") && a.contains("acceptEdits"))
    #expect(Array((fs["denyWrite"] ?? []).prefix(4)) == ["config", "hooks", "info", "commondir"].map { root.path + "/.git/" + $0 })
    #expect(a[a.firstIndex(of: "--disallowedTools")! + 1].contains("Edit(/" + root.path + "/**/.git/**)"))
    #expect(Worktree.scratchIsSafe(URL(fileURLWithPath: NSHomeDirectory() + "/Library/Caches/dev.trojar.minigateway/Scratch")))
    #expect(!Worktree.scratchIsSafe(URL(fileURLWithPath: "/private/tmp/scratch"))
            && !Worktree.scratchIsSafe(URL(fileURLWithPath: NSHomeDirectory() + "/Library/Developer/Xcode/DerivedData/s")))
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
    #expect(Worktree.fetch(top: client.path, bundle: notBundle, branch: "minigw/claude-0123abcd-0") == "the router did not send a git bundle")
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

/// H2: Grok v načinu worktree ni podprt (400, preden se karkoli priklopi); M3: Codex brez pisanja tudi ne.
@Test func worktreeRefusesGrokAndReadOnlyCodex() async throws {
    var c = RouterConfig()
    c.grok.enabled = true; c.codex.enabled = true; c.codex.write = false
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
        #expect(w.status == 400 && w.text.contains(model == "claude-grok" ? "Grok is not supported in worktree mode" : "Codex may not write"), "\(model): \(w.text)")
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
