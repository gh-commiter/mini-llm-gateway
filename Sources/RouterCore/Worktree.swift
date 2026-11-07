import Foundation

/// --minigw-work=worktree (docs/plans/plan-remote-worktrees.md): agent dela na routerju v klonu git na svoji veji
/// `minigw/<agent>-<seja>-<n>`; rezultat pride k odjemalcu kot veja, nikoli v delovno mapo uporabnika.
///
/// Prenos (prek obstoječe povezave minigw, šifrirano; nikoli git daemon):
/// 1. WORKTREE-PUSH: minigw naredi posnetek stanja (HEAD + neshranjene spremembe z začasnim indeksom, brez spremembe
///    indeksa ali vej uporabnika) in pošlje git bundle brez objektov, ki jih zrcalo routerja že ima.
/// 2. Router uvozi bundle v golo zrcalo (scratch/repos), naredi `git clone --shared` (scratch/worktrees) na novi veji.
/// 3. Po agentu router neshranjene spremembe sam shrani v commit in pošlje bundle veje (WORKTREE-PULL); minigw ga
///    prevzame le kot ref veje (fsck, brez hookov, podmodulov in checkouta).
///
/// Agentov klon je v celoti agentov (tudi .git/config, hooks): router v njem nikoli ne zažene git z njegovim .git;
/// bere le datoteki HEAD in ref, objekte pa prek GIT_ALTERNATE_OBJECT_DIRECTORIES z nastavitvami zrcala.
public enum Worktree {
    static let git = "/usr/bin/git"

    /// git z izhodom; `safe`: brez sistemske in uporabniške nastavitve in brez hookov (router, prevzem pri odjemalcu).
    @discardableResult
    /// `hooks: false` (odjemalec): hooki uporabnika ne tečejo ob začasnem refu refs/minigw/push in posnetku.
    static func run(_ args: [String], cwd: String? = nil, env extra: [String: String] = [:], safe: Bool = true, hooks: Bool = true,
                    input: Data? = nil) -> (status: Int32, out: String) {
        let p = Process(), out = Pipe(), inp = Pipe()
        p.executableURL = URL(fileURLWithPath: git)
        let guardrails = ["-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "submodule.recurse=false",
                          "-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "maintenance.auto=false", "-c", "gc.auto=0"]
        p.arguments = (safe ? guardrails : hooks ? [] : ["-c", "core.hooksPath=/dev/null"]) + args
        if let cwd { p.currentDirectoryURL = URL(fileURLWithPath: cwd) }
        var env = ProcessInfo.processInfo.environment
        for k in env.keys where k.hasPrefix("GIT_") { env[k] = nil }
        if safe { env["GIT_CONFIG_NOSYSTEM"] = "1"; env["GIT_CONFIG_GLOBAL"] = "/dev/null" }
        env["GIT_TERMINAL_PROMPT"] = "0"
        p.environment = env.merging(extra) { $1 }
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        p.standardInput = input == nil ? FileHandle.nullDevice : inp
        guard (try? p.run()) != nil else { return (-1, "") }
        if let input { try? inp.fileHandleForWriting.write(contentsOf: input); try? inp.fileHandleForWriting.close() }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        return (p.terminationStatus, String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines))
    }

    public static func isSHA(_ s: String) -> Bool { s.count == 40 && s.allSatisfy { $0.isHexDigit && !$0.isUppercase } }

    /// Ime veje agenta (router ni zaupanja vreden: minigw zapiše le take refe).
    public static func validBranch(_ b: String) -> Bool { b.range(of: #"^minigw/[a-z]+-[0-9a-f]{8}-[0-9]+$"#, options: .regularExpression) != nil }

    // MARK: odjemalec (minigw)

    /// Koren repozitorija za cwd (git uporabnika, njegove nastavitve); nil zunaj repozitorija.
    public static func toplevel(_ cwd: String) -> String? {
        let r = run(["rev-parse", "--show-toplevel"], cwd: cwd, safe: false, hooks: false)
        return r.status == 0 && !r.out.isEmpty ? r.out : nil
    }

    /// Posnetek stanja: HEAD ali commit z neshranjenimi spremembami (sledene in nove, neprezrte), narejen z začasnim
    /// indeksom: indeks, veje in delovna mapa uporabnika ostanejo nespremenjeni. nil = brez HEAD (prazen repozitorij).
    public static func snapshot(top: String) -> String? {
        guard case let h = run(["rev-parse", "--verify", "-q", "HEAD^{commit}"], cwd: top, safe: false, hooks: false), h.status == 0, isSHA(h.out) else { return nil }
        let idx = FileManager.default.temporaryDirectory.appendingPathComponent("minigw-index-" + UUID().uuidString).path
        defer { unlink(idx) }
        let env = ["GIT_INDEX_FILE": idx]
        guard run(["read-tree", "HEAD"], cwd: top, env: env, safe: false, hooks: false).status == 0,
              run(["add", "-A"], cwd: top, env: env, safe: false, hooks: false).status == 0,
              case let t = run(["write-tree"], cwd: top, env: env, safe: false, hooks: false), t.status == 0, isSHA(t.out) else { return nil }
        if run(["rev-parse", "HEAD^{tree}"], cwd: top, safe: false, hooks: false).out == t.out { return h.out }
        let c = run(["-c", "user.name=minigw", "-c", "user.email=minigw@localhost", "commit-tree", t.out, "-p", h.out,
                     "-m", "minigw: your uncommitted changes"], cwd: top, safe: false, hooks: false)
        return c.status == 0 && isSHA(c.out) ? c.out : nil
    }

    /// Bundle posnetka brez objektov, ki jih ima router (`have`, le znani tukaj). false = ni česa poslati.
    public static func bundle(top: String, snapshot: String, have: [String], to file: URL) -> Bool? {
        let ref = "refs/minigw/push"
        guard run(["update-ref", ref, snapshot], cwd: top, safe: false, hooks: false).status == 0 else { return nil }
        defer { run(["update-ref", "-d", ref], cwd: top, safe: false, hooks: false) }
        let known = have.filter { isSHA($0) && run(["cat-file", "-e", $0 + "^{commit}"], cwd: top, safe: false, hooks: false).status == 0 }
        if known.contains(snapshot) { return false }
        let r = run(["bundle", "create", file.path, ref] + known.map { "^" + $0 }, cwd: top, safe: false, hooks: false)
        return r.status == 0 ? true : FileManager.default.fileExists(atPath: file.path) ? nil : false  // »empty bundle«
    }

    /// Prevzem veje agenta iz bundla routerja: le objekti in ref `refs/heads/<veja>` (fsck), brez hookov, podmodulov,
    /// FETCH_HEAD in checkouta; .git/config ostane nespremenjen. nil = uspeh, sicer razlog.
    public static func fetch(top: String, bundle: URL, branch: String) -> String? {
        guard validBranch(branch) else { return "invalid branch name from the router" }
        if run(["symbolic-ref", "-q", "HEAD"], cwd: top, safe: false).out == "refs/heads/" + branch {
            return "\(branch) is checked out here; not updated"
        }
        let r = run(["-c", "core.hooksPath=/dev/null", "-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true",
                     "-c", "fetch.recurseSubmodules=false", "-c", "submodule.recurse=false", "-c", "fetch.writeCommitGraph=false",
                     "-c", "maintenance.auto=false", "-c", "gc.auto=0",
                     "fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--quiet",
                     bundle.path, "+refs/heads/\(branch):refs/heads/\(branch)"], cwd: top, safe: false)
        return r.status == 0 ? nil : "git fetch of \(branch) failed"
    }

    // MARK: router

    /// Pisanje agentov v načinu worktree: klon in predpomnilniki gradnje. Ne ves ~/Library/Caches (tam je scratch z
    /// zrcali: agent ne sme pisati v zrcalo, ki ga router bere s svojim git).
    public static func writable(root: URL, home: String = NSHomeDirectory()) -> [String] {
        let tmp = Sync.real(URL(fileURLWithPath: NSTemporaryDirectory()))?.deletingLastPathComponent().path
        return [root.path, "\(home)/.m2", "\(home)/.swiftpm", "\(home)/Library/org.swift.swiftpm", "\(home)/Library/Developer",
                "\(home)/Library/Caches/org.swift.swiftpm", "/private/tmp"] + (tmp.map { [$0] } ?? [])
    }

    /// Okolje procesov agenta (claude -p, codex, grok) v načinu worktree. Ti sami (zunaj peskovnika orodij) poganjajo git
    /// v klonu, ki ga agent lahko spremeni: nastavitve iz ukazne vrstice preglasijo .git/config klona (fsmonitor, hooki,
    /// datoteka atributov), GIT_ATTR_SOURCE = prazno drevo izklopi .gitattributes klona (filtri, textconv).
    /// ponytail: .git/info/attributes klona ostane; v peskovniku Claude Code je .git/config zaščiten, Codex ima .git le za branje.
    public static let harnessEnv: [String: String] = {
        let pairs = [("core.fsmonitor", "false"), ("core.hooksPath", "/dev/null"), ("core.attributesFile", "/dev/null")]
        var env = ["GIT_CONFIG_COUNT": String(pairs.count), "GIT_ATTR_SOURCE": "4b825dc642cb6eb9a060e54bf8d69288fbee4904"]
        for (i, (k, v)) in pairs.enumerated() { env["GIT_CONFIG_KEY_\(i)"] = k; env["GIT_CONFIG_VALUE_\(i)"] = v }
        return env
    }()

    /// Scratch (zrcala) ni v nobeni pisljivi mapi agentov (npr. izbran v /tmp): sicer worktree ni varen.
    static func scratchIsSafe(_ scratch: URL, home: String = NSHomeDirectory()) -> Bool {
        let s = (Sync.real(scratch) ?? scratch).path
        return !writable(root: URL(fileURLWithPath: "/nonexistent"), home: home).dropFirst().contains { s == $0 || s.hasPrefix($0 + "/") }
    }

    /// Vrhovi refov zrcala (odjemalec po njih pošlje le manjkajoče).
    static func tips(_ repo: URL) -> [String] {
        run(["--git-dir", repo.path, "for-each-ref", "--format=%(objectname)"]).out.split(separator: "\n").map(String.init).filter(isSHA)
    }

    static func ensureMirror(_ repo: URL) -> Bool {
        if FileManager.default.fileExists(atPath: repo.appendingPathComponent("HEAD").path) { return true }
        try? FileManager.default.createDirectory(at: repo.deletingLastPathComponent(), withIntermediateDirectories: true,
                                                 attributes: [.posixPermissions: 0o700])
        return run(["init", "-q", "--bare", repo.path]).status == 0
    }

    /// Uvoz bundla odjemalca (ref refs/minigw/push) kot `base` zrcala; brez bundla mora posnetek v zrcalu že obstajati.
    static func importBase(_ repo: URL, bundle: URL?, snapshot: String, base: String) -> Bool {
        if let bundle {
            guard run(["--git-dir", repo.path, "fetch", "-q", "--no-tags", bundle.path, "+refs/minigw/push:\(base)"]).status == 0 else { return false }
        } else {
            guard run(["--git-dir", repo.path, "update-ref", base, snapshot]).status == 0 else { return false }
        }
        return run(["--git-dir", repo.path, "rev-parse", base]).out == snapshot
    }

    /// Klon za agenta na veji `branch` iz posnetka (objekti zrcala le za branje, novi v klonu).
    static func makeClone(_ repo: URL, at wt: URL, branch: String, snapshot: String) -> Bool {
        try? FileManager.default.removeItem(at: wt)  // ostanek prekinjenega zagona
        try? FileManager.default.createDirectory(at: wt.deletingLastPathComponent(), withIntermediateDirectories: true,
                                                 attributes: [.posixPermissions: 0o700])
        return run(["clone", "-q", "--shared", "--no-checkout", repo.path, wt.path]).status == 0
            && run(["-C", wt.path, "checkout", "-q", "-b", branch, snapshot]).status == 0
    }

    /// HEAD agentovega klona brez git v njem: »ref: refs/heads/…« → loose ref ali packed-refs; sicer odcepljen sha.
    static func head(_ gitDir: URL) -> String? {
        func read(_ p: String) -> String? {
            Sync.read(gitDir.appendingPathComponent(p), max: 4096).map { String(decoding: $0, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) }
        }
        guard let h = read("HEAD") else { return nil }
        guard h.hasPrefix("ref: ") else { return isSHA(h) ? h : nil }
        let ref = String(h.dropFirst(5))
        guard ref.hasPrefix("refs/"), !ref.contains(".."), !ref.contains("\0") else { return nil }
        if let v = read(ref), isSHA(v) { return v }
        for line in (read("packed-refs") ?? "").split(separator: "\n") where line.hasSuffix(" " + ref) {
            let sha = String(line.prefix(40))
            if isSHA(sha) { return sha }
        }
        return nil
    }

    /// Po agentu: commiti agenta + njegove neshranjene spremembe (commit routerja) → `refs/heads/<branch>` zrcala in bundle
    /// od posnetka do vrha v `out`. Vse z nastavitvami zrcala; objekti klona le kot dodatni vir. Vrne vrh ali nil.
    static func finalize(repo: URL, wt: URL, branch: String, base: String, agent: String, out: URL) -> String? {
        let objects = wt.appendingPathComponent(".git/objects").path
        let env = ["GIT_ALTERNATE_OBJECT_DIRECTORIES": objects]
        var tip = head(wt.appendingPathComponent(".git")) ?? base
        // vrh mora biti commit, potomec posnetka (agent ne more podtakniti tuje zgodovine kot »svoje«)
        if run(["--git-dir", repo.path, "merge-base", "--is-ancestor", base, tip], env: env).status != 0 { tip = base }
        let idx = FileManager.default.temporaryDirectory.appendingPathComponent("minigw-wt-index-" + UUID().uuidString).path
        defer { unlink(idx) }
        let e = env.merging(["GIT_INDEX_FILE": idx, "GIT_WORK_TREE": wt.path]) { $1 }
        let g = ["--git-dir", repo.path]
        if run(g + ["read-tree", tip], env: e).status == 0, run(g + ["add", "-A"], cwd: wt.path, env: e).status == 0,
           case let t = run(g + ["write-tree"], env: e), isSHA(t.out), run(g + ["rev-parse", tip + "^{tree}"], env: env).out != t.out {
            let c = run(g + ["-c", "user.name=Mini Gateway", "-c", "user.email=minigw@localhost", "commit-tree", t.out, "-p", tip,
                             "-m", "minigw: uncommitted changes of \(agent)"], env: env)
            if isSHA(c.out) { tip = c.out }
        }
        guard run(g + ["update-ref", "refs/heads/" + branch, tip], env: env).status == 0 else { return nil }
        try? FileManager.default.removeItem(at: out)
        guard tip != base else { return tip }  // agent ni spremenil ničesar
        guard run(g + ["bundle", "create", out.path, "refs/heads/" + branch, "^" + base], env: env).status == 0,
              run(g + ["bundle", "unbundle", out.path]).status == 0 else { return nil }  // objekti tudi v zrcalo (klon izgine)
        return tip
    }
}

extension ShareHub {
    /// Klon izvajanja (le v izvajanju agenta; Siri ga nima): najnižje prosto mesto seje in agenta. Zaporedne zahteve
    /// nadaljujejo v istem klonu in na isti veji; vzporedne dobijo svoje (dve veji). Nov klon začne iz posnetka odjemalca.
    func worktreeMount(_ s: Share, _ tree: Tree) async -> URL? {
        guard let id = Self.run, !s.readOnly else { return nil }
        let agent = Self.agent ?? "agent", short = String(Sync.folderID(s.session).prefix(8))
        await acquire(tree.repo)
        var n = 0
        while slots["\(s.session)|\(agent)|\(n)"]?.busy == true { n += 1 }
        let k = "\(s.session)|\(agent)|\(n)"
        if let slot = slots[k], FileManager.default.fileExists(atPath: slot.wt.appendingPathComponent(".git").path) {
            slot.busy = true; runSlot[id] = k; unlock(tree.repo)
            return Self.dir(slot.wt, tree.sub)
        }
        let branch = "minigw/\(agent)-\(short)-\(n)", wt = tree.worktrees.appendingPathComponent("\(agent)-\(short)-\(n)", isDirectory: true)
        let base = "refs/minigw/base/\(agent)-\(short)-\(n)", repo = tree.repo
        let have = await Task.detached { Worktree.ensureMirror(repo) ? Worktree.tips(repo) : nil }.value
        guard let have, !Task.isCancelled else { unlock(tree.repo); return nil }
        let incoming = repo.deletingLastPathComponent().appendingPathComponent(repo.lastPathComponent + ".incoming")
        try? FileManager.default.removeItem(at: incoming)
        s.pushing = incoming
        let job = DAVJob(id: UUID().uuidString, method: "WORKTREE-PUSH", path: "", prefix: "", headers: [:],
                         body: (try? JSONSerialization.data(withJSONObject: ["have": have])) ?? Data())
        let r = await send(s, job, timeout: 900)
        s.pushing = nil
        let snapshot = r.flatMap { $0.status == 200 ? (try? JSONSerialization.jsonObject(with: $0.body) as? [String: String])?["snapshot"] : nil }
        let ok: Bool
        if let snapshot, Worktree.isSHA(snapshot) {
            let file: URL? = FileManager.default.fileExists(atPath: incoming.path) ? incoming : nil
            ok = await Task.detached {
                Worktree.importBase(repo, bundle: file, snapshot: snapshot, base: base)
                    && Worktree.makeClone(repo, at: wt, branch: branch, snapshot: snapshot)
            }.value
        } else { ok = false }
        try? FileManager.default.removeItem(at: incoming)
        if ok, let snapshot { slots[k] = Slot(wt: wt, branch: branch, base: snapshot); runSlot[id] = k }
        unlock(tree.repo)
        return ok ? Self.dir(wt, tree.sub) : nil
    }

    static func dir(_ wt: URL, _ sub: String) -> URL {
        let d = sub.isEmpty ? wt : wt.appendingPathComponent(sub, isDirectory: true)
        return FileManager.default.fileExists(atPath: d.path) ? d : wt  // podmapa, ki je git ne pozna (prazna)
    }

    /// Klon in veja tega izvajanja (za navodilo agentu in peskovnik).
    func worktree(session: String) -> (root: URL, branch: String)? {
        guard let id = Self.run, let k = runSlot[id], k.hasPrefix(session + "|"), let slot = slots[k] else { return nil }
        return (slot.wt, slot.branch)
    }

    /// Po agentu: veja v zrcalu (z neshranjenimi spremembami) in prevzem pri odjemalcu (WORKTREE-PULL).
    func worktreeBack(_ s: Share, _ tree: Tree) async {
        guard let id = Self.run, let k = runSlot[id], let slot = slots[k] else { return }
        await acquire(tree.repo)
        let out = tree.repo.deletingLastPathComponent().appendingPathComponent(tree.repo.lastPathComponent + ".result")
        let (repo, agent) = (tree.repo, String(k.split(separator: "|")[1]))
        let tip = await Task.detached {
            Worktree.finalize(repo: repo, wt: slot.wt, branch: slot.branch, base: slot.base, agent: agent, out: out)
        }.value
        if let tip, tip != slot.base, FileManager.default.fileExists(atPath: out.path) {
            s.result = out
            let body = (try? JSONSerialization.data(withJSONObject: ["branch": slot.branch, "tip": tip])) ?? Data()
            _ = await send(s, DAVJob(id: UUID().uuidString, method: "WORKTREE-PULL", path: "", prefix: "", headers: [:], body: body), timeout: 300)
            s.result = nil
        }
        try? FileManager.default.removeItem(at: out)
        unlock(tree.repo)
    }

    /// Del bundla odjemalca (WORKTREE-PUSH): del 0 začne datoteko. Status HTTP.
    /// ponytail: brez skupne omejitve velikosti (minigw pošlje le manjkajoče objekte); dodaj, če bo treba.
    func worktreePut(key: String, part: Int, data: Data, device: String?) -> Int {
        guard let s = share(key: key, device: device), let file = s.pushing else { return 409 }
        s.lastSeen = Date()
        if part == 0 { FileManager.default.createFile(atPath: file.path, contents: nil, attributes: [.posixPermissions: 0o600]) }
        guard let h = FileHandle(forWritingAtPath: file.path) else { return 500 }
        defer { try? h.close() }
        do { try h.seekToEnd(); try h.write(contentsOf: data) } catch { return 500 }
        return 204
    }

    /// Bundle veje za odjemalca med WORKTREE-PULL.
    func worktreeResult(key: String, device: String?) -> Data? {
        guard let s = share(key: key, device: device), let file = s.result else { return nil }
        s.lastSeen = Date()
        return Sync.read(file, max: Int(Sync.maxFile))
    }

    /// Ob koncu seje: kloni gredo (veje ostanejo v zrcalu in pri odjemalcu).
    func dropSlots(session: String) {
        for (k, slot) in slots where k.hasPrefix(session + "|") && !slot.busy {
            slots[k] = nil
            let wt = slot.wt
            Task.detached { try? FileManager.default.removeItem(at: wt) }
        }
    }
}
