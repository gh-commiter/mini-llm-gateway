import Foundation
import Testing
@testable import RouterCore

/// #107 (kot client/root_test.go): povezave v deljeni mapi in operacije prek odprtega korena. Vse v začasnih mapah.
private func tmp(_ name: String, resolved: Bool = true) throws -> URL {
    let u = FileManager.default.temporaryDirectory.appendingPathComponent("minigw-test-\(name)-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: u, withIntermediateDirectories: true)
    return resolved ? Sync.real(u)! : u  // /private/var/…, kot F_GETPATH
}
private func put(_ s: String, _ u: URL) throws { try Data(s.utf8).write(to: u) }
private func read(_ u: URL) -> String? { (try? Data(contentsOf: u)).map { String(decoding: $0, as: UTF8.self) } }
private func link(_ target: String, _ at: URL) throws { try FileManager.default.createSymbolicLink(atPath: at.path, withDestinationPath: target) }
private func isLink(_ u: URL) -> Bool { (try? FileManager.default.destinationOfSymbolicLink(atPath: u.path)) != nil }
private func gone(_ u: URL) -> Bool { (try? FileManager.default.attributesOfItem(atPath: u.path)) == nil }
private func dav(_ root: URL, _ m: String, _ p: String, _ h: [String: String] = [:], _ b: String = "") -> DAVResponse {
    WebDAV.handle(method: m, path: "/s" + p, headers: h, body: Data(b.utf8), root: root, prefix: "/s")
}
private func text(_ r: DAVResponse) -> String { String(decoding: r.body, as: UTF8.self) }
private func entry(_ p: String, _ data: String) -> Sync.Entry { Sync.Entry(path: p, size: Int64(data.utf8.count), mtime: 1, sha256: Sync.sha256(Data(data.utf8))) }

@Test func shareRelativeLinkInside() throws {
    let root = try tmp("root")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try link("d", root.appendingPathComponent("link"))
    #expect(dav(root, "PUT", "/link/x", [:], "inside").status == 201)
    #expect(read(root.appendingPathComponent("d/x")) == "inside")
    let r = dav(root, "GET", "/link/x")
    #expect(r.status == 200 && text(r) == "inside")
    #expect(dav(root, "MKCOL", "/link/sub").status == 201)
    #expect(dav(root, "COPY", "/link/x", ["destination": "/s/link/sub/y"]).status == 201)
    #expect(read(root.appendingPathComponent("d/sub/y")) == "inside")
}

/// Absolutna povezava v mapi, zapisana z drugim imenom mape (/var/… namesto /private/var/…), in veriga povezav.
@Test func shareAbsoluteLinkInsideAndChain() throws {
    let root = try tmp("root", resolved: false)  // /var/folders/… je povezava na /private/var/…
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try put("old", root.appendingPathComponent("d/old"))
    try link(root.appendingPathComponent("d").path, root.appendingPathComponent("abs"))
    try link("abs", root.appendingPathComponent("rel"))
    try link(root.appendingPathComponent("rel").path, root.appendingPathComponent("b"))
    for l in ["abs", "rel", "b"] {
        #expect(text(dav(root, "GET", "/\(l)/old")) == "old", "\(l)")
        #expect([201, 204].contains(dav(root, "PUT", "/\(l)/x", [:], l).status), "\(l)")
    }
    #expect(read(root.appendingPathComponent("d/x")) == "b")
    // PROPFIND: povezava na mapo je zbirka, otroci so navedeni pod imenom povezave.
    let list = text(dav(root, "PROPFIND", "/abs", ["depth": "1"]))
    #expect(list.contains("/s/abs/x") && list.contains("/s/abs/old"))
    #expect(dav(root, "MOVE", "/abs/x", ["destination": "/s/abs/y"]).status == 201)
    #expect(read(root.appendingPathComponent("d/y")) == "b")
    // DELETE povezave odstrani povezavo, ne cilja.
    #expect(dav(root, "DELETE", "/abs").status == 204)
    #expect(gone(root.appendingPathComponent("abs")) && read(root.appendingPathComponent("d/old")) == "old")
}

@Test func shareLinkLeavingTheShareIsRefusedWithReason() throws {
    let root = try tmp("root"), out = try tmp("out")
    try put("secret", out.appendingPathComponent("secret"))
    try link(out.path, root.appendingPathComponent("dir"))                          // absolutna, ven
    try link("../" + out.lastPathComponent, root.appendingPathComponent("rel"))     // relativna, ven (nikoli v mapi)
    try link(out.appendingPathComponent("secret").path, root.appendingPathComponent("file"))
    try link(out.appendingPathComponent("nothing").path, root.appendingPathComponent("dangling-out"))
    for (m, p) in [("GET", "/dir/secret"), ("PUT", "/dir/x"), ("MKCOL", "/dir/n"), ("DELETE", "/dir/secret"), ("PROPFIND", "/dir"),
                   ("GET", "/rel/secret"), ("PUT", "/rel/x"), ("GET", "/file"), ("GET", "/dangling-out"), ("PUT", "/dangling-out")] {
        let r = dav(root, m, p, [:], "bad")
        #expect(r.status == 403, "\(m) \(p)")
        #expect(text(r).contains("links that lead outside the shared folder are not followed"), "\(m) \(p): \(text(r))")
    }
    #expect(read(out.appendingPathComponent("secret")) == "secret")
    #expect((try? FileManager.default.contentsOfDirectory(atPath: out.path)) == ["secret"])
    // PROPFIND mape: povezava, ki zapusti mapo, ni datoteka z dolžino cilja povezave.
    // Stran PROPFIND za povezavo ven: datoteka velikosti 0 (ne zbirka, ne dolžina cilja povezave).
    let list = text(dav(root, "PROPFIND", "/", ["depth": "1"]))
    let resp = list.components(separatedBy: "<D:response>").dropFirst()
    func part(_ n: String) -> String { resp.first { $0.contains("<D:href>/s/\(n)</D:href>") || $0.contains("<D:href>/s/\(n)/</D:href>") } ?? "" }
    for n in ["file", "dir", "rel", "dangling-out"] {
        #expect(part(n).contains("<D:getcontentlength>0<") && !part(n).contains("<D:collection/>"), "\(n): \(part(n))")
    }
}

@Test func shareDanglingLinkListsButDoesNotRead() throws {
    let root = try tmp("root")
    try link("missing", root.appendingPathComponent("dangling"))
    try link(root.appendingPathComponent("also-missing").path, root.appendingPathComponent("dangling-abs"))
    let list = text(dav(root, "PROPFIND", "/", ["depth": "1"]))
    #expect(list.contains("/s/dangling<") && list.contains("/s/dangling-abs<"))
    #expect(list.contains("<D:getcontentlength>0<") && !list.contains("<D:getcontentlength>7<"))
    for p in ["/dangling", "/dangling-abs"] { #expect(dav(root, "GET", p).status >= 400, "\(p)") }
    #expect(dav(root, "DELETE", "/dangling").status == 204)  // povezava sama gre
    #expect(gone(root.appendingPathComponent("dangling")) && !gone(root.appendingPathComponent("dangling-abs")))
}

@Test func shareLinkLoopStops() throws {
    let root = try tmp("root")
    try link(root.appendingPathComponent("b").path, root.appendingPathComponent("a"))
    try link(root.appendingPathComponent("a").path, root.appendingPathComponent("b"))
    for m in ["PUT", "GET"] { #expect(dav(root, m, "/a/x", [:], "x").status >= 400) }
    let rt = try #require(ShareRoot(root))
    #expect(throws: ShareRoot.Refused.self) { try rt.resolve("a/x", followLast: true) }
}

@Test func shareLinkIntoProtectedFolderIsRefused() throws {
    let root = try tmp("root")
    for d in [".claude", ".git"] { try FileManager.default.createDirectory(at: root.appendingPathComponent(d), withIntermediateDirectories: true) }
    try put("n", root.appendingPathComponent(".claude/notes.md"))
    try link(root.appendingPathComponent(".claude").path, root.appendingPathComponent("abs"))
    try link(".claude", root.appendingPathComponent("rel"))
    try link(root.appendingPathComponent(".git").path, root.appendingPathComponent("git"))
    for l in ["abs", "rel"] {
        for (m, p) in [("PUT", "settings.json"), ("PUT", "hooks"), ("MKCOL", "hooks"), ("PUT", "commands/x")] {
            #expect(dav(root, m, "/\(l)/\(p)", [:], "{}").status >= 400, "\(m) \(l)/\(p)")
        }
    }
    #expect(dav(root, "PUT", "/git/config", [:], "x").status >= 400)
    try put("ok", root.appendingPathComponent("ok"))
    #expect(dav(root, "MOVE", "/ok", ["destination": "/s/abs/settings.json"]).status >= 400)
    for p in [".claude/settings.json", ".claude/hooks", ".git/config"] { #expect(gone(root.appendingPathComponent(p)), "\(p)") }
    #expect(dav(root, "GET", "/abs/notes.md").status == 200)  // branje ostane
    // Sync: zapis in izbris prek povezave.
    var state: [String: Sync.Entry] = [:]
    let (notes, done) = Sync.apply(Sync.Changes(changed: [entry("abs/settings.json", "{}"), entry("git/config", "{}")], deleted: ["abs/settings.local.json"]),
                                   root: root, state: &state) { _ in Data("{}".utf8) }
    #expect(notes.count == 3 && done.count == 3, "\(notes) \(done)")
    #expect(gone(root.appendingPathComponent(".claude/settings.json")) && gone(root.appendingPathComponent(".git/config")))
}

/// MOVE in DELETE delata na povezavi sami (datoteka in mapa), cilj ostane.
@Test func shareMoveAndDeleteActOnTheLinkItself() throws {
    let root = try tmp("root")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try put("data", root.appendingPathComponent("f"))
    try link("f", root.appendingPathComponent("fl"))
    try link("d", root.appendingPathComponent("dl"))
    #expect(dav(root, "MOVE", "/fl", ["destination": "/s/fl2"]).status == 201)
    #expect(isLink(root.appendingPathComponent("fl2")) && gone(root.appendingPathComponent("fl")) && read(root.appendingPathComponent("f")) == "data")
    #expect(dav(root, "MOVE", "/dl", ["destination": "/s/dl2"]).status == 201)
    #expect(isLink(root.appendingPathComponent("dl2")))
    #expect(dav(root, "DELETE", "/fl2").status == 204 && dav(root, "DELETE", "/dl2").status == 204)
    #expect(gone(root.appendingPathComponent("fl2")) && gone(root.appendingPathComponent("dl2")))
    #expect(read(root.appendingPathComponent("f")) == "data" && !gone(root.appendingPathComponent("d")))
    // Cilj MOVE je povezava: zamenjana je povezava, ne njen cilj.
    try link("f", root.appendingPathComponent("fl"))
    try put("new", root.appendingPathComponent("n"))
    #expect(dav(root, "MOVE", "/n", ["destination": "/s/fl"]).status == 204)
    #expect(read(root.appendingPathComponent("f")) == "data" && read(root.appendingPathComponent("fl")) == "new" && !isLink(root.appendingPathComponent("fl")))
}

/// PROPFIND: vrsta in velikost povezav (napaka iz 0.5.26: povezava je bila datoteka z dolžino cilja povezave).
@Test func sharePropfindTypeAndSizeOfLinks() throws {
    let root = try tmp("root")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try put("12345678901", root.appendingPathComponent("d/file"))
    try link("d/file", root.appendingPathComponent("flink"))
    try link(root.appendingPathComponent("d").path, root.appendingPathComponent("dlink"))
    let xml = text(dav(root, "PROPFIND", "/", ["depth": "1"]))
    let resp = xml.components(separatedBy: "<D:response>").dropFirst()
    func part(_ name: String) -> String { resp.first { $0.contains("<D:href>/s/\(name)") } ?? "" }
    #expect(part("flink").contains("<D:getcontentlength>11<") && !part("flink").contains("<D:collection/>"))
    #expect(part("dlink").contains("<D:collection/>") && part("dlink").contains("<D:href>/s/dlink/</D:href>"))
    #expect(dav(root, "HEAD", "/flink").status == 200 && dav(root, "HEAD", "/flink").headers["Content-Length"] == "11")
}


/// Neposredno prek korena (kot Go TestRootOperationsRefuseSwappedParent): pot izbrana, nato starš zamenjan s povezavo.
@Test func shareRootOperationsRefuseSwappedParent() throws {
    let root = try tmp("root"), out = try tmp("out"), fm = FileManager.default
    try fm.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try put("outside", out.appendingPathComponent("x"))
    try put("inside", root.appendingPathComponent("source"))
    let rt = try #require(ShareRoot(root))
    let chosen = try rt.resolve("d/x", followLast: true)
    try fm.moveItem(at: root.appendingPathComponent("d"), to: root.appendingPathComponent("old"))
    try link(out.path, root.appendingPathComponent("d"))
    #expect(rt.writeAtomic(chosen, Data("bad".utf8), mode: nil, mtime: nil) != 0)
    #expect(!rt.copyTree("d/x", "copy") && !rt.copyTree("source", "d/copy"))
    #expect(!rt.rename("d/x", "moved") && !rt.rename("source", "d/moved"))
    #expect(!rt.removeAll("d/x") && !rt.remove("d/x"))
    #expect(rt.mkdir("d/new") != 0 && rt.create("d/new") == nil)
    #expect(rt.openRead("d/x") == nil && rt.list("d") == nil && !rt.chmod("d/x", 0o777))
    #expect(Sync.write(Data("bad".utf8), "d/x", in: rt, mtime: 0, exec: false) != nil)
    #expect(Sync.entry(rt, "d/x", path: "d/x", previous: nil) == nil)
    #expect(read(out.appendingPathComponent("x")) == "outside" && (try? fm.contentsOfDirectory(atPath: out.path)) == ["x"])
}

@Test func shareSyncApplyThroughLinks() throws {
    let root = try tmp("root"), out = try tmp("out")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try link("d", root.appendingPathComponent("rel"))
    try link(root.appendingPathComponent("d").path, root.appendingPathComponent("abs"))
    try link(out.path, root.appendingPathComponent("out"))
    var state: [String: Sync.Entry] = [:]
    var (notes, done) = Sync.apply(Sync.Changes(changed: [entry("rel/a", "A"), entry("abs/b", "B")]), root: root, state: &state) { p in Data((p == "rel/a" ? "A" : "B").utf8) }
    #expect(notes.isEmpty && done.count == 2, "\(notes)")
    #expect(read(root.appendingPathComponent("d/a")) == "A" && read(root.appendingPathComponent("d/b")) == "B")
    (notes, done) = Sync.apply(Sync.Changes(changed: [entry("out/new", "bad")]), root: root, state: &state) { _ in Data("bad".utf8) }
    #expect(done == ["out/new"] && notes.count == 1)  // potrjena (sicer bi jo router pošiljal v nedogled), ne zapisana
    #expect(gone(out.appendingPathComponent("new")))
    // Izbris poti prek povezave odstrani ciljno datoteko, izbris same povezave odstrani povezavo.
    state["rel/a"] = entry("rel/a", "A")
    _ = Sync.apply(Sync.Changes(deleted: ["rel/a"], base: ["rel/a": Sync.sha256(Data("A".utf8))]), root: root, state: &state) { _ in nil }
    #expect(gone(root.appendingPathComponent("d/a")))
    try put("x", root.appendingPathComponent("d/keep")); try link("d/keep", root.appendingPathComponent("fl"))
    _ = Sync.apply(Sync.Changes(deleted: ["fl"]), root: root, state: &state) { _ in nil }
    #expect(read(root.appendingPathComponent("d/keep")) == "x")  // povezava ni datoteka za sync: ni izbrisana
}

@Test func shareSyncApplyRefusesParentSwappedDuringFetch() throws {
    let root = try tmp("root"), out = try tmp("out"), fm = FileManager.default
    try fm.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    var state: [String: Sync.Entry] = [:]
    let (notes, done) = Sync.apply(Sync.Changes(changed: [entry("d/x", "gateway")]), root: root, state: &state) { _ in
        try? fm.moveItem(at: root.appendingPathComponent("d"), to: root.appendingPathComponent("old"))
        try? fm.createSymbolicLink(atPath: root.appendingPathComponent("d").path, withDestinationPath: out.path)
        return Data("gateway".utf8)
    }
    #expect(done.isEmpty && notes.joined().contains("cannot write"), "\(notes) \(done)")
    #expect(gone(out.appendingPathComponent("x")))
}

/// Sync: povezave se ne pošiljajo (ne datoteka, ne mapa) in se ne berejo; poizvedba po povezavi vrne nil.
@Test func shareSyncScanSkipsLinksAndReadRefusesThem() throws {
    let root = try tmp("root"), out = try tmp("out")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try put("real", root.appendingPathComponent("d/a")); try put("secret", out.appendingPathComponent("s"))
    try link("d/a", root.appendingPathComponent("fl")); try link("d", root.appendingPathComponent("dl"))
    try link(out.path, root.appendingPathComponent("out")); try link("nowhere", root.appendingPathComponent("dangling"))
    #expect(Set(Sync.scan(root, trusted: true).files.keys) == ["d/a"])
    #expect(Sync.readShared(root, "d/a") == Data("real".utf8))
    for p in ["fl", "dl/a", "out/s", "dangling"] { #expect(Sync.readShared(root, p) == nil, "\(p)") }
}

/// R1 F1: zavrnitev drevesa ne pusti odprtega deskriptorja.
@Test func shareRefusedTreeDoesNotLeakDescriptors() throws {
    let root = try tmp("root"), fm = FileManager.default
    try fm.createDirectory(at: root.appendingPathComponent("d/.claude"), withIntermediateDirectories: true)
    try put("{}", root.appendingPathComponent("d/.claude/settings.json"))
    let rt = try #require(ShareRoot(root))
    // Le deskriptorji na poti te mape (F_GETPATH): drugi testi v vzporednem teku ne vplivajo na štetje.
    func mine() -> Int { (3..<1024).filter { ShareRoot.path(of: Int32($0))?.hasPrefix(root.path) == true }.count }
    let before = mine()
    for _ in 0..<50 { #expect(!rt.removeAll("d")) }
    #expect(mine() == before, "leaked \(mine() - before)")
    #expect(!gone(root.appendingPathComponent("d/.claude/settings.json")))
}

/// R1 F2: nečitljiva podmapa je v zaščitenem pregledu zaščitena; F3: drevo se izbriše prek ene odprtine.
@Test func shareTreeProtectedFailsClosedAndRemoveAllDeletes() throws {
    let root = try tmp("root"), fm = FileManager.default
    try fm.createDirectory(at: root.appendingPathComponent("d/sub"), withIntermediateDirectories: true)
    try fm.createDirectory(at: root.appendingPathComponent("t/a/b/c"), withIntermediateDirectories: true)
    try put("x", root.appendingPathComponent("t/a/b/c/f")); try put("y", root.appendingPathComponent("t/g"))
    try link("g", root.appendingPathComponent("t/l"))
    let rt = try #require(ShareRoot(root))
    #expect(!rt.treeProtected("t", base: ["t"]) && !rt.treeProtected("missing", base: ["missing"]))
    try fm.setAttributes([.posixPermissions: 0], ofItemAtPath: root.appendingPathComponent("d/sub").path)
    defer { try? fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: root.appendingPathComponent("d/sub").path) }
    #expect(rt.treeProtected("d", base: ["d"]) && !rt.removeAll("d"))
    #expect(rt.removeAll("t") && gone(root.appendingPathComponent("t")))
}

/// R1 F4: preimenovana mapa deljenja še dela, zaščita velja glede na trenutno mesto.
@Test func shareRenamedRootKeepsWorking() throws {
    let root = try tmp("root"), fm = FileManager.default
    try fm.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    let rt = try #require(ShareRoot(root))
    let moved = root.deletingLastPathComponent().appendingPathComponent("moved-" + UUID().uuidString)
    try fm.moveItem(at: root, to: moved)
    defer { try? fm.removeItem(at: moved) }
    #expect(rt.writeAtomic("d/x", Data("ok".utf8), mode: nil, mtime: nil) == 0)
    #expect(read(moved.appendingPathComponent("d/x")) == "ok")
    #expect(rt.writeAtomic(".claude/settings.json", Data("{}".utf8), mode: nil, mtime: nil) != 0)
    #expect(rt.path?.hasSuffix(moved.lastPathComponent) == true)
}

/// R1 F5: »..« in absolutna pot sta zavrnjena v vsaki točki vstopa.
@Test func shareRefusesDotDotEverywhere() throws {
    let root = try tmp("root"), out = try tmp("out"), fm = FileManager.default
    try fm.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try put("o", out.appendingPathComponent("x")); try put("i", root.appendingPathComponent("f"))
    let rt = try #require(ShareRoot(root))
    let up = "../" + out.lastPathComponent + "/x"
    for bad in [up, "d/../f", "d/../../" + out.lastPathComponent, out.appendingPathComponent("x").path] {
        #expect(rt.lstat(bad) == nil && rt.readlink(bad) == nil && rt.openRead(bad) == nil && rt.list(bad) == nil, "\(bad)")
        #expect(rt.readRange(bad, start: 0, count: 1) == nil && rt.readAll(bad, max: 10) == nil, "\(bad)")
        #expect(rt.treeProtected(bad, base: ["x"]) && !rt.copyTree(bad, "c") && !rt.copyTree("f", bad), "\(bad)")
        #expect(rt.mkdir(bad) != 0 && !rt.mkdirAll(bad) && !rt.remove(bad) && !rt.rename("f", bad) && rt.create(bad) == nil, "\(bad)")
        #expect(throws: ShareRoot.Refused.self) { try rt.resolve(bad, followLast: true) }
    }
    #expect((try? fm.contentsOfDirectory(atPath: out.path)) == ["x"] && read(out.appendingPathComponent("x")) == "o")
}

/// R1 F6: celotna datoteka prek ene odprtine do meje; večja = nil.
@Test func shareReadSharedReadsWholeFileWithinLimit() throws {
    let root = try tmp("root")
    try put(String(repeating: "a", count: 200_000), root.appendingPathComponent("big"))
    #expect(Sync.readShared(root, "big")?.count == 200_000)
    let rt = try #require(ShareRoot(root))
    #expect(rt.readAll("big", max: 1000) == nil && rt.readAll("big", max: 200_000)?.count == 200_000)
}

/// R1 F8: čas pred letom 1970 ne da negativnega tv_nsec.
@Test func shareWriteAtomicNegativeMtime() throws {
    let root = try tmp("root")
    let rt = try #require(ShareRoot(root))
    #expect(rt.writeAtomic("old", Data("x".utf8), mode: 0o644, mtime: -1500) == 0)
    let st = try #require(rt.lstat("old"))
    #expect(st.st_mtimespec.tv_sec == -2 && st.st_mtimespec.tv_nsec == 500_000_000)
}


/// G2: pregled in brisanje ne gresta globlje od `maxDepth` (zaščiteno oziroma neizbrisano).
@Test func shareTreeDepthIsBounded() throws {
    let root = try tmp("root"), fm = FileManager.default
    try fm.createDirectory(at: root.appendingPathComponent("t/a/b/c/d"), withIntermediateDirectories: true)
    let rt = try #require(ShareRoot(root))
    let old = ShareRoot.maxDepth
    defer { ShareRoot.maxDepth = old }
    ShareRoot.maxDepth = 10
    #expect(!rt.treeProtected("t", base: ["t"]) && rt.removeAll("t"))
    try fm.createDirectory(at: root.appendingPathComponent("u/a/b/c/d/e/f/g/h/i/j/k"), withIntermediateDirectories: true)
    ShareRoot.maxDepth = 3
    #expect(rt.treeProtected("u", base: ["u"]) && !rt.removeAll("u"))
    #expect(!gone(root.appendingPathComponent("u/a")))
}

/// G3 (F3): mapa se po branju seznama zamenja z drugo pravo mapo; brisanje gre po prvotni odprtini, druga mapa ostane.
@Test func shareRemoveAllUsesOneOpenFolder() throws {
    let root = try tmp("root"), fm = FileManager.default
    try fm.createDirectory(at: root.appendingPathComponent("t"), withIntermediateDirectories: true)
    try fm.createDirectory(at: root.appendingPathComponent("other"), withIntermediateDirectories: true)
    try put("f", root.appendingPathComponent("t/f")); try put("keep", root.appendingPathComponent("other/keep"))
    let rt = try #require(ShareRoot(root))
    let target = try #require(rt.path) + "/t"
    ShareRoot.afterList = { dir in
        guard dir == target else { return }
        try? fm.moveItem(at: root.appendingPathComponent("t"), to: root.appendingPathComponent("t-old"))
        try? fm.moveItem(at: root.appendingPathComponent("other"), to: root.appendingPathComponent("t"))
    }
    defer { ShareRoot.afterList = nil }
    #expect(!rt.removeAll("t"))  // izprazni prvotno mapo, a imena »t« (zdaj druga, polna mapa) ne odstrani
    #expect(read(root.appendingPathComponent("t/keep")) == "keep", "the folder swapped in was emptied")
    #expect(gone(root.appendingPathComponent("t-old/f")))
}

/// Testa si delita globalni `WebDAV.beforeMutation`: izvajata se zaporedno (sicer eden pobriše kavelj drugemu).
@Suite(.serialized) struct ShareHookTests {
    /// Starš se po preverjanju zamenja s povezavo: operacija mora zavrniti (kot Go TestDavRefusesProtectedParentSwappedBeforeMutation).
    @Test func shareParentSwappedBeforeMutationIsRefused() throws {
        for method in ["PUT", "MKCOL", "DELETE", "LOCK", "MOVE", "COPY"] {
            let root = try tmp("root"), out = try tmp("out")
            let fm = FileManager.default
            try fm.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
            try fm.createDirectory(at: root.appendingPathComponent(".claude"), withIntermediateDirectories: true)
            try put("keep", out.appendingPathComponent("x"))
            try put("keep", root.appendingPathComponent("d/x"))
            try put("src", root.appendingPathComponent("src"))
            for target in [out.path, root.appendingPathComponent(".claude").path] {
                try fm.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
                try put("keep", root.appendingPathComponent("d/x"))
                WebDAV.beforeMutation = { r in
                    guard r == root.path else { return }
                    try? fm.moveItem(at: root.appendingPathComponent("d"), to: root.appendingPathComponent("old\(UUID().uuidString)"))
                    try? fm.createSymbolicLink(atPath: root.appendingPathComponent("d").path, withDestinationPath: target)
                }
                defer { WebDAV.beforeMutation = nil; try? fm.removeItem(at: root.appendingPathComponent("d")) }
                let r: DAVResponse
                switch method {
                case "MOVE", "COPY": r = dav(root, method, "/src", ["destination": "/s/d/moved"])
                case "DELETE": r = dav(root, method, "/d/x")
                default: r = dav(root, method, "/d/new", [:], "bad")
                }
                #expect(isLink(root.appendingPathComponent("d")), "\(method): the swap did not happen")
                #expect(r.status >= 400, "\(method) -> \(target): \(r.status)")
                #expect((try? fm.contentsOfDirectory(atPath: out.path)) == ["x"] && read(out.appendingPathComponent("x")) == "keep", "\(method) outside")
                #expect((try? fm.contentsOfDirectory(atPath: root.appendingPathComponent(".claude").path)) == [], "\(method) .claude")
            }
        }
    }

    /// Mapa, preimenovana na zaščiteno ime pred pisanjem: odpiranje po prvotnem imenu ne uspe, nič ne pristane v .claude.
    /// Preverjanje F_GETPATH v `parent()` ta test ne doseže (brez povezave je pot odprte mape vedno enaka zahtevani).
    @Test func shareParentRenamedAwayBeforePutIsRefusedNearProtectedName() throws {
        let root = try tmp("root"), fm = FileManager.default
        try fm.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
        try fm.createDirectory(at: root.appendingPathComponent(".claude"), withIntermediateDirectories: true)
        WebDAV.beforeMutation = { r in
            guard r == root.path else { return }
            try? fm.moveItem(at: root.appendingPathComponent(".claude"), to: root.appendingPathComponent("aside"))
            try? fm.moveItem(at: root.appendingPathComponent("d"), to: root.appendingPathComponent(".claude"))
        }
        defer { WebDAV.beforeMutation = nil }
        #expect(dav(root, "PUT", "/d/settings.json", [:], "{}").status >= 400)
        #expect((try? fm.contentsOfDirectory(atPath: root.appendingPathComponent(".claude").path)) == [])
    }
}
