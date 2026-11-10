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
    /// `stderr`: tudi napake gita v izhod (razlog za uporabnika).
    static func run(_ args: [String], cwd: String? = nil, env extra: [String: String] = [:], safe: Bool = true, hooks: Bool = true,
                    input: Data? = nil, sandbox: String? = nil, timeout: TimeInterval? = nil, trim: Bool = true,
                    stderr: Bool = false) -> (status: Int32, out: String) {
        let guardrails = ["-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "submodule.recurse=false",
                          "-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "maintenance.auto=false", "-c", "gc.auto=0",
                          "-c", "core.commitGraph=false", "-c", "core.multiPackIndex=false"]
        let gitArgs = (safe ? guardrails : hooks ? [] : ["-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"]) + args
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
        if stderr { posix_spawn_file_actions_adddup2(&fa, o[1], 2) } else { posix_spawn_file_actions_addopen(&fa, 2, "/dev/null", O_WRONLY, 0) }
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
        let out = String(decoding: data, as: UTF8.self)
        return (status, trim ? out.trimmingCharacters(in: .whitespacesAndNewlines) : out)
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
    /// `safe` (native na prehodu): brez sistemske in globalne nastavitve, z varovali prehoda.
    public static func toplevel(_ cwd: String, safe: Bool = false) -> String? {
        let r = run(["rev-parse", "--show-toplevel"], cwd: cwd, safe: safe, hooks: false)
        return r.status == 0 && !r.out.isEmpty ? r.out : nil
    }

    /// Repozitorij ima HEAD (brez njega ni posnetka za worktree; auto in worktree tedaj na sync).
    public static func hasCommit(top: String) -> Bool {
        let h = run(["rev-parse", "--verify", "-q", "HEAD^{commit}"], cwd: top, safe: false, hooks: false)
        return h.status == 0 && isSHA(h.out)
    }

    /// Zakaj ni posnetka (nil = je): repozitorij brez commita (kot Go snapshotBlocker). Mapa v njem načina nikoli ne
    /// spremeni: ugnezden repozitorij brez commita gre iz posnetka (nestedNoCommit).
    public static func snapshotBlocker(top: String) -> String? {
        guard hasCommit(top: top) else { return "the git repository \(Profiles.line(top, max: 500)) has no commit yet, so worktree mode is not possible" }
        return nil
    }

    /// Največ ugnezdenih repozitorijev v enem zagonu worktree (več: napaka, način ostane; kot Go maxNestedRepos).
    public static let maxNested = 32

    /// Neprezrti ugnezdeni repozitoriji v top na kateri koli globini (podmoduli z mapo .git in nesledeni, s commitom ali
    /// brez), relativne poti, starš pred otrokom; največ maxNested+1 (kot Go nestedRepos). Prezrte poti (build/_deps) niso tu.
    /// `safe`: native na prehodu (git brez nastavitev sistema in uporabnika, z varovali; globalne izključitve posebej).
    public static func nestedRepos(top: String, safe: Bool = false) -> [String] {
        var out: [String] = []
        func walk(_ rel: String) {
            for p in nestedIn(rel.isEmpty ? top : top + "/" + rel, safe: safe) {
                if out.count > maxNested { return }
                let r = rel.isEmpty ? p : rel + "/" + p
                guard nestedInside(top: top, r, safe: safe) else { continue }  // N2: povezava v poti ali repozitorij zunaj mape
                out.append(r)
                walk(r)
            }
        }
        walk("")
        return out
    }

    /// N2 (kot Go nestedInside): ugnezden repozitorij rel je res v top: vsak del poti je prava mapa (ne povezava), je svoj
    /// koren in leži pod top tudi po razrešitvi poti. Tudi ob prevzemu.
    public static func nestedInside(top: String, _ rel: String, safe: Bool = false) -> Bool {
        var d = URL(fileURLWithPath: top), st = stat()
        for c in rel.split(separator: "/") {
            d = d.appendingPathComponent(String(c), isDirectory: true)
            guard lstat(d.path, &st) == 0, st.st_mode & S_IFMT == S_IFDIR else { return false }
        }
        guard let t = toplevel(d.path, safe: safe), let rt = Sync.real(URL(fileURLWithPath: top))?.path, let rd = Sync.real(d)?.path else { return false }
        return Sync.real(URL(fileURLWithPath: t))?.path == rd && rd.hasPrefix(rt + "/")
    }

    /// Poti gitlinkov (160000) v indeksu repozitorija dir.
    static func gitlinks(_ dir: String, safe: Bool = false) -> Set<String> {
        Set(run(["-c", "core.fsmonitor=false", "ls-files", "-s", "-z"], cwd: dir, safe: safe, hooks: false, trim: false).out
            .split(separator: "\0").compactMap { l in
                let f = l.split(separator: "\t", maxSplits: 1, omittingEmptySubsequences: false)
                return f.count == 2 && f[0].hasPrefix("160000 ") ? String(f[1]) : nil
            })
    }

    /// Neposredni ugnezdeni repozitoriji repozitorija dir: gitlinki iz indeksa (le izvlečeni), nesledene mape (git jih strne
    /// z --directory, zato le v njih .git po datotečnem sistemu in check-ignore).
    static func nestedIn(_ dir: String, safe: Bool = false) -> [String] {
        let ex = safe ? globalExcludes().map { ["-c", "core.excludesFile=" + $0.path] } ?? [] : []
        func z(_ args: [String], _ input: Data? = nil) -> [String] {
            run(["-c", "core.fsmonitor=false"] + ex + args, cwd: dir, safe: safe, hooks: false, input: input, trim: false).out
                .split(separator: "\0").map(String.init)
        }
        func isRepo(_ p: String) -> Bool { var st = stat(); return lstat(dir + "/" + p + "/.git", &st) == 0 }
        var out = gitlinks(dir, safe: safe).filter(isRepo).sorted()
        var deep: [String] = []
        for d in z(["ls-files", "-o", "--exclude-standard", "--directory", "--no-empty-directory", "-z"]) where d.hasSuffix("/") {
            if isRepo(String(d.dropLast())) { out.append(String(d.dropLast())); continue }
            let base = URL(fileURLWithPath: dir).appendingPathComponent(d)
            guard let e = FileManager.default.enumerator(at: base, includingPropertiesForKeys: [.isDirectoryKey, .isSymbolicLinkKey]) else { continue }
            for case let u as URL in e {
                guard let v = try? u.resourceValues(forKeys: [.isDirectoryKey, .isSymbolicLinkKey]), v.isDirectory == true, v.isSymbolicLink != true
                else { continue }
                let rel = d + String(u.path.dropFirst(base.path.count + 1))
                if isRepo(rel) { deep.append(rel + "/"); e.skipDescendants() }  // ugnezdene v njem najde njegov nestedIn
            }
        }
        guard !deep.isEmpty else { return out }
        let ignored = Set(z(["check-ignore", "--stdin", "-z"], Data((deep.joined(separator: "\0") + "\0").utf8)))
        return out + deep.filter { !ignored.contains($0) }.map { String($0.dropLast()) }
    }

    /// Neposredni otroci repozitorija rel (»« = vrhnji) med ugnezdenimi, relativno nanj (kot Go nestedChildren).
    public static func nestedChildren(_ rel: String, _ nested: [String]) -> [String] {
        nested.compactMap { p in
            guard p != rel, rel.isEmpty || p.hasPrefix(rel + "/") else { return nil }
            let inner = nested.contains { q in q != p && p.hasPrefix(q + "/") && (rel.isEmpty || q.hasPrefix(rel + "/")) }
            return inner ? nil : rel.isEmpty ? p : String(p.dropFirst(rel.count + 1))
        }
    }

    /// Relativna pot ugnezdenega repozitorija (odjemalec in prehod; H1, kot Go validNestedPath): deli niso prazni, ».«,
    /// »..« ali .git, brez »\«, »:« in kontrolnih znakov (MINIGW_GIT_DIRS je ločen z novo vrstico).
    public static func validNestedPath(_ p: String) -> Bool {
        guard !p.isEmpty, p.utf8.count <= 1024, !p.contains("\\"), !p.contains(":"),
              !p.unicodeScalars.contains(where: { $0.value < 0x20 || $0.value == 0x7f || Sync.hfsIgnorable($0) }),  // N5
              Sync.normName(String(p.split(separator: "/", omittingEmptySubsequences: false)[0])) != ".build" else { return false }  // N6
        return p.split(separator: "/", omittingEmptySubsequences: false).allSatisfy { c in
            !c.isEmpty && c != "." && c != ".." && !Sync.isGitPath(String(c))
        }
    }

    /// Glava x-nested (i-ti ugnezdeni; 0 = vrhnji brez glave) in njena vrednost na prehodu (-1 = napačna), kot Go.
    public static func nestedHeader(_ h: [String: String], _ idx: Int) -> [String: String] {
        idx > 0 ? h.merging(["x-nested": String(idx)]) { $1 } : h
    }
    static func parseNested(_ v: String?) -> Int {
        guard let v else { return 0 }
        guard let n = Int(v), (0...maxNested).contains(n) else { return -1 }
        return n
    }

    /// Ključ zrcala ugnezdenega repozitorija (odjemalec in prehod, kot Go nestedKey).
    public static func nestedKey(_ p: String) -> String { String(Sync.sha256(Data(p.utf8)).prefix(16)) }

    /// Posnetki za WORKTREE-PUSH (kot Go wtPush): vrhnji in vsak ugnezdeni (brez commita: commit brez starša); neposredni
    /// otroci brez commita gredo iz posnetka starša. `gateway`: prehod pozna ugnezdene (»nested« v zahtevi). Napaka:
    /// (ključ za enkratno sporočilo, sporočilo).
    public static func snapshots(top: String, gateway: Bool)
        -> (snap: String, nested: [(path: String, snapshot: String)], failure: (key: String, message: String)?) {
        func fail(_ k: String, _ m: String) -> (String, [(path: String, snapshot: String)], (key: String, message: String)?) { ("", [], (k, m)) }
        let nested = nestedRepos(top: top)
        if nested.count > maxNested {
            return fail("nested-max", "worktree: \(Profiles.line(top, max: 500)) has more than \(maxNested) nested git repositories; worktree mode takes at most \(maxNested); use sync")
        }
        if let p = nested.first, !gateway {
            return fail("nested-old", "worktree: the gateway does not support nested repositories (\(Profiles.line(p, max: 500))); update the gateway")
        }
        if let p = nested.first(where: { !validNestedPath($0) }) {
            return fail("nested-name", "worktree: the nested repository \(Profiles.line(p, max: 500)) has a name worktree mode cannot use (\":\", \"\\\", control characters or in .build); use sync")
        }
        // R2 (kot Go): filtri ugnezdenega ne tečejo (N3), zato z njimi ne začne (način ostane)
        if let p = nested.first(where: { !run(["config", "--local", "--includes", "--get-regexp", #"^filter\."#], cwd: top + "/" + $0, safe: false, hooks: false).out.isEmpty }) {
            return fail("nested-filter:" + p, "worktree: the nested repository \(Profiles.line(p, max: 500)) defines git filters (filter.* in its .git/config), which minigw does not run for nested repositories; use sync")
        }
        if !nested.isEmpty, !attrSource { // N3 (kot Go): brez --attr-source bi tekli filtri ugnezdenih
            return fail("nested-git", "worktree: nested git repositories need git 2.40 or later; update git or use sync")
        }
        // Neposredni otroci, ki v indeksu starša niso gitlink (nesledeni), ali so brez commita: ne v posnetek starša.
        func skip(_ rel: String) -> [String] {
            let dir = rel.isEmpty ? top : top + "/" + rel, links = gitlinks(dir)
            return nestedChildren(rel, nested).filter { !links.contains($0) || !hasCommit(top: dir + "/" + $0) }
        }
        guard let snap = snapshot(top: top, skip: skip("")) else {
            return fail("snapshot", "worktree: cannot take a snapshot of \(top) (git add or write-tree failed)")
        }
        var out: [(path: String, snapshot: String)] = []
        for p in nested {
            let dir = top + "/" + p
            guard let sn = snapshot(top: dir, root: !hasCommit(top: dir), noAttr: true, skip: skip(p)) else {
                return fail("snapshot:" + p, "worktree: cannot take a snapshot of the nested repository \(Profiles.line(p, max: 500)) (git add or write-tree failed)")
            }
            out.append((p, sn))
        }
        return (snap, out, nil)
    }

    /// Posnetek stanja: HEAD ali commit z neshranjenimi spremembami (sledene in nove, neprezrte), narejen z začasnim
    /// indeksom: indeks, veje in delovna mapa uporabnika ostanejo nespremenjeni. nil = brez HEAD (prazen repozitorij).
    /// skip: poti, ki ne gredo v posnetek (ugnezdeni repozitoriji brez commita).
    /// root: repozitorij brez commita (ugnezden) dobi commit brez starša iz kopije svojega indeksa in delovne mape.
    /// noAttr (N3, ugnezdeni, kot Go): brez atributov, zato git add ne požene filtra clean iz .gitattributes.
    /// Git pred 2.40 (GIT_ATTR_SOURCE tiho prezre): nil.
    public static func snapshot(top: String, root: Bool = false, noAttr: Bool = false, skip: [String] = []) -> String? {
        if noAttr, !attrSource { return nil }
        let h = run(["rev-parse", "--verify", "-q", "HEAD^{commit}"], cwd: top, safe: false, hooks: false)
        guard root || h.status == 0 && isSHA(h.out) else { return nil }
        let idx = FileManager.default.temporaryDirectory.appendingPathComponent("minigw-index-" + UUID().uuidString).path
        defer { unlink(idx) }
        let env = ["GIT_INDEX_FILE": idx].merging(noAttr ? ["GIT_ATTR_SOURCE": "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "GIT_CONFIG_COUNT": "1",
                                                     "GIT_CONFIG_KEY_0": "core.attributesFile", "GIT_CONFIG_VALUE_0": "/dev/null"] : [:]) { $1 }
        if root {
            let p = run(["rev-parse", "--git-path", "index"], cwd: top, safe: false, hooks: false).out
            if !p.isEmpty, let d = Sync.read(URL(fileURLWithPath: p.hasPrefix("/") ? p : top + "/" + p), max: 1 << 30) {
                guard FileManager.default.createFile(atPath: idx, contents: d, attributes: [.posixPermissions: 0o600]) else { return nil }
            }
        } else if run(["read-tree", "HEAD"], cwd: top, env: env, safe: false, hooks: false).status != 0 { return nil }
        guard run(["add", "-A", "--", "."] + skip.map { ":(exclude,literal)" + $0 }, cwd: top, env: env, safe: false, hooks: false).status == 0,
              case let t = run(["write-tree"], cwd: top, env: env, safe: false, hooks: false), t.status == 0, isSHA(t.out) else { return nil }
        if !root, run(["rev-parse", "HEAD^{tree}"], cwd: top, safe: false, hooks: false).out == t.out { return h.out }
        let c = run(["-c", "user.name=minigw", "-c", "user.email=minigw@localhost", "commit-tree", t.out] + (root ? [] : ["-p", h.out])
                    + ["-m", "minigw: your uncommitted changes"], cwd: top, safe: false, hooks: false)
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

    /// Vrstica o dostavljeni veji (enaka v odjemalcu Go): le delo agenta, brez posnetka uporabnika. nested: pot
    /// ugnezdenega repozitorija (M2: stroga oblika le za pot iz safePath, drugače vrstica brez ukazov).
    public static func branchLine(branch: String, base: String, nested: String? = nil) -> String {
        let r = base + ".." + branch
        guard let p = nested else {
            return "agent work is on branch \(branch): review with git diff \(r) (\(base) = your state when the agent started), "
                + "then bring it in with git cherry-pick \(r) or git diff --binary \(r) | git apply"
        }
        guard p.range(of: safePath, options: .regularExpression) != nil else {
            return "the agent's work in a nested repository (its name has characters minigw does not print in a command) is on branch \(branch) there, starting at \(base)"
        }
        let g = "git -C \(p) "
        return "agent work is on branch \(branch) in nested repository \(p): review with \(g)diff \(r) (\(base) = your state in \(p) when the agent started), "
            + "then bring it in with \(g)cherry-pick \(r) or \(g)diff --binary \(r) | \(g)apply"
    }

    /// Git pozna GIT_ATTR_SOURCE (2.40+, Apple git v Xcode CLT je lahko starejši); starejši --attr-source zavrne.
    /// TaskLocal: testi prehoda simulirajo starejši git.
    @TaskLocal static var attrSource = run(["--attr-source=4b825dc642cb6eb9a060e54bf8d69288fbee4904", "version"], safe: false, hooks: false).status == 0

    /// #57: ugnezdeni repozitoriji na prehodu le z git 2.40+ (harnessEnv: GIT_ATTR_SOURCE starejši tiho prezre). Zavrne pred
    /// zagonom agenta, način ostane. nil = v redu.
    static func nestedGitProblem(_ nested: Int, attr: Bool = attrSource, or alternative: String = "use sync") -> String? {
        nested > 0 && !attr ? "nested git repositories need git 2.40 or later on the gateway; update git or \(alternative)" : nil
    }

    /// fetch iz bundla pred git 2.46 ne preveri objektov (fetch.fsckObjects prezre). Zato novi objekti `ref`
    /// (brez dosegljivih iz drugih refov) v fsckPack. Kot Go fsckIncoming. nil = v redu, sicer razlog.
    static func fsckIncoming(top: String, ref: String = "refs/minigw/incoming", safe: Bool = false) -> String? {
        let refs = run(["for-each-ref", "--format=%(objectname) %(refname)"], cwd: top, safe: safe, hooks: false)
        guard refs.status == 0 else { return "cannot list refs" }
        let not = refs.out.split(separator: "\n").compactMap { l -> String? in
            let f = l.split(separator: " ", maxSplits: 1).map(String.init)
            return f.count == 2 && isSHA(f[0]) && f[1] != ref ? f[0] + "\n" : nil
        }.joined()
        return fsckPack(top: top, revs: "\(ref)\n--not\n\(not)", safe: safe)
    }

    /// Objekti `revs` (vhod pack-objects --revs): pack-objects in index-pack --fsck-objects v prazen repozitorij (kot
    /// finalize), objekti repozitorija kot alternates (drevo sme kazati na obstoječ blob). Zavrne npr. drevo z ».git«.
    /// #58: skupna meja `until` (privzeto 10 min) za oba koraka (velik repozitorij, zastal disk). nil = v redu, sicer razlog.
    static func fsckPack(top: String, revs: String, safe: Bool = false, until: Date = Date() + 600) -> String? {
        let o = run(["rev-parse", "--git-path", "objects"], cwd: top, safe: safe, hooks: false)
        guard o.status == 0, !o.out.isEmpty else { return "cannot find the objects folder" }
        var objs = o.out
        if !objs.hasPrefix("/") { objs = top + "/" + objs }
        // Mapa ob objects (kot Go fsckBase): pack-objects sestavi paket v objects/pack in ga preimenuje v dir; začasna mapa
        // na drugem disku (Scratch na zunanjem disku, repozitorij zunaj $TMPDIR) da »Cross-device link« in vsak uvoz pade.
        let dir = URL(fileURLWithPath: objs).deletingLastPathComponent().appendingPathComponent("minigw-fsck-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        guard (try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])) != nil
        else { return "cannot create a temporary folder" }
        func why(_ r: (status: Int32, out: String)) -> String {
            r.out.split(separator: "\n").first.map { Profiles.line(String($0), max: 300) } ?? "git exited with \(r.status)"
        }
        let p = run(["pack-objects", "--revs", "-q", dir.appendingPathComponent("p").path], cwd: top, safe: safe, hooks: false,
                    input: Data(revs.utf8), timeout: max(1, until.timeIntervalSinceNow), stderr: true)
        guard p.status == 0 else { return why(p) }
        let pack = String(p.out.split(separator: "\n").last ?? "")  // pred njim morda opozorila gita
        guard isSHA(pack) else { return "pack-objects gave no pack" }
        let fsck = dir.appendingPathComponent("fsck.git").path
        guard run(["init", "-q", "--bare", fsck], cwd: dir.path).status == 0,
              FileManager.default.createFile(atPath: fsck + "/objects/info/alternates", contents: Data((objs + "\n").utf8)) else {
            return "cannot create the check repository"
        }
        let r = run(["--git-dir", fsck, "index-pack", "--fsck-objects", dir.appendingPathComponent("p-\(pack).pack").path], cwd: dir.path,
                    timeout: max(1, until.timeIntervalSinceNow), stderr: true)
        return r.status == 0 ? nil : why(r)
    }

    /// Pot ugnezdenega repozitorija, ki gre v vrstico o veji (enaka v skillsih in odjemalcu Go).
    static let safePath = #"^[A-Za-z0-9_][A-Za-z0-9._+-]*(/[A-Za-z0-9_][A-Za-z0-9._+-]*)*$"#

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
        if let why = fsckIncoming(top: top) { return "the gateway's branch failed the object check (fsck): " + why }  // #58
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
    /// #40 (kot Go): brez lastnikovih nastavitev git (identiteta, podpis, poverilnice, insteadOf; v peskovniku ~ da EPERM),
    /// avtor commitov je agent (`agent`: claude, codex …).
    public static func harnessEnv(root: URL, maven: URL, agent: String) -> [String: String] {
        // N9: status in diff ne vstopita v podmodul (submodule.<ime>.ignore iz .gitmodules to preglasi: claudeEnv)
        let pairs = [("core.fsmonitor", "false"), ("core.hooksPath", "/dev/null"), ("core.attributesFile", "/dev/null"),
                     ("diff.ignoreSubmodules", "all"), ("submodule.recurse", "false"), ("core.excludesFile", "/dev/null"),
                     ("user.name", author(agent)), ("user.email", agent + "@minigw.invalid")]
        let git = root.appendingPathComponent(".git").path
        var env = ["GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
                   "GIT_CONFIG_COUNT": String(pairs.count), "GIT_ATTR_SOURCE": "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
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
    /// `gits`: .git vseh klonov (vrhnji in ugnezdeni, ShareHub.worktree); prazno = le vrhnji.
    /// #40: PATH najprej `gitexec`/bin (pathWrapScript) in brez GIT_DIR, GIT_WORK_TREE, GIT_COMMON_DIR vrhnjega klona (git -C
    /// <ugnezdeni> bi sicer delal v vrhnjem; ovoj jih nastavi za vsak klic, git mimo njega najde repozitorij sam, ne nad
    /// klonom: GIT_CEILING_DIRECTORIES). Harness Codex (zunaj peskovnika) jih obdrži, ukazi dobijo ovoj prek Codex.args.
    /// `path`: PATH procesa (kot Go worktreeClaudeEnv).
    public static func claudeEnv(root: URL, maven: URL, gitexec: URL, gits: [String] = [], agent: String,
                                 path: String = ProcessInfo.processInfo.environment["PATH"] ?? "") -> [String: String]? {
        guard let wrap = wrapEnv(gitDirs: gits.isEmpty ? [root.appendingPathComponent(".git").path] : gits, gitexec: gitexec) else { return nil }
        let env = harnessEnv(root: root, maven: maven, agent: agent).merging(wrap) { $1 }
        if agent == "codex" { return env }
        return env.filter { !["GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR"].contains($0.key) }.merging(
            ["PATH": agentPath(gitexec: gitexec, path: path), "MINIGW_CLONE": root.path, "GIT_CEILING_DIRECTORIES": root.deletingLastPathComponent().path]) { $1 }
    }

    /// PATH agenta, najprej ovoj `gitexec`/bin; L2: le absolutne poti (kot Go agentPath).
    static func agentPath(gitexec: URL, path: String) -> String {
        let rest = path.split(separator: ":").map(String.init).filter { $0.hasPrefix("/") }
        return ([gitexec.appendingPathComponent("bin").path] + (rest.isEmpty ? ["/usr/bin", "/bin"] : rest)).joined(separator: ":")
    }

    /// Ime avtorja commitov agenta (kot Go agentAuthor).
    static func author(_ agent: String) -> String { agent.prefix(1).uppercased() + agent.dropFirst() + " (Mini Gateway)" }

    /// #40 (enak kot Go pathWrapScript, testdata/pathwrap.golden): »git« na PATH agenta dela v registriranem klonu
    /// (MINIGW_GIT_DIRS), ki vsebuje trenutno mapo ali pot -C; drugače v vrhnjem (prvi vnos).
    static func pathWrapScript(real: String) -> String {
        "#!/bin/sh\n# minigw: git works in the registered clone (MINIGW_GIT_DIRS) that holds the folder or the -C <path>\n"
            + "real='" + real.replacingOccurrences(of: "'", with: #"'\''"#) + #"'; d=$(pwd -P) || exec "$real" "$@""# + "\n"
            + #"i=1; while [ $i -le $# ]; do eval "a=\${$i}"; case $a in"# + "\n"
            + #"  -C) i=$((i+1)); eval "p=\${$i}"; case $p in /*) ;; *) p=$d/$p;; esac; q=$(cd "$p" 2>/dev/null && pwd -P) && d=$q || break;;"# + "\n"
            + #"  -c) i=$((i+1));; -*) ;; *) break;; esac; i=$((i+1)); done"# + "\n"
            + "b=; g=; set -f; IFS='\n'\n"
            + #"for e in $MINIGW_GIT_DIRS; do w=$(cd "${e%/.git}" 2>/dev/null && pwd -P) || continue; case $d/ in "$w"/*) [ ${#w} -gt ${#b} ] && b=$w g=$e;; esac; done"# + "\n"
            + #"[ -n "$g" ] || g=${MINIGW_GIT_DIRS%%'"# + "\n" + #"'*}; [ -n "$g" ] && export GIT_DIR="$g" GIT_COMMON_DIR="$g" GIT_WORK_TREE="${g%/.git}""# + "\n"
            + #"exec "$real" "$@""# + "\n"
    }

    /// GIT_EXEC_PATH z ovojem v `gitexec` in MINIGW_GIT_DIRS (edini GIT_DIR, ki jih ovoj pusti; absolutne poti brez nove
    /// vrstice); nil = ovoja ni (kot Go gitWrapEnv).
    static func wrapEnv(gitDirs: [String], gitexec: URL) -> [String: String]? {
        guard !gitDirs.isEmpty, gitDirs.allSatisfy({ $0.hasPrefix("/") && !$0.contains("\n") && !$0.contains("\r") }), gitExecWrap(gitexec) else { return nil }
        return ["GIT_EXEC_PATH": gitexec.path, "MINIGW_GIT_DIRS": gitDirs.joined(separator: "\n")]
    }

    /// Mapa ovoja v Scratch zrcala `repo` (scratch/repos/<naprava>/<id>.git → scratch/gitexec), kot config.scratch("gitexec").
    static func gitexecDir(repo: URL) -> URL {
        repo.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("gitexec", isDirectory: true)
    }

    private static let wrapLock = NSLock()

    static func gitExecWrap(_ dir: URL) -> Bool {
        wrapLock.lock(); defer { wrapLock.unlock() }
        let real = run(["--exec-path"]).out.trimmingCharacters(in: .whitespacesAndNewlines)
        guard real.hasPrefix("/") else { return false }
        let fm = FileManager.default, wrapper = dir.appendingPathComponent("git"), pathWrapper = dir.appendingPathComponent("bin/git")
        // Enak kot Go wrapScript: točno ujemanje GIT_DIR z enim od nepraznih vnosov MINIGW_GIT_DIRS.
        let script = "#!/bin/sh\n# minigw (N9): git runs only in the clones minigw registered, not in a nested repository the agent made\n"
            + "ok=; set -f; IFS='\n'\n"
            + #"for d in $MINIGW_GIT_DIRS; do [ -n "$GIT_DIR" ] && [ "$GIT_DIR" = "$d" ] && ok=1; done"# + "\n"
            + #"[ -n "$ok" ] || { echo "minigw: git in a nested repository is off in worktree mode" >&2; exit 128; }"# + "\n"
            + "exec '" + (real + "/git").replacingOccurrences(of: "'", with: #"'\''"#) + #"' "$@""# + "\n"
        let path = pathWrapScript(real: real + "/git")
        if fm.contents(atPath: wrapper.path) == Data(script.utf8), fm.contents(atPath: pathWrapper.path) == Data(path.utf8) { return true }
        try? fm.removeItem(at: dir)
        guard (try? fm.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])) != nil,
              let names = try? fm.contentsOfDirectory(atPath: real) else { return false }
        for n in names where n != "git" {
            guard (try? fm.createSymbolicLink(atPath: dir.appendingPathComponent(n).path, withDestinationPath: real + "/" + n)) != nil else { return false }
        }
        return fm.createFile(atPath: wrapper.path, contents: Data(script.utf8), attributes: [.posixPermissions: 0o700])
            && (try? fm.createDirectory(at: pathWrapper.deletingLastPathComponent(), withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])) != nil
            && fm.createFile(atPath: pathWrapper.path, contents: Data(path.utf8), attributes: [.posixPermissions: 0o700])
    }

    /// N9 (kot Go nestedProblem): .git (mapa ali datoteka) pod korenom klona na poti, ki je git ne prezre, razen točno
    /// klonov ugnezdenih repozitorijev odjemalca (`nested`, vsak s svojim zrcalom): sporočilo, zakaj veja ne gre nazaj;
    /// nil = ni ga ali je v prezrti poti (build/_deps od CMake FetchContent). O prezrtju odloči git repozitorija, v katerem
    /// je (check-ignore z ovojem in nastavitvami njegovega zrcala, kot finalize: .git klona je agentov). V vsakem klonu se
    /// izpusti le točno njegov ».git«.
    static func nestedProblem(_ wt: URL, repo: URL, nested: [ShareHub.Nested] = []) -> String? {
        let own = Set(nested.map { $0.dir(wt).standardizedFileURL.path })
        func check(_ root: URL, _ repo: URL, _ prefix: String) -> String? {
            guard let e = FileManager.default.enumerator(at: root, includingPropertiesForKeys: nil) else { return nil }
            var parts: [String] = [], found: [String] = []
            for case let u as URL in e {
                parts = Array(parts.prefix(e.level - 1)) + [u.lastPathComponent]
                if e.level == 1, u.lastPathComponent == ".git" { e.skipDescendants(); continue }
                if own.contains(root.appendingPathComponent(parts.joined(separator: "/")).standardizedFileURL.path) { e.skipDescendants(); continue }
                guard u.lastPathComponent.lowercased() == ".git" else { continue }
                found.append(parts.dropLast().joined(separator: "/") + "/")
                e.skipDescendants()
            }
            guard !found.isEmpty else { return nil }
            var ignored = Set<String>()
            if let env = wrapEnv(gitDirs: [repo.path], gitexec: gitexecDir(repo: repo)) {
                let out = run(["--git-dir", repo.path, "--work-tree", root.path, "check-ignore", "--stdin"], cwd: root.path, env: env, input: Data((found.joined(separator: "\n") + "\n").utf8)).out
                ignored = Set(out.split(separator: "\n").map(String.init))
            }
            return found.first { !ignored.contains($0) }.map {
                "changes inside the nested repository \(prefix)\($0.dropLast()) do not come back in worktree mode; use sync"
            }
        }
        if let why = check(wt, repo, "") { return why }
        for n in nested { if let why = check(n.dir(wt), n.repo, n.path + "/") { return why } }
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
            guard isBundle(bundle) else { return logFail(repo, "the client did not send a git bundle") }
            let f = run(["--git-dir", repo.path, "-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true",
                         "fetch", "-q", "--no-tags", bundle.path, "+refs/minigw/push:\(base)"], stderr: true)
            guard f.status == 0 else { return logFail(repo, "git fetch of the client's bundle", f.out) }
        } else {
            let u = run(["--git-dir", repo.path, "update-ref", base, snapshot], stderr: true)
            guard u.status == 0 else { return logFail(repo, "the snapshot is not in the mirror", u.out) }
        }
        // #57: fetch iz bundla pred git 2.46 ne preveri objektov; brez bundla sme posnetek kazati na ostanek zavrnjenega
        // bundla v zrcalu. Dosegljivo iz drugih refov: prazen paket.
        if let why = fsckIncoming(top: repo.path, ref: base, safe: true) {
            run(["--git-dir", repo.path, "update-ref", "-d", base])
            return logFail(repo, "the client's snapshot failed the object check (fsck)", why)
        }
        return run(["--git-dir", repo.path, "rev-parse", base]).out == snapshot || logFail(repo, "the bundle does not hold the snapshot")
    }

    /// Neuspel korak uvoza ali klona v dnevnik routerja s prvo vrstico gita (nikoli 502 brez razloga); vedno false.
    static func logFail(_ repo: URL, _ step: String, _ git: String = "") -> Bool {
        let line = git.split(separator: "\n").first.map { ": " + Profiles.line(String($0), max: 300) } ?? ""
        Router.log.error("worktree \(repo.lastPathComponent, privacy: .public): \(step, privacy: .public)\(line, privacy: .public)")
        return false
    }

    /// Pot kot dobesedni vzorec .gitignore (kot Go excludeEscape).
    static func excludeEscape(_ p: String) -> String {
        var r = ""
        for (i, ch) in p.enumerated() {
            if "*?[\\".contains(ch) || ch == " " && i == p.count - 1 { r.append("\\") }
            r.append(ch)
        }
        return r
    }

    /// Golo zrcalo ugnezdenega repozitorija na poti p ob zrcalu repo (<id>-<ključ>.git; prune ga obravnava kot vsako zrcalo).
    static func nestedMirror(_ repo: URL, _ p: String) -> URL {
        repo.deletingLastPathComponent().appendingPathComponent(String(repo.lastPathComponent.dropLast(4)) + "-" + nestedKey(p) + ".git", isDirectory: true)
    }

    /// H1 (kot Go cloneTarget): klon ugnezdenega repozitorija gre v wt/p: vsak obstoječ del poti je prava mapa (ne povezava),
    /// cilj ne obstaja ali je prazna mapa (gitlink).
    static func cloneTarget(_ wt: URL, _ p: String) -> Bool {
        var d = wt, st = stat()
        let parts = p.split(separator: "/")
        for (i, c) in parts.enumerated() {
            d = d.appendingPathComponent(String(c), isDirectory: true)
            if lstat(d.path, &st) != 0 { return errno == ENOENT }
            guard st.st_mode & S_IFMT == S_IFDIR else { return false }
            if i == parts.count - 1 { return (try? FileManager.default.contentsOfDirectory(atPath: d.path))?.isEmpty == true }
        }
        return false
    }

    /// Klon za agenta na veji `branch` iz posnetka (objekti zrcala le za branje, novi v klonu).
    static func makeClone(_ repo: URL, at wt: URL, branch: String, snapshot: String) -> Bool {
        try? FileManager.default.removeItem(at: wt)  // ostanek prekinjenega zagona
        try? FileManager.default.createDirectory(at: wt.deletingLastPathComponent(), withIntermediateDirectories: true,
                                                 attributes: [.posixPermissions: 0o700])
        let c = run(["clone", "-q", "--shared", "--no-checkout", repo.path, wt.path], stderr: true)
        guard c.status == 0 else { return logFail(repo, "git clone of the mirror", c.out) }
        let o = run(["-C", wt.path, "checkout", "-q", "-b", branch, snapshot], stderr: true)
        return o.status == 0 || logFail(repo, "git checkout of the snapshot", o.out)
    }

    /// Native z git (repozitorij na tem Macu): posnetek mape uporabnika v zrcalu kot `base`, brez git v njegovem
    /// repozitoriju. Objekti prek alternates zrcala (le branje), HEAD iz datotek; neshranjene spremembe z začasnim indeksom
    /// zrcala in njegovimi nastavitvami (brez filtrov, hookov, fsmonitor), v peskovniku (branje le mapa, zrcalo, začasna
    /// mapa), izključitve še iz .git/info/exclude uporabnika. Mapa, indeks, HEAD in veje uporabnika ostanejo nespremenjeni.
    /// ponytail: datoteke LFS gredo v posnetek kot vsebina (zrcalo nima filtra lfs).
    /// `gitDir` (ugnezden, localGitDir): git mapa repozitorija; brez HEAD (brez commita) commit brez starša iz delovne mape.
    /// `skip`: neposredni ugnezdeni otroci, ki ne gredo v posnetek (nesledeni ali brez commita).
    static func localSnapshot(repo: URL, top: URL, base: String, excludes: URL? = globalExcludes(), gitDir: URL? = nil, skip: [String] = []) -> String? {
        let dotGit = gitDir ?? top.appendingPathComponent(".git")
        guard let realTop = Sync.real(top), realTop.path == top.path, Sync.real(dotGit)?.path == dotGit.path,
              gitDir != nil || dotGit.path == top.path + "/.git", let objects = Sync.real(dotGit.appendingPathComponent("objects")),
              objects.path == dotGit.path + "/objects", ensureMirror(repo) else { return nil }
        let headSHA = head(dotGit)
        guard headSHA != nil || gitDir != nil else { return nil }
        let info = repo.appendingPathComponent("objects/info")
        try? FileManager.default.createDirectory(at: info, withIntermediateDirectories: true)
        guard (try? Data((objects.path + "\n").utf8).write(to: info.appendingPathComponent("alternates"))) != nil else { return nil }
        FileManager.default.createFile(atPath: repo.appendingPathComponent("minigw-used").path, contents: nil)  // prune: zadnja raba
        let tmp = repo.deletingLastPathComponent().appendingPathComponent(".snapshot-" + UUID().uuidString, isDirectory: true)
        guard (try? FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])) != nil,
              let box = finalizeProfile(wt: top, repo: repo, tmp: tmp, out: tmp.appendingPathComponent("unused"), read: gitDir.map { [$0] } ?? []) else { return nil }
        defer { try? FileManager.default.removeItem(at: tmp) }
        let g = ["--git-dir", repo.path], e = ["GIT_INDEX_FILE": tmp.appendingPathComponent("index").path, "GIT_WORK_TREE": top.path]
        // M2: izključitve repozitorija (.git/info/exclude) in globalne uporabnika (core.excludesFile, ~/.config/git/ignore)
        let exclude = tmp.appendingPathComponent("exclude")
        let sources = [dotGit.appendingPathComponent("info/exclude"), excludes].compactMap { $0 }
        try? sources.compactMap { Sync.read($0, max: 1 << 20) }.reduce(Data()) { $0 + $1 + Data("\n".utf8) }.write(to: exclude)
        func git(_ a: [String], cwd: String? = nil) -> (status: Int32, out: String) {
            run(g + a, cwd: cwd ?? tmp.path, env: e, sandbox: box, timeout: 600)  // L2
        }
        guard git(headSHA.map { ["read-tree", $0] } ?? ["read-tree", "--empty"]).status == 0,
              git(["-c", "core.excludesFile=" + exclude.path, "add", "-A", "--", "."] + skip.map { ":(exclude,literal)" + $0 }, cwd: top.path).status == 0,
              case let t = git(["write-tree"]), isSHA(t.out) else { return nil }
        var snap = headSHA ?? ""
        if headSHA == nil || git(["rev-parse", snap + "^{tree}"]).out != t.out {
            let c = git(["-c", "user.name=minigw", "-c", "user.email=minigw@localhost", "commit-tree", t.out] + (headSHA.map { ["-p", $0] } ?? [])
                        + ["-m", "minigw: your uncommitted changes"])
            guard isSHA(c.out) else { return nil }
            snap = c.out
        }
        return git(["update-ref", base, snap]).status == 0 ? snap : nil
    }

    /// Native z git: bundle veje za repozitorij uporabnika. Posnetek (z njegovimi neshranjenimi spremembami) je le v zrcalu,
    /// zato je pogoj trenutni HEAD uporabnika (ta ga ima), ne posnetek.
    /// #57: objekte bundla pred tem preveri fsckPack (fetch iz bundla pred git 2.46 jih ne). nil = v redu, sicer razlog.
    static func localBundle(repo: URL, top: URL, branch: String, out: URL, gitDir: URL? = nil) -> String? {
        try? FileManager.default.removeItem(at: out)
        let user = head(gitDir ?? top.appendingPathComponent(".git")), until = Date() + 600  // skupna meja za fsck in bundle
        if let why = fsckPack(top: repo.path, revs: "refs/heads/\(branch)\n" + (user.map { "--not\n\($0)\n" } ?? ""), safe: true, until: until) {
            return "\(branch) failed the object check (fsck): " + why
        }
        let have = user.map { ["^" + $0] } ?? []
        return run(["--git-dir", repo.path, "bundle", "create", out.path, "refs/heads/" + branch] + have, timeout: max(1, until.timeIntervalSinceNow)).status == 0 && isBundle(out)
            ? nil : "cannot bundle \(branch)"
    }

    /// Native z git: veja iz bundla naravnost v repozitorij uporabnika v enem koraku (H1). git le s pravili prehoda (brez
    /// sistemske in globalne nastavitve; fsmonitor, alternateRefsCommand, hooki izklopljeni z ukazno vrstico), v peskovniku:
    /// izvajanje le git in njegovih pomožnih programov, pisanje le v .git. Brez »+«: le naprej, izvlečena veja zavrnjena.
    /// Pred tem znova: .git je prava mapa (ne povezava ali datoteka gitdir), brez commondir. nil = uspeh, sicer razlog.
    /// `gitDir` (ugnezden, localGitDir): git mapa, podana gitu izrecno (--git-dir).
    static func localFetch(top: URL, bundle: URL, branch: String, gitDir: URL? = nil) -> String? {
        let dotGit = gitDir ?? top.appendingPathComponent(".git")
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
                     "--git-dir", dotGit.path, "fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--quiet",
                     bundle.path, "refs/heads/\(branch):refs/heads/\(branch)"], cwd: top.path, safe: true, sandbox: box, timeout: 300)
        return r.status == 0 ? nil
            : "\(branch) was not updated in \(top.path) (it has commits the agent's version does not have, or it is checked out)"
    }

    /// Native (A): git mapa ugnezdenega repozitorija p v top: .git kot prava mapa ali datoteka »gitdir: …« (podmodul) na
    /// pravo mapo pod top (navadno .git/modules), z objects kot pravo mapo in brez commondir; pot sama brez povezav. nil sicer.
    static func localGitDir(top: URL, _ p: String) -> URL? {
        guard let rt = Sync.real(top), rt.path == top.path, validNestedPath(p) else { return nil }
        var d = top, st = stat()
        for c in p.split(separator: "/") {
            d = d.appendingPathComponent(String(c), isDirectory: true)
            guard lstat(d.path, &st) == 0, st.st_mode & S_IFMT == S_IFDIR else { return nil }
        }
        let dot = d.appendingPathComponent(".git")
        guard lstat(dot.path, &st) == 0 else { return nil }
        var g = dot
        if st.st_mode & S_IFMT == S_IFREG {
            guard let t = Sync.read(dot, max: 4096).map({ String(decoding: $0, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) }),
                  t.hasPrefix("gitdir: ") else { return nil }
            let target = String(t.dropFirst(8))
            g = target.hasPrefix("/") ? URL(fileURLWithPath: target) : d.appendingPathComponent(target)
        } else if st.st_mode & S_IFMT != S_IFDIR { return nil }
        guard let rg = Sync.real(g), rg.path.hasPrefix(rt.path + "/"), lstat(rg.path, &st) == 0, st.st_mode & S_IFMT == S_IFDIR,
              Sync.real(rg.appendingPathComponent("objects"))?.path == rg.path + "/objects",
              lstat(rg.appendingPathComponent("commondir").path, &st) != 0 else { return nil }
        return rg
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
    /// `links`: neposredni ugnezdeni klon (pot v wt) → njegov novi vrh (»« = nespremenjen): git add ga izpusti, gitlink na
    /// tej poti (podmodul) dobi novi vrh (kot Go finalize).
    /// `exact` (N1, ugnezdeni): wt je pričakovana prava pot (iz korena klona prehoda); vsaka povezava v njej = brez rezultata,
    /// peskovnik git dovoli branje le tam (zamenjava s povezavo med tekom ne prebere ničesar drugega).
    static func finalize(repo: URL, wt: URL, branch: String, base: String, agent: String, out: URL, read: [URL] = [],
                         links: [String: String] = [:], exact: Bool = false) -> String? {
        killProcesses(in: wt)
        if exact, Sync.real(wt)?.path != wt.path { return nil }
        if exact { duringFinalize?() }
        // .git in objects klona morata biti pravi mapi (ne povezavi ven): sicer brisanje in pisanje spodaj zadeneta tujo mapo.
        // exact (N1): pot je že prava; po tej preverbi se nič več ne razreši (O_NOFOLLOW_ANY, profil iz te poti), zato
        // poznejša povezava naleti na zavrnitev v peskovniku.
        let dotGit = wt.appendingPathComponent(".git"), objectsDir = dotGit.appendingPathComponent("objects")
        let realObjects: String
        if exact {
            realObjects = objectsDir.path
        } else {
            guard Sync.real(dotGit)?.path == (Sync.real(wt)?.path).map({ $0 + "/.git" }),
                  Sync.real(objectsDir)?.path == (Sync.real(wt)?.path).map({ $0 + "/.git/objects" }),
                  let r = Sync.real(objectsDir)?.path else { return nil }
            realObjects = r
        }
        // Brisanje in zapis relativno na deskriptor mape objects (O_NOFOLLOW_ANY): zamenjava poti s povezavo med preverjanjem
        // in dejanjem ne preusmeri ničesar.
        guard case let objFd = open(realObjects, O_RDONLY | O_DIRECTORY | O_NOFOLLOW_ANY), objFd >= 0 else { return nil }
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
        // N9: tudi git prehoda v klonu ne vstopi v ugnezden repozitorij (ovoj, berljiv v peskovniku; --git-dir spodaj je
        // GIT_DIR otrok, npr. pack-objects)
        guard let wrap = wrapEnv(gitDirs: [repo.path], gitexec: gitexecDir(repo: repo)),
              let box = finalizeProfile(wt: wt, repo: repo, tmp: tmp, out: out, read: read + [gitexecDir(repo: repo)], exact: exact) else { return nil }
        let objects = wt.appendingPathComponent(".git/objects").path
        let env = ["GIT_ALTERNATE_OBJECT_DIRECTORIES": objects].merging(wrap) { $1 }
        func run(_ a: [String], cwd: String? = nil, env: [String: String], input: Data? = nil) -> (status: Int32, out: String) {
            Worktree.run(a, cwd: cwd ?? tmp.path, env: env, input: input, sandbox: box)  // trenutna mapa routerja je lahko v domači (zavrnjena)
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
        func linked() -> Bool {
            for (p, t) in links where !t.isEmpty {
                let l = run(g + ["ls-files", "-s", "--", ":(literal)" + p], env: e).out
                if l.hasPrefix("160000 "), !l.contains("\n") { _ = run(g + ["update-index", "--cacheinfo", "160000,\(t),\(p)"], env: e) }
            }
            return true
        }
        if run(g + ["read-tree", tip], env: e).status == 0,
           run(g + ["add", "-A", "--", "."] + links.keys.map { ":(exclude,literal)" + $0 }, cwd: wt.path, env: e).status == 0, linked(),
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
        // »bundle unbundle« ne pozna fsck: fetch iz bundla (fetch.fsckObjects) v začasni ref, brez objektov klona. Ref veje pred
        // tem nazaj: ko je agent vse commital sam, kaže na commit le v klonu (preverba povezanosti bi padla), potem na vrh.
        func restore() { _ = run(g + (isSHA(prev.out) ? ["update-ref", "refs/heads/" + branch, prev.out] : ["update-ref", "-d", "refs/heads/" + branch]), env: [:]) }
        // I1 (kot Go): fsck vseh objektov base..tip (pack-objects, index-pack --fsck-objects v praznem repozitoriju; fetch
        // bi objekte, ki jih je git add že zapisal v zrcalo, preskočil), nato objekti v zrcalo (unbundle) brez objektov klona.
        let created = run(g + ["bundle", "create", out.path, "refs/heads/" + branch, "^" + base], env: env).status == 0 && isBundle(out)
        let pack = created ? run(g + ["pack-objects", "--revs", "-q", tmp.appendingPathComponent("p").path], env: env,
                                 input: Data("\(tip)\n^\(base)\n".utf8)).out : ""
        let fsck = tmp.appendingPathComponent("fsck.git").path
        let checked = isSHA(pack) && run(["init", "-q", "--bare", fsck], env: [:]).status == 0
            && run(["--git-dir", fsck, "index-pack", "--fsck-objects", tmp.appendingPathComponent("p-\(pack).pack").path], env: [:]).status == 0
        if created { restore() }
        let fetched = checked && run(g + ["bundle", "unbundle", out.path], env: [:]).status == 0
        guard fetched, run(g + ["update-ref", "refs/heads/" + branch, tip], env: [:]).status == 0 else {
            restore()
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
    /// `exact` (N1): wt je že prava pot; uporabi se, kot je (brez ponovne razrešitve, ki bi sledila podtaknjeni povezavi).
    static func finalizeProfile(wt: URL, repo: URL, tmp: URL, out: URL, read: [URL] = [], exact: Bool = false) -> String? {
        guard let w = exact ? wt.path : Sync.real(wt)?.path, let r = Sync.real(repo)?.path, let t = Sync.real(tmp)?.path,
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
    /// N1: tudi proces, ki je zapustil klon (cd /) ali sejo: okolje agenta nosi GIT_WORK_TREE klona (harnessEnv).
    static func killProcesses(in wt: URL) {
        guard let root = Sync.real(wt)?.path else { return }
        let marks: Set<String> = ["GIT_WORK_TREE=" + wt.path, "GIT_WORK_TREE=" + root]
        let n = proc_listallpids(nil, 0)
        guard n > 0 else { return }
        var pids = [pid_t](repeating: 0, count: Int(n) + 64)
        let count = proc_listallpids(&pids, Int32(pids.count * MemoryLayout<pid_t>.size))
        for pid in pids.prefix(max(0, Int(count))) where pid > 1 && pid != getpid() {
            if !marks.isDisjoint(with: procStrings(pid)) { kill(pid, SIGKILL); continue }
            var info = proc_vnodepathinfo()
            guard proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, &info, Int32(MemoryLayout<proc_vnodepathinfo>.size)) > 0 else { continue }
            let cwd = withUnsafeBytes(of: info.pvi_cdir.vip_path) { String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) }
            if cwd == root || cwd.hasPrefix(root + "/") { kill(pid, SIGKILL) }
        }
    }

    /// Le za teste (N1: zamenjava poti pred, med in po git).
    nonisolated(unsafe) static var beforeNestedFinalize: ((ShareHub.Nested) -> Void)?
    nonisolated(unsafe) static var afterNestedFinalize: ((ShareHub.Nested) -> Void)?
    nonisolated(unsafe) static var duringFinalize: (() -> Void)?

    /// N1 (kot Go finalizeNested): finalize ugnezdenega klona po pričakovani pravi poti (pravi koren klona prehoda + pot),
    /// peskovnik iz nje; pred in po git preverba poti in .git; zamenjava zavrže rezultat in ref zrcala gre nazaj.
    static func finalizeNested(_ slot: ShareHub.Slot, _ n: ShareHub.Nested, agent: String, out: URL, links: [String: String],
                               read: [URL] = []) -> (String?, String) {
        let moved = "the nested repository \(n.path) was moved or replaced in the agent's clone"
        guard let root = Sync.real(slot.wt), n.intact(slot.wt) else { return (nil, moved) }
        let expected = root.appendingPathComponent(n.path, isDirectory: true)
        let prev = run(["--git-dir", n.repo.path, "rev-parse", "-q", "--verify", "refs/heads/\(slot.branch)^{commit}"]).out
        beforeNestedFinalize?(n)
        let tip = finalize(repo: n.repo, wt: expected, branch: slot.branch, base: n.base, agent: agent, out: out, read: read, links: links, exact: true)
        afterNestedFinalize?(n)
        guard n.intact(slot.wt), Sync.real(n.dir(slot.wt))?.path == expected.path else {
            _ = run(["--git-dir", n.repo.path] + (isSHA(prev) ? ["update-ref", "refs/heads/" + slot.branch, prev] : ["update-ref", "-d", "refs/heads/" + slot.branch]))
            try? FileManager.default.removeItem(at: out)
            return (nil, moved)
        }
        return tip.map { ($0, "") } ?? (nil, "the gateway could not save the agent's work in the nested repository \(n.path) as a branch (see the gateway log)")
    }

    /// N1: skupine procesov agentov izvajanja (ShareHub.run), zajete ob zagonu (AgentRun.started): Process da vsakemu
    /// agentu svojo skupino, ki jo podedujejo vsi potomci. Ob finalize jih SIGKILL. ponytail: proces, ki naredi setsid ali
    /// setpgid, uide (ostane pri tem peskovnik Seatbelt: finalize bere le pričakovano pravo pot ugnezdenega klona).
    static let runGroups = OSAllocatedUnfairLock<[String: Set<pid_t>]>(initialState: [:])

    static func recordGroup(run: String, pid: pid_t) {
        let g = getpgid(pid)
        guard g > 1, g != getpgrp() else { return }  // nikoli lastna skupina prehoda
        runGroups.withLock { $0[run, default: []].insert(g) }
    }

    static func killGroups(run: String) {
        for g in runGroups.withLock({ $0.removeValue(forKey: run) ?? [] }) where g > 1 && g != getpgrp() { _ = Darwin.kill(-g, SIGKILL) }
    }

    /// Pot programa, argumenti in okolje procesa (KERN_PROCARGS2; le procesi istega uporabnika), ločeni na NUL.
    static func procStrings(_ pid: pid_t) -> Set<String> {
        var mib: [Int32] = [CTL_KERN, KERN_PROCARGS2, pid], size = 0
        guard sysctl(&mib, 3, nil, &size, nil, 0) == 0, size > 4 else { return [] }
        var buf = [UInt8](repeating: 0, count: size)
        guard sysctl(&mib, 3, &buf, &size, nil, 0) == 0, size > 4 else { return [] }
        return Set(buf[4..<size].split(separator: 0).map { String(decoding: $0, as: UTF8.self) })
    }

    /// Ob zagonu aplikacije: zrcala, ki jih 30 dni ni uporabil noben agent, in njihovi kloni.
    public static func prune(scratch: URL, days: Double = 30) {
        let fm = FileManager.default, repos = scratch.appendingPathComponent("repos"), cut = Date().addingTimeInterval(-days * 86400)
        let wts = scratch.appendingPathComponent("worktrees")  // L6: ohranjeni kloni (<klon>-kept-<čas>); L2: le prave mape
        func realDir(_ u: URL) -> Bool { var st = stat(); return lstat(u.path, &st) == 0 && st.st_mode & S_IFMT == S_IFDIR }
        for owner in (try? fm.contentsOfDirectory(atPath: wts.path)) ?? [] where realDir(wts.appendingPathComponent(owner)) {
            for id in (try? fm.contentsOfDirectory(atPath: wts.appendingPathComponent(owner).path)) ?? [] {
                let dir = wts.appendingPathComponent(owner).appendingPathComponent(id)
                if realDir(dir) { ShareHub.pruneKept(dir, before: cut) }
            }
        }
        for owner in (try? fm.contentsOfDirectory(atPath: repos.path)) ?? [] {
            for name in (try? fm.contentsOfDirectory(atPath: repos.appendingPathComponent(owner).path)) ?? [] where name.hasSuffix(".git") {
                let repo = repos.appendingPathComponent(owner).appendingPathComponent(name)
                let used = (try? fm.attributesOfItem(atPath: repo.appendingPathComponent("minigw-used").path))?[.modificationDate] as? Date
                    ?? (try? fm.attributesOfItem(atPath: repo.path))?[.modificationDate] as? Date ?? .distantPast
                guard used < cut else { continue }
                try? fm.removeItem(at: repo)
                let clones = scratch.appendingPathComponent("worktrees").appendingPathComponent(owner).appendingPathComponent(String(name.dropLast(4)))
                for e in (realDir(clones.deletingLastPathComponent()) && realDir(clones) ? (try? fm.contentsOfDirectory(atPath: clones.path)) ?? [] : [])
                where !e.contains("-kept-") {  // L6: ohranjeni imajo svojih 30 dni
                    try? fm.removeItem(at: clones.appendingPathComponent(e))
                }
                rmdir(clones.path)
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
        if let slot = slots[k], slot.gitIntact, slot.nested.allSatisfy({ $0.intact(slot.wt) }), Worktree.nestedProblem(slot.wt, repo: tree.repo, nested: slot.nested) == nil {  // sicer nov klon (H2, N9; #40: ugnezdeni na svoji poti)
            slot.busy = true; runSlot[id] = k; unlock(tree.repo)
            return Self.dir(slot.wt, tree.sub)
        }
        if let old = slots[k], let kept = old.kept {  // L6 (L1): nedostavljeno delo pred novim klonom v kept
            old.kept = nil
            Self.keepClone(old.wt, kept)
        }
        let branch = "minigw/\(agent)-\(short)-\(n)", wt = tree.worktrees.appendingPathComponent("\(agent)-\(short)-\(n)", isDirectory: true)
        let base = "refs/minigw/base/\(agent)-\(short)-\(n)", repo = tree.repo
        // »nested«: vrhovi obstoječih zrcal ugnezdenih repozitorijev po ključu (Worktree.nestedKey); prisotnost pove
        // odjemalcu, da prehod ugnezdene pozna (kot Go worktreeMount).
        let tips = await offPool { () -> ([String], [String: [String]])? in
            guard Worktree.ensureMirror(repo) else { return nil }
            FileManager.default.createFile(atPath: repo.appendingPathComponent("minigw-used").path, contents: nil)  // prune: zadnja raba
            let prefix = String(repo.lastPathComponent.dropLast(4)) + "-"
            var nested: [String: [String]] = [:]
            for n in (try? FileManager.default.contentsOfDirectory(atPath: repo.deletingLastPathComponent().path)) ?? []
            where n.hasPrefix(prefix) && n.hasSuffix(".git") && n.count == prefix.count + 20 {
                nested[String(n.dropFirst(prefix.count).dropLast(4))] = Worktree.tips(repo.deletingLastPathComponent().appendingPathComponent(n))
            }
            return (Worktree.tips(repo), nested)
        }
        guard let (have, nestedHave) = tips, !Task.isCancelled else { unlock(tree.repo); return nil }
        if let local = tree.local {  // native z git: posnetek tu (mapa še vedno ista in v dovoljenih mapah), brez prenosa
            guard Sync.real(local)?.path == local.path, let roots, ClaudeCLI.resolveCwd(local.path, roots: roots, scratch: scratch) != nil
            else { unlock(tree.repo); return nil }
            // A: ugnezdeni repozitoriji kot v worktree (vsak svoj posnetek v svojem zrcalu, klon in veja); brez commita commit
            // brez starša; git na prehodu brez nastavitev sistema in uporabnika (safe).
            // Zaustavitev zaradi ugnezdenega: razlog uporabniku (WORKTREE-NOTE), z namigom na native-direct.
            let attr = Worktree.attrSource  // TaskLocal ne gre v offPool
            let r = await offPool { () -> Result<(String, [Nested]), StopReason> in
                let nested = Worktree.nestedRepos(top: local.path, safe: true)
                let hint = "; use --minigw-work=native-direct"
                guard nested.count <= Worktree.maxNested else {
                    return .failure(StopReason("\(local.path) has more than \(Worktree.maxNested) nested git repositories; native mode with branches takes at most \(Worktree.maxNested)" + hint))
                }
                if let why = Worktree.nestedGitProblem(nested.count, attr: attr, or: "use --minigw-work=native-direct") { return .failure(StopReason(why)) }
                if let p = nested.first(where: { !Worktree.validNestedPath($0) }) {
                    return .failure(StopReason("the nested repository \(Profiles.line(p, max: 500)) has a name native mode cannot use (\":\", \"\\\", control characters or in .build)" + hint))
                }
                let dirs = nested.map { Worktree.localGitDir(top: local, $0) }
                if let i = dirs.firstIndex(where: { $0 == nil }) {
                    return .failure(StopReason("the nested repository \(Profiles.line(nested[i], max: 500)) is a linked worktree, has its git folder outside the share or is not a plain repository" + hint))
                }
                func skip(_ rel: String) -> [String] {
                    let links = Worktree.gitlinks(rel.isEmpty ? local.path : local.path + "/" + rel, safe: true)
                    return Worktree.nestedChildren(rel, nested).filter { c in
                        let p = rel.isEmpty ? c : rel + "/" + c
                        return !links.contains(c) || nested.firstIndex(of: p).flatMap { dirs[$0] }.flatMap(Worktree.head) == nil
                    }
                }
                guard let snapshot = Worktree.localSnapshot(repo: repo, top: local, base: base, skip: skip("")),
                      Worktree.makeClone(repo, at: wt, branch: branch, snapshot: snapshot) else { return .failure(StopReason(nil)) }
                var list: [(path: String, repo: URL, base: String)] = []
                for (i, p) in nested.enumerated() {
                    let mirror = Worktree.nestedMirror(repo, p)
                    guard let snap = Worktree.localSnapshot(repo: mirror, top: local.appendingPathComponent(p, isDirectory: true), base: base,
                                                            gitDir: dirs[i], skip: skip(p)) else {
                        return .failure(StopReason("cannot take a snapshot of the nested repository \(Profiles.line(p, max: 500))" + hint, final: false))
                    }
                    list.append((p, mirror, snap))
                }
                guard let n = Worktree.cloneNested(wt: wt, repo: repo, snapshot: snapshot, branch: branch, list) else { return .failure(StopReason(nil)) }
                return .success((snapshot, n))
            }
            if case .failure(let why) = r, let m = why.message {  // trajna zavrnitev: 400, ne 502 (Claude Code bi ponavljal)
                if why.final { refusals[id] = (400, "native: " + m) }
                Task { await self.note(s, "native: " + m) }  // ne čaka na minigw
            }
            let ok = try? r.get()
            return finishMount(ok?.0, k: k, id: id, wt: wt, branch: branch, tree: tree, nested: ok?.1 ?? [])
        }
        let incoming = repo.deletingLastPathComponent().appendingPathComponent(repo.lastPathComponent + ".incoming")
        let incomings = [incoming] + (1...Worktree.maxNested).map { URL(fileURLWithPath: incoming.path + ".\($0)") }
        defer { incomings.forEach { try? FileManager.default.removeItem(at: $0) } }
        incomings.forEach { try? FileManager.default.removeItem(at: $0) }  // ostanki prekinjenega zagona
        s.pushing = incoming
        let job = DAVJob(id: UUID().uuidString, method: "WORKTREE-PUSH", path: "", prefix: "", headers: [:],
                         body: (try? JSONSerialization.data(withJSONObject: ["have": have, "nested": nestedHave] as [String: Any])) ?? Data())
        let r = await send(s, job, timeout: 900)
        s.pushing = nil
        let reply = r.flatMap { $0.status == 200 ? (try? JSONSerialization.jsonObject(with: $0.body) as? [String: Any]) : nil }
        if reply == nil {  // nikoli 502 brez razloga v dnevniku (kot Go worktreeMount)
            _ = Worktree.logFail(repo, r.map { "minigw could not send the snapshot (status \($0.status); the reason is in minigw's output on the client)" }
                                 ?? "minigw did not answer WORKTREE-PUSH (gone or timed out)")
        }
        let snapshot = reply?["snapshot"] as? String
        // Ugnezdeni (kot Go): poti preverjene (H1), vsak v svoje zrcalo <repo>-<ključ>.git in klon na svoji poti, starš pred otrokom.
        let pairs = ((reply?["nested"] as? [[String: Any]]) ?? []).map { ($0["path"] as? String ?? "", $0["snapshot"] as? String ?? "") }
        let valid = pairs.count <= Worktree.maxNested && Set(pairs.map(\.0)).count == pairs.count
            && pairs.allSatisfy { Worktree.validNestedPath($0.0) && Worktree.isSHA($0.1) }
        var nested: [Nested]?
        if valid, let why = Worktree.nestedGitProblem(pairs.count) {  // kot Go refused: 400, ne 502 (Claude Code bi ponavljal)
            refusals[id] = (400, "worktree: " + why)
            return finishMount(nil, k: k, id: id, wt: wt, branch: branch, tree: tree)
        }
        if let snapshot, Worktree.isSHA(snapshot), valid {
            nested = await offPool { () -> [Nested]? in
                guard Worktree.importBase(repo, bundle: FileManager.default.fileExists(atPath: incoming.path) ? incoming : nil, snapshot: snapshot, base: base),
                      Worktree.makeClone(repo, at: wt, branch: branch, snapshot: snapshot) else { return nil }
                var out: [(path: String, repo: URL, base: String)] = []
                for (i, (p, snap)) in pairs.enumerated() {
                    let mirror = Worktree.nestedMirror(repo, p), file = incomings[i + 1]
                    guard Worktree.ensureMirror(mirror),
                          Worktree.importBase(mirror, bundle: FileManager.default.fileExists(atPath: file.path) ? file : nil, snapshot: snap, base: base)
                    else { return nil }
                    FileManager.default.createFile(atPath: mirror.appendingPathComponent("minigw-used").path, contents: nil)
                    out.append((p, mirror, snap))
                }
                return Worktree.cloneNested(wt: wt, repo: repo, snapshot: snapshot, branch: branch, out)
            }
        }
        return finishMount(nested != nil ? snapshot : nil, k: k, id: id, wt: wt, branch: branch, tree: tree, nested: nested ?? [])
    }
}

/// Razlog, zakaj native z git ne more začeti (nil = brez sporočila uporabniku).
/// `final`: trajna zavrnitev (400); false = morda prehodno (502, Claude Code ponovi), sporočilo le v opombi.
struct StopReason: Error { let message: String?, final: Bool; init(_ m: String?, final: Bool = true) { message = m; self.final = final } }

extension Worktree {
    /// Ugnezdeni kloni v wt (starš pred otrokom; H1 cloneTarget) in cilj 1: nesleden (v drevesu starša ni gitlink) v
    /// info/exclude klona starša, preden agent teče (kot Go worktreeMount). nil = ni uspelo.
    static func cloneNested(wt: URL, repo: URL, snapshot: String, branch: String, _ list: [(path: String, repo: URL, base: String)]) -> [ShareHub.Nested]? {
        let out = list.sorted { $0.path.split(separator: "/").count < $1.path.split(separator: "/").count }
        for n in out {
            guard Worktree.cloneTarget(wt, n.path),
                  Worktree.makeClone(n.repo, at: wt.appendingPathComponent(n.path, isDirectory: true), branch: branch, snapshot: n.base) else { return nil }
            // Cilj 1 (kot Go): nesleden (v drevesu starša ni gitlink) v info/exclude klona starša, preden agent teče
            let parent = out.filter { n.path.hasPrefix($0.path + "/") }.max { $0.path.count < $1.path.count }
            let rel = parent.map { String(n.path.dropFirst($0.path.count + 1)) } ?? n.path
            let (prepo, psnap) = parent.map { ($0.repo, $0.base) } ?? (repo, snapshot)
            let pdir = parent.map { wt.appendingPathComponent($0.path, isDirectory: true) } ?? wt
            if !Worktree.run(["--git-dir", prepo.path, "ls-tree", psnap, "--", rel]).out.hasPrefix("160000 ") {
                guard let h = FileHandle(forWritingAtPath: pdir.appendingPathComponent(".git/info/exclude").path)
                    ?? { FileManager.default.createFile(atPath: pdir.appendingPathComponent(".git/info/exclude").path, contents: nil)
                         return FileHandle(forWritingAtPath: pdir.appendingPathComponent(".git/info/exclude").path) }() else { return nil }
                _ = try? h.seekToEnd(); try? h.write(contentsOf: Data(("\n/" + Worktree.excludeEscape(rel) + "\n").utf8)); try? h.close()
            }
        }
        return out.map { ShareHub.Nested(path: $0.path, repo: $0.repo, base: $0.base, wt: wt) }
    }
}

extension ShareHub {
    /// Mesto izvajanja po novem klonu (`snapshot` nil = ni uspelo); sprosti zaklep zrcala.
    private func finishMount(_ snapshot: String?, k: String, id: String, wt: URL, branch: String, tree: Tree, nested: [Nested] = []) -> URL? {
        if let snapshot {
            slots[k] = Slot(wt: wt, branch: branch, base: snapshot, nested: nested); runSlot[id] = k
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
    /// `gits`: .git vseh klonov (vrhnji in ugnezdeni) za ovoj git in denyWrite.
    func worktree(session: String) -> (root: URL, branch: String, maven: URL, read: [URL], gits: [String])? {
        guard let id = Self.run, let k = runSlot[id], k.hasPrefix(session + "|"), let slot = slots[k],
              let tree = bySessionTree(session) else { return nil }
        let read = [tree.repo.appendingPathComponent("objects")] + slot.nested.map { $0.repo.appendingPathComponent("objects") }
            + (tree.localObjectsIntact ? slot.nested.compactMap { n in tree.local.flatMap { Worktree.localGitDir(top: $0, n.path) }?.appendingPathComponent("objects") } : [])
            + (tree.localObjectsIntact ? tree.local.map { [$0.appendingPathComponent(".git/objects")] } ?? [] : [])
        return (slot.wt, slot.branch, tree.worktrees.appendingPathComponent(".maven", isDirectory: true), read, slot.gitDirs)
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

    /// Po agentu: veje v zrcalih (z neshranjenimi spremembami; ugnezdeni najprej, globlji pred plitvejšimi: starš dobi novi
    /// gitlink) in prevzem pri odjemalcu (WORKTREE-PULL: veja, nestedOnly, spremenjeni ugnezdeni; kot Go worktreeBack).
    func worktreeBack(_ s: Share, _ tree: Tree) async {
        guard let id = Self.run, let k = runSlot[id], let slot = slots[k] else { return }
        await acquire(tree.repo)
        await offPool {  // N1: pred vsakim izhodom spodaj (tudi kept), ne šele ob finalize
            Worktree.killGroups(run: id)  // skupine procesov agentov tega izvajanja
            Worktree.killProcesses(in: slot.wt)  // ves klon, preden ugnezdeni (finalize ustavi le procese v svoji mapi)
        }
        let out = tree.repo.deletingLastPathComponent().appendingPathComponent(tree.repo.lastPathComponent + ".result")
        let (repo, agent) = (tree.repo, String(k.split(separator: "|")[1]))
        // L6: kar ni prišlo do odjemalca, ostane v klonu; ob koncu seje gre klon v kept (prune: 30 dni).
        let fmt = DateFormatter(), now = Date()
        fmt.locale = Locale(identifier: "en_US_POSIX"); fmt.dateFormat = "yyyyMMdd-HHmmss"
        let nanos = String(format: "%09d", Int(now.timeIntervalSince1970.truncatingRemainder(dividingBy: 1) * 1e9))  // L4: brez trka
        let kept = URL(fileURLWithPath: slot.wt.path + "-kept-" + fmt.string(from: now) + "-" + nanos, isDirectory: true)
        // L3: pot glede na Scratch prehoda; M2: le datoteke, .git preimenovani (keepClone)
        let rel = ["worktrees", tree.worktrees.deletingLastPathComponent().lastPathComponent, tree.worktrees.lastPathComponent, kept.lastPathComponent].joined(separator: "/")
        let keep = "; the agent's files are kept on the gateway for 30 days as \(rel) after this session (files only; its .git folders renamed .git.agent, or nothing is kept if that fails — do not run git there)"
        if tree.local != nil, !tree.localObjectsIntact {  // M1
            slot.kept = kept
            await note(s, "\(tree.local!.path): the repository's .git/objects was replaced; \(slot.branch) was not delivered" + keep)
            unlock(tree.repo); return
        }
        if let why = await offPool({ Worktree.nestedProblem(slot.wt, repo: repo, nested: slot.nested) }) {  // N9; sporočilo kot pri Go (WORKTREE-PULL)
            slot.kept = kept
            await note(s, "worktree: cannot get \(slot.branch) from the gateway: " + why + keep)
            unlock(tree.repo); return
        }
        let read = tree.local.map { [$0.appendingPathComponent(".git/objects")] } ?? []
        // L6: zavrnjen ugnezdeni (razlog) ne ustavi vrhnjega in ostalih; njegova pot je pri staršu izpuščena, gitlink ostane.
        let (nested, top, skipped) = await offPool { () -> ([(path: String, tip: String, out: URL)], String?, [String]) in
            var links: [String: [String: String]] = ["": [:]], changed: [(path: String, tip: String, out: URL)] = [], skipped: [String] = []
            for n in slot.nested { links[n.path] = [:] }
            for (i, n) in slot.nested.enumerated().reversed() {
                let o = URL(fileURLWithPath: out.path + ".\(i + 1)")
                let nread = tree.local.flatMap { Worktree.localGitDir(top: $0, n.path) }.map { [$0.appendingPathComponent("objects")] } ?? []
                var (t, why) = Worktree.finalizeNested(slot, n, agent: agent, out: o, links: links[n.path] ?? [:], read: nread)
                if let tip = t, tip != n.base, ((try? FileManager.default.attributesOfItem(atPath: o.path))?[.size] as? NSNumber)?.int64Value ?? 0 > Sync.maxFile {
                    (t, why) = (nil, "the agent's branch in the nested repository \(n.path) is larger than 64 MB; it stays on the gateway's mirror (\(slot.branch))")
                }
                let parent = slot.nested.map(\.path).filter { n.path.hasPrefix($0 + "/") }.max { $0.count < $1.count } ?? ""
                let rel = parent.isEmpty ? n.path : String(n.path.dropFirst(parent.count + 1))
                links[parent, default: [:]][rel] = ""
                guard let tip = t else { skipped.append(why); continue }
                if tip != n.base { links[parent, default: [:]][rel] = tip; changed.append((n.path, tip, o)) }
            }
            return (changed, Worktree.finalize(repo: repo, wt: slot.wt, branch: slot.branch, base: slot.base, agent: agent, out: out, read: read,
                                               links: links[""] ?? [:]), skipped)
        }
        defer { for i in 0...slot.nested.count { try? FileManager.default.removeItem(at: URL(fileURLWithPath: out.path + (i == 0 ? "" : ".\(i)"))) } }
        slot.kept = top == nil || !skipped.isEmpty ? kept : nil
        for m in skipped {
            Router.log.error("worktree \(slot.branch, privacy: .public): \(m, privacy: .public)")
            await note(s, "worktree: " + m + keep)
        }
        guard let tip = top else {
            let why = "the gateway could not save the agent's work as a branch (see the gateway log)"
            Router.log.error("worktree \(slot.branch, privacy: .public): \(why, privacy: .public)")
            await note(s, "worktree: cannot get \(slot.branch) from the gateway: " + why + keep)
            unlock(tree.repo); return
        }
        if let local = tree.local {
            // native z git: veja naravnost v repozitorij uporabnika (le naprej, brez hookov, fsck, v peskovniku); mapa, indeks,
            // HEAD ostanejo. Ugnezdeni (A) vsak v svoj repozitorij. Neuspeh dobi uporabnik (kot pri WORKTREE-PULL) in dnevnik.
            let branch = slot.branch
            var deliver = nested.compactMap { n in slot.nested.first { $0.path == n.path }.map { (n.path, n.out, $0.base) } }
            if tip != slot.base, FileManager.default.fileExists(atPath: out.path) { deliver.insert(("", out, slot.base), at: 0) }
            for (p, o, base) in deliver {
                let mirror = p.isEmpty ? repo : Worktree.nestedMirror(repo, p), dir = p.isEmpty ? local : local.appendingPathComponent(p, isDirectory: true)
                if let why = await offPool({ () -> String? in
                    let gd = p.isEmpty ? nil : Worktree.localGitDir(top: local, p)
                    if !p.isEmpty, gd == nil { return "\(dir.path): the nested repository changed; \(branch) was not delivered" }
                    return Worktree.localBundle(repo: mirror, top: dir, branch: branch, out: o, gitDir: gd)
                        ?? Worktree.localFetch(top: dir, bundle: o, branch: branch, gitDir: gd)
                }) {
                    Router.log.error("native worktree: \(why, privacy: .public)")
                    slot.kept = kept
                    await note(s, why + keep)
                } else {  // minigw izpiše vrstico o veji z obsegom od posnetka (le delo agenta)
                    await note(s, Worktree.branchLine(branch: branch, base: base, nested: p.isEmpty ? nil : p),
                               extra: ["branch": branch, "base": base].merging(p.isEmpty ? [:] : ["nested": p]) { $1 })
                }
            }
        } else if tree.local == nil, tip != slot.base && FileManager.default.fileExists(atPath: out.path) || !nested.isEmpty {
            s.results = [out] + nested.map(\.out)
            let msg: [String: Any] = ["branch": slot.branch, "tip": tip, "nestedOnly": tip == slot.base,
                                      "nested": nested.map { ["path": $0.path, "tip": $0.tip] }]
            let body = (try? JSONSerialization.data(withJSONObject: msg)) ?? Data()
            _ = await send(s, DAVJob(id: UUID().uuidString, method: "WORKTREE-PULL", path: "", prefix: "", headers: [:], body: body), timeout: 300)
            s.results = []
        }
        unlock(tree.repo)
    }

    /// Največji bundle odjemalca (prvi prenos velikega repozitorija); večji → 413.
    static let maxIncoming: Int64 = 4 << 30

    /// Del bundla odjemalca (WORKTREE-PUSH): del 0 začne datoteko; nested > 0: i-ti ugnezdeni. Status HTTP.
    func worktreePut(key: String, part: Int, nested: Int = 0, data: Data, device: String?) -> Int {
        guard let s = share(key: key, device: device), let top = s.pushing, part >= 0, (0...Worktree.maxNested).contains(nested) else { return 409 }
        let file = nested == 0 ? top : URL(fileURLWithPath: top.path + ".\(nested)")
        s.lastSeen = Date()
        if part == 0 { FileManager.default.createFile(atPath: file.path, contents: nil, attributes: [.posixPermissions: 0o600]) }
        // N4: vsi bundli enega WORKTREE-PUSH skupaj največ maxIncoming
        let size = (0...Worktree.maxNested).map { i -> Int64 in
            ((try? FileManager.default.attributesOfItem(atPath: i == 0 ? top.path : top.path + ".\(i)"))?[.size] as? NSNumber)?.int64Value ?? 0
        }.reduce(0, +)
        guard size + Int64(data.count) <= Self.maxIncoming else { try? FileManager.default.removeItem(at: file); return 413 }
        guard let h = FileHandle(forWritingAtPath: file.path) else { return 500 }
        defer { try? h.close() }
        do { try h.seekToEnd(); try h.write(contentsOf: data) } catch { return 500 }
        return 204
    }

    /// Bundle veje za odjemalca med WORKTREE-PULL (nested > 0: i-ti ugnezdeni iz sporočila).
    func worktreeResult(key: String, device: String?, nested: Int = 0) -> Data? {
        guard let s = share(key: key, device: device), s.results.indices.contains(nested) else { return nil }
        let file = s.results[nested]
        s.lastSeen = Date()
        return Sync.read(file, max: Int(Sync.maxFile))
    }

    /// Konec izvajanja: mesto prosto; če seje ni več (minigw je končal med agentom), klon gre takoj.
    func releaseSlot(run id: String) {
        guard let k = runSlot.removeValue(forKey: id), let slot = slots[k] else { return }
        slot.busy = false
        if bySessionTree(String(k.split(separator: "|")[0])) == nil {
            slots[k] = nil
            Self.dropClone(slot)
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
            Self.dropClone(slot)
        }
    }

    /// Klon gre; z nedostavljenim delom (L6) ostane kot kept (keepClone; kot Go dropClones).
    static func dropClone(_ slot: Slot) {
        if let kept = slot.kept {
            keepClone(slot.wt, kept)  // premik v akterju (O(1)), hoja v ozadju
            return
        }
        let wt = slot.wt
        Task.detached { removeClone(wt) }
    }

    /// Največ ohranjenih klonov v mapi klonov repozitorija (M1; najstarejši gredo).
    static let maxKept = 3

    /// L6 (kot Go keepClone): klon z nedostavljenim delom gre v kept: premik takoj (O(1), renameat relativno na odprt
    /// starš; nov klon na isti poti ne tekmuje z njim), razorožitev (disarmKept) in pruneKept v ozadju. Neuspeh premika:
    /// klon gre (dnevnik).
    nonisolated static func keepClone(_ wt: URL, _ kept: URL) {
        let pfd = open(wt.deletingLastPathComponent().path, O_RDONLY | O_DIRECTORY | O_CLOEXEC)
        guard pfd >= 0 else { return }
        defer { close(pfd) }
        var st = stat()
        guard fstatat(pfd, wt.lastPathComponent, &st, AT_SYMLINK_NOFOLLOW) == 0, st.st_mode & S_IFMT == S_IFDIR else { return }
        guard renameat(pfd, wt.lastPathComponent, pfd, kept.lastPathComponent) == 0 else {
            Router.log.error("worktree: cannot keep the agent's clone \(wt.path, privacy: .public); removed")
            removeClone(wt); return
        }
        var ts = [timespec(tv_sec: 0, tv_nsec: Int(UTIME_NOW)), timespec(tv_sec: 0, tv_nsec: Int(UTIME_NOW))]
        _ = utimensat(pfd, kept.lastPathComponent, &ts, AT_SYMLINK_NOFOLLOW)
        Task.detached {
            if !disarmKept(kept) { Router.log.error("worktree: cannot rename the git folders in the kept clone \(kept.path, privacy: .public); removed") }
            pruneKept(kept.deletingLastPathComponent(), before: .distantPast)
        }
    }

    /// M2 (kot Go disarmKept): v ohranjenem klonu vsak .git (mapa ali datoteka, katerakoli globina) in HEAD v mapi z obliko
    /// golega repozitorija (HEAD, objects, refs) dobi ime *.agent: git ju tam ne najde (config, kljuke, fsmonitor agenta ne
    /// tečejo). L3: sklad relativnih poti, ena odprta mapa naenkrat (openat z O_NOFOLLOW_ANY iz korena: povezava kjerkoli
    /// v poti ne preusmeri ničesar ven); mapa brez pravic (npr. 555) dobi 0700 in poskus znova. Neuspeh ali zamenjan
    /// koren (L4): klon gre (false).
    nonisolated static func disarmKept(_ kept: URL) -> Bool {
        let pfd = open(kept.deletingLastPathComponent().path, O_RDONLY | O_DIRECTORY | O_CLOEXEC)
        guard pfd >= 0 else { return false }
        defer { close(pfd) }
        let name = kept.lastPathComponent
        var ok = false
        var st = stat(), rst = stat()
        if fstatat(pfd, name, &st, AT_SYMLINK_NOFOLLOW) == 0, st.st_mode & S_IFMT == S_IFDIR {
            let root = openat(pfd, name, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
            if root >= 0 {
                if fstat(root, &rst) == 0, rst.st_dev == st.st_dev, rst.st_ino == st.st_ino { ok = disarmRoot(root) }
                close(root)
            } else { return false }  // povezava ali nič: ne briši
        } else { return false }
        if !ok { try? FileManager.default.removeItem(at: kept) }
        return ok
    }

    nonisolated private static func disarmRoot(_ root: Int32) -> Bool {
        var stack = ["."]
        while let rel = stack.popLast() {
            // L2: mapa brez pravic → 0700 (relativno na koren, brez sledenja povezavam) in znova
            func openDir() -> Int32 { openat(root, rel, O_RDONLY | O_DIRECTORY | O_NOFOLLOW_ANY | O_CLOEXEC) }
            var fd = openDir()
            if fd < 0, fchmodat(root, rel, 0o700, AT_SYMLINK_NOFOLLOW_ANY) == 0 { fd = openDir() }
            guard fd >= 0 else { return false }  // L3: prava mapa, ki je ni mogoče odpreti
            defer { close(fd) }
            guard let d = fdopendir(dup(fd)) else { return false }
            var names: [String] = []
            while let e = readdir(d) {
                let n = withUnsafeBytes(of: e.pointee.d_name) { String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) }
                if n != "." && n != ".." { names.append(n) }
            }
            closedir(d)
            func isDir(_ n: String) -> Bool { var st = stat(); return fstatat(fd, n, &st, AT_SYMLINK_NOFOLLOW) == 0 && st.st_mode & S_IFMT == S_IFDIR }
            func rename(_ n: String) -> Bool { renameat(fd, n, fd, n + ".agent") == 0 || renameat(fd, n, fd, n + ".agent-" + UUID().uuidString) == 0 }
            for n in names {
                if Sync.normName(n) == ".git" || n.uppercased() == "HEAD" && isDir("objects") && isDir("refs") {
                    guard rename(n) || fchmod(fd, 0o700) == 0 && rename(n) else { return false }  // L2
                } else if isDir(n) {
                    stack.append(rel == "." ? n : rel + "/" + n)
                }
            }
        }
        return true
    }

    /// Ohranjeni kloni v mapi klonov `dir` starejši od `before` in vsi razen maxKept najnovejših (lstat, brez povezav).
    nonisolated static func pruneKept(_ dir: URL, before: Date) {
        let kept = ((try? FileManager.default.contentsOfDirectory(atPath: dir.path)) ?? []).filter { $0.contains("-kept-") }
            .compactMap { n -> (String, Date)? in
                var st = stat()
                guard lstat(dir.appendingPathComponent(n).path, &st) == 0, st.st_mode & S_IFMT == S_IFDIR else { return nil }
                return (n, Date(timeIntervalSince1970: TimeInterval(st.st_mtimespec.tv_sec)))
            }.sorted { $0.1 > $1.1 }
        for (i, (n, t)) in kept.enumerated() where i >= maxKept || t < before {
            try? FileManager.default.removeItem(at: dir.appendingPathComponent(n))
        }
    }
}
