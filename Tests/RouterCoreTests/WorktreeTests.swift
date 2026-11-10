import Foundation
import os
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
/// conv (#66): ključ pogovora; nadaljevanje na prejšnji veji kot ShareClient.
private func fakeClient(_ hub: ShareHub, key: String, top: URL, notes: OSAllocatedUnfairLock<[String]>? = nil,
                        conv: Worktree.Conv = Worktree.Conv()) -> Task<Void, Never> {
    Task {
        var chain: [String: Worktree.ConvRecord]?, sent: [String] = [], contTip: String?, contRec: Worktree.ConvRecord?
        while !Task.isCancelled {
            guard case .some(.some(let job)) = await hub.poll(key: key, wait: 0.2) else { continue }
            var r = DAVResponse(403)
            if job.method == "WORKTREE-PUSH" {
                let req = (try? JSONSerialization.jsonObject(with: job.body) as? [String: Any]) ?? [:]
                let have = req["have"] as? [String] ?? [], nestedHave = req["nested"] as? [String: [String]]
                // git odjemalca zunaj bazena Swift concurrency (kot minigw v svojem procesu): vzporedni testi ne čakajo nanj
                var snaps = await offPool { Worktree.snapshots(top: top.path, gateway: nestedHave != nil) }
                if snaps.failure == nil, conv != Worktree.Conv() {
                    if chain == nil {  // kot ShareClient: odločitev enkrat na zagon
                        chain = [:]
                        let c = Worktree.continueFrom(top: top.path, conv: conv.from, gateway: "https://gw-a:8443", snap: snaps.snap, nested: !snaps.nested.isEmpty)
                        if let m = c.message { notes?.withLock { $0.append(m) } }
                        if let tip = c.tip, let r = c.record { contTip = tip; contRec = Worktree.chain(top: top.path, base: r.base, snap: snaps.snap) }
                    }
                    if let tip = contTip, let r = contRec { snaps.snap = tip; chain?[tip] = r } else { chain?[snaps.snap] = Worktree.chain(top: top.path, base: snaps.snap, snap: snaps.snap) }
                    sent.append(snaps.snap)
                }
                if snaps.failure == nil {
                    for (i, (dir, snap, h)) in ([(top.path, snaps.snap, have)] + snaps.nested.map { (top.path + "/" + $0.path, $0.snapshot, nestedHave?[Worktree.nestedKey($0.path)] ?? []) }).enumerated() {
                        let f = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".bundle")
                        if await offPool({ Worktree.bundle(top: dir, snapshot: snap, have: h, to: f) == true }) {
                            _ = await hub.worktreePut(key: key, part: 0, nested: i, data: try! Data(contentsOf: f), device: "dev")
                        }
                        try? FileManager.default.removeItem(at: f)
                    }
                    r = DAVResponse(200, [:], try! JSONSerialization.data(withJSONObject: ["snapshot": snaps.snap,
                                                                                           "nested": snaps.nested.map { ["path": $0.path, "snapshot": $0.snapshot] }] as [String: Any]))
                } else { r = DAVResponse(500) }
            } else if job.method == "WORKTREE-PULL" {
                let req = (try? JSONSerialization.jsonObject(with: job.body) as? [String: Any]) ?? [:]
                let branch = req["branch"] as? String ?? ""
                var ok = true
                let repos = (req["nestedOnly"] as? Bool == true ? [] : [top.path]) + ((req["nested"] as? [[String: Any]]) ?? []).map { top.path + "/" + ($0["path"] as? String ?? "") }
                for (i, dir) in repos.enumerated() {
                    let f = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".bundle")
                    let idx = req["nestedOnly"] as? Bool == true ? i + 1 : i
                    if let d = await hub.worktreeResult(key: key, device: "dev", nested: idx) { try? d.write(to: f) }
                    ok = await offPool { Worktree.fetch(top: dir, bundle: f, branch: branch) } == nil && ok
                    if conv != Worktree.Conv(), dir == top.path {
                        let r = Worktree.remember(top: dir, conv: conv, gateway: "https://gw-a:8443", branch: branch, sent: Worktree.agentBase(top: dir, branch: branch, sent: sent), chain: chain ?? [:])
                        if r.continued { notes?.withLock { $0.append(Worktree.continuedLine(branch: branch, base: r.base)) } }
                    }
                    try? FileManager.default.removeItem(at: f)
                }
                r = DAVResponse(ok ? 200 : 500)
            } else if job.method == "WORKTREE-NOTE", let notes {
                let m = (try? JSONSerialization.jsonObject(with: job.body) as? [String: Any])?["message"] as? String ?? ""
                notes.withLock { $0.append(m) }
                r = DAVResponse(200)
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
    let env = Worktree.harnessEnv(root: root, maven: maven, agent: "claude")
    #expect(env["GIT_DIR"] == root.path + "/.git" && env["GIT_WORK_TREE"] == root.path && env["GIT_COMMON_DIR"] == root.path + "/.git"
            && env["MAVEN_OPTS"] == "-Dmaven.repo.local=" + maven.path + " -Djava.io.tmpdir=" + root.path + "/.build/tmp")
    #expect((sb["network"] as? [String: [String]])?["allowedDomains"] == [])
    #expect(a.contains("--add-dir") && a.contains("default") && a.contains("acceptEdits"))
    #expect(Array((fs["denyWrite"] ?? []).prefix(4)) == ["config", "hooks", "info", "commondir"].map { root.path + "/.git/" + $0 })
    #expect((fs["denyWrite"] ?? []).contains("/private/tmp/mgw-\(getuid())"))  // #67 M2
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
    agentGit(Worktree.harnessEnv(root: repo, maven: root.appendingPathComponent("m2"), agent: "claude"))
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

/// A3 (kot Go TestCloneExcludesBuild): .build klona (TMPDIR agenta) ni v git status agenta (info/exclude klona).
@Test func worktreeCloneExcludesBuild() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    _ = try await agentRun(hub, session) { dir in
        try file(dir, ".build/tmp/x", "x\n")
        #expect(g(dir, "status", "--porcelain").isEmpty)
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
    let env = Worktree.harnessEnv(root: root, maven: root, agent: "claude")
    #expect(env["TMPDIR"] == root.path + "/.build/tmp/" && env["CLANG_MODULE_CACHE_PATH"] == root.path + "/.build/ModuleCache"
            && env["xcrun_nocache"] == "1")
    let codex = Codex.args(cwd: root, extraDirs: [], agent: true, output: root, write: true)
    #expect(codex.contains("sandbox_workspace_write.exclude_tmpdir_env_var=true"))
    // #67: $MINIGW_DEPS in skupni predpomnilniki pisljivi (Claude, Codex); ukazi Codex z oznako peskovnika, harness ne
    #expect(claudeWrite.contains(root.path + ".deps") && claudeWrite.contains("/Volumes/X/Scratch/worktrees/dev/x/.cache"))
    #expect(Codex.treeDirs(root: root, cwd: root, maven: root).contains(root.path + ".deps"))
    let prof = Codex.treeProfile(writable: [root.path], gits: [root.path + "/.git"], network: false, path: "/usr/bin")
    #expect(prof.contains(#"shell_environment_policy.set.MINIGW_SANDBOXED="1""#))
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

/// #24: okolje minigw run usmeri vse začasne mape v zasebno mapo zagona (v Scratch); skrivnosti agentov in SSH agent ne.
@Test func runEnvKeepsTempUnderRunFolder() {
    let run = URL(fileURLWithPath: "/Volumes/Fast SSD/Scratch/tmp/run-1")
    let env = Native.runEnv(["TMPDIR": "/var/folders/x/T/", "TMP": "/tmp", "JAVA_TOOL_OPTIONS": "-Xmx1g", "ANTHROPIC_API_KEY": "k",
                             "SSH_AUTH_SOCK": "/s", "xcrun_db": "/d", "PATH": "/usr/bin"], runTmp: run)
    #expect(env["TMPDIR"] == run.path + "/" && env["TMP"] == run.path && env["TEMP"] == run.path)
    #expect(env["CLANG_MODULE_CACHE_PATH"] == run.path + "/ModuleCache")
    #expect(env["JAVA_TOOL_OPTIONS"] == "-Xmx1g -Djava.io.tmpdir=\"" + run.path + "\"")  // presledek: v narekovajih
    let plain = URL(fileURLWithPath: "/Volumes/SSD/Scratch/tmp/run-1")
    #expect(Native.runEnv([:], runTmp: plain)["JAVA_TOOL_OPTIONS"] == "-Djava.io.tmpdir=" + plain.path)  // brez: JDK 8
    #expect(env["ANTHROPIC_API_KEY"] == nil && env["SSH_AUTH_SOCK"] == nil && env["xcrun_db"] == nil && env["xcrun_nocache"] == "1")
    #expect(env["PATH"]?.hasPrefix("/usr/bin:") == true)
}

/// #24 F1: zasebne mape zagona ni mogoče ustvariti (tmp je datoteka): nil, zagon se takoj konča z jasno napako.
@Test func makeRunTmpFailsClearly() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("mk24-" + UUID().uuidString)
    defer { try? fm.removeItem(at: base) }
    try fm.createDirectory(at: base, withIntermediateDirectories: true)
    try Data("x".utf8).write(to: base.appendingPathComponent("file"))
    #expect(Native.makeRunTmp(base.appendingPathComponent("file/run-req_x")) == nil)
    let ok = base.appendingPathComponent("tmp/run-req_y")
    #expect(Native.makeRunTmp(ok) == ok.path && fm.fileExists(atPath: ok.path + "/ModuleCache"))
}

/// #24 L1: Scratch z narekovajem v poti (JAVA_TOOL_OPTIONS, profil peskovnika) je zavrnjen, tudi kot obstoječa mapa.
@Test func scratchRefusesQuoteInPath() throws {
    #expect(RouterConfig.scratchProblem(URL(fileURLWithPath: "/Volumes/X/a\"b"), allowedRoots: "") == "its path contains a quotation mark")
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("q24-" + UUID().uuidString)
    defer { try? fm.removeItem(at: base) }
    let real = base.appendingPathComponent("a\"b"), link = base.appendingPathComponent("link")
    try fm.createDirectory(at: real, withIntermediateDirectories: true)
    try fm.createSymbolicLink(at: link, withDestinationURL: real)
    #expect(RouterConfig.scratchProblem(link, allowedRoots: "") == "its path contains a quotation mark")  // razrešena pot
}

/// #24 L2: ob zagonu ostanejo le mape run-req_<32 hex>; druga imena, datoteke in povezave (ter njihov cilj) ostanejo.
@Test func cleanupRunTempsRemovesOnlyRunFolders() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("rt24-" + UUID().uuidString)
    defer { try? fm.removeItem(at: base) }
    let tmp = base.appendingPathComponent("tmp"), target = base.appendingPathComponent("target")
    let hex = String(repeating: "0123456789abcdef", count: 2), hex2 = String(repeating: "fedcba9876543210", count: 2)
    try fm.createDirectory(at: tmp.appendingPathComponent("run-req_" + hex + "/ModuleCache"), withIntermediateDirectories: true)
    for n in ["native-markers", "run-req_XYZ", "run-" + UUID().uuidString] { try fm.createDirectory(at: tmp.appendingPathComponent(n), withIntermediateDirectories: true) }
    try fm.createDirectory(at: target, withIntermediateDirectories: true)
    try Data("k".utf8).write(to: target.appendingPathComponent("keep"))
    try fm.createSymbolicLink(at: tmp.appendingPathComponent("run-req_" + hex2), withDestinationURL: target)
    NativeHub.cleanupRunTemps(tmp)
    let left = Set(try fm.contentsOfDirectory(atPath: tmp.path))
    #expect(!left.contains("run-req_" + hex) && left.contains("native-markers") && left.contains("run-req_XYZ") && left.count == 4)
    #expect(fm.fileExists(atPath: target.appendingPathComponent("keep").path))  // cilj povezave nedotaknjen
}

/// #24: peskovnik minigw run ne dovoli /private/tmp in ne datotek v DARWIN_USER_TEMP_DIR; z okoljem runEnv gre začasno
/// pisanje (sh, java.io.tmpdir) v zasebno mapo zagona v skritem Scratch. Ob nepričakovanem uspehu počisti.
@Test func runProfileKeepsTempInScratch() throws {
    let fm = FileManager.default
    var b = [CChar](repeating: 0, count: 1024)
    #expect(confstr(_CS_DARWIN_USER_TEMP_DIR, &b, b.count) > 0)
    let t = try #require(Sync.real(URL(fileURLWithPath: String(cString: b)))).path
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("run24-" + UUID().uuidString)
    let dir = base.appendingPathComponent("proj"), scratch = base.appendingPathComponent("scratch")
    let run = scratch.appendingPathComponent("tmp/run-x")
    for d in [dir, run.appendingPathComponent("ModuleCache")] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    let id = UUID().uuidString, shared = "/private/tmp/minigw-test-" + id, inT = t + "/minigw-test-" + id
    defer { for p in [shared, inT] { try? fm.removeItem(atPath: p) }; try? fm.removeItem(at: base) }
    let profile = try #require(Native.sandboxProfile(dir: dir.path, network: false, hidden: [scratch.path], runTmp: run.path))
    #expect(!profile.contains("\"/private/tmp\""))
    func sh(_ c: String) throws -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
        p.arguments = ["-p", profile, "/bin/sh", "-c", c]
        p.environment = Native.runEnv(ProcessInfo.processInfo.environment, runTmp: run)
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try p.run(); p.waitUntilExit()
        return p.terminationStatus
    }
    #expect(try sh("echo x > '\(shared)'") != 0 && !fm.fileExists(atPath: shared))
    #expect(try sh("echo x > '\(inT)'") != 0 && !fm.fileExists(atPath: inT))
    #expect(try sh("echo x > \"$TMPDIR/a\" && echo x > \"$TMP/b\" && echo x > \"$TEMP/c\"") == 0)
    #expect(["a", "b", "c"].allSatisfy { fm.fileExists(atPath: run.appendingPathComponent($0).path) })
    #expect(try sh("echo x > '\(scratch.path)/tmp/other'") != 0)  // drugje v Scratch ne
}

/// #24: mapa na drugem disku dobi le nove NSIRD_* v <disk>/.TemporaryItems/folders.<uid>/TemporaryItems (in manjkajoče
/// mape do tja), mapa na zagonskem disku nič na drugem disku. Drug disk: prvi priklopljeni (sicer le del za zagonski disk).
@Test func runProfileVolumeSaveCopies() throws {
    var b = [CChar](repeating: 0, count: 1024)
    #expect(confstr(_CS_DARWIN_USER_TEMP_DIR, &b, b.count) > 0)
    let t = try #require(Sync.real(URL(fileURLWithPath: String(cString: b)))).path
    let boot = try #require(Sync.real(FileManager.default.temporaryDirectory)).path
    #expect(Native.volumeTempItems(boot, temp: t) == nil)
    #expect(Native.darwinTempRules(dirs: [boot])?.contains(".TemporaryItems") == false)
    let vols = FileManager.default.mountedVolumeURLs(includingResourceValuesForKeys: nil, options: [.skipHiddenVolumes]) ?? []
    guard let vol = vols.compactMap({ Sync.real($0)?.path }).first(where: { $0.hasPrefix("/Volumes/") && Native.volumeTempItems($0, temp: t) != nil }) else { return }
    let ti = vol + "/.TemporaryItems/folders.\(getuid())/TemporaryItems"
    #expect(Native.volumeTempItems(vol, temp: t) == ti)
    let rules = try #require(Native.darwinTempRules(dirs: [vol, vol]))
    let ve = NSRegularExpression.escapedPattern(for: ti)
    #expect(rules.contains("(allow file-write* (regex #\"^\(ve)/NSIRD_[^/]+/\"))"))
    #expect(rules.contains("(regex #\"^\(ve)/NSIRD_[^/]+$\") (literal \"\(ti)\")"))
    #expect(!rules.contains("(subpath") && rules.components(separatedBy: vol).count == 4)  // nič drugega na disku: le tri mape na poti (regex sta ubežna)
}

/// #72: DerivedData mape v Scratch (stalno ime iz poti), ne ~/Library/Developer; povezavo, ki jo pusti gradnja, zamenja z
/// mapo (cilj ostane); peskovnik dovoli pisanje le v DerivedData te mape, ne v sosednje.
@Test func runDerivedDataUnderScratch() throws {
    let fm = FileManager.default
    #expect(!Native.buildCaches().contains { $0.hasSuffix("/Library/Developer") })
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("dd72-" + UUID().uuidString)
    defer { try? fm.removeItem(at: base) }
    let scratch = base.appendingPathComponent("scratch"), dd = scratch.appendingPathComponent("deriveddata")
    let proj = base.appendingPathComponent("proj"), target = base.appendingPathComponent("target")
    for d in [proj, target] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    let a = try #require(Native.makeDerivedData(dd, dir: proj.path))
    #expect(Native.makeDerivedData(dd, dir: proj.path) == a && Native.makeDerivedData(dd, dir: "/other") != a)
    #expect(a.hasPrefix(dd.path + "/") && URL(fileURLWithPath: a).lastPathComponent.count == 16)
    try fm.removeItem(atPath: a)
    try fm.createSymbolicLink(atPath: a, withDestinationPath: target.path)
    #expect(Native.makeDerivedData(dd, dir: proj.path) == a && (try? fm.destinationOfSymbolicLink(atPath: a)) == nil && fm.fileExists(atPath: target.path))
    let other = try #require(Native.makeDerivedData(dd, dir: "/other"))
    let profile = try #require(Native.sandboxProfile(dir: proj.path, network: false, hidden: [scratch.path], derivedData: a))
    func sh(_ c: String) throws -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
        p.arguments = ["-p", profile, "/bin/sh", "-c", c]
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try p.run(); p.waitUntilExit()
        return p.terminationStatus
    }
    #expect(try sh("echo x > '\(a)/f'") == 0)
    #expect(try sh("echo x > '\(other)/f'") != 0 && !fm.fileExists(atPath: other + "/f"))
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
    let w2 = await ShareHub.$run.withValue("w2") { await hub.takeRefusal() }
    #expect(w2?.status == 409 && w2?.message.contains("Another agent is still changing /Volumes/share/x") == true)
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
    let r2 = await ShareHub.$run.withValue("run-2") { await hub.takeRefusal() }
    #expect(r2?.status == 409 && r2?.message.contains("/x/a/b") == true)
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
    #expect(p.contains(#"(regex #"/\.git/(config|config\.worktree|hooks|info|commondir|worktrees)(/|$)")"#) && p.contains(#"(regex #"/\.git$")"#))
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
    // Ugnezdeni (kot Go TestSnapshotSkipsIgnoredFilesAndNeedsACommit): neprezrti s commitom ali brez, na kateri koli
    // globini, so v seznamu; prezrt ne. Brez commita gre iz posnetka starša in dobi svoj commit brez starša.
    #expect(Worktree.nestedRepos(top: root.path).isEmpty)
    g(root.appendingPathComponent("build"), "init", "-q")
    #expect(Worktree.nestedRepos(top: root.path).isEmpty)
    let lib = root.appendingPathComponent("vendor/lib")
    try FileManager.default.createDirectory(at: lib, withIntermediateDirectories: true)
    try file(lib, "a.txt", "x\n")
    g(lib, "init", "-q")
    try file(root, "vendor/keep.txt", "x\n")
    let n = Worktree.nestedRepos(top: root.path)
    #expect(n == ["vendor/lib"])
    let s2 = try #require(Worktree.snapshot(top: root.path, skip: n))
    #expect(g(root, "ls-tree", "-r", "--name-only", s2) == ".gitignore\nnew.txt\nvendor/keep.txt")
    try file(lib, "b.txt", "y\n"); g(lib, "add", "a.txt")
    let rs = try #require(Worktree.snapshot(top: lib.path, root: true))
    #expect(g(lib, "rev-list", "--parents", "-n1", rs) == rs && g(lib, "ls-tree", "-r", "--name-only", rs) == "a.txt\nb.txt" && !Worktree.hasCommit(top: lib.path))
    g(lib, "commit", "-qm", "lib")
    let deeper = lib.appendingPathComponent("inner/z")
    try file(deeper, "z.txt", "z\n"); g(deeper, "init", "-q")
    #expect(Worktree.nestedRepos(top: root.path) == ["vendor/lib", "vendor/lib/inner/z"] && Worktree.snapshot(top: root.path) != nil)
    #expect(Worktree.nestedChildren("", ["vendor/lib", "vendor/lib/inner/z", "x"]) == ["vendor/lib", "x"])
    #expect(Worktree.nestedChildren("vendor/lib", ["vendor/lib", "vendor/lib/inner/z", "vendor/libx/y"]) == ["inner/z"])
    #expect(Worktree.snapshots(top: root.path, gateway: false).failure?.key == "nested-old")
    g(lib, "config", "filter.lfs.clean", "cat")  // R2: filtri ugnezdenega: ne začne
    #expect(Worktree.snapshots(top: root.path, gateway: true).failure?.key == "nested-filter:vendor/lib")
    g(lib, "config", "--remove-section", "filter.lfs")
    let all = Worktree.snapshots(top: root.path, gateway: true)
    #expect(all.failure == nil && all.nested.map(\.path) == ["vendor/lib", "vendor/lib/inner/z"])
    try FileManager.default.removeItem(at: root.appendingPathComponent("vendor"))
    let deep = root.appendingPathComponent("new/x\ny")  // globoko v nesledeni mapi, ime z novo vrstico: najden, prehod ga ne vzame
    try FileManager.default.createDirectory(at: deep, withIntermediateDirectories: true)
    try file(root, "new/f.txt", "x\n")
    g(deep, "init", "-q")
    #expect(Worktree.nestedRepos(top: root.path) == ["new/x\ny"] && !Worktree.validNestedPath("new/x\ny"))
    #expect(Worktree.snapshots(top: root.path, gateway: true).failure?.key == "nested-name")
    #expect(Worktree.snapshot(top: root.path, skip: ["new/x\ny"]) != nil)
    for p in ["a", "a/b", "a b/c-d.e", ".vendor/x"] { #expect(Worktree.validNestedPath(p), "\(p)") }
    for p in ["", "/a", "a/", "a//b", "../a", "a/../b", "./a", "a/.git", "a/.GIT/b", "a\\b", "c:x", "a\u{1}"] { #expect(!Worktree.validNestedPath(p), "\(p)") }
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
    // Ugnezden (M2, kot Go): stroga oblika le za varno pot, drugače brez ukazov in brez »agent work is on branch«.
    #expect(Worktree.branchLine(branch: branch, base: snap, nested: "sub/lib") == "agent work is on branch \(branch) in nested repository sub/lib: review with git -C sub/lib diff \(snap)..\(branch) (\(snap) = your state in sub/lib when the agent started), then bring it in with git -C sub/lib cherry-pick \(snap)..\(branch) or git -C sub/lib diff --binary \(snap)..\(branch) | git -C sub/lib apply")
    for p in ["a b", "-x", ".hidden", "a/$(id)", "a;b"] {
        let l = Worktree.branchLine(branch: branch, base: snap, nested: p)
        #expect(!l.hasPrefix("agent work is on branch") && !l.contains(p), "\(p)")
    }
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

/// Odjemalec Go na Windows pošlje x-worktree-top z »/« (git), cwd z »\« (0.5.19: Mac prehod je worktree zavrnil).
@Test func worktreeClientPathsFromWindows() {
    #expect(Worktree.clientTree(top: "C:/Users/u/repo", cwd: #"C:\Users\u\repo"#)! == ("C:/Users/u/repo", ""))
    #expect(Worktree.clientTree(top: "C:/Users/u/repo", cwd: #"C:\Users\u\repo\a\b"#)! == ("C:/Users/u/repo", "a/b"))
    #expect(Worktree.clientTree(top: "/Users/u/repo/", cwd: "/Users/u/repo/src")! == ("/Users/u/repo", "src"))
    #expect(Worktree.clientTree(top: "C:/Users/u/repo", cwd: #"C:\Users\u\repo2"#) == nil)
    #expect(Worktree.clientTree(top: "", cwd: "/x") == nil && Worktree.clientTree(top: "/", cwd: "/x") == nil)
}

/// H1: povezava kjerkoli v podmapi (a -> domača mapa in cwd a/.ssh, b -> ../outside) ne prestavi agenta iz klona.
@Test func worktreeDirRefusesSymlinks() throws {
    let fm = FileManager.default
    let root = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("wtdir-" + UUID().uuidString)
    defer { try? fm.removeItem(at: root) }
    let wt = root.appendingPathComponent("wt"), home = root.appendingPathComponent("home")
    for d in [wt.appendingPathComponent("x/y"), home.appendingPathComponent(".ssh"), root.appendingPathComponent("outside/z")] {
        try fm.createDirectory(at: d, withIntermediateDirectories: true)
    }
    try fm.createSymbolicLink(atPath: wt.appendingPathComponent("a").path, withDestinationPath: home.path)
    try fm.createSymbolicLink(atPath: wt.appendingPathComponent("b").path, withDestinationPath: "../outside")
    try fm.createSymbolicLink(atPath: wt.appendingPathComponent("x/s").path, withDestinationPath: home.appendingPathComponent(".ssh").path)
    for sub in ["a", "a/.ssh", "b", "b/z", "x/s", "x/../a/.ssh", "missing"] {
        #expect(ShareHub.dir(wt, sub).path == wt.path, "\(sub)")
    }
    #expect(ShareHub.dir(wt, "x/y").path == wt.appendingPathComponent("x/y").path)
}

/// M4: native v mapi sami: tudi preusmeritve gitdir (commondir, worktrees) niso pisljive.
@Test func agentArgsDenyGitIndirection() throws {
    var cfg = ClaudeCLIConfig()
    cfg.allowLocalTools = true
    let args = cfg.agentArgs(cwd: URL(fileURLWithPath: "/p"), allTools: [])
    let s = args[try #require(args.firstIndex(of: "--settings")) + 1]
    for d in ["config", "hooks", "info", "commondir", "worktrees", "config.worktree"] {
        #expect(s.contains(#""\/p\/.git\/"# + d + "\""), "\(d): \(s)")
        #expect(s.contains(#""\/p\/**\/.git\/"# + d + "\"") && args.contains { $0.contains("Edit(//p/**/.git/\(d))") }, "nested \(d): \(s)")  // N3
    }
}

/// H2 (kot Go): zamenjan .git (kopija z novim config) ali podtaknjen info/attributes ali commondir: mesto se ne nadaljuje.
@Test func slotGitIntact() throws {
    let fm = FileManager.default
    let wt = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("wtslot-" + UUID().uuidString)
    defer { try? fm.removeItem(at: wt) }
    try fm.createDirectory(at: wt, withIntermediateDirectories: true)
    g(wt, "init", "-q")
    let slot = ShareHub.Slot(wt: wt, branch: "b", base: "x"), git = wt.appendingPathComponent(".git")
    #expect(slot.gitIntact)
    for p in ["info/attributes", "commondir"] {
        try file(wt, ".git/" + p, "x\n")
        #expect(!slot.gitIntact, "\(p)")
        try fm.removeItem(at: git.appendingPathComponent(p))
    }
    #expect(slot.gitIntact)
    try fm.moveItem(at: git, to: wt.appendingPathComponent(".g"))
    try fm.copyItem(at: wt.appendingPathComponent(".g"), to: git)
    #expect(!slot.gitIntact)
}

/// N9: claude -p v klonu (worktree) brez »@pot« in ukazov s poševnico; lažni claude brez obeh konča brez odgovora.
@Test func worktreeClaudeRunHasNoMentionsNoSlashCommands() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let fake = root.appendingPathComponent("claude")
    try Data("""
        #!/bin/sh
        [ "$1" = --version ] && { echo '2.1.0 (Claude Code)'; exit 0; }
        [ "$CLAUDE_CODE_DISABLE_FILE_MENTIONS" = 1 ] || exit 9
        case " $* " in *" --disable-slash-commands "*) ;; *) exit 9 ;; esac
        cat > /dev/null
        echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
        echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'

        """.utf8).write(to: fake)
    chmod(fake.path, 0o755)
    var c = RouterConfig()
    c.claudeCLI.enabled = true; c.claudeCLI.executable = fake.path
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let hub = Router.shares, session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    var r = request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": session, "x-claude-mini-cwd": client.path],
                    body: #"{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}"#)
    r.device = Noise.Device(id: "dev", name: "dev")
    let w = FakeWriter()
    await router.handle(r, writer: w)
    #expect(w.status == 200 && w.text.contains("done"), "\(w.text)")
}

/// N9: agent v klonu naredi ugnezden repozitorij (mapa .git ali datoteka gitdir:) s svojim filtrom, gitlink v indeksu in
/// .gitmodules z ignore=none; git status samega claude (okolje claudeEnv) filtra ne sme izvesti, nestedGit ga najde.
@Test func nestedRepoFilterDoesNotRunUnderClaudeEnv() throws {
    for gitfile in [false, true] {
        let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("n9-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: root) }
        let wt = root.appendingPathComponent("wt"), sub = wt.appendingPathComponent("sub"), marker = root.appendingPathComponent("ran").path
        try FileManager.default.createDirectory(at: wt.appendingPathComponent("build"), withIntermediateDirectories: true)
        g(wt, "init", "-q", "-b", "main"); try file(wt, "a.txt", "a\n"); g(wt, "add", "a.txt"); g(wt, "commit", "-qm", "first")
        #expect(Worktree.nestedProblem(wt, repo: root.appendingPathComponent("repos/d/r.git")) == nil)
        if gitfile { g(wt, "init", "-q", "--separate-git-dir=" + wt.appendingPathComponent("build/g").path, "sub") } else { g(wt, "init", "-q", "sub") }
        try file(sub, "f", "hello\n"); g(sub, "add", "f"); g(sub, "commit", "-qm", "s")
        let sha = g(sub, "rev-parse", "HEAD").trimmingCharacters(in: .whitespacesAndNewlines)
        let gd = URL(fileURLWithPath: g(sub, "rev-parse", "--absolute-git-dir").trimmingCharacters(in: .whitespacesAndNewlines))
        let cfg = try FileHandle(forWritingTo: gd.appendingPathComponent("config"))
        cfg.seekToEndOfFile(); cfg.write(Data("[filter \"x\"]\n\tclean = touch \(marker); cat\n".utf8)); try cfg.close()
        try file(gd, "info/attributes", "* filter=x\n")
        try file(sub, "f", "hellO\n")
        try file(wt, ".gitmodules", "[submodule \"sub\"]\n\tpath = sub\n\turl = ./sub\n\tignore = none\n")
        g(wt, "add", ".gitmodules"); g(wt, "update-index", "--add", "--cacheinfo", "160000,\(sha),sub")
        func status(_ extra: [String: String]) -> Bool {
            try? FileManager.default.removeItem(atPath: marker)
            let p = Process()
            p.executableURL = URL(fileURLWithPath: Worktree.git)
            p.arguments = ["--no-optional-locks", "status", "--porcelain"]
            p.currentDirectoryURL = wt
            p.environment = ProcessInfo.processInfo.environment.merging(extra) { $1 }
            p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
            try? p.run(); p.waitUntilExit()
            return FileManager.default.fileExists(atPath: marker)
        }
        let maven = root.appendingPathComponent("m")
        #expect(status(Worktree.harnessEnv(root: wt, maven: maven, agent: "claude")), "gitfile=\(gitfile): the test does not reproduce the attack")
        let env = try #require(Worktree.claudeEnv(root: wt, maven: maven, gitexec: root.appendingPathComponent("gitexec"), agent: "claude"))
        #expect(!status(env), "gitfile=\(gitfile): nested filter ran under the Claude CLI environment")
        // Tudi registriran ugnezdeni klon (0.5.20): git samega claude vanj vstopi z relativnim GIT_DIR, ovoj ga zavrne.
        let reg = try #require(Worktree.claudeEnv(root: wt, maven: maven, gitexec: root.appendingPathComponent("gitexec"),
                                                  gits: [wt.appendingPathComponent(".git").path, gd.path], agent: "claude"))
        #expect(!status(reg), "gitfile=\(gitfile): registered nested filter ran")
        #expect(Worktree.nestedProblem(wt, repo: root.appendingPathComponent("repos/d/r.git"))
                == "changes inside the nested repository sub do not come back in worktree mode; use sync")
    }
}

/// N9 (odločitev 0.5.20, kot Go TestWorktreeNestedRepoIgnoredDelivered): ugnezden repozitorij v prezrti poti (build/_deps od
/// CMake FetchContent) ne ustavi veje; v poti, ki je git ne prezre, veja ne gre nazaj (in klon se ne uporabi znova).
@Test func worktreeNestedRepoIgnoredDeliveredOtherwiseNot() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    try file(client, ".gitignore", "build/\n"); g(client, "add", ".gitignore"); g(client, "commit", "-qm", "ignore build")
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    let br = "minigw/claude-\(String(Sync.folderID(session).prefix(8)))-0"
    let id = try await agentRun(hub, session) { dir in
        let dep = dir.appendingPathComponent("build/_deps/foo-src")
        try FileManager.default.createDirectory(at: dep, withIntermediateDirectories: true)
        g(dep, "init", "-q"); try file(dep, "x.c", "int x;\n")
        try file(dir, "agent.txt", "A\n")
    }
    await hub.finish(run: id)
    #expect(g(client, "show", "\(br):agent.txt").trimmingCharacters(in: .whitespacesAndNewlines) == "A")
    #expect(g(client, "ls-tree", "-r", "--name-only", br).contains("build/") == false)
    let id2 = try await agentRun(hub, session) { dir in
        let lib = dir.appendingPathComponent("vendor/lib")
        try FileManager.default.createDirectory(at: lib, withIntermediateDirectories: true)
        g(lib, "init", "-q")
        try file(dir, "agent2.txt", "B\n")
    }
    await hub.finish(run: id2)
    #expect(!g(client, "ls-tree", "-r", "--name-only", br).contains("agent2.txt"))
}

/// N9: ovoj pusti le git, katerega GIT_DIR je točno eden od registriranih .git (vrhnji in ugnezdeni kloni); prazen seznam,
/// prazen ali relativen GIT_DIR, predpona in repozitorij agenta ne (kot Go TestGitExecWrapNeedsGitDir).
@Test func gitExecWrapNeedsGitDir() throws {
    let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("gx-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: dir) }
    #expect(Worktree.gitExecWrap(dir))
    func run(_ gitDir: String, _ want: String) -> Int32 {
        let p = Process()
        p.executableURL = dir.appendingPathComponent("git")
        p.arguments = ["--version"]
        p.environment = ProcessInfo.processInfo.environment.merging(["GIT_DIR": gitDir, "MINIGW_GIT_DIRS": want]) { $1 }
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try? p.run(); p.waitUntilExit()
        return p.terminationStatus
    }
    let reg = "/w/.git\n/w/lib/.git\n/w/lib/sub/.git"
    for (d, w) in [("", ""), ("", reg), (".git", "/w/.git"), (".git", reg), ("/w/evil/.git", reg), ("/w/lib", reg), ("/w/lib/.git/", reg),
                   ("/w/x/.git", "/w/*/.git"), ("/w/.git", "\n\n")] {
        #expect(run(d, w) != 0, "\(d) \(w)")
    }
    for d in ["/w/.git", "/w/lib/.git", "/w/lib/sub/.git"] { #expect(run(d, reg) == 0, "\(d)") }
    #expect(Worktree.wrapEnv(gitDirs: ["/w/.git", "rel/.git"], gitexec: dir) == nil && Worktree.wrapEnv(gitDirs: ["/w/a\nb/.git"], gitexec: dir) == nil)
}

/// 0.5.20 (kot Go TestServeWorktreeNestedRoundTrip): podmodul, nesleden ugnezden repozitorij s commitom in brez, globina 2
/// in prezrt ugnezden repozitorij. Vsak neprezrt dobi svojo vejo nazaj, vrhnja veja zapiše novi gitlink podmodula; agentov
/// repozitorij v ugnezdenem klonu ustavi vrnitev; nameščen ugnezdeni klon je v nestedProblem sprejet.
@Test func worktreeNestedRepositoriesRoundTrip() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let src = root.appendingPathComponent("subsrc")
    try FileManager.default.createDirectory(at: src, withIntermediateDirectories: true)
    g(src, "init", "-q", "-b", "main"); try file(src, "s.txt", "s\n"); g(src, "add", "-A"); g(src, "commit", "-qm", "sub")
    try file(client, ".gitignore", "build/\n"); g(client, "add", ".gitignore")
    g(client, "-c", "protocol.file.allow=always", "submodule", "add", "-q", src.path, "sub")
    g(client, "commit", "-qm", "sub")
    for (p, commit) in [("sub/deep", true), ("vendor/lib", true), ("nc", false), ("build/_deps/x", true)] {
        let d = client.appendingPathComponent(p)
        try file(d, "f.txt", p + "\n")
        g(d, "init", "-q", "-b", "main")
        if commit { g(d, "add", "-A"); g(d, "commit", "-qm", p) }
    }
    try file(client, "vendor/lib/f.txt", "vendor/lib\nuncommitted\n")
    let heads = ["", "sub", "sub/deep", "vendor/lib"].map { g(client.appendingPathComponent($0), "rev-parse", "HEAD") }
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    let br = "minigw/claude-\(String(Sync.folderID(session).prefix(8)))-0"
    let id = try await agentRun(hub, session) { dir in
        for p in [".", "sub", "sub/deep", "vendor/lib", "nc", "build/_deps/x"] { try file(dir.appendingPathComponent(p), "agent.txt", "agent\n") }
        #expect(try String(contentsOf: dir.appendingPathComponent("vendor/lib/f.txt"), encoding: .utf8).contains("uncommitted"))
        // Cilj 1: git add -A agenta v vrhnjem ne zapiše nesledenih ugnezdenih (info/exclude klona); B: commit v podmodulu ostane
        g(dir, "add", "-A"); g(dir, "commit", "-qm", "agent top")
        g(dir.appendingPathComponent("sub"), "add", "agent.txt"); g(dir.appendingPathComponent("sub"), "commit", "-qm", "agent in sub")
    }
    await hub.finish(run: id)
    for p in ["", "sub", "sub/deep", "vendor/lib", "nc"] {
        #expect(g(client.appendingPathComponent(p), "ls-tree", "-r", "--name-only", br).split(separator: "\n").contains("agent.txt"), "\(p)")
    }
    #expect(g(client.appendingPathComponent("build/_deps/x"), "branch", "--list", "minigw/*").isEmpty)
    let top = g(client, "ls-tree", "-r", "--name-only", br)
    #expect(!top.contains("build/") && !top.contains("nc/") && !top.contains("vendor/lib/") && !top.contains("sub/"))
    #expect(g(client, "ls-tree", br, "sub").hasPrefix("160000 commit " + g(client.appendingPathComponent("sub"), "rev-parse", br)))
    #expect(g(client.appendingPathComponent("sub"), "ls-tree", br, "deep").isEmpty)  // nesleden v sub: ni gitlink
    #expect(g(client, "ls-tree", br, "vendor/lib").isEmpty && g(client, "ls-tree", br, "nc").isEmpty)
    #expect(g(client.appendingPathComponent("sub"), "log", "--format=%s", br).contains("agent in sub"))
    #expect(g(client.appendingPathComponent("nc"), "rev-list", "--count", br) == "2")
    #expect(["", "sub", "sub/deep", "vendor/lib"].map { g(client.appendingPathComponent($0), "rev-parse", "HEAD") } == heads)
    let base = g(client, "log", "--format=%H", "--grep", "minigw: your uncommitted changes", "-1", br)
    g(client, "stash", "-q", "-u")  // cherry-pick -n potrebuje čisto mapo; uporabnikovo delo nazaj potem
    #expect(Worktree.run(["cherry-pick", "-n", base + ".." + br], cwd: client.path, safe: false).status == 0)
    g(client, "reset", "-q", "--hard"); g(client, "stash", "pop", "-q")
    // Agent naredi repozitorij v ugnezdenem klonu: veja ne gre nazaj (in klon se ne uporabi znova).
    let id2 = try await agentRun(hub, session) { dir in
        let evil = dir.appendingPathComponent("vendor/lib/x/evil")
        try FileManager.default.createDirectory(at: evil, withIntermediateDirectories: true)
        g(evil, "init", "-q")
        try file(dir, "agent2.txt", "B\n")
    }
    await hub.finish(run: id2)
    #expect(!g(client, "ls-tree", "-r", "--name-only", br).contains("agent2.txt"))
}

/// H1 (kot Go cloneTarget): klon ugnezdenega repozitorija ne gre skozi povezavo ali v neprazno mapo.
@Test func worktreeNestedCloneTarget() throws {
    let wt = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("ct-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: wt) }
    try file(wt, "full/f", "x"); try file(wt, "f.txt", "x")
    try FileManager.default.createDirectory(at: wt.appendingPathComponent("empty"), withIntermediateDirectories: true)
    try FileManager.default.createSymbolicLink(at: wt.appendingPathComponent("link"), withDestinationURL: FileManager.default.temporaryDirectory)
    for p in ["link/x", "link", "full", "f.txt/x"] { #expect(!Worktree.cloneTarget(wt, p), "\(p)") }
    for p in ["new/deep/x", "empty"] { #expect(Worktree.cloneTarget(wt, p), "\(p)") }
    #expect(Worktree.nestedMirror(URL(fileURLWithPath: "/s/repos/d/abc.git"), "lib").path == "/s/repos/d/abc-\(Worktree.nestedKey("lib")).git")
}

/// N1 (kot Go TestWorktreeNestedSwapDuringFinalize): zamenjava poti ugnezdenega klona s povezavo ven po preverbi zavrže
/// rezultat; ref zrcala ostane prejšnji, bundla ni.
@Test func worktreeNestedSwapDuringFinalize() throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let wt = root.appendingPathComponent("wt"), outside = root.appendingPathComponent("outside"), lib = client.appendingPathComponent("lib")
    for d in [lib, outside] { try file(d, "f.txt", "x\n"); g(d, "init", "-q", "-b", "main"); g(d, "add", "f.txt"); g(d, "commit", "-qm", "x") }
    try file(outside, "secret.txt", "gateway secret\n")
    let mirror = Worktree.nestedMirror(tree.repo, "lib"), base = g(lib, "rev-parse", "HEAD"), b = root.appendingPathComponent("push.bundle")
    g(lib, "update-ref", "refs/minigw/push", base); g(lib, "bundle", "create", b.path, "refs/minigw/push")
    #expect(Worktree.ensureMirror(mirror) && Worktree.importBase(mirror, bundle: b, snapshot: base, base: "refs/minigw/base/x"))
    try FileManager.default.createDirectory(at: wt, withIntermediateDirectories: true)
    #expect(Worktree.makeClone(mirror, at: wt.appendingPathComponent("lib"), branch: "minigw/claude-12345678-0", snapshot: base))
    let n = ShareHub.Nested(path: "lib", repo: mirror, base: base, wt: wt)
    let slot = ShareHub.Slot(wt: wt, branch: "minigw/claude-12345678-0", base: base, nested: [n])
    try file(wt.appendingPathComponent("lib"), "agent.txt", "agent\n")
    Worktree.beforeNestedFinalize = { _ in
        try? FileManager.default.moveItem(at: wt.appendingPathComponent("lib"), to: wt.appendingPathComponent("lib.real"))
        try? FileManager.default.createSymbolicLink(at: wt.appendingPathComponent("lib"), withDestinationURL: outside)
    }
    defer { Worktree.beforeNestedFinalize = nil }
    let out = root.appendingPathComponent("out.bundle")
    let (tip, why) = Worktree.finalizeNested(slot, n, agent: "claude", out: out, links: [:])
    #expect(tip == nil && why.contains("moved or replaced"))
    #expect(Worktree.run(["--git-dir", mirror.path, "rev-parse", "-q", "--verify", "refs/heads/minigw/claude-12345678-0"]).out.isEmpty)
    #expect(!FileManager.default.fileExists(atPath: out.path))
}

/// N2/N5/N6 (kot Go TestNestedReposRefuseSymlinks): povezava na poti gitlinka ali ugnezdenega repozitorija ni ugnezden
/// repozitorij mape; imena s HFS+ prezrtimi znaki in pot v .build ne.
@Test func nestedReposRefuseSymlinks() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("n2-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let top = root.appendingPathComponent("top"), outside = root.appendingPathComponent("outside")
    for d in [top, outside, top.appendingPathComponent("real")] { try file(d, "f.txt", "x\n"); g(d, "init", "-q", "-b", "main"); g(d, "add", "f.txt"); g(d, "commit", "-qm", "x") }
    let sha = g(outside, "rev-parse", "HEAD")
    try FileManager.default.createSymbolicLink(at: top.appendingPathComponent("link"), withDestinationURL: outside)
    g(top, "update-index", "--add", "--cacheinfo", "160000,\(sha),link")
    try FileManager.default.createDirectory(at: top.appendingPathComponent("d"), withIntermediateDirectories: true)
    try FileManager.default.createSymbolicLink(at: top.appendingPathComponent("d/inner"), withDestinationURL: outside)
    #expect(Worktree.nestedRepos(top: top.path) == ["real"])
    #expect(!Worktree.nestedInside(top: top.path, "link") && !Worktree.nestedInside(top: top.path, "d/inner") && Worktree.nestedInside(top: top.path, "real"))
    #expect(Sync.normName(".g\u{200C}it") == ".git")
    for p in ["a\u{200D}/b", "x\u{FEFF}", ".build/x", ".BUILD"] { #expect(!Worktree.validNestedPath(p), "\(p)") }
}

/// Agent je vse commital sam: veja se shrani in objekti ostanejo v zrcalu (kot Go TestWorktreeFinalizeAllCommitted).
@Test func worktreeFinalizeAllCommitted() throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let head = g(client, "rev-parse", "HEAD"), b = root.appendingPathComponent("push.bundle"), wt = root.appendingPathComponent("wt")
    g(client, "update-ref", "refs/minigw/push", head); g(client, "bundle", "create", b.path, "refs/minigw/push")
    #expect(Worktree.ensureMirror(tree.repo) && Worktree.importBase(tree.repo, bundle: b, snapshot: head, base: "refs/minigw/base/x")
            && Worktree.makeClone(tree.repo, at: wt, branch: "minigw/claude-12345678-0", snapshot: head))
    for f in ["a.txt", "new.txt"] { try? FileManager.default.removeItem(at: wt.appendingPathComponent(f)) }
    g(wt, "checkout", "-q", "--", "."); try file(wt, "b.txt", "b\n"); g(wt, "add", "b.txt"); g(wt, "commit", "-qm", "agent")
    let agentTip = g(wt, "rev-parse", "HEAD")
    let tip = Worktree.finalize(repo: tree.repo, wt: wt, branch: "minigw/claude-12345678-0", base: head, agent: "claude", out: root.appendingPathComponent("out.bundle"))
    #expect(tip == agentTip)
    try? FileManager.default.removeItem(at: wt)
    #expect(Worktree.run(["--git-dir", tree.repo.path, "show", "minigw/claude-12345678-0:b.txt"]).out == "b")
}

/// A (0.5.20): native z git obravnava ugnezdene kot worktree: podmodul (gitfile v .git/modules), nesleden s commitom in brez
/// commita dobijo vsak svojo vejo naravnost v svoj repozitorij; nesledeni niso gitlink v vrhnji veji; prezrt ostane zunaj;
/// repozitoriji uporabnika (HEAD, indeks) ostanejo.
@Test func nativeGitNestedRepositories() async throws {
    let (root, client, mirror) = try fixture()
    defer { cleanup(root) }
    let src = root.appendingPathComponent("subsrc")
    try file(src, "s.txt", "s\n"); g(src, "init", "-q", "-b", "main"); g(src, "add", "-A"); g(src, "commit", "-qm", "sub")
    try file(client, ".gitignore", "build/\n"); g(client, "add", ".gitignore")
    g(client, "-c", "protocol.file.allow=always", "submodule", "add", "-q", src.path, "sub"); g(client, "commit", "-qm", "sub")
    for (p, commit) in [("vendor/lib", true), ("nc", false), ("build/_deps/x", true)] {
        let d = client.appendingPathComponent(p)
        try file(d, "f.txt", p + "\n"); g(d, "init", "-q", "-b", "main")
        if commit { g(d, "add", "-A"); g(d, "commit", "-qm", p) }
    }
    let heads = ["", "sub", "vendor/lib"].map { g(client.appendingPathComponent($0), "rev-parse", "HEAD") }
    let (top, sub) = try #require(Router.nativeBranches(client, inPlace: false, roots: root.path, scratch: mirror.repo))
    #expect(sub.isEmpty)
    let tree = ShareHub.Tree(repo: mirror.repo, worktrees: mirror.worktrees, sub: sub, local: top)
    let hub = ShareHub(), session = UUID().uuidString
    _ = try #require(await hub.register(session: session, cwd: "/Volumes/share/client", key: nil, device: "dev", tree: tree))
    let id = UUID().uuidString
    let dir = try #require(await ShareHub.$run.withValue(id) { await ShareHub.$agent.withValue("claude") {
        await hub.mount(session: session, cwd: nil, base: "", device: "dev", roots: root.path, writes: true) } })
    for p in [".", "sub", "vendor/lib", "nc"] { try file(dir.appendingPathComponent(p), "agent.txt", "agent\n") }
    await ShareHub.$run.withValue(id) { await hub.syncBack(session: session) }
    await hub.finish(run: id)
    let br = "minigw/claude-\(String(Sync.folderID("dev|" + session).prefix(8)))-0"
    for p in ["", "sub", "vendor/lib", "nc"] {
        #expect(g(client.appendingPathComponent(p), "ls-tree", "-r", "--name-only", br).split(separator: "\n").contains("agent.txt"), "\(p)")
    }
    #expect(g(client, "ls-tree", br, "vendor/lib").isEmpty && g(client, "ls-tree", br, "nc").isEmpty)
    #expect(g(client, "ls-tree", br, "sub").hasPrefix("160000 commit " + g(client.appendingPathComponent("sub"), "rev-parse", br)))
    #expect(g(client.appendingPathComponent("build/_deps/x"), "branch", "--list", "minigw/*").isEmpty)
    #expect(["", "sub", "vendor/lib"].map { g(client.appendingPathComponent($0), "rev-parse", "HEAD") } == heads)
    #expect(!Worktree.hasCommit(top: client.appendingPathComponent("nc").path))
    // Povezan worktree ugnezdenega (commondir): izvajanje se ne začne (razlog uporabniku z namigom na native-direct).
    g(client.appendingPathComponent("vendor/lib"), "worktree", "add", "-q", client.appendingPathComponent("linked").path)
    #expect(Worktree.localGitDir(top: top, "linked") == nil)
    let id2 = UUID().uuidString, session2 = UUID().uuidString  // nova seja: nov posnetek (stara bi nadaljevala v svojem klonu)
    _ = try #require(await hub.register(session: session2, cwd: "/Volumes/share/client", key: nil, device: "dev", tree: tree))
    #expect(await ShareHub.$run.withValue(id2) { await ShareHub.$agent.withValue("claude") {
        await hub.mount(session: session2, cwd: nil, base: "", device: "dev", roots: root.path, writes: true) } } == nil)
}

/// N1 (recenzija 2): zamenjava poti ugnezdenega klona s povezavo na tuj repozitorij takoj po preverbi exact in vrnitev
/// pred končno preverbo: finalize nič ne razreši znova (O_NOFOLLOW_ANY, profil iz pričakovane poti), zato tuja skrivnost
/// ne pride v vejo in tuj objects/info ostane.
@Test func worktreeNestedSwapAndRestore() throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let wt = root.appendingPathComponent("wt"), outside = root.appendingPathComponent("outside"), lib = client.appendingPathComponent("lib")
    for d in [lib, outside] { try file(d, "f.txt", "x\n"); g(d, "init", "-q", "-b", "main"); g(d, "add", "f.txt"); g(d, "commit", "-qm", "x") }
    try file(outside, "secret.txt", "gateway secret\n")
    try file(outside, ".git/objects/info/marker", "keep\n")
    let mirror = Worktree.nestedMirror(tree.repo, "lib"), base = g(lib, "rev-parse", "HEAD"), b = root.appendingPathComponent("push.bundle")
    g(lib, "update-ref", "refs/minigw/push", base); g(lib, "bundle", "create", b.path, "refs/minigw/push")
    #expect(Worktree.ensureMirror(mirror) && Worktree.importBase(mirror, bundle: b, snapshot: base, base: "refs/minigw/base/x"))
    try FileManager.default.createDirectory(at: wt, withIntermediateDirectories: true)
    #expect(Worktree.makeClone(mirror, at: wt.appendingPathComponent("lib"), branch: "minigw/claude-12345678-0", snapshot: base))
    let n = ShareHub.Nested(path: "lib", repo: mirror, base: base, wt: wt)
    let slot = ShareHub.Slot(wt: wt, branch: "minigw/claude-12345678-0", base: base, nested: [n])
    try file(wt.appendingPathComponent("lib"), "agent.txt", "agent\n")
    Worktree.duringFinalize = {
        try? FileManager.default.moveItem(at: wt.appendingPathComponent("lib"), to: wt.appendingPathComponent("lib.real"))
        try? FileManager.default.createSymbolicLink(at: wt.appendingPathComponent("lib"), withDestinationURL: outside)
    }
    Worktree.afterNestedFinalize = { _ in
        try? FileManager.default.removeItem(at: wt.appendingPathComponent("lib"))
        try? FileManager.default.moveItem(at: wt.appendingPathComponent("lib.real"), to: wt.appendingPathComponent("lib"))
    }
    defer { Worktree.duringFinalize = nil; Worktree.afterNestedFinalize = nil }
    let (tip, _) = Worktree.finalizeNested(slot, n, agent: "claude", out: root.appendingPathComponent("out.bundle"), links: [:])
    if let tip { #expect(!Worktree.run(["--git-dir", mirror.path, "ls-tree", "-r", "--name-only", tip]).out.contains("secret.txt")) }
    #expect((try? String(contentsOf: outside.appendingPathComponent(".git/objects/info/marker"), encoding: .utf8)) == "keep\n")
    #expect(Worktree.run(["--git-dir", mirror.path, "cat-file", "-e", Worktree.run(["hash-object", "--stdin"], input: Data("gateway secret\n".utf8)).out]).status != 0)
}

/// N3 (kot Go TestNestedSnapshotRunsNoCleanFilter): posnetek ugnezdenega ne požene filtra clean iz .gitattributes.
@Test func nestedSnapshotRunsNoCleanFilter() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("n3-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let dir = root.appendingPathComponent("r"), marker = root.appendingPathComponent("ran").path
    try file(dir, "a.txt", "a\n"); g(dir, "init", "-q", "-b", "main"); g(dir, "add", "a.txt"); g(dir, "commit", "-qm", "a")
    g(dir, "config", "filter.evil.clean", "touch \(marker); cat")
    try file(dir, ".gitattributes", "* filter=evil\n"); try file(dir, "a.txt", "b\n")
    // git pred 2.40: brez posnetka (filter ne teče)
    #expect((Worktree.snapshot(top: dir.path, noAttr: true) != nil) == Worktree.attrSource && !FileManager.default.fileExists(atPath: marker))
    _ = Worktree.snapshot(top: dir.path)
    #expect(FileManager.default.fileExists(atPath: marker), "the test does not reproduce the filter")
}

/// Razorožitev ohranjenega klona teče v ozadju: počakaj, da so poti `want` v njem (največ 5 s).
private func waitKept(_ kept: String, _ want: [String] = [".git.agent"]) async {
    func lexists(_ p: String) -> Bool { var st = stat(); return lstat(p, &st) == 0 }
    for _ in 0..<100 where !want.allSatisfy({ lexists(kept + "/" + $0) }) { try? await Task.sleep(nanoseconds: 50_000_000) }
}

/// Absolutna pot ohranjenega klona iz sporočila (L3: pot glede na Scratch prehoda).
private func keptPath(_ line: String, _ tree: ShareHub.Tree) -> String? {
    guard let r = line.range(of: #"as worktrees/\S+-kept-\d{8}-\d{6}-\d{9} after this session \(files only; its \.git folders renamed \.git\.agent, or nothing is kept if that fails — do not run git there\)$"#, options: .regularExpression) else { return nil }
    let rel = line[r].dropFirst(3).prefix { $0 != " " }
    return tree.worktrees.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent(String(rel)).path
}

/// L6 (kot Go TestWorktreeNestedSkippedTopDelivered): zavrnjen ugnezdeni (premaknjen, večji od 64 MB) ne ustavi vrhnjega:
/// vrhnja veja pride (brez vsebine zavrnjenega), ena vrstica pove, kateri in zakaj, klon z delom po koncu seje ostane.
@Test(arguments: ["moved", "big"]) func worktreeNestedSkippedTopDelivered(_ kind: String) async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    for p in ["lib", "other"] {
        let d = client.appendingPathComponent(p)
        try file(d, "f.txt", "f\n"); g(d, "init", "-q", "-b", "main"); g(d, "add", "f.txt"); g(d, "commit", "-qm", "first")
    }
    let hub = ShareHub(), session = UUID().uuidString, notes = OSAllocatedUnfairLock<[String]>(initialState: [])
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client, notes: notes)
    defer { loop.cancel() }
    let br = "minigw/claude-\(String(Sync.folderID(session).prefix(8)))-0"
    var wt: URL?
    let id = try await agentRun(hub, session) { dir in
        wt = dir
        for p in [".", "lib", "other"] { try file(dir.appendingPathComponent(p), "agent.txt", "agent\n") }
        if kind == "moved" {
            try FileManager.default.removeItem(at: dir.appendingPathComponent("lib/.git"))
        } else {
            try FileHandle(forReadingAtPath: "/dev/urandom")!.readData(ofLength: 70_000_000).write(to: dir.appendingPathComponent("lib/big.bin"))
        }
    }
    await hub.finish(run: id)
    let why = kind == "moved" ? "the nested repository lib was moved or replaced in the agent's clone"
        : "the agent's branch in the nested repository lib is larger than 64 MB; it stays on the gateway's mirror (\(br))"
    #expect(g(client, "show", br + ":agent.txt") == "agent")
    let top = g(client, "ls-tree", "-r", "--name-only", br)
    #expect(!top.contains("lib/") && !top.contains("other/"))
    #expect(g(client.appendingPathComponent("lib"), "branch", "--list", br).isEmpty)
    #expect(g(client.appendingPathComponent("other"), "show", br + ":agent.txt") == "agent")
    let line = try #require(notes.withLock { $0 }.first { $0.hasPrefix("worktree: " + why + "; the agent's files are kept on the gateway for 30 days as worktrees/") })
    let kept = try #require(keptPath(line, tree))
    if kind == "moved" {  // L1: naslednja zahteva naredi nov klon na isti poti; nedostavljeno delo prej v kept
        let id2 = try await agentRun(hub, session) { _ in }
        await hub.finish(run: id2)
        #expect((try? String(contentsOfFile: kept + "/lib/agent.txt", encoding: .utf8)) == "agent\n")
    }
    // M2: config agenta s fsmonitor v .git klona ob ohranitvi ne sme več teči
    let marker = root.appendingPathComponent("fsmonitor-ran")
    for d in ["", "/other"] where FileManager.default.fileExists(atPath: wt!.path + d + "/.git") {
        g(URL(fileURLWithPath: wt!.path + d), "config", "core.fsmonitor", "touch \(marker.path); false")
    }
    await hub.unregister(key: key)  // konec seje: klon z nedostavljenim delom ostane (keepClone zunaj akterja)
    await waitKept(kept)
    #expect((try? String(contentsOfFile: kept + "/lib/agent.txt", encoding: .utf8)) == "agent\n")
    for d in ["", "/lib", "/other"] {
        var st = stat()
        #expect(lstat(kept + d + "/.git", &st) != 0, "\(d)")
        #expect(Worktree.run(["status"], cwd: kept + d, env: ["GIT_CEILING_DIRECTORIES": URL(fileURLWithPath: kept).deletingLastPathComponent().path], safe: false).status != 0, "\(d)")
    }
    #expect(FileManager.default.fileExists(atPath: kept + "/.git.agent") && !FileManager.default.fileExists(atPath: marker.path))
    // Zrcalo po 30 dneh gre, ohranjeni klon (mlajši od 30 dni) ne; po svojih 30 dneh gre tudi on.
    let scratch = tree.repo.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    try FileManager.default.setAttributes([.modificationDate: Date().addingTimeInterval(40 * 86400)], ofItemAtPath: kept)  // prune premakne mejo (days), ne ure
    Worktree.prune(scratch: scratch, days: -31)
    #expect(FileManager.default.fileExists(atPath: kept) && !FileManager.default.fileExists(atPath: tree.repo.path))
    Worktree.prune(scratch: scratch, days: -51)
    #expect(!FileManager.default.fileExists(atPath: kept))
}

/// L6 (M1, native): zamenjan .git/objects uporabnika ustavi dostavo; sporočilo pove, da klon z delom ostane, ob koncu
/// seje gre v kept.
@Test func nativeObjectsReplacedKeepsClone() async throws {
    let (root, client, mirror) = try fixture()
    defer { cleanup(root) }
    let (top, sub) = try #require(Router.nativeBranches(client, inPlace: false, roots: root.path, scratch: mirror.repo))
    let tree = ShareHub.Tree(repo: mirror.repo, worktrees: mirror.worktrees, sub: sub, local: top)
    let hub = ShareHub(), session = UUID().uuidString, notes = OSAllocatedUnfairLock<[String]>(initialState: [])
    let key = try #require(await hub.register(session: session, cwd: "/Volumes/share/client", key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client, notes: notes)
    defer { loop.cancel() }
    let id = UUID().uuidString
    let wt = try #require(await ShareHub.$run.withValue(id) { await ShareHub.$agent.withValue("claude") {
        await hub.mount(session: session, cwd: nil, base: "", device: "dev", roots: root.path, writes: true) } })
    try file(wt, "agent.txt", "agent\n")
    let objects = client.appendingPathComponent(".git/objects")
    try FileManager.default.moveItem(at: objects, to: client.appendingPathComponent(".git/objects.real"))
    try FileManager.default.createSymbolicLink(at: objects, withDestinationURL: client.appendingPathComponent(".git/objects.real"))
    await ShareHub.$run.withValue(id) { await hub.syncBack(session: session) }
    await hub.finish(run: id)
    let line = try #require(notes.withLock { $0 }.first { $0.contains(".git/objects was replaced") })
    let kept = try #require(keptPath(line, tree))
    await hub.unregister(key: key)
    await waitKept(kept)
    #expect((try? String(contentsOfFile: kept + "/agent.txt", encoding: .utf8)) == "agent\n")
    #expect(!FileManager.default.fileExists(atPath: wt.path))
}

/// M1, L2 (kot Go TestPruneKept): največ maxKept ohranjenih (najstarejši gredo), starejši od meje gredo; povezava se ne sledi.
@Test func pruneKeptCapsAndSkipsLinks() throws {
    let base = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("pk-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: base) }
    let dir = base.appendingPathComponent("d"), outside = base.appendingPathComponent("o/x-kept-1")
    try file(outside, "f", "x"); try file(dir.appendingPathComponent("c-0"), "f", "x")
    try FileManager.default.createSymbolicLink(at: dir.appendingPathComponent("link-kept-0"), withDestinationURL: outside)
    let now = Date()
    for i in 1...5 {
        let d = dir.appendingPathComponent("c-kept-\(i)")
        try file(d, "f", "x")
        try FileManager.default.setAttributes([.modificationDate: now.addingTimeInterval(Double(i) * 3600)], ofItemAtPath: d.path)
    }
    ShareHub.pruneKept(dir, before: .distantPast)
    #expect(try FileManager.default.contentsOfDirectory(atPath: dir.path).sorted() == ["c-0", "c-kept-3", "c-kept-4", "c-kept-5", "link-kept-0"])
    ShareHub.pruneKept(dir, before: now.addingTimeInterval(4 * 3600))
    #expect(!FileManager.default.fileExists(atPath: dir.appendingPathComponent("c-kept-3").path))
    #expect(FileManager.default.fileExists(atPath: outside.appendingPathComponent("f").path))
}

/// #57, #58: tanek bundle (evil..c2, c2 ponovno uporabi drevo z ».git«, ki ga je fetch brez fsck pustil v objektih) fetch
/// sprejme tudi na novem gitu; fsck ga zavrne pri odjemalcu (lastno sporočilo), ob uvozu v zrcalo prehoda in pri native.
@Test func thinBundleFailsTheObjectCheck() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("thin-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let src = root.appendingPathComponent("src"), client = root.appendingPathComponent("client"), mirror = root.appendingPathComponent("m.git")
    try FileManager.default.createDirectory(at: src, withIntermediateDirectories: true)
    g(src, "init", "-q", "-b", "main")
    try file(src, "a.txt", "a\n")
    g(src, "add", "-A"); g(src, "commit", "-qm", "a")
    g(root, "clone", "-q", "--no-local", src.path, client.path)
    g(root, "clone", "-q", "--bare", "--no-local", src.path, mirror.path)
    func stdin(_ text: String, _ args: String...) -> String { Worktree.run(args, cwd: src.path, safe: false, input: Data(text.utf8)).out }
    let dg = stdin("100644 blob \(stdin("evil\n", "hash-object", "-w", "--stdin"))\tconfig\n", "mktree")
    let top = stdin("040000 tree \(dg)\t.git\n", "mktree")
    let evil = g(src, "commit-tree", top, "-p", "HEAD", "-m", "evil"), c2 = g(src, "commit-tree", top, "-p", evil, "-m", "c2")
    let branch = "minigw/claude-0123abcd-0"
    g(src, "update-ref", "refs/heads/bad", evil); g(src, "update-ref", "refs/heads/" + branch, c2); g(src, "update-ref", "refs/minigw/push", c2)
    let bad = root.appendingPathComponent("bad.bundle"), thin = root.appendingPathComponent("thin.bundle"), push = root.appendingPathComponent("push.bundle")
    g(src, "bundle", "create", bad.path, "refs/heads/bad")
    g(src, "bundle", "create", thin.path, "refs/heads/" + branch, "^" + evil)
    g(src, "bundle", "create", push.path, "refs/minigw/push", "^" + evil)
    for dir in [client, mirror] {  // zavrnjen fetch pusti objekte, ki jih noben ref ne doseže
        g(dir, "-c", "fetch.fsckObjects=false", "fetch", "-q", bad.path, "+refs/heads/bad:refs/scratch")
        g(dir, "update-ref", "-d", "refs/scratch")
    }

    let why = try #require(Worktree.fetch(top: client.path, bundle: thin, branch: branch))
    #expect(why.hasPrefix("the gateway's branch failed the object check (fsck): ") && why.contains(".git"), "\(why)")
    #expect(g(client, "rev-parse", "-q", "--verify", "refs/heads/" + branch).isEmpty)
    // #57 prehod: uvoz bundla odjemalca v zrcalo
    #expect(!Worktree.importBase(mirror, bundle: push, snapshot: c2, base: "refs/minigw/base/x"))
    #expect(g(mirror, "rev-parse", "-q", "--verify", "refs/minigw/base/x").isEmpty)
    // ostanek zavrnjenega bundla prek zahteve le s posnetkom (brez bundla): prav tako zavrnjen
    #expect(!Worktree.importBase(mirror, bundle: nil, snapshot: evil, base: "refs/minigw/base/y"))
    #expect(g(mirror, "rev-parse", "-q", "--verify", "refs/minigw/base/y").isEmpty)
    let head = g(mirror, "rev-parse", "HEAD")  // dosegljiv posnetek: prazen paket, sprejet
    #expect(Worktree.importBase(mirror, bundle: nil, snapshot: head, base: "refs/minigw/base/z"))
    // #57 native: veja zrcala (tu src) v repozitorij uporabnika
    let out = root.appendingPathComponent("out.bundle")
    let local = try #require(Worktree.localBundle(repo: src.appendingPathComponent(".git"), top: client, branch: branch, out: out))
    #expect(local.hasPrefix("\(branch) failed the object check (fsck): ") && !FileManager.default.fileExists(atPath: out.path), "\(local)")
    g(src, "update-ref", "refs/heads/" + branch, g(src, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "fine"))
    #expect(Worktree.localBundle(repo: src.appendingPathComponent(".git"), top: client, branch: branch, out: out) == nil)
}

/// Scratch na drugem disku kot $TMPDIR (zunanji disk): fsck uvoza ne sme pasti na »Cross-device link« (pack-objects
/// preimenuje paket iz objects/pack v začasno mapo), sicer vsak worktree vrne 502.
@Test func importBaseWithMirrorOnAnotherVolume() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("xdev-" + UUID().uuidString)
    let mnt = root.appendingPathComponent("mnt")
    try FileManager.default.createDirectory(at: mnt, withIntermediateDirectories: true)
    func hdiutil(_ a: String...) -> Int32 {
        let p = Process(); p.executableURL = URL(fileURLWithPath: "/usr/bin/hdiutil"); p.arguments = a
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try? p.run(); p.waitUntilExit(); return p.terminationStatus
    }
    let img = root.appendingPathComponent("v.dmg").path
    try #require(hdiutil("create", "-size", "16m", "-fs", "HFS+", "-volname", "xdev", img) == 0)
    try #require(hdiutil("attach", "-nobrowse", "-mountpoint", mnt.path, img) == 0)
    defer { _ = hdiutil("detach", "-force", mnt.path); try? FileManager.default.removeItem(at: root) }
    let src = root.appendingPathComponent("src"), mirror = mnt.appendingPathComponent("m.git")
    try FileManager.default.createDirectory(at: src, withIntermediateDirectories: true)
    g(src, "init", "-q", "-b", "main")
    try file(src, "a.txt", "a\n")
    g(src, "add", "-A"); g(src, "commit", "-qm", "a")
    let snap = g(src, "rev-parse", "HEAD")
    g(src, "update-ref", "refs/minigw/push", snap)
    let push = root.appendingPathComponent("push.bundle")
    g(src, "bundle", "create", push.path, "refs/minigw/push")
    g(root, "init", "-q", "--bare", mirror.path)
    #expect(Worktree.importBase(mirror, bundle: push, snapshot: snap, base: "refs/minigw/base/x"))
    #expect(Worktree.fsckIncoming(top: mirror.path, ref: "refs/minigw/base/x", safe: true) == nil)
}

/// #57: ugnezdeni repozitoriji na prehodu z gitom pred 2.40 zavrnjeni pred zagonom (način ostane).
@Test func nestedNeedsAttrSourceOnTheGateway() {
    #expect(Worktree.nestedGitProblem(1, attr: false)?.hasSuffix("update git or use sync") == true)
    #expect(Worktree.nestedGitProblem(1, attr: true) == nil && Worktree.nestedGitProblem(0, attr: false) == nil)
}

/// #57: git prehoda pred 2.40 z ugnezdenim repozitorijem: trajna zavrnitev (400 z razlogom, kot Go refused), ne mount nil
/// → 502, ki ga Claude Code ponavlja; prek odjemalca in pri native.
@Test func nestedOnOldGatewayGitIsRefusedWith400() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let lib = client.appendingPathComponent("vendor/lib")
    try file(lib, "f.txt", "x\n"); g(lib, "init", "-q", "-b", "main"); g(lib, "add", "-A"); g(lib, "commit", "-qm", "lib")
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    let (dir, why) = await Worktree.$attrSource.withValue(false) {
        await ShareHub.$run.withValue(UUID().uuidString) { await ShareHub.$agent.withValue("claude") {
            (await hub.mount(session: session, cwd: nil, base: "", device: "dev"), await hub.takeRefusal()) } }
    }
    #expect(dir == nil && why?.status == 400
            && why?.message == "worktree: nested git repositories need git 2.40 or later on the gateway; update git or use sync", "\(String(describing: why))")

    let (top, _) = try #require(Router.nativeBranches(client, inPlace: false, roots: root.path, scratch: tree.repo))
    let native = ShareHub.Tree(repo: tree.repo, worktrees: tree.worktrees, sub: "", local: top), s2 = UUID().uuidString
    _ = try #require(await hub.register(session: s2, cwd: "/Volumes/share/client", key: nil, device: "dev", tree: native))
    let (d2, w2) = await Worktree.$attrSource.withValue(false) {
        await ShareHub.$run.withValue(UUID().uuidString) { await ShareHub.$agent.withValue("claude") {
            (await hub.mount(session: s2, cwd: nil, base: "", device: "dev", roots: root.path, writes: true), await hub.takeRefusal()) } }
    }
    #expect(d2 == nil && w2?.status == 400 && w2?.message.hasSuffix("update git or use --minigw-work=native-direct") == true, "\(String(describing: w2))")
}

/// M2, #4, #1 (kot Go TestKeepCloneDisarmsGit): keepClone preimenuje vsak .git in HEAD golega repozitorija v klonu, nikoli
/// prek povezave ven; klon, ki je povezava, ostane, kot je.
@Test func keepCloneDisarmsGit() async throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("kc-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let wt = root.appendingPathComponent("c-0"), outside = root.appendingPathComponent("outside"), kept = root.appendingPathComponent("c-0-kept-1")
    try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    g(root, "init", "-q", wt.path); g(root, "init", "-q", "--bare", wt.appendingPathComponent("bare").path)
    g(root, "init", "-q", outside.path); g(root, "init", "-q", "--bare", outside.appendingPathComponent("b").path)
    try file(wt.appendingPathComponent("sub"), ".git", "gitdir: ../x\n"); try file(wt, "f.txt", "agent\n")
    try FileManager.default.createSymbolicLink(at: wt.appendingPathComponent("link"), withDestinationURL: outside)
    try FileManager.default.createSymbolicLink(at: wt.appendingPathComponent("blink"), withDestinationURL: outside.appendingPathComponent("b"))
    g(root, "init", "-q", wt.appendingPathComponent("ro/r").path)  // L2: agent vzame pravice mapam
    chmod(wt.appendingPathComponent("ro/r").path, 0o555); chmod(wt.appendingPathComponent("ro").path, 0o555)
    defer { for d in ["ro", "ro/r"] { chmod(kept.path + "/" + d, 0o755) } }
    ShareHub.keepClone(wt, kept)
    func exists(_ p: String) -> Bool { var st = stat(); return lstat(p, &st) == 0 }
    #expect(!exists(wt.path) && exists(kept.path))  // L1: premik takoj
    let want = [".git.agent", "sub/.git.agent", "bare/HEAD.agent", "ro/r/.git.agent", "f.txt"]
    await waitKept(kept.path, want)
    for p in want { #expect(exists(kept.path + "/" + p), "\(p)") }
    for p in [".git", "sub/.git", "bare/HEAD", "ro/r/.git"] { #expect(!exists(kept.path + "/" + p), "\(p)") }
    for d in ["", "/bare", "/sub"] {
        #expect(Worktree.run(["rev-parse", "--git-dir"], cwd: kept.path + d, env: ["GIT_CEILING_DIRECTORIES": root.path], safe: false).status != 0, "\(d)")
    }
    #expect(exists(outside.path + "/.git") && exists(outside.path + "/b/HEAD"))
    try FileManager.default.createSymbolicLink(at: root.appendingPathComponent("c-1"), withDestinationURL: outside)
    ShareHub.keepClone(root.appendingPathComponent("c-1"), root.appendingPathComponent("c-1-kept-1"))
    #expect(exists(outside.path + "/.git") && !exists(root.path + "/c-1-kept-1"))
    // L4: ohranjeni klon, ki je povezava, se ne razoroži in ne pobriše (cilj ostane)
    try FileManager.default.createSymbolicLink(at: root.appendingPathComponent("l-kept-1"), withDestinationURL: outside)
    #expect(!ShareHub.disarmKept(root.appendingPathComponent("l-kept-1")) && exists(outside.path + "/.git"))
}

/// #40 (kot Go TestPathWrapScript): besedilo enako kot v Go (client/testdata/pathwrap.golden); git dela v registriranem
/// klonu, ki vsebuje mapo ali -C <pot> (najdaljši), drugače ostane okolje; neregistriran .git in predpona /x/ab ne veljata.
@Test func pathWrapScriptMapsRegisteredClones() throws {
    let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    #expect(try String(contentsOf: repo.appendingPathComponent("client/testdata/pathwrap.golden"), encoding: .utf8) == Worktree.pathWrapScript(real: "/REAL/git"))
    let fm = FileManager.default
    let root = fm.temporaryDirectory.appendingPathComponent("pw-" + UUID().uuidString + "/a b'c")  // ne kanonična pot (/var → /private/var)
    defer { try? fm.removeItem(at: root.deletingLastPathComponent()) }
    let real = root.appendingPathComponent("real/git"), wrap = root.appendingPathComponent("bin/git")
    try file(root, "real/git", "#!/bin/sh\necho \"$GIT_DIR|$GIT_WORK_TREE|$GIT_COMMON_DIR|$*\"\n")
    try file(root, "bin/git", Worktree.pathWrapScript(real: real.path))
    chmod(real.path, 0o755); chmod(wrap.path, 0o755)
    let top = root.path + "/w", lib = top + "/lib", ab = root.path + "/x/ab"
    for d in [top, lib, lib + "/inner", root.path + "/x/a", ab, lib + "/src"] { try fm.createDirectory(atPath: d + "/.git", withIntermediateDirectories: true) }
    let reg = [top + "/.git", lib + "/.git", root.path + "/x/a/.git"].joined(separator: "\n")
    func inn(_ p: String) -> String { p + "/.git|" + p + "|" + p + "/.git" }
    for (cwd, args, want) in [(top, ["status"], inn(top)), (lib + "/src", ["status"], inn(lib)), (top, ["-C", "lib", "commit", "-m", "x"], inn(lib)),
                              (top, ["-c", "a=b", "--no-pager", "-C", "lib", "status"], inn(lib)), (top, ["-C", "lib", "-C", "src", "status"], inn(lib)),
                              (top, ["-C", lib + "/..", "status"], inn(top)), (top, ["status", "-C", "lib"], inn(top)), (lib + "/inner", ["status"], inn(lib)),
                              (ab, ["status"], inn(top)), (root.path, ["status"], inn(top)), (top, ["-C", "missing", "status"], inn(top))] {
        let p = Process(), out = Pipe()
        p.executableURL = wrap; p.arguments = args; p.currentDirectoryURL = URL(fileURLWithPath: cwd); p.standardOutput = out
        p.environment = ProcessInfo.processInfo.environment.merging(["MINIGW_GIT_DIRS": reg]) { $1 }  // #40: brez GIT_DIR; brez ujemanja vrhnji
        try p.run(); p.waitUntilExit()
        let got = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        #expect(got == want + "|" + args.joined(separator: " "), "\(cwd) \(args)")
    }
}

/// #40 (kot Go TestWorktreeHarnessEnvGitConfig): brez lastnikovih nastavitev git, avtor je agent, isti vrstni red parov;
/// PATH najprej ovoj gitexec/bin.
@Test func harnessEnvOwnGitConfig() throws {
    let env = Worktree.harnessEnv(root: URL(fileURLWithPath: "/w"), maven: URL(fileURLWithPath: "/m"), agent: "codex")
    let want = ["core.fsmonitor=false", "core.hooksPath=/dev/null", "core.attributesFile=/dev/null", "diff.ignoreSubmodules=all",
                "submodule.recurse=false", "core.excludesFile=/dev/null", "user.name=Codex (Mini Gateway)", "user.email=codex@minigw.invalid", "safe.bareRepository=explicit",
                "init.templateDir="]  // #67
    #expect((0..<want.count).map { env["GIT_CONFIG_KEY_\($0)"]! + "=" + env["GIT_CONFIG_VALUE_\($0)"]! } == want)
    #expect(env["GIT_CONFIG_GLOBAL"] == "/dev/null" && env["GIT_CONFIG_NOSYSTEM"] == "1" && env["GIT_CONFIG_COUNT"] == "10")
    #expect(env["MINIGW_DEPS"] == "/w.deps" && env["CLAUDE_CODE_TMPDIR"] == "/w.deps/tmp" && env["GOMODCACHE"] == "/.cache/go-mod")  // #67
    let gx = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("gx-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: gx) }
    let c = try #require(Worktree.claudeEnv(root: URL(fileURLWithPath: "/w"), maven: URL(fileURLWithPath: "/m"), gitexec: gx, agent: "claude", path: ":/usr/bin::.:bin:./x:/bin:"))
    #expect(c["GIT_DIR"] == nil && c["GIT_WORK_TREE"] == nil && c["GIT_COMMON_DIR"] == nil && c["MINIGW_CLONE"] == "/w" && c["GIT_CEILING_DIRECTORIES"] == "/")  // #40
    #expect(c["PATH"] == gx.path + "/bin:/usr/bin:/bin" && c["GIT_CONFIG_VALUE_6"] == "Claude (Mini Gateway)")
    #expect(FileManager.default.isExecutableFile(atPath: gx.path + "/bin/git"))
}

/// #40: Claude CLI v klonu commita v ugnezdenem klonu z git -C (ovoj na PATH); commit pride na vejo ugnezdenega
/// repozitorija z avtorjem agenta, vrhnja veja ga nima.
@Test func worktreeClaudeCommitsInNestedClone() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let lib = client.appendingPathComponent("lib")
    try file(lib, "f.txt", "lib\n"); g(lib, "init", "-q", "-b", "main"); g(lib, "add", "-A"); g(lib, "commit", "-qm", "lib")
    let fake = root.appendingPathComponent("claude")
    try Data("""
        #!/bin/sh
        [ "$1" = --version ] && { echo '2.1.0 (Claude Code)'; exit 0; }
        cat > /dev/null
        if [ -d lib ]; then  # ne poskus claude -p x zunaj klona
          echo x > lib/n.txt && git -C lib add -A && git -C lib commit -qm "agent in lib" || exit 9
          echo y > top.txt && git add -A && git commit -qm "agent top" || exit 9
        fi
        echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
        echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'

        """.utf8).write(to: fake)
    chmod(fake.path, 0o755)
    var c = RouterConfig()
    c.claudeCLI.enabled = true; c.claudeCLI.executable = fake.path
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let hub = Router.shares, session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    var r = request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": session, "x-claude-mini-cwd": client.path],
                    body: #"{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}"#)
    r.device = Noise.Device(id: "dev", name: "dev")
    let w = FakeWriter()
    await router.handle(r, writer: w)
    #expect(w.status == 200 && w.text.contains("done"), "\(w.text)")
    let br = "minigw/claude-\(String(Sync.folderID(session).prefix(8)))-0"
    #expect(g(lib, "log", "-1", "--format=%an <%ae> %s", br) == "Claude (Mini Gateway) <claude@minigw.invalid> agent in lib")
    let top = g(client, "ls-tree", "-r", "--name-only", br)
    #expect(top.contains("top.txt") && !top.contains("lib/") && g(client, "log", "-1", "--format=%an", br) == "Claude (Mini Gateway)")
}

/// #40 (kot Go TestCodexTreeProfile): Codex v klonu dobi profil dovoljenj: commit v vrhnjem in ugnezdenem klonu gre,
/// config, hooks, info in commondir .git ne; s pravim codex (če je nameščen) še zares.
@Test func codexTreeProfileCommitsButProtectsGitConfig() throws {
    let fm = FileManager.default
    let wt = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("cx-" + UUID().uuidString)
    defer { try? fm.removeItem(at: wt) }
    let gits = [wt.path + "/.git", wt.path + "/lib/.git"]
    let a = Codex.args(cwd: wt, extraDirs: ["/m"], agent: true, output: wt, write: true, gits: gits, path: "/gw/bin:/usr/bin", deny: ["/cfg"]).joined(separator: " ")
    #expect(a.contains(#""/cfg"="none""#) && a.contains("shell_environment_policy.set.GIT_CEILING_DIRECTORIES=\"" + wt.deletingLastPathComponent().path + "\""))
    for want in [#"default_permissions="minigw""#, "\"\(gits[1])\"=\"write\"", "\"\(gits[1])/config\"=\"read\"", "\"\(gits[0])/hooks\"=\"read\"",
                 #""/m"="write""#, #"shell_environment_policy.exclude=["GIT_DIR","GIT_WORK_TREE","GIT_COMMON_DIR"]"#, #"shell_environment_policy.set.PATH="/gw/bin:/usr/bin""#] {
        #expect(a.contains(want), "\(want)")
    }
    #expect(!a.contains("workspace-write") && !a.contains("--sandbox"))
    let cenv = Worktree.claudeEnv(root: wt, maven: wt, gitexec: wt.appendingPathComponent("gx"), agent: "codex")
    #expect(cenv?["GIT_DIR"] == "/dev/null" && cenv?["GIT_COMMON_DIR"] == "/dev/null" && cenv?["MINIGW_CLONE"] == wt.path && cenv?["PATH"] == nil)  // M2, N1
    guard let exe = ["/opt/homebrew/bin/codex", "/usr/local/bin/codex"].first(where: { fm.isExecutableFile(atPath: $0) }) else { return }
    for d in gits.map({ String($0.dropLast(5)) }) { try fm.createDirectory(atPath: d, withIntermediateDirectories: true); g(URL(fileURLWithPath: d), "init", "-q") }
    let prof = Codex.treeProfile(writable: [wt.path], gits: gits, network: false, path: "/usr/bin:/bin")
    let p = Process(), out = Pipe()
    p.executableURL = URL(fileURLWithPath: exe); p.currentDirectoryURL = wt; p.standardOutput = out; p.standardError = out
    p.arguments = ["sandbox"] + prof.prefix(6) + ["-P", "minigw", "--", "sh", "-c",
        "git -C lib -c user.name=c -c user.email=c@x commit -q --allow-empty -m sub && git -c user.name=c -c user.email=c@x commit -q --allow-empty -m top && echo COMMITS; "
        + "for f in .git/config lib/.git/config lib/.git/hooks/x lib/.git/info/attributes lib/.git/commondir; do (echo x >> $f) 2>/dev/null && echo WROTE-$f; done; echo end"]
    try p.run(); p.waitUntilExit()
    let s = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
    #expect(s.contains("COMMITS") && !s.contains("WROTE-") && s.contains("end"), "\(s)")
}

/// #40: klon se znova uporabi le, če je vsak ugnezdeni klon še na svoji poti (pot, zamenjana s povezavo na isti .git,
/// prestane gitIntact): sicer nov klon.
@Test func worktreeReuseNeedsNestedIntact() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let lib = client.appendingPathComponent("lib")
    try file(lib, "f.txt", "lib\n"); g(lib, "init", "-q", "-b", "main"); g(lib, "add", "-A"); g(lib, "commit", "-qm", "lib")
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    let fm = FileManager.default
    var first: URL?
    await hub.finish(run: try await agentRun(hub, session) { first = $0 })
    let wt = try #require(first)
    let away = root.appendingPathComponent("lib-away")  // zunaj klona: nestedProblem je ne vidi kot tuj repozitorij
    try fm.moveItem(at: wt.appendingPathComponent("lib"), to: away)
    try fm.createSymbolicLink(atPath: wt.appendingPathComponent("lib").path, withDestinationPath: away.path)
    var linked = true
    await hub.finish(run: try await agentRun(hub, session) { dir in
        linked = (try? fm.destinationOfSymbolicLink(atPath: dir.appendingPathComponent("lib").path)) != nil
    })
    #expect(!linked, "clone with a moved nested clone was reused")
}

/// #40 N1: proces agenta, ki je zapustil klon (cd /), s okoljem Claude CLI ali Codex (brez GIT_WORK_TREE) killProcesses
/// ustavi pred finalize (znak MINIGW_CLONE).
@Test func killProcessesFindsAgentsWithoutGitWorkTree() throws {
    let fm = FileManager.default
    let wt = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("kp-" + UUID().uuidString)
    defer { try? fm.removeItem(at: wt) }
    try fm.createDirectory(at: wt, withIntermediateDirectories: true)
    // Ne sistemski program: macOS sistemskim (/bin/sleep) okolja v KERN_PROCARGS2 ne pokaže, agentom (node, codex) ga.
    let src = wt.appendingPathComponent("s.c"), exe = wt.appendingPathComponent("s")
    try Data("#include <unistd.h>\nint main(void){sleep(5);return 0;}\n".utf8).write(to: src)
    let cc = Process(); cc.executableURL = URL(fileURLWithPath: "/usr/bin/xcrun"); cc.arguments = ["clang", "-o", exe.path, src.path]
    try cc.run(); cc.waitUntilExit()
    try #require(cc.terminationStatus == 0)
    for agent in ["claude", "codex"] {
        let env = try #require(Worktree.claudeEnv(root: wt, maven: wt, gitexec: wt.appendingPathComponent("gx"), agent: agent))
        #expect(env["GIT_WORK_TREE"] == nil || agent == "codex")
        let p = Process()
        p.executableURL = exe; p.currentDirectoryURL = URL(fileURLWithPath: "/")
        p.environment = ["MINIGW_CLONE": env["MINIGW_CLONE"] ?? "", "PATH": "/usr/bin:/bin"]
        try p.run()
        Worktree.killProcesses(in: wt)
        p.waitUntilExit()
        #expect(p.terminationReason == .uncaughtSignal && p.terminationStatus == SIGKILL, "\(agent)")
    }
}

/// #61 (kot Go TestForeignGit): .git, ki ga je agent naredil v začetni mapi harnessa ali mapi nad njo (datoteka gitdir:),
/// zavrne zagon; .git v prezrtem build/_deps zunaj te poti in registriran ugnezdeni klon kot začetna mapa ne; povezavi ne sledi.
@Test func foreignGitRefusesPlantedGitOnCwdPath() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("fg-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let wt = root.appendingPathComponent("wt"), out = root.appendingPathComponent("out")
    for d in [".git/info", "lib/.git", "lib/src", "app/src", "build/_deps/foo/.git"] {
        try FileManager.default.createDirectory(at: wt.appendingPathComponent(d), withIntermediateDirectories: true)
    }
    try FileManager.default.createDirectory(at: out.appendingPathComponent("x/.git"), withIntermediateDirectories: true)
    try file(wt, ".gitignore", "build/\n")
    let lib = ShareHub.Nested(path: "lib", repo: root.appendingPathComponent("m.git"), base: "", wt: wt)
    let cwd = wt.appendingPathComponent("app/src")
    #expect(Worktree.foreignGit(wt, cwd: cwd, nested: [lib]) == nil)
    #expect(Worktree.foreignGit(wt, cwd: wt, nested: [lib]) == nil)
    #expect(Worktree.foreignGit(wt, cwd: wt.appendingPathComponent("lib/src"), nested: [lib]) == nil)
    #expect(Worktree.foreignGit(wt, cwd: wt.appendingPathComponent("lib/src"))?.contains("a .git appeared at lib/.git") == true)
    try file(wt, "app/.git", "gitdir: \(out.path)/x/.git\n")
    #expect(Worktree.foreignGit(wt, cwd: cwd, nested: [lib]) == "The agent's clone was changed (a .git appeared at app/.git): start a new conversation")
    try FileManager.default.removeItem(at: wt.appendingPathComponent("app"))
    try FileManager.default.createSymbolicLink(at: wt.appendingPathComponent("app"), withDestinationURL: out)
    #expect(Worktree.foreignGit(wt, cwd: wt.appendingPathComponent("app/x"), nested: [lib])?.contains("a link at app") == true)
}

/// #61 M1: git harnessa (po absolutni poti, okolje claudeEnv brez GIT_DIR) v mapi z golim repozitorijem agenta ga ne uporabi
/// (safe.bareRepository=explicit); brez tega para ga (test ponovi napad).
@Test func harnessGitRefusesPlantedBareRepo() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("bare-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: root) }
    let wt = root.appendingPathComponent("wt")
    try FileManager.default.createDirectory(at: wt, withIntermediateDirectories: true)
    g(wt, "init", "-q", "-b", "main"); g(wt, "init", "-q", "--bare", "app")
    let env = try #require(Worktree.claudeEnv(root: wt, maven: root.appendingPathComponent("m"), gitexec: root.appendingPathComponent("gitexec"), agent: "claude"))
    func gitDir(_ env: [String: String]) -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: Worktree.git)
        p.arguments = ["rev-parse", "--git-dir"]
        p.currentDirectoryURL = wt.appendingPathComponent("app")
        p.environment = ProcessInfo.processInfo.environment.filter { !$0.key.hasPrefix("GIT_") }.merging(env) { $1 }
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try? p.run(); p.waitUntilExit()
        return p.terminationStatus
    }
    #expect(env["GIT_CONFIG_KEY_8"] == "safe.bareRepository" && env["GIT_DIR"] == nil)
    #expect(gitDir(env.merging(["GIT_CONFIG_COUNT": "8"]) { $1 }) == 0, "the test does not reproduce the bare repository")
    #expect(gitDir(env) != 0, "harness git used the planted bare repository")
}

/// #61 (kot Go TestServeWorktreePlantedGitOnCwdPath): seja v prezrti podmapi app; agent v 1. krogu naredi app/.git,
/// 2. krog v istem klonu je zavrnjen z natančnim sporočilom.
@Test func worktreePlantedGitOnCwdPathRefusedNextTurn() async throws {
    let (root, client, base) = try fixture()
    defer { cleanup(root) }
    try file(client, ".gitignore", "app/\n"); g(client, "add", ".gitignore"); g(client, "commit", "-qm", "ignore app")
    try file(client, "app/x.txt", "x\n")
    let tree = ShareHub.Tree(repo: base.repo, worktrees: base.worktrees, sub: "app")
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    let id = try await agentRun(hub, session) { dir in try file(dir, "app/.git", "gitdir: /nowhere\n") }
    await hub.finish(run: id)
    let why = await ShareHub.$run.withValue(UUID().uuidString) {
        await ShareHub.$agent.withValue("claude") { () -> String? in
            await hub.mount(session: session, cwd: nil, base: "http://127.0.0.1:1", device: "dev") == nil ? await hub.takeRefusal()?.message : nil
        }
    }
    #expect(why == "The agent's clone was changed (a .git appeared at app/.git): start a new conversation")
}

/// #66: --continue v načinu worktree: drugi zagon (nova seja, isti pogovor) začne na veji prvega, tudi ko se je mapa
/// spremenila (z vrstico); iz mape le brez veje in pri ugnezdenih. Ključ po mapah, izbirnik, fork, stara oblika zapisa.
@Test func worktreeContinueConversation() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let hub = ShareHub(), head = g(client, "rev-parse", "HEAD"), notes = OSAllocatedUnfairLock<[String]>(initialState: [])
    func turn(_ session: String, cont: Bool, _ work: @escaping (URL) throws -> Void) async throws -> String {
        let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
        let conv = Worktree.conversation(top: client.path, rel: ".", session: session, parent: "", cont: cont, picker: false, fork: false)
        let loop = fakeClient(hub, key: key, top: client, notes: notes, conv: conv)
        defer { loop.cancel() }
        let id = try await agentRun(hub, session, work: work)
        await hub.finish(run: id)
        return "minigw/claude-\(Sync.folderID(session).prefix(8))-0"
    }
    func has(_ dir: URL, _ f: String) -> Bool { FileManager.default.fileExists(atPath: dir.appendingPathComponent(f).path) }
    let b1 = try await turn("s1", cont: false) { try file($0, "agent.txt", "agent\n") }
    let b2 = try await turn("s2", cont: true) { dir in
        #expect(has(dir, "agent.txt"))
        try file(dir, "b.txt", "b\n")
    }
    #expect(b1 != b2 && g(client, "show", "\(b2):agent.txt") == "agent" && g(client, "show", "\(b2):b.txt") == "b")
    let (st, _) = Worktree.convLoad(client.path)
    #expect(st.last == [".": "s1"] && st.convs["s1"]?.branch == b2 && st.convs["s1"]?.head == head)
    // Začetek obsega je stanje uporabnika ob prvem zagonu (posnetek z neshranjenimi spremembami), prednik obeh vej.
    let base = try #require(st.convs["s1"]?.base)
    #expect(g(client, "show", "\(base):new.txt") == "untracked" && g(client, "rev-parse", "\(b1)~1") == base)
    #expect(notes.withLock { $0 }.contains(Worktree.continuedLine(branch: b2, base: base)))
    #expect(Worktree.continuedLine(branch: b2, base: base).contains("(\(base) = your state when this conversation started; this branch includes the earlier turns)"))
    #expect(g(client, "rev-parse", "HEAD") == head)

    func snap() throws -> String { try #require(Worktree.snapshot(top: client.path)) }
    func check(_ tip: String?, _ msg: String?) throws {
        let c = try Worktree.continueFrom(top: client.path, conv: "s1", gateway: "https://gw-a:8443", snap: snap(), nested: false)
        #expect(c.tip == tip && c.message == msg)
    }
    func changed(_ b: String) -> String { "worktree: continuing from \(b); changes in your folder since the last turn are not in the agent's copy" }
    try check(g(client, "rev-parse", b2), nil)
    #expect(try Worktree.continueFrom(top: client.path, conv: "s1", gateway: "https://gw-a:8443", snap: snap(), nested: true).message
        == "worktree: starting from your folder, not from \(b2): nested git repositories always start from your folder")
    try file(client, "mine.txt", "m\n")
    try check(g(client, "rev-parse", b2), changed(b2))
    let b3 = try await turn("s3", cont: true) { dir in
        #expect(has(dir, "b.txt") && !has(dir, "mine.txt"))
        try file(dir, "c.txt", "c\n")
    }
    #expect(notes.withLock { $0 }.contains(changed(b2)))
    try check(g(client, "rev-parse", b3), nil)
    g(client, "add", "-A"); g(client, "commit", "-qm", "mine")
    try check(g(client, "rev-parse", b3), changed(b3))
    let b4 = try await turn("s4", cont: true) { dir in
        #expect(has(dir, "c.txt"))
        try file(dir, "d.txt", "d\n")
    }
    g(client, "branch", "-D", b4)
    try check(nil, "worktree: starting from your folder, not from \(b4): the branch no longer exists")
    let b5 = try await turn("s5", cont: true) { dir in
        #expect(!has(dir, "agent.txt") && has(dir, "mine.txt"))
        try file(dir, "e.txt", "e\n")
    }
    // L-B: drug prehod začne iz mape.
    #expect(try Worktree.continueFrom(top: client.path, conv: "s1", gateway: "https://gw-b:8443", snap: snap(), nested: false).message
        == "worktree: starting from your folder, not from \(b5): the conversation ran on another gateway")
    // L-A: veja, že v HEAD, in novejši commit uporabnika: iz mape (agent vidi novejši commit), brez vrstice.
    g(client, "add", "-A"); g(client, "commit", "-qm", "rest")  // fixture: neshranjene spremembe uporabnika
    g(client, "merge", "-q", "--no-edit", b5)
    try file(client, "newer.txt", "n\n"); g(client, "add", "-A"); g(client, "commit", "-qm", "newer")
    try check(nil, nil)
    let b6 = try await turn("s5m", cont: true) { dir in
        #expect(has(dir, "newer.txt") && has(dir, "e.txt"))
        try file(dir, "f.txt", "f\n")
    }
    #expect(Worktree.convLoad(client.path).0.convs["s1"]?.branch == b6 && Worktree.convLoad(client.path).0.convs["s1"]?.base == g(client, "rev-parse", "HEAD"))

    // Ključ: po mapah (kot claude -c), izbirnik ne nadaljuje in ne zapiše, fork nadaljuje starša pod novo sejo.
    func conv(_ s: String, rel: String = ".", parent: String = "", cont: Bool = false, picker: Bool = false, fork: Bool = false) -> Worktree.Conv {
        Worktree.conversation(top: client.path, rel: rel, session: s, parent: parent, cont: cont, picker: picker, fork: fork)
    }
    #expect(conv("s6") == Worktree.Conv(from: "", to: "s6") && conv("x", parent: "s1") == Worktree.Conv(from: "s1", to: "s1")
        && conv("s7", cont: true) == Worktree.Conv(from: "s1", to: "s1"))
    #expect(conv("s8", rel: "sub", cont: true) == Worktree.Conv(from: "", to: "s8") && conv("s9", cont: true).from == "s1")
    #expect(conv("s10", picker: true) == Worktree.Conv() && conv("s11", cont: true).from == "s1")
    #expect(conv("f1", parent: "s1", fork: true) == Worktree.Conv(from: "s1", to: "f1") && conv("s12", cont: true).from == "f1")
    let (_, p) = Worktree.convLoad(client.path)
    #expect(((try? FileManager.default.attributesOfItem(atPath: p))?[.posixPermissions] as? Int) == 0o600)
    #expect(Worktree.relCwd(top: client.path, cwd: client.appendingPathComponent("sub").path) == "." && Worktree.relCwd(top: client.path, cwd: client.path) == ".")
    try FileManager.default.createDirectory(at: client.appendingPathComponent("sub"), withIntermediateDirectories: true)
    #expect(Worktree.relCwd(top: client.path, cwd: client.appendingPathComponent("sub").path) == "sub")
    // Starejša oblika (»last« kot niz): neznano, zapisi ostanejo.
    let old = try String(contentsOfFile: p, encoding: .utf8)
    let n = Worktree.convLoad(client.path).0.convs.count
    try old.replacingOccurrences(of: #""last":{"#, with: #""last":"s1","x":{"#).write(toFile: p, atomically: true, encoding: .utf8)
    #expect(Worktree.convLoad(client.path).0.last.isEmpty && Worktree.convLoad(client.path).0.convs.count == n && conv("s13", cont: true).from == "")
}

/// #66: odločitev enkrat na zagon: drugi WORKTREE-PUSH istega zagona začne iz istega vrha, tudi ko je vmes prišla nova
/// veja; naslednji zagon nadaljuje iz zadnje prevzete veje.
@Test func worktreeContinueDecidedOncePerRun() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let hub = ShareHub()
    func session(_ s: String, cont: Bool) async throws -> (String, Task<Void, Never>) {
        let key = try #require(await hub.register(session: s, cwd: client.path, key: nil, device: "dev", tree: tree))
        return (s, fakeClient(hub, key: key, top: client, conv: Worktree.conversation(top: client.path, rel: ".", session: s, parent: "", cont: cont, picker: false, fork: false)))
    }
    let (s1, l1) = try await session("o1", cont: false)
    let a = try await agentRun(hub, s1) { try file($0, "one.txt", "1\n") }
    await hub.finish(run: a); l1.cancel()
    let b1 = "minigw/claude-\(Sync.folderID(s1).prefix(8))-0"
    // Zagon 2: agent A konča (nova veja), agent B (drug agent, svoj klon) začne pozneje v istem zagonu: iz istega vrha kot A.
    let (s2, l2) = try await session("o2", cont: true)
    defer { l2.cancel() }
    let ra = UUID().uuidString
    let dirA = try await ShareHub.$run.withValue(ra) { try await ShareHub.$agent.withValue("claude") { try #require(await hub.mount(session: s2, cwd: nil, base: "", device: "dev")) } }
    let rb = UUID().uuidString
    let dirB = try await ShareHub.$run.withValue(rb) { try await ShareHub.$agent.withValue("codex") { () async throws -> URL in
        try file(dirA, "two.txt", "2\n")
        await ShareHub.$run.withValue(ra) { await hub.syncBack(session: s2) }
        await hub.finish(run: ra)  // veja A je prevzeta pred začetkom B
        return try #require(await hub.mount(session: s2, cwd: nil, base: "", device: "dev"))
    } }
    let short = Sync.folderID(s2).prefix(8)
    #expect(g(client, "show", "minigw/claude-\(short)-0:two.txt") == "2")
    #expect(FileManager.default.fileExists(atPath: dirB.appendingPathComponent("one.txt").path))
    #expect(!FileManager.default.fileExists(atPath: dirB.appendingPathComponent("two.txt").path))
    try file(dirB, "three.txt", "3\n")
    await ShareHub.$run.withValue(rb) { await hub.syncBack(session: s2) }
    await hub.finish(run: rb)
    #expect(g(client, "show", "minigw/codex-\(short)-0:three.txt") == "3" && g(client, "show", "minigw/codex-\(short)-0:one.txt") == "1")
    #expect(Worktree.convLoad(client.path).0.convs["o1"]?.branch == "minigw/codex-\(short)-0" && b1 != "minigw/codex-\(short)-0")
}

/// #67 (kot Go TestPathWrapDeps): git init/clone agenta le v $MINIGW_DEPS in le z oznako peskovnika (SANDBOX_RUNTIME ali
/// MINIGW_SANDBOXED); drugje zavrnitev z namigom, klon nespremenjen; brez oznake (harness) v $MINIGW_DEPS git klona.
@Test func pathWrapDepsRealGit() throws {
    let fm = FileManager.default
    let root = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("pd-" + UUID().uuidString)
    defer { try? fm.removeItem(at: root) }
    let wt = root.appendingPathComponent("worktrees/d/id/claude-12345678-0"), src = root.appendingPathComponent("src")
    for d in [wt, src] {
        try fm.createDirectory(at: d, withIntermediateDirectories: true)
        g(d, "init", "-q", "-b", "main"); g(d, "commit", "-q", "--allow-empty", "-m", "c")
    }
    let deps = Worktree.deps(wt), head = g(wt, "rev-parse", "HEAD")
    try fm.createDirectory(at: deps, withIntermediateDirectories: true)
    let env = try #require(Worktree.claudeEnv(root: wt, maven: root.appendingPathComponent("m"), gitexec: root.appendingPathComponent("gitexec"), agent: "claude"))
    #expect(env["MINIGW_DEPS"] == deps.path)
    func run(_ cwd: URL, _ extra: [String: String], _ args: String...) throws -> (String, Int32) {
        let p = Process(), out = Pipe()
        p.executableURL = root.appendingPathComponent("gitexec/bin/git"); p.arguments = args; p.currentDirectoryURL = cwd
        p.standardOutput = out; p.standardError = out
        p.environment = ProcessInfo.processInfo.environment.filter { !$0.key.hasPrefix("GIT_") && !Worktree.sandboxMarkers.contains($0.key) }
            .merging(env) { $1 }.merging(extra) { $1 }
        try p.run(); p.waitUntilExit()
        return (String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines), p.terminationStatus)
    }
    let hint = "minigw: create repositories only under $MINIGW_DEPS (\(deps.path))"
    for mark in ["SANDBOX_RUNTIME", "MINIGW_SANDBOXED"] {
        try? fm.removeItem(at: deps); try fm.createDirectory(at: deps, withIntermediateDirectories: true)
        let sb = [mark: "1"]
        #expect(try run(wt, sb, "init", "-q", deps.path + "/x").1 == 0, "\(mark)")
        #expect(!fm.fileExists(atPath: deps.path + "/x/.git/hooks"), "templates copied")
        #expect(try run(deps, sb, "clone", "-q", src.path, "y").1 == 0, "\(mark)")
        let y = deps.appendingPathComponent("y")
        try file(y, "f", "f\n")
        #expect(try run(y, sb, "add", "f").1 == 0 && run(y, sb, "commit", "-qm", "f").1 == 0)
        #expect(try run(y, sb, "rev-parse", "--absolute-git-dir").0 == y.path + "/.git")
        for args in [["init", "-q", "build/_deps/foo"], ["clone", "-q", src.path, "build/_deps/bar"], ["init", "-q", root.path + "/z"]] {
            let (out, code) = args.count == 3 ? try run(wt, sb, args[0], args[1], args[2]) : try run(wt, sb, args[0], args[1], args[2], args[3])
            #expect(code == 128 && out.hasPrefix(hint), "\(mark) \(args): \(out)")
        }
        let (out, code) = try run(deps, sb.merging(["MINIGW_NETWORK_OFF": "Network for builds"]) { $1 }, "clone", "-q", "https://example.invalid/a.git")
        #expect(code == 128 && out.contains("(Network for builds)"), "\(out)")
    }
    #expect(try run(deps.appendingPathComponent("y"), [:], "rev-parse", "--absolute-git-dir").0 == wt.path + "/.git")  // harness: pravilo klona
    #expect(try run(deps, [:], "init", "-q", "h").1 == 128)
    #expect(try run(deps, ["SANDBOX_RUNTIME": "1", "MINIGW_DEPS": ""], "init", "-q", "h").0 == "minigw: this run cannot write (read-only)")
    #expect(g(wt, "rev-parse", "HEAD") == head && g(wt, "status", "--porcelain").isEmpty && !fm.fileExists(atPath: wt.path + "/build"))
}

/// #67: $MINIGW_DEPS nastane s klonom in ostane med zagoni pogovora (veja ga ne vsebuje), gre s klonom ob koncu seje;
/// ohranjen klon (kept) ga nima.
@Test func worktreeDepsLifetime() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    var deps: URL?
    let first = try await agentRun(hub, session) { dir in
        deps = Worktree.deps(dir)
        #expect(FileManager.default.fileExists(atPath: deps!.path + "/tmp") && FileManager.default.fileExists(atPath: Worktree.cache(dir).path))
        try file(deps!, "x/a.txt", "dep\n")
        try file(dir, "one.txt", "1\n")
    }
    await hub.finish(run: first)
    let second = try await agentRun(hub, session) { dir in
        #expect(Worktree.deps(dir) == deps && FileManager.default.fileExists(atPath: deps!.path + "/x/a.txt"), "\(dir.path) \(deps!.path) \((try? FileManager.default.contentsOfDirectory(atPath: deps!.path)) ?? [])")
    }
    let branch = "minigw/claude-\(String(Sync.folderID(session).prefix(8)))-0"
    #expect(g(client, "ls-tree", "-r", "--name-only", branch) == "a.txt\nnew.txt\none.txt")  // brez $MINIGW_DEPS
    await hub.finish(run: second)
    await hub.unregister(key: key)
    for _ in 0..<100 where FileManager.default.fileExists(atPath: deps!.path) { try await Task.sleep(for: .milliseconds(50)) }
    #expect(!FileManager.default.fileExists(atPath: deps!.path))
    // kept: premik klona, odvisnosti ne ostanejo
    let wt = root.appendingPathComponent("k/c"), kept = root.appendingPathComponent("k/c-kept-1")
    try file(wt, "f", "f\n"); try file(Worktree.deps(wt), "x", "x\n")
    ShareHub.keepClone(wt, kept)
    #expect(!FileManager.default.fileExists(atPath: Worktree.deps(wt).path))
    for _ in 0..<100 where FileManager.default.fileExists(atPath: Worktree.deps(kept).path) { try await Task.sleep(for: .milliseconds(50)) }
    #expect(!FileManager.default.fileExists(atPath: Worktree.deps(kept).path) && FileManager.default.fileExists(atPath: kept.path + "/f"))
}

/// #67: kratka povezava za CLAUDE_CODE_TMPDIR (≤ 44 bajtov s /claude-<uid>) kaže v $MINIGW_DEPS/tmp, zapis gre pod Scratch;
/// tuja ali odprta mapa mgw-<uid> (ne 0700) je ne dobi; removeClone jo odstrani.
@Test func tmpLinkShortIntoScratch() throws {
    let fm = FileManager.default, base = "/tmp/t" + UUID().uuidString.prefix(4)
    defer { try? fm.removeItem(atPath: base) }
    let root = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("tl-" + UUID().uuidString + "/worktrees/d/i/claude-12345678-0")
    defer { try? fm.removeItem(at: root.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()) }
    try fm.createDirectory(at: root, withIntermediateDirectories: true)
    try fm.createDirectory(atPath: base, withIntermediateDirectories: true)
    let link = try #require(Worktree.makeTmpLink(root, base: base))
    #expect((link + "/claude-\(getuid())").utf8.count <= 44)
    #expect(try fm.destinationOfSymbolicLink(atPath: link) == Worktree.deps(root).path + "/tmp")
    try Data("x".utf8).write(to: URL(fileURLWithPath: link + "/probe"))
    #expect(fm.fileExists(atPath: Worktree.deps(root).path + "/tmp/probe"))
    #expect(Worktree.makeTmpLink(root, base: base) == link)  // znova: zamenjava naenkrat
    chmod(base + "/mgw-\(getuid())", 0o755)
    #expect(Worktree.makeTmpLink(root, base: base) == nil)
    chmod(base + "/mgw-\(getuid())", 0o700)
    let real = try #require(Worktree.makeTmpLink(root))  // privzeta /private/tmp/mgw-<uid>
    var st = stat()
    #expect(lstat(real, &st) == 0)
    ShareHub.removeClone(root)
    #expect(lstat(real, &st) != 0)
}

/// #67 H1 (kot Go TestGrantDirsReplacesPlantedLinks): povezava ali datoteka namesto mape za pisanje se odstrani in naredi
/// znova (0700), cilj ostane nedotaknjen; v profilu Copilot le dobesedne poti in prepoved zamenjave.
@Test func grantDirsReplacesPlantedLinks() throws {
    let fm = FileManager.default
    let root = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("gd-" + UUID().uuidString)
    defer { try? fm.removeItem(at: root) }
    let wt = root.appendingPathComponent("clones/c"), home = root.appendingPathComponent("home")
    try fm.createDirectory(at: wt, withIntermediateDirectories: true)
    try file(home, "secret", "s")
    try fm.createSymbolicLink(at: Worktree.deps(wt), withDestinationURL: home)
    try fm.createSymbolicLink(at: Worktree.cache(wt), withDestinationURL: home)
    try file(root, "clones/.maven", "x")
    let dirs = try #require(Worktree.grantDirs(root: wt))
    for d in dirs {
        var st = stat()
        #expect(lstat(d, &st) == 0 && st.st_mode & S_IFMT == S_IFDIR && st.st_mode & 0o777 == 0o700, "\(d)")
    }
    #expect(try String(contentsOf: home.appendingPathComponent("secret"), encoding: .utf8) == "s")
}

/// #67: navodilo z oznako peskovnika in brez; imena nastavitev omrežja po agentu (Mac).
@Test func depsNoteAndNetworkOff() {
    #expect(Router.depsNote("/d").contains("$MINIGW_DEPS (/d), kept for this conversation; git init and git clone work only there."))
    #expect(Router.depsNote("/d", sandboxed: false).contains("git init and git clone are not available on this gateway (no sandbox)."))
    var c = RouterConfig()
    c.worktreeNetwork = false; c.worktreeCodexNetwork = false; c.codex.network = true; c.copilot.network = true
    #expect(Worktree.networkOff("claude", c) == "Settings → General → Worktree mode → Network for builds")
    #expect(Worktree.networkOff("codex", c) == "Settings → General → Worktree mode → Network for Codex builds")
    #expect(Worktree.networkOff("copilot", c) == "Settings → General → Worktree mode → Network for Codex builds")
    c.codex.network = false
    #expect(Worktree.networkOff("codex", c) == "Settings → Codex → Allow network access for commands")  // kot Go
    c.worktreeNetwork = true; c.worktreeCodexNetwork = true; c.codex.network = true
    #expect(Worktree.networkOff("claude", c) == nil && Worktree.networkOff("codex", c) == nil && Worktree.networkOff("copilot", c) == nil)
}

/// #67 (Air): claude -p v klonu ima ovoj git (gitexec) berljiv v peskovniku tudi brez »May ask other models« (sicer git z
/// drugega PATH in neberljiv exec-path: remote-https, CMake GIT_EXECUTABLE); navodilo da recepte z dobesedno potjo.
@Test func worktreeClaudeReadsGitWrapperWithoutAskOthers() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let fake = root.appendingPathComponent("claude"), dump = root.appendingPathComponent("args")
    try Data("""
        #!/bin/sh
        [ "$1" = --version ] && { echo '2.1.0 (Claude Code)'; exit 0; }
        printf '%s\\n' "$@" > '\(dump.path)'
        [ -n "$MINIGW_KEEP_ARGS" ] && { printf '%s\\0' "$@" > "$MINIGW_KEEP_ARGS.args"; env -0 > "$MINIGW_KEEP_ARGS.env"; }
        cat > /dev/null
        echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
        echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'

        """.utf8).write(to: fake)
    chmod(fake.path, 0o755)
    var c = RouterConfig()
    c.claudeCLI.enabled = true; c.claudeCLI.executable = fake.path; c.claudeCLI.askOthers = false
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    let hub = Router.shares, session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    var r = request("POST", "/v1/messages", headers: ["Authorization": "Bearer tok", "x-claude-mini-session": session, "x-claude-mini-cwd": client.path],
                    body: #"{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}"#)
    r.device = Noise.Device(id: "dev", name: "dev")
    let w = FakeWriter()
    await router.handle(r, writer: w)
    #expect(w.status == 200, "\(w.text)")
    let args = try String(contentsOf: dump, encoding: .utf8).split(separator: "\n").map(String.init)
    let settings = try #require(args.firstIndex(of: "--settings").map { args[$0 + 1] })
    let fs = try #require(((JSONSerialization.jsonObject(with: Data(settings.utf8)) as? [String: Any])?["sandbox"] as? [String: Any])?["filesystem"] as? [String: Any])
    #expect((fs["allowRead"] as? [String] ?? []).contains(c.scratch("gitexec").path), "\(fs)")
    let note = args.joined(separator: "\n")
    #expect(note.contains(#"-DFETCHCONTENT_BASE_DIR=""#) && !note.contains(#"="$MINIGW_DEPS/cmake""#) && note.contains(".deps/cmake\""))
}

// #71: varni git pripne pager, zunanji diff, ssh in pomočnik za poverilnice (agent piše .git/config klona).
@Test func gitGuardrailsPinPagerDiffSshCredential() {
    let g = Worktree.guardrails.joined(separator: " ")
    for pin in ["core.pager=cat", "diff.external=", "core.sshCommand=", "credential.helper=", "core.hooksPath=/dev/null"] {
        #expect(g.contains("-c " + pin), "\(pin)")
    }
}

/// #73: drevo agenta ni ustavljeno (abandon): klon worktree gre ob koncu teka ven; naslednji tek dobi nov klon (kot po H2)
/// brez tega, kar je pisal opuščeni.
@Test func worktreeAbandonedRunDropsClone() async throws {
    let (root, client, tree) = try fixture()
    defer { cleanup(root) }
    let hub = ShareHub(), session = UUID().uuidString
    let key = try #require(await hub.register(session: session, cwd: client.path, key: nil, device: "dev", tree: tree))
    let loop = fakeClient(hub, key: key, top: client)
    defer { loop.cancel() }
    await hub.finish(run: try await agentRun(hub, session) { try file($0, "first.txt", "1\n") })
    let short = String(Sync.folderID(session).prefix(8))
    #expect(g(client, "show", "minigw/claude-\(short)-0:first.txt") == "1")
    var abandoned: URL?
    let id = UUID().uuidString
    await ShareHub.$run.withValue(id) {
        await ShareHub.$agent.withValue("claude") {
            abandoned = await hub.mount(session: session, cwd: nil, base: "http://127.0.0.1:1", device: "dev")
            try? file(abandoned!, "tainted.txt", "x\n")
            await hub.abandon()
        }
    }
    await hub.finish(run: id)
    #expect(!FileManager.default.fileExists(atPath: abandoned!.appendingPathComponent("tainted.txt").path))
    _ = try await agentRun(hub, session) { dir in
        #expect(!FileManager.default.fileExists(atPath: dir.appendingPathComponent("tainted.txt").path))  // nov klon
    }
}
