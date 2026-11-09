import Foundation
import os

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
    /// Pravi git, ne /usr/bin/git: ta je shim xcrun, ki izvede, kar piše v predpomnilniku xcrun ($TMPDIR/xcrun_db ali
    /// env xcrun_db), in ta bi ga lahko podtaknil agent. Enkrat ob zagonu: xcode-select -p (pravi program), sicer Command
    /// Line Tools; zadnja možnost /usr/bin/git z xcrun_nocache=1 (run tega tudi sicer nastavi).
    static let git: String = {
        let p = Process(), out = Pipe()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/xcode-select")
        p.arguments = ["-p"]
        p.standardOutput = out; p.standardError = FileHandle.nullDevice
        let dev = (try? p.run()).map { _ -> String in
            let d = out.fileHandleForReading.readDataToEndOfFile(); p.waitUntilExit()
            return String(decoding: d, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        } ?? ""
        return [dev + "/usr/bin/git", "/Library/Developer/CommandLineTools/usr/bin/git"]
            .first { !$0.hasPrefix("/usr/bin") && FileManager.default.isExecutableFile(atPath: $0) } ?? "/usr/bin/git"
    }()

    /// git z izhodom; `safe`: brez sistemske in uporabniške nastavitve in brez hookov (router, prevzem pri odjemalcu).
    @discardableResult
    /// `hooks: false` (odjemalec): hooki uporabnika ne tečejo ob začasnem refu refs/minigw/push in posnetku.
    /// `sandbox`: profil sandbox-exec (finalize: branje le v klonu, zrcalu in začasni mapi).
    /// `timeout`: git, ki teče dlje, dobi SIGKILL (L2: ogromen repozitorij, zastal disk).
    static func run(_ args: [String], cwd: String? = nil, env extra: [String: String] = [:], safe: Bool = true, hooks: Bool = true,
                    input: Data? = nil, sandbox: String? = nil, timeout: TimeInterval? = nil) -> (status: Int32, out: String) {
        let guardrails = ["-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "submodule.recurse=false",
                          "-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "maintenance.auto=false", "-c", "gc.auto=0",
                          "-c", "core.commitGraph=false", "-c", "core.multiPackIndex=false"]
        let gitArgs = (safe ? guardrails : hooks ? [] : ["-c", "core.hooksPath=/dev/null"]) + args
        let exe = sandbox == nil ? git : "/usr/bin/sandbox-exec"
        var env = ProcessInfo.processInfo.environment
        for k in env.keys where k.hasPrefix("GIT_") { env[k] = nil }
        if safe { env["GIT_CONFIG_NOSYSTEM"] = "1"; env["GIT_CONFIG_GLOBAL"] = "/dev/null" }
        env["GIT_TERMINAL_PROMPT"] = "0"
        env.merge(extra) { $1 }
        env["xcrun_db"] = nil; env["xcrun_nocache"] = "1"  // tudi če bi git klical shim xcrun: brez predpomnilnika
        // posix_spawn s POSIX_SPAWN_CLOEXEC_DEFAULT: git ne podeduje odprtih datotek routerja (vtičnice odjemalcev),
        // kot bi jih s Process.
        var o: [Int32] = [0, 0], i: [Int32] = [0, 0]
        guard pipe(&o) == 0 else { return (-1, "") }
        guard pipe(&i) == 0 else { close(o[0]); close(o[1]); return (-1, "") }
        for fd in o + i { _ = fcntl(fd, F_SETFD, FD_CLOEXEC) }
        var fa: posix_spawn_file_actions_t?, at: posix_spawnattr_t?
        posix_spawn_file_actions_init(&fa); posix_spawnattr_init(&at)
        defer { posix_spawn_file_actions_destroy(&fa); posix_spawnattr_destroy(&at) }
        if input == nil { posix_spawn_file_actions_addopen(&fa, 0, "/dev/null", O_RDONLY, 0) } else { posix_spawn_file_actions_adddup2(&fa, i[0], 0) }
        posix_spawn_file_actions_adddup2(&fa, o[1], 1)
        posix_spawn_file_actions_addopen(&fa, 2, "/dev/null", O_WRONLY, 0)
        if let cwd { posix_spawn_file_actions_addchdir_np(&fa, cwd) }
        // Svoja skupina procesov: časovna omejitev ubije tudi pomožne procese git (ti držijo izhod odprt).
        posix_spawnattr_setflags(&at, Int16(POSIX_SPAWN_CLOEXEC_DEFAULT | POSIX_SPAWN_SETPGROUP))
        posix_spawnattr_setpgroup(&at, 0)
        let argv = ([exe] + (sandbox.map { ["-p", $0, git] } ?? []) + gitArgs).map { strdup($0) } + [nil]
        let envp = env.map { strdup("\($0.key)=\($0.value)") } + [nil]
        defer { argv.forEach { free($0) }; envp.forEach { free($0) } }
        var pid: pid_t = 0
        let rc = posix_spawn(&pid, exe, &fa, &at, argv, envp)
        close(o[1]); close(i[0])
        guard rc == 0 else { close(o[0]); close(i[1]); return (-1, "") }
        // L2: časovnik pred pisanjem vhoda in branjem izhoda (oba čakata na git)
        let done = OSAllocatedUnfairLock(initialState: false)
        if let timeout {
            DispatchQueue.global().asyncAfter(deadline: .now() + timeout) { [pid] in done.withLock { if !$0 { _ = kill(-pid, SIGKILL) } } }
        }
        let w = FileHandle(fileDescriptor: i[1], closeOnDealloc: true)
        if let input { try? w.write(contentsOf: input) }
        try? w.close()
        let data = FileHandle(fileDescriptor: o[0], closeOnDealloc: true).readDataToEndOfFile()
        var st: Int32 = 0
        while waitpid(pid, &st, 0) == -1 && errno == EINTR {}
        done.withLock { $0 = true }
        let status = st & 0x7f == 0 ? (st >> 8) & 0xff : 128 + (st & 0x7f)
        return (status, String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines))
    }

    /// Datoteka je git bundle (v2 ali v3): sicer je git fetch ne dobi (npr. pot do repozitorija namesto bundla).
    public static func isBundle(_ url: URL) -> Bool {
        guard let f = Sync.openRegular(url), let d = try? f.read(upToCount: 32),
              let line = d.split(separator: 0x0A).first else { return false }
        return ["# v2 git bundle", "# v3 git bundle"].contains(String(decoding: line, as: UTF8.self))
    }

    public static func isSHA(_ s: String) -> Bool { s.count == 40 && s.allSatisfy { $0.isHexDigit && !$0.isUppercase } }

    /// Ime veje agenta (router ni zaupanja vreden: minigw zapiše le take refe).
    public static func validBranch(_ b: String) -> Bool { b.range(of: #"^minigw/[a-z]+-[0-9a-f]{8}-[0-9]+$"#, options: .regularExpression) != nil }

    /// Repozitorij in podmapa iz poti odjemalca (x-worktree-top, x-claude-mini-cwd); odjemalec Go na Windows pošlje
    /// »C:/repo« in »C:\repo«: »\« → »/« kot worktreeTree v minigw serve. nil = cwd ni v repozitoriju.
    static func clientTree(top: String, cwd: String) -> (top: String, sub: String)? {
        var top = top.replacingOccurrences(of: "\\", with: "/")
        let cwd = cwd.replacingOccurrences(of: "\\", with: "/")
        while top.hasSuffix("/") { top.removeLast() }
        guard !top.isEmpty, cwd == top || cwd.hasPrefix(top + "/") else { return nil }
        return (top, String(cwd.dropFirst(top.count)).trimmingCharacters(in: CharacterSet(charactersIn: "/")))
    }

    // MARK: odjemalec (minigw)

    /// Koren repozitorija za cwd (git uporabnika, njegove nastavitve); nil zunaj repozitorija.
    public static func toplevel(_ cwd: String) -> String? {
        let r = run(["rev-parse", "--show-toplevel"], cwd: cwd, safe: false, hooks: false)
        return r.status == 0 && !r.out.isEmpty ? r.out : nil
    }

    /// Repozitorij ima HEAD (brez njega ni posnetka za worktree; auto in worktree tedaj na sync).
    public static func hasCommit(top: String) -> Bool {
        let h = run(["rev-parse", "--verify", "-q", "HEAD^{commit}"], cwd: top, safe: false, hooks: false)
        return h.status == 0 && isSHA(h.out)
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

    /// Začetek dela agenta: zadnji poslani posnetek, ki je prednik veje (HEAD z neshranjenimi spremembami in nesledenimi
    /// datotekami uporabnika; ti niso delo agenta), sicer HEAD. Prehodu ne verjame: le posnetki tega odjemalca (kot Go agentBase).
    public static func agentBase(top: String, branch: String, sent: [String]) -> String {
        sent.reversed().first { run(["merge-base", "--is-ancestor", $0, "refs/heads/" + branch], cwd: top, safe: false, hooks: false).status == 0 } ?? "HEAD"
    }

    /// R1: veja in začetek iz sporočila prehoda (native) le, če sta veljavna, veja obstaja v repozitoriju uporabnika in je
    /// začetek njen prednik (kot Go deliveredBranch).
    public static func delivered(root: String, branch: String, base: String) -> Bool {
        guard validBranch(branch), isSHA(base), let top = toplevel(root),
              run(["rev-parse", "--verify", "-q", "refs/heads/\(branch)^{commit}"], cwd: top, safe: false, hooks: false).status == 0 else { return false }
        return run(["merge-base", "--is-ancestor", base, "refs/heads/" + branch], cwd: top, safe: false, hooks: false).status == 0
    }

    /// Vrstica o dostavljeni veji (enaka v odjemalcu Go): le delo agenta, brez posnetka uporabnika.
    public static func branchLine(branch: String, base: String) -> String {
        let r = base + ".." + branch
        return "agent work is on branch \(branch): review with git diff \(r) (\(base) = your state when the agent started), "
            + "then bring it in with git cherry-pick \(r) or git diff --binary \(r) | git apply"
    }

    /// Prevzem veje agenta iz bundla routerja: le objekti in ref `refs/heads/<veja>` (fsck), brez hookov, podmodulov,
    /// FETCH_HEAD in checkouta; .git/config ostane nespremenjen. nil = uspeh, sicer razlog.
    /// Brez sile: če veja tukaj ni prednik agentove (uporabnik je nanjo commital), ostane uporabnikova.
    public static func fetch(top: String, bundle: URL, branch: String) -> String? {
        guard validBranch(branch) else { return "invalid branch name from the gateway" }
        guard isBundle(bundle) else { return "the gateway did not send a git bundle" }
        if run(["symbolic-ref", "-q", "HEAD"], cwd: top, safe: false).out == "refs/heads/" + branch {
            return "\(branch) is checked out here; not updated"
        }
        let r = run(["-c", "core.hooksPath=/dev/null", "-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true",
                     "-c", "fetch.recurseSubmodules=false", "-c", "submodule.recurse=false", "-c", "fetch.writeCommitGraph=false",
                     "-c", "maintenance.auto=false", "-c", "gc.auto=0",
                     "fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--quiet",
                     bundle.path, "+refs/heads/\(branch):refs/minigw/incoming"], cwd: top, safe: false, hooks: false)
        defer { run(["update-ref", "-d", "refs/minigw/incoming"], cwd: top, safe: false, hooks: false) }
        let new = run(["rev-parse", "-q", "--verify", "refs/minigw/incoming^{commit}"], cwd: top, safe: false, hooks: false).out
        guard r.status == 0, isSHA(new) else { return "git fetch of \(branch) failed (the gateway's branch was rejected)" }
        // Premik veje z git fetch brez sile: git sam zavrne nehitro posodobitev in vejo, izvlečeno v kateri koli delovni mapi.
        let u = run(["-c", "fetch.recurseSubmodules=false", "-c", "submodule.recurse=false", "-c", "maintenance.auto=false", "-c", "gc.auto=0",
                     "fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--quiet",
                     ".", "refs/minigw/incoming:refs/heads/\(branch)"], cwd: top, safe: false, hooks: false)
        if u.status == 0 { return nil }
        let old = run(["rev-parse", "-q", "--verify", "refs/heads/" + branch], cwd: top, safe: false, hooks: false).out
        if isSHA(old), run(["merge-base", "--is-ancestor", old, new], cwd: top, safe: false, hooks: false).status != 0 {
            return "\(branch) has commits here that the agent's version does not have; kept yours, the agent's work was not fetched"
        }
        return "cannot update \(branch) (is it checked out in a worktree?)"
    }

    // MARK: router

    /// Pisanje agentov v načinu worktree: klon in predpomnilniki gradnje. Ne ves ~/Library/Caches (tam je scratch z
    /// zrcali: agent ne sme pisati v zrcalo, ki ga router bere s svojim git).
    /// Lastnikovih predpomnilnikov (~/.m2, SwiftPM) ne: tja bi agent podtaknil vtičnik ali nastavitve za lastnikovo naslednjo
    /// gradnjo. Maven dobi svoj lokalni repozitorij (`maven`, MAVEN_OPTS), SwiftPM dela v .build klona.
    public static func writable(root: URL, maven: URL, home: String = NSHomeDirectory()) -> [String] {
        [root.path, maven.path] + caches(home: home)
    }

    /// Skupni pisljivi predpomnilniki gradnje (začasne mape, DerivedData).
    /// ponytail: DerivedData je skupen z lastnikovimi projekti; ločen (-derivedDataPath), če bo treba.
    /// Nikoli DARWIN_USER_TEMP_DIR in DARWIN_USER_CACHE_DIR (/var/folders/…/T, …/C) ali njuna nadrejena mapa: tam sta
    /// predpomnilnik xcrun (xcrun_db, po njem /usr/bin/git in drugi shimi izvedejo program) in ModuleCache clang.
    /// Agent ima svoj TMPDIR in ModuleCache v .build klona (harnessEnv), DerivedData z -derivedDataPath .build/DerivedData.
    static func caches(home: String = NSHomeDirectory()) -> [String] { ["/private/tmp"] }

    /// Okolje procesov agenta (claude -p, codex, grok) v načinu worktree. Ti sami (zunaj peskovnika orodij) poganjajo git
    /// v klonu, ki ga agent lahko spremeni: nastavitve iz ukazne vrstice preglasijo .git/config klona (fsmonitor, hooki,
    /// datoteka atributov), GIT_ATTR_SOURCE = prazno drevo izklopi .gitattributes klona (filtri, textconv).
    /// ponytail: .git/info/attributes klona ostane; v peskovniku Claude Code je .git/config zaščiten, Codex ima .git le za branje.
    /// GIT_DIR, GIT_WORK_TREE, GIT_COMMON_DIR: git ne išče repozitorija (ugnezden .git agenta ne velja); MAVEN_OPTS: lokalni
    /// repozitorij Maven agentov namesto lastnikovega ~/.m2/repository.
    public static func harnessEnv(root: URL, maven: URL) -> [String: String] {
        // N9: status in diff ne vstopita v podmodul (submodule.<ime>.ignore iz .gitmodules to preglasi: claudeEnv)
        let pairs = [("core.fsmonitor", "false"), ("core.hooksPath", "/dev/null"), ("core.attributesFile", "/dev/null"),
                     ("diff.ignoreSubmodules", "all"), ("submodule.recurse", "false")]
        let git = root.appendingPathComponent(".git").path
        var env = ["GIT_CONFIG_COUNT": String(pairs.count), "GIT_ATTR_SOURCE": "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
                   "GIT_DIR": git, "GIT_WORK_TREE": root.path, "GIT_COMMON_DIR": git,
                   "MAVEN_OPTS": "-Dmaven.repo.local=\(maven.path) -Djava.io.tmpdir=\(root.appendingPathComponent(".build/tmp").path)",
                   "TMPDIR": root.appendingPathComponent(".build/tmp").path + "/",
                   "CLANG_MODULE_CACHE_PATH": root.appendingPathComponent(".build/ModuleCache").path, "xcrun_nocache": "1"]
        for (i, (k, v)) in pairs.enumerated() { env["GIT_CONFIG_KEY_\(i)"] = k; env["GIT_CONFIG_VALUE_\(i)"] = v }
        return env
    }

    /// N9 (kot Go worktreeClaudeEnv): okolje claude -p v klonu: harnessEnv in GIT_EXEC_PATH z ovojem `gitexec`. Git zažene
    /// vgnezden git (stanje podmodula: git status v ugnezdenem repozitoriju z GIT_DIR=.git, kjer so config in atributi
    /// agentovi) z »git« iz mape exec: ovoj pusti le git s klonovim GIT_DIR (MINIGW_GIT_DIR), drugo zavrne. Ostale datoteke
    /// so povezave na pravo mapo exec. `gitexec` je v Scratch (agenti tja ne pišejo). nil = ovoja ni.
    public static func claudeEnv(root: URL, maven: URL, gitexec: URL) -> [String: String]? {
        guard gitExecWrap(gitexec) else { return nil }
        var env = harnessEnv(root: root, maven: maven)
        env["GIT_EXEC_PATH"] = gitexec.path
        env["MINIGW_GIT_DIR"] = env["GIT_DIR"]
        return env
    }

    private static let wrapLock = NSLock()

    static func gitExecWrap(_ dir: URL) -> Bool {
        wrapLock.lock(); defer { wrapLock.unlock() }
        let real = run(["--exec-path"]).out.trimmingCharacters(in: .whitespacesAndNewlines)
        guard real.hasPrefix("/") else { return false }
        let fm = FileManager.default, wrapper = dir.appendingPathComponent("git")
        let script = "#!/bin/sh\n# minigw (N9): git in a nested repository of a worktree clone does not run\n"
            + #"[ "$GIT_DIR" = "$MINIGW_GIT_DIR" ] || { echo "minigw: git in a nested repository is off in worktree mode" >&2; exit 128; }"# + "\n"
            + "exec '" + (real + "/git").replacingOccurrences(of: "'", with: #"'\''"#) + #"' "$@""# + "\n"
        if fm.contents(atPath: wrapper.path) == Data(script.utf8) { return true }
        try? fm.removeItem(at: dir)
        guard (try? fm.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])) != nil,
              let names = try? fm.contentsOfDirectory(atPath: real) else { return false }
        for n in names where n != "git" {
            guard (try? fm.createSymbolicLink(atPath: dir.appendingPathComponent(n).path, withDestinationPath: real + "/" + n)) != nil else { return false }
        }
        return fm.createFile(atPath: wrapper.path, contents: Data(script.utf8), attributes: [.posixPermissions: 0o700])
    }

    /// N9 (kot Go cloneProblem): prvi .git (mapa ali datoteka) pod korenom klona, relativno; nil = ni ga.
    static func nestedGit(_ wt: URL) -> String? {
        guard let e = FileManager.default.enumerator(at: wt, includingPropertiesForKeys: nil) else { return nil }
        var parts: [String] = []
        for case let u as URL in e {
            parts = Array(parts.prefix(e.level - 1)) + [u.lastPathComponent]
            guard u.lastPathComponent.lowercased() == ".git" else { continue }
            if e.level == 1 { e.skipDescendants(); continue }
            return parts.joined(separator: "/")
        }
        return nil
    }

    /// Scratch (zrcala) ni v nobeni pisljivi mapi agentov (npr. izbran v /tmp): sicer worktree ni varen.
    static func scratchIsSafe(_ scratch: URL, home: String = NSHomeDirectory()) -> Bool {
        let s = (Sync.real(scratch) ?? scratch).path
        return !caches(home: home).contains { s == $0 || s.hasPrefix($0 + "/") }
    }

    /// Vrhovi refov zrcala (odjemalec po njih pošlje le manjkajoče).
    static func tips(_ repo: URL) -> [String] {
        run(["--git-dir", repo.path, "for-each-ref", "--format=%(objectname)"]).out.split(separator: "\n").map(String.init).filter(isSHA)
    }

    /// info/exclude zrcala: izhod gradnje (.build, target, DerivedData …) nikoli v vejo, tudi brez .gitignore projekta
    /// (finalize dela z GIT_DIR zrcala).
    static func ensureMirror(_ repo: URL) -> Bool {
        if FileManager.default.fileExists(atPath: repo.appendingPathComponent("HEAD").path) { return true }
        try? FileManager.default.createDirectory(at: repo.deletingLastPathComponent(), withIntermediateDirectories: true,
                                                 attributes: [.posixPermissions: 0o700])
        guard run(["init", "-q", "--bare", repo.path]).status == 0 else { return false }
        let exclude = Sync.defaultIgnore.filter { $0 != ".git/" }.joined(separator: "\n") + "\n"
        try? FileManager.default.createDirectory(at: repo.appendingPathComponent("info"), withIntermediateDirectories: true)
        return (try? Data(exclude.utf8).write(to: repo.appendingPathComponent("info/exclude"))) != nil
    }

    /// Uvoz bundla odjemalca (ref refs/minigw/push) kot `base` zrcala; brez bundla mora posnetek v zrcalu že obstajati.
    static func importBase(_ repo: URL, bundle: URL?, snapshot: String, base: String) -> Bool {
        if let bundle {
            guard isBundle(bundle), run(["--git-dir", repo.path, "-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true",
                                         "fetch", "-q", "--no-tags", bundle.path, "+refs/minigw/push:\(base)"]).status == 0 else { return false }
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

    /// Native z git (repozitorij na tem Macu): posnetek mape uporabnika v zrcalu kot `base`, brez git v njegovem
    /// repozitoriju. Objekti prek alternates zrcala (le branje), HEAD iz datotek; neshranjene spremembe z začasnim indeksom
    /// zrcala in njegovimi nastavitvami (brez filtrov, hookov, fsmonitor), v peskovniku (branje le mapa, zrcalo, začasna
    /// mapa), izključitve še iz .git/info/exclude uporabnika. Mapa, indeks, HEAD in veje uporabnika ostanejo nespremenjeni.
    /// ponytail: datoteke LFS gredo v posnetek kot vsebina (zrcalo nima filtra lfs).
    static func localSnapshot(repo: URL, top: URL, base: String, excludes: URL? = globalExcludes()) -> String? {
        guard let realTop = Sync.real(top), realTop.path == top.path, case let dotGit = top.appendingPathComponent(".git"),
              Sync.real(dotGit)?.path == top.path + "/.git", let objects = Sync.real(dotGit.appendingPathComponent("objects")),
              objects.path == top.path + "/.git/objects", let headSHA = head(dotGit), ensureMirror(repo) else { return nil }
        let info = repo.appendingPathComponent("objects/info")
        try? FileManager.default.createDirectory(at: info, withIntermediateDirectories: true)
        guard (try? Data((objects.path + "\n").utf8).write(to: info.appendingPathComponent("alternates"))) != nil else { return nil }
        FileManager.default.createFile(atPath: repo.appendingPathComponent("minigw-used").path, contents: nil)  // prune: zadnja raba
        let tmp = repo.deletingLastPathComponent().appendingPathComponent(".snapshot-" + UUID().uuidString, isDirectory: true)
        guard (try? FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])) != nil,
              let box = finalizeProfile(wt: top, repo: repo, tmp: tmp, out: tmp.appendingPathComponent("unused")) else { return nil }
        defer { try? FileManager.default.removeItem(at: tmp) }
        let g = ["--git-dir", repo.path], e = ["GIT_INDEX_FILE": tmp.appendingPathComponent("index").path, "GIT_WORK_TREE": top.path]
        // M2: izključitve repozitorija (.git/info/exclude) in globalne uporabnika (core.excludesFile, ~/.config/git/ignore)
        let exclude = tmp.appendingPathComponent("exclude")
        let sources = [dotGit.appendingPathComponent("info/exclude"), excludes].compactMap { $0 }
        try? sources.compactMap { Sync.read($0, max: 1 << 20) }.reduce(Data()) { $0 + $1 + Data("\n".utf8) }.write(to: exclude)
        func git(_ a: [String], cwd: String? = nil) -> (status: Int32, out: String) {
            run(g + a, cwd: cwd ?? tmp.path, env: e, sandbox: box, timeout: 600)  // L2
        }
        guard git(["read-tree", headSHA]).status == 0, git(["-c", "core.excludesFile=" + exclude.path, "add", "-A"], cwd: top.path).status == 0,
              case let t = git(["write-tree"]), isSHA(t.out) else { return nil }
        var snap = headSHA
        if git(["rev-parse", headSHA + "^{tree}"]).out != t.out {
            let c = git(["-c", "user.name=minigw", "-c", "user.email=minigw@localhost", "commit-tree", t.out, "-p", headSHA,
                         "-m", "minigw: your uncommitted changes"])
            guard isSHA(c.out) else { return nil }
            snap = c.out
        }
        return git(["update-ref", base, snap]).status == 0 ? snap : nil
    }

    /// Native z git: bundle veje za repozitorij uporabnika. Posnetek (z njegovimi neshranjenimi spremembami) je le v zrcalu,
    /// zato je pogoj trenutni HEAD uporabnika (ta ga ima), ne posnetek.
    static func localBundle(repo: URL, top: URL, branch: String, out: URL) -> Bool {
        try? FileManager.default.removeItem(at: out)
        let have = head(top.appendingPathComponent(".git")).map { ["^" + $0] } ?? []
        return run(["--git-dir", repo.path, "bundle", "create", out.path, "refs/heads/" + branch] + have, timeout: 600).status == 0 && isBundle(out)
    }

    /// Native z git: veja iz bundla naravnost v repozitorij uporabnika v enem koraku (H1). git le s pravili prehoda (brez
    /// sistemske in globalne nastavitve; fsmonitor, alternateRefsCommand, hooki izklopljeni z ukazno vrstico), v peskovniku:
    /// izvajanje le git in njegovih pomožnih programov, pisanje le v .git. Brez »+«: le naprej, izvlečena veja zavrnjena.
    /// Pred tem znova: .git je prava mapa (ne povezava ali datoteka gitdir), brez commondir. nil = uspeh, sicer razlog.
    static func localFetch(top: URL, bundle: URL, branch: String) -> String? {
        let dotGit = top.appendingPathComponent(".git")
        var isDir: ObjCBool = false
        guard validBranch(branch), Sync.real(top)?.path == top.path, Sync.real(dotGit)?.path == dotGit.path,
              FileManager.default.fileExists(atPath: dotGit.path, isDirectory: &isDir), isDir.boolValue,
              !FileManager.default.fileExists(atPath: dotGit.appendingPathComponent("commondir").path),
              Sync.real(dotGit.appendingPathComponent("objects"))?.path == dotGit.path + "/objects" else {
            return "\(top.path): the repository changed (its .git is no longer a plain folder); \(branch) was not delivered"
        }
        let exec = run(["--exec-path"]).out
        guard !exec.isEmpty else { return "git has no exec path" }
        let q = { (p: String) in "\"" + p.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\"" }
        let box = "(version 1)\n(allow default)\n(deny process-exec*)\n"
            + "(allow process-exec* (literal \(q(git))) (literal \(q(Sync.real(URL(fileURLWithPath: git))?.path ?? git))) (subpath \(q(exec))))\n"
            + "(deny file-write*)\n(allow file-write* (regex #\"^/dev/\") (subpath \(q(dotGit.path))))\n"
        let r = run(["-c", "core.alternateRefsCommand=", "-c", "core.alternateRefsPrefixes=", "-c", "fetch.fsckObjects=true",
                     "-c", "transfer.fsckObjects=true", "-c", "fetch.recurseSubmodules=false", "-c", "fetch.writeCommitGraph=false",
                     "fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--quiet",
                     bundle.path, "refs/heads/\(branch):refs/heads/\(branch)"], cwd: top.path, safe: true, sandbox: box, timeout: 300)
        return r.status == 0 ? nil
            : "\(branch) was not updated in \(top.path) (it has commits the agent's version does not have, or it is checked out)"
    }

    /// Globalni seznam izključitev uporabnika (core.excludesFile ali ~/.config/git/ignore); nil, ko ga ni.
    static func globalExcludes(home: String = NSHomeDirectory(), env: [String: String] = [:]) -> URL? {
        let set = run(["config", "--global", "--path", "--get", "core.excludesFile"], env: env, safe: false, hooks: false, timeout: 10).out
        let xdg = (ProcessInfo.processInfo.environment.merging(env) { $1 }["XDG_CONFIG_HOME"].flatMap { $0.isEmpty ? nil : $0 } ?? home + "/.config") + "/git/ignore"
        let p = URL(fileURLWithPath: set.isEmpty ? xdg : (set as NSString).expandingTildeInPath)
        return FileManager.default.fileExists(atPath: p.path) ? p : nil
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
    /// Pred tem: procesi agenta v klonu so ustavljeni (killProcesses), objects/info klona (alternates, commit-graph) izbrisan
    /// in po tem znova le alternates zrcala; git teče v sandbox-exec (branje le v klonu, zrcalu in svoji začasni mapi),
    /// zato zamenjava datoteke s povezavo med »git add« ne prebere ničesar drugega.
    /// `read`: še berljivo v peskovniku (native: objekti repozitorija uporabnika, na katere kaže alternates zrcala).
    static func finalize(repo: URL, wt: URL, branch: String, base: String, agent: String, out: URL, read: [URL] = []) -> String? {
        killProcesses(in: wt)
        // .git in objects klona morata biti pravi mapi (ne povezavi ven): sicer brisanje in pisanje spodaj zadeneta tujo mapo.
        let dotGit = wt.appendingPathComponent(".git"), objectsDir = dotGit.appendingPathComponent("objects")
        guard Sync.real(dotGit)?.path == (Sync.real(wt)?.path).map({ $0 + "/.git" }),
              Sync.real(objectsDir)?.path == (Sync.real(wt)?.path).map({ $0 + "/.git/objects" }) else { return nil }
        // Brisanje in zapis relativno na deskriptor mape objects (O_NOFOLLOW_ANY): zamenjava poti s povezavo med preverjanjem
        // in dejanjem ne preusmeri ničesar.
        guard let realObjects = Sync.real(objectsDir)?.path, case let objFd = open(realObjects, O_RDONLY | O_DIRECTORY | O_NOFOLLOW_ANY),
              objFd >= 0 else { return nil }
        defer { close(objFd) }
        // Takoj nova info z le alternates zrcala (agent je ustavljen); karkoli ne uspe ali ni tako, kot pričakovano: brez rezultata.
        removeAt(objFd, "info")
        var st = stat()
        guard fstatat(objFd, "info", &st, AT_SYMLINK_NOFOLLOW) != 0, errno == ENOENT,
              mkdirat(objFd, "info", 0o755) == 0, case let infoFd = openat(objFd, "info", O_RDONLY | O_DIRECTORY | O_NOFOLLOW), infoFd >= 0
        else { return nil }
        defer { close(infoFd) }
        let alt = openat(infoFd, "alternates", O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, 0o644)
        guard alt >= 0 else { return nil }
        let line = Data((repo.appendingPathComponent("objects").path + "\n").utf8)
        let wrote = line.withUnsafeBytes { write(alt, $0.baseAddress, $0.count) } == line.count
        close(alt)
        guard wrote else { return nil }
        // Začasna mapa v Scratch (ob zrcalu), ne v sistemski začasni mapi.
        let tmp = repo.deletingLastPathComponent().appendingPathComponent(".finalize-" + UUID().uuidString, isDirectory: true)
        guard (try? FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])) != nil
        else { return nil }
        defer { try? FileManager.default.removeItem(at: tmp) }
        guard let box = finalizeProfile(wt: wt, repo: repo, tmp: tmp, out: out, read: read) else { return nil }
        let objects = wt.appendingPathComponent(".git/objects").path
        let env = ["GIT_ALTERNATE_OBJECT_DIRECTORIES": objects]
        func run(_ a: [String], cwd: String? = nil, env: [String: String]) -> (status: Int32, out: String) {
            Worktree.run(a, cwd: cwd ?? tmp.path, env: env, sandbox: box)  // trenutna mapa routerja je lahko v domači (zavrnjena)
        }
        var tip = head(wt.appendingPathComponent(".git")) ?? base
        // vrh mora biti commit, potomec posnetka (agent ne more podtakniti tuje zgodovine kot »svoje«)
        let g = ["--git-dir", repo.path]
        func ancestor(_ a: String, _ b: String) -> Bool { run(g + ["merge-base", "--is-ancestor", a, b], env: env).status == 0 }
        if !ancestor(base, tip) { tip = base }
        // Veja se pri odjemalcu le premika naprej (prevzem brez sile): prejšnji vrh (s commitom routerja) je prednik novega.
        var parents = [tip]
        let prev = run(g + ["rev-parse", "-q", "--verify", "refs/heads/\(branch)^{commit}"], env: env)
        if case let p = prev, isSHA(p.out), p.out != tip {
            if ancestor(tip, p.out) { tip = p.out; parents = [tip] } else if !ancestor(p.out, tip) { parents = [tip, p.out] }
        }
        let idx = tmp.appendingPathComponent("index").path
        let e = env.merging(["GIT_INDEX_FILE": idx, "GIT_WORK_TREE": wt.path]) { $1 }
        if run(g + ["read-tree", tip], env: e).status == 0, run(g + ["add", "-A"], cwd: wt.path, env: e).status == 0,
           case let t = run(g + ["write-tree"], env: e), isSHA(t.out),
           parents.count > 1 || run(g + ["rev-parse", tip + "^{tree}"], env: env).out != t.out {
            let c = run(g + ["-c", "user.name=Mini Gateway", "-c", "user.email=minigw@localhost", "commit-tree", t.out]
                        + parents.flatMap { ["-p", $0] } + ["-m", "minigw: uncommitted changes of \(agent)"], env: env)
            if isSHA(c.out) { tip = c.out }
        }
        guard run(g + ["update-ref", "refs/heads/" + branch, tip], env: env).status == 0 else { return nil }
        try? FileManager.default.removeItem(at: out)
        guard tip != base else { return tip }  // agent ni spremenil ničesar
        // Objekti tudi v zrcalo (klon izgine); fsck zavrne npr. drevo z ».git«. Ob neuspehu ref zrcala nazaj (ali ga ni):
        // sicer bi kazal na objekte, ki so bili le v klonu, in vsak naslednji klon zrcala bi padel (kot b246c91 v Go).
        guard run(g + ["bundle", "create", out.path, "refs/heads/" + branch, "^" + base], env: env).status == 0, isBundle(out),
              run(g + ["-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true", "bundle", "unbundle", out.path], env: [:]).status == 0
        else {
            _ = run(g + (isSHA(prev.out) ? ["update-ref", "refs/heads/" + branch, prev.out] : ["update-ref", "-d", "refs/heads/" + branch]), env: [:])
            try? FileManager.default.removeItem(at: out)
            return nil
        }
        return tip
    }

    /// Izbriše `name` v mapi `dir` (deskriptor) brez sledenja povezavam: povezavo kot povezavo, mapo rekurzivno prek openat.
    /// Globina največ `depth` (globlje: ne izbriše, finalize nato zazna, da info še obstaja).
    static func removeAt(_ dir: Int32, _ name: String, depth: Int = 16) {
        if unlinkat(dir, name, 0) == 0 || errno == ENOENT { return }
        guard depth > 0 else { return }
        let fd = openat(dir, name, O_RDONLY | O_DIRECTORY | O_NOFOLLOW)
        guard fd >= 0, let d = fdopendir(fd) else { if fd >= 0 { close(fd) }; return }
        var names: [String] = []
        while let e = readdir(d) {
            let n = withUnsafeBytes(of: e.pointee.d_name) { String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) }
            if n != ".", n != ".." { names.append(n) }
        }
        for n in names { removeAt(dirfd(d), n, depth: depth - 1) }
        closedir(d)  // zapre tudi fd
        unlinkat(dir, name, AT_REMOVEDIR)
    }

    /// Branje le v klonu, zrcalu in začasni mapi (ne v domači mapi); pisanje le v zrcalo, začasno mapo in `out`.
    static func finalizeProfile(wt: URL, repo: URL, tmp: URL, out: URL, read: [URL] = []) -> String? {
        guard let w = Sync.real(wt)?.path, let r = Sync.real(repo)?.path, let t = Sync.real(tmp)?.path,
              let o = Sync.real(out.deletingLastPathComponent())?.appendingPathComponent(out.lastPathComponent).path
        else { return nil }
        let extra = read.compactMap { Sync.real($0)?.path }
        let q = { (p: String) in "\"" + p.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\"" }
        // Vsebina (file-read-data) privzeto nikjer, le sistem, git (Xcode) in ta klon, zrcalo, začasna mapa: ne domača mapa,
        // ne zrcala drugih naprav v Scratch. Metapodatki ostanejo (git razreši poti prek nadrejenih map).
        return "(version 1)\n(allow default)\n(deny file-read-data (subpath \"/\"))\n"
            + "(allow file-read-data (literal \"/\") (subpath \"/bin\") (subpath \"/usr\") (subpath \"/System\") (subpath \"/Library\")"
            + " (subpath \"/private/var/db\") (subpath \"/dev\") (regex #\"^/Applications/Xcode[^/]*\\.app(/|$)\")"
            + " (subpath \(q(w))) (subpath \(q(r))) (subpath \(q(t))) (literal \(q(o))) (literal \(q(o + ".lock")))"
            + extra.map { " (subpath \(q($0)))" }.joined() + ")\n"
            + "(deny file-write*)\n(allow file-write* (regex #\"^/dev/\") (subpath \(q(r))) (subpath \(q(t)))"
            + " (literal \(q(o))) (literal \(q(o + ".lock"))))\n"  // bundle create piše <out>.lock in ga preimenuje
    }

    /// Procesi istega uporabnika s trenutno mapo v klonu (agent v ozadju): SIGKILL pred finalize.
    /// ponytail: proces, ki je zapustil klon (cd drugam), ostane; finalize ga omeji s peskovnikom.
    static func killProcesses(in wt: URL) {
        guard let root = Sync.real(wt)?.path else { return }
        let n = proc_listallpids(nil, 0)
        guard n > 0 else { return }
        var pids = [pid_t](repeating: 0, count: Int(n) + 64)
        let count = proc_listallpids(&pids, Int32(pids.count * MemoryLayout<pid_t>.size))
        for pid in pids.prefix(max(0, Int(count))) where pid > 1 && pid != getpid() {
            var info = proc_vnodepathinfo()
            guard proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, &info, Int32(MemoryLayout<proc_vnodepathinfo>.size)) > 0 else { continue }
            let cwd = withUnsafeBytes(of: info.pvi_cdir.vip_path) { String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) }
            if cwd == root || cwd.hasPrefix(root + "/") { kill(pid, SIGKILL) }
        }
    }

    /// Ob zagonu aplikacije: zrcala, ki jih 30 dni ni uporabil noben agent, in njihovi kloni.
    public static func prune(scratch: URL, days: Double = 30) {
        let fm = FileManager.default, repos = scratch.appendingPathComponent("repos"), cut = Date().addingTimeInterval(-days * 86400)
        for owner in (try? fm.contentsOfDirectory(atPath: repos.path)) ?? [] {
            for name in (try? fm.contentsOfDirectory(atPath: repos.appendingPathComponent(owner).path)) ?? [] where name.hasSuffix(".git") {
                let repo = repos.appendingPathComponent(owner).appendingPathComponent(name)
                let used = (try? fm.attributesOfItem(atPath: repo.appendingPathComponent("minigw-used").path))?[.modificationDate] as? Date
                    ?? (try? fm.attributesOfItem(atPath: repo.path))?[.modificationDate] as? Date ?? .distantPast
                guard used < cut else { continue }
                try? fm.removeItem(at: repo)
                try? fm.removeItem(at: scratch.appendingPathComponent("worktrees").appendingPathComponent(owner)
                    .appendingPathComponent(String(name.dropLast(4))))
            }
        }
    }
}

extension ShareHub {
    /// Klon izvajanja (le v izvajanju agenta; Siri ga nima): najnižje prosto mesto seje in agenta. Zaporedne zahteve
    /// nadaljujejo v istem klonu in na isti veji; vzporedne dobijo svoje (dve veji). Nov klon začne iz posnetka odjemalca.
    func worktreeMount(_ s: Share, _ tree: Tree, roots: String? = nil, scratch: URL? = nil) async -> URL? {
        // Native z git brez izvajanja (Siri, priloge): prava mapa le za branje
        if Self.run == nil, let local = tree.local {
            guard Sync.real(local)?.path == local.path, let roots, ClaudeCLI.resolveCwd(local.path, roots: roots, scratch: scratch) != nil else { return nil }
            return Self.dir(local, tree.sub)
        }
        guard let id = Self.run, !s.readOnly else { return nil }
        // M1: objekti uporabnika so še prava mapa (ne povezava, npr. na domačo mapo), preden jih agent sme brati
        guard tree.localObjectsIntact else { return nil }
        // L3: pri native ime veje iz naprave in seje (repozitorij si delijo naprave; seja je od odjemalca)
        let agent = Self.agent ?? "agent", short = String(Sync.folderID((tree.local == nil ? "" : (s.device ?? "") + "|") + s.session).prefix(8))
        await acquire(tree.repo)
        var n = 0
        while slots["\(s.session)|\(agent)|\(n)"]?.busy == true { n += 1 }
        let k = "\(s.session)|\(agent)|\(n)"
        if let slot = slots[k], slot.gitIntact, Worktree.nestedGit(slot.wt) == nil {  // sicer nov klon (H2, N9)
            slot.busy = true; runSlot[id] = k; unlock(tree.repo)
            return Self.dir(slot.wt, tree.sub)
        }
        let branch = "minigw/\(agent)-\(short)-\(n)", wt = tree.worktrees.appendingPathComponent("\(agent)-\(short)-\(n)", isDirectory: true)
        let base = "refs/minigw/base/\(agent)-\(short)-\(n)", repo = tree.repo
        let have = await offPool { () -> [String]? in
            guard Worktree.ensureMirror(repo) else { return nil }
            FileManager.default.createFile(atPath: repo.appendingPathComponent("minigw-used").path, contents: nil)  // prune: zadnja raba
            return Worktree.tips(repo)
        }
        guard let have, !Task.isCancelled else { unlock(tree.repo); return nil }
        if let local = tree.local {  // native z git: posnetek tu (mapa še vedno ista in v dovoljenih mapah), brez prenosa
            guard Sync.real(local)?.path == local.path, let roots, ClaudeCLI.resolveCwd(local.path, roots: roots, scratch: scratch) != nil
            else { unlock(tree.repo); return nil }
            let snapshot = await offPool { Worktree.localSnapshot(repo: repo, top: local, base: base) }
            let ok = await offPool { snapshot.map { Worktree.makeClone(repo, at: wt, branch: branch, snapshot: $0) } ?? false }
            return finishMount(ok ? snapshot : nil, k: k, id: id, wt: wt, branch: branch, tree: tree)
        }
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
            ok = await offPool {
                Worktree.importBase(repo, bundle: file, snapshot: snapshot, base: base)
                    && Worktree.makeClone(repo, at: wt, branch: branch, snapshot: snapshot)
            }
        } else { ok = false }
        try? FileManager.default.removeItem(at: incoming)
        return finishMount(ok ? snapshot : nil, k: k, id: id, wt: wt, branch: branch, tree: tree)
    }

    /// Mesto izvajanja po novem klonu (`snapshot` nil = ni uspelo); sprosti zaklep zrcala.
    private func finishMount(_ snapshot: String?, k: String, id: String, wt: URL, branch: String, tree: Tree) -> URL? {
        if let snapshot {
            slots[k] = Slot(wt: wt, branch: branch, base: snapshot); runSlot[id] = k
            for d in [".build/tmp", ".build/ModuleCache"] {  // TMPDIR in ModuleCache agenta (harnessEnv)
                try? FileManager.default.createDirectory(at: wt.appendingPathComponent(d), withIntermediateDirectories: true,
                                                         attributes: [.posixPermissions: 0o700])
            }
            try? FileManager.default.createDirectory(at: tree.worktrees.appendingPathComponent(".maven"), withIntermediateDirectories: true)
        }
        unlock(tree.repo)
        return snapshot == nil ? nil : Self.dir(wt, tree.sub)
    }

    /// Podmapa `sub` v klonu; vsak del poti prava mapa (H1: povezava kjerkoli v poti, npr. a -> ~ in cwd a/.ssh, bi
    /// delovno mapo agenta prestavila iz klona), sicer koren klona (tudi podmapa, ki je git ne pozna).
    static func dir(_ wt: URL, _ sub: String) -> URL {
        var d = wt
        for c in sub.split(separator: "/") where c != "." {
            d = d.appendingPathComponent(String(c), isDirectory: true)
            var st = stat()
            guard c != "..", lstat(d.path, &st) == 0, st.st_mode & S_IFMT == S_IFDIR else { return wt }
        }
        return d
    }

    /// Klon in veja tega izvajanja (za navodilo agentu in peskovnik).
    /// `maven`: lokalni repozitorij Maven agentov tega repozitorija (ne lastnikov ~/.m2/repository).
    /// `read`: objekti, ki jih git v klonu bere prek alternates (zrcalo, pri native še repozitorij uporabnika).
    func worktree(session: String) -> (root: URL, branch: String, maven: URL, read: [URL])? {
        guard let id = Self.run, let k = runSlot[id], k.hasPrefix(session + "|"), let slot = slots[k],
              let tree = bySessionTree(session) else { return nil }
        let read = [tree.repo.appendingPathComponent("objects")] + (tree.localObjectsIntact ? tree.local.map { [$0.appendingPathComponent(".git/objects")] } ?? [] : [])
        return (slot.wt, slot.branch, tree.worktrees.appendingPathComponent(".maven", isDirectory: true), read)
    }

    /// Veja mesta izvajanja `id` (zgodovina seje: veja te zahteve, ne prve v seji).
    func runBranch(_ id: String) -> String? { runSlot[id].flatMap { slots[$0]?.branch } }

    /// Seja dela v načinu worktree (pred mount: Grok in Codex brez pisanja ga zavrneta).
    func isWorktree(session: String) -> Bool { bySessionTree(session) != nil }

    /// Worktree brez native: agent brez pisanja sme v klonu le brati (veje ne vrne); native z git ga zavrne.
    /// true = zavrni (native z git), false = bere v klonu (sporočilo uporabniku prek minigw).
    func readOnlyInTree(session: String, agent: String) async -> Bool {
        if bySessionTree(session)?.local != nil { return true }
        let msg = "\(agent) may not write, so it only reads in the clone; no branch comes back"
        Task { await self.note(session: session, msg) }  // ne čaka na odgovor minigw
        return false
    }

    /// Zavrnitev agenta, ki v načinu worktree ne more delati: besedilo za worktree ali native z git (tam native-direct).
    func worktreeRefusal(session: String, worktree: String, native: String) -> String {
        bySessionTree(session)?.local != nil ? native + " on this native git repository: agents work on branches here; use --minigw-work=native-direct"
            : worktree
    }

    /// Po agentu: veja v zrcalu (z neshranjenimi spremembami) in prevzem pri odjemalcu (WORKTREE-PULL).
    func worktreeBack(_ s: Share, _ tree: Tree) async {
        guard let id = Self.run, let k = runSlot[id], let slot = slots[k] else { return }
        await acquire(tree.repo)
        let out = tree.repo.deletingLastPathComponent().appendingPathComponent(tree.repo.lastPathComponent + ".result")
        let (repo, agent) = (tree.repo, String(k.split(separator: "|")[1]))
        if tree.local != nil, !tree.localObjectsIntact {  // M1
            await note(s, "\(tree.local!.path): the repository's .git/objects was replaced; \(slot.branch) was not delivered")
            unlock(tree.repo); return
        }
        if let n = await offPool({ Worktree.nestedGit(slot.wt) }) {  // N9
            await note(s, "the agent's clone contains a nested git repository (\(n)); \(slot.branch) was not delivered")
            unlock(tree.repo); return
        }
        let read = tree.local.map { [$0.appendingPathComponent(".git/objects")] } ?? []
        let tip = await offPool {
            Worktree.finalize(repo: repo, wt: slot.wt, branch: slot.branch, base: slot.base, agent: agent, out: out, read: read)
        }
        if let local = tree.local, let tip, tip != slot.base, FileManager.default.fileExists(atPath: out.path) {
            // native z git: veja naravnost v repozitorij uporabnika (le naprej, brez hookov, fsck, v peskovniku); mapa, indeks,
            // HEAD ostanejo. Neuspeh dobi uporabnik (kot pri WORKTREE-PULL) in dnevnik.
            let branch = slot.branch
            if let why = await offPool({ Worktree.localBundle(repo: repo, top: local, branch: branch, out: out)
                    ? Worktree.localFetch(top: local, bundle: out, branch: branch) : "cannot bundle \(branch)" }) {
                Router.log.error("native worktree: \(why, privacy: .public)")
                await note(s, why)
            } else {  // minigw izpiše vrstico o veji z obsegom od posnetka (le delo agenta)
                await note(s, Worktree.branchLine(branch: branch, base: slot.base), extra: ["branch": branch, "base": slot.base])
            }
        } else if let tip, tip != slot.base, FileManager.default.fileExists(atPath: out.path) {
            s.result = out
            let body = (try? JSONSerialization.data(withJSONObject: ["branch": slot.branch, "tip": tip])) ?? Data()
            _ = await send(s, DAVJob(id: UUID().uuidString, method: "WORKTREE-PULL", path: "", prefix: "", headers: [:], body: body), timeout: 300)
            s.result = nil
        }
        try? FileManager.default.removeItem(at: out)
        unlock(tree.repo)
    }

    /// Največji bundle odjemalca (prvi prenos velikega repozitorija); večji → 413.
    static let maxIncoming: Int64 = 4 << 30

    /// Del bundla odjemalca (WORKTREE-PUSH): del 0 začne datoteko. Status HTTP.
    func worktreePut(key: String, part: Int, data: Data, device: String?) -> Int {
        guard let s = share(key: key, device: device), let file = s.pushing else { return 409 }
        s.lastSeen = Date()
        if part == 0 { FileManager.default.createFile(atPath: file.path, contents: nil, attributes: [.posixPermissions: 0o600]) }
        let size = ((try? FileManager.default.attributesOfItem(atPath: file.path))?[.size] as? NSNumber)?.int64Value ?? 0
        guard size + Int64(data.count) <= Self.maxIncoming else { try? FileManager.default.removeItem(at: file); return 413 }
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

    /// Konec izvajanja: mesto prosto; če seje ni več (minigw je končal med agentom), klon gre takoj.
    func releaseSlot(run id: String) {
        guard let k = runSlot.removeValue(forKey: id), let slot = slots[k] else { return }
        slot.busy = false
        if bySessionTree(String(k.split(separator: "|")[0])) == nil {
            slots[k] = nil
            let wt = slot.wt
            Task.detached { Self.removeClone(wt) }
        }
    }

    /// Klon, nato še prazen .maven in mapa klonov, ko v njej ni drugega klona (kot removeClone v Go); .maven z vsebino
    /// je predpomnilnik Maven in ostane do odstranitve zrcala.
    nonisolated static func removeClone(_ wt: URL) {
        try? FileManager.default.removeItem(at: wt)
        let dir = wt.deletingLastPathComponent()
        if (try? FileManager.default.contentsOfDirectory(atPath: dir.path)) == [".maven"], rmdir(dir.appendingPathComponent(".maven").path) == 0 {
            rmdir(dir.path)
        }
    }

    /// Ob koncu seje: kloni gredo (veje ostanejo v zrcalu in pri odjemalcu).
    func dropSlots(session: String) {
        for (k, slot) in slots where k.hasPrefix(session + "|") && !slot.busy {
            slots[k] = nil
            let wt = slot.wt
            Task.detached { Self.removeClone(wt) }
        }
    }
}
