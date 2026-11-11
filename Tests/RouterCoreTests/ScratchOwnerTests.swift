import Foundation
import Testing
@testable import RouterCore

/// N4, odločitev 2: datoteka lastnika, zaklep, prevzem = preimenovanje (kot serve_scratchowner_test.go).
private func tempBase() throws -> URL {
    let u = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0].appendingPathComponent("minigw-test-owner-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: u, withIntermediateDirectories: true)
    return u
}

private func put(_ dir: URL, _ rel: String, _ text: String = "x") throws {
    let f = dir.appendingPathComponent(rel)
    try FileManager.default.createDirectory(at: f.deletingLastPathComponent(), withIntermediateDirectories: true)
    try Data(text.utf8).write(to: f)
}

@Test func scratchOwnerClaimLockAndTakeOver() throws {
    let fm = FileManager.default
    let base = try tempBase()
    defer { try? fm.removeItem(at: base) }
    let a = base.appendingPathComponent("ana"), other = base.appendingPathComponent("other")
    // prva raba zapiše lastnika; ta proces drži zaklep (kot drug prehod, ki teče)
    #expect(ScratchOwner.claim(a, id: "gw-old") == "" && ScratchOwner.owner(a) == "gw-old")
    #expect(ScratchOwner.state(a, id: "gw-old") == "")
    #expect(ScratchOwner.state(a, id: "gw-new") == ScratchOwner.inUse)
    try put(a, "worktrees/dev/f", "old")
    #expect(throws: (any Error).self) { try ScratchOwner.takeOver(a, id: "gw-new") }
    #expect(ScratchOwner.owner(a) == "gw-old")
    // zaklep gre na drugo mapo: stari lastnik ne teče več
    ScratchOwner.claim(other, id: "gw-old")
    #expect(ScratchOwner.state(a, id: "gw-new") == ScratchOwner.stale)
    // lastna mapa s pogovori: ob strani, nič združeno
    let dst = base.appendingPathComponent("gw-new")
    try put(dst, "state/s.json", "new")
    let day = Date(timeIntervalSince1970: 1_794_312_000)  // 2026-11-10
    let r = try ScratchOwner.takeOver(a, id: "gw-new", now: day)
    #expect(r.folder.path == dst.path && r.aside?.lastPathComponent == "gw-new.old-2026-11-10")
    #expect(!fm.fileExists(atPath: a.path) && ScratchOwner.owner(dst) == "gw-new")
    #expect((try? String(contentsOf: dst.appendingPathComponent("worktrees/dev/f"), encoding: .utf8)) == "old")
    #expect(!fm.fileExists(atPath: dst.appendingPathComponent("state").path))
    #expect((try? String(contentsOf: r.aside!.appendingPathComponent("state/s.json"), encoding: .utf8)) == "new")
    // lastna mapa brez pogovorov (tmp, predpomnilniki): izbrisana
    let b = base.appendingPathComponent("bob"), dst2 = base.appendingPathComponent("gw-two")
    try put(b, ScratchOwner.ownerFile, "gw-gone\n")
    try put(dst2, "tmp/x")
    try put(dst2, "caches/y")
    let r2 = try ScratchOwner.takeOver(b, id: "gw-two")
    #expect(r2.aside == nil && !fm.fileExists(atPath: dst2.appendingPathComponent("tmp/x").path) && ScratchOwner.owner(dst2) == "gw-two")
    ScratchOwner.blocked.withLock { $0 = false }
}

@Test func scratchConversationsAndTarget() throws {
    let base = try tempBase()
    defer { try? FileManager.default.removeItem(at: base) }
    try put(base, ScratchOwner.ownerFile)
    try put(base, "tmp/a")
    try put(base, "deriveddata/b")
    try FileManager.default.createDirectory(at: base.appendingPathComponent("sync/runs"), withIntermediateDirectories: true)
    #expect(!ScratchOwner.hasConversations(base))
    try put(base, "sync/runs/c")
    #expect(ScratchOwner.hasConversations(base))
    // odločitev 3: osnova → <osnova>/<instanca>; brez ID-ja kot prej
    var c = RouterConfig()
    #expect(c.scratchTarget(base).lastPathComponent == RouterConfig.dedicatedName && c.scratchURL == RouterConfig.defaultScratch)
    c.instanceID = "gw-abc"
    #expect(c.scratchTarget(base) == base.appendingPathComponent("gw-abc", isDirectory: true))
    #expect(c.scratchTarget(nil) == RouterConfig.defaultBase.appendingPathComponent("gw-abc", isDirectory: true))
    c.scratchFolder = base.path
    c.scratchName = "gw-abc"
    #expect(c.scratchURL.path == base.appendingPathComponent("gw-abc").path)
    // premik nese datoteko lastnika
    let to = base.deletingLastPathComponent().appendingPathComponent(base.lastPathComponent + "-moved")
    defer { try? FileManager.default.removeItem(at: to) }
    try RouterConfig.moveScratch(from: base, to: to)
    #expect(ScratchOwner.owner(to) == "x")
}

@Test func adminScratchTakeOver() async throws {
    let base = try tempBase()
    defer { try? FileManager.default.removeItem(at: base) }
    try put(base.appendingPathComponent("ana"), ScratchOwner.ownerFile, "gw-gone\n")
    var c = RouterConfig()
    c.instanceID = "gw-me"
    c.scratchFolder = base.path
    c.scratchName = "ana"
    let app = FakeApp(c)
    let g = await admin(app.router(), "GET", "/general")
    #expect(g.text.contains(#""scratchState":"stale""#), "\(g.text)")
    let r = await admin(app.router(scratchInUse: false), "POST", "/general/scratch/take")
    #expect(r.status == 200 && app.config.scratchName == "gw-me", "\(r.text)")
    #expect(ScratchOwner.owner(base.appendingPathComponent("gw-me")) == "gw-me")
    #expect(await admin(app.router(scratchInUse: false), "POST", "/general/scratch/take").status == 409)
}

/// Glavni primer po ponovni namestitvi: stara mapa je sosednja v osnovi; prevzem po imenu, trenutna mapa ob strani.
@Test func adminScratchSiblingTakeOver() async throws {
    let fm = FileManager.default
    let base = try tempBase()
    defer { try? fm.removeItem(at: base) }
    try put(base.appendingPathComponent("gw-old"), ScratchOwner.ownerFile, "gw-old\n")
    try put(base.appendingPathComponent("gw-old"), "keep", "old")
    try put(base.appendingPathComponent("gw-z.old-2026-01-01"), ScratchOwner.ownerFile, "gw-z\n")
    try put(base.appendingPathComponent("gw-me"), ScratchOwner.ownerFile, "gw-me\n")
    try put(base.appendingPathComponent("gw-me"), "chat/c.json", "new")
    var c = RouterConfig()
    c.instanceID = "gw-me"
    c.scratchFolder = base.path
    c.scratchName = "gw-me"
    let app = FakeApp(c)
    let g = await admin(app.router(), "GET", "/general")
    #expect(g.text.contains(#""scratchOthers":[{"#) && g.text.contains(#""name":"gw-old""#) && !g.text.contains("gw-z") && !g.text.contains("scratchState"), "\(g.text)")
    for bad in ["../gw-old", "nope", "gw-z.old-2026-01-01", "gw-me"] {
        #expect(await admin(app.router(scratchInUse: false), "POST", "/general/scratch/take", body: #"{"name":"\#(bad)"}"#).status == 400, "\(bad)")
    }
    let r = await admin(app.router(scratchInUse: false), "POST", "/general/scratch/take", body: #"{"name":"gw-old"}"#)
    #expect(r.status == 200 && r.text.contains("warning"), "\(r.text)")
    let me = base.appendingPathComponent("gw-me")
    #expect(ScratchOwner.owner(me) == "gw-me" && (try? String(contentsOf: me.appendingPathComponent("keep"), encoding: .utf8)) == "old")
    #expect(!fm.fileExists(atPath: base.appendingPathComponent("gw-old").path) && !fm.fileExists(atPath: me.appendingPathComponent("chat").path))
    #expect(app.config.scratchURL.path == me.path)
}

/// Zaprto: cev namesto datoteke lastnika ne blokira in šteje kot tuj lastnik; povezava se ne prevzame.
@Test func scratchOwnerFailClosed() throws {
    let fm = FileManager.default
    let base = try tempBase()
    defer { try? fm.removeItem(at: base) }
    let d = base.appendingPathComponent("fifo")
    try fm.createDirectory(at: d, withIntermediateDirectories: true)
    #expect(mkfifo(d.appendingPathComponent(ScratchOwner.ownerFile).path, 0o600) == 0)
    #expect(ScratchOwner.state(d, id: "gw-me") == ScratchOwner.stale)
    let real = base.appendingPathComponent("real"), link = base.appendingPathComponent("link")
    try put(real, ScratchOwner.ownerFile, "gw-gone\n")
    try fm.createSymbolicLink(at: link, withDestinationURL: real)
    #expect(throws: (any Error).self) { try ScratchOwner.takeOver(link, id: "gw-me") }
    #expect(ScratchOwner.siblings(base.appendingPathComponent("gw-me"), id: "gw-me").map(\.name) == ["fifo", "real"])
}

/// M1 (0.5.24): peskovniki skrijejo tudi mape ob strani, mape drugih instanc, staro ime in nadomestno mapo; osnove in
/// tujih map v njej ne.
@Test func hiddenScratchCoversSiblings() throws {
    let fm = FileManager.default
    let base = try tempBase()
    defer { try? fm.removeItem(at: base) }
    let cur = base.appendingPathComponent("gw-mine")
    try put(cur, "chat/x")
    try put(base, "gw-mine.old-2026-11-10/chat/x")
    try put(base, "gw-other/" + ScratchOwner.ownerFile, "gw-other\n")
    try put(base, "Mini Gateway Scratch/chat/x")
    try put(base, "gw-mine.old-2026-11-10-142233/chat/x")  // druga ob strani istega dne
    try put(base, "Photos/a.jpg")
    try put(base, "Work.old-2025/a.txt")  // uporabnikova mapa, ne ob strani
    let h = RouterConfig.hiddenScratch(cur)
    for n in ["gw-mine", "gw-mine.old-2026-11-10", "gw-mine.old-2026-11-10-142233", "gw-other", "Mini Gateway Scratch"] {
        #expect(h.contains(base.appendingPathComponent(n).path), "\(n): \(h)")
    }
    #expect(h.contains(RouterConfig.defaultBase.appendingPathComponent("gw-mine").path))  // nadomestna mapa scratch(_:)
    #expect(h.contains(RouterConfig.defaultScratch.path))
    #expect(!h.contains(base.appendingPathComponent("Work.old-2025").path))
    #expect(!h.contains(base.appendingPathComponent("Photos").path) && !h.contains(base.path) && !h.contains(base.standardizedFileURL.path))
}

/// L1 (0.5.24): osnova Scratch ne sme biti v mapi drugega prehoda (ali ta mapa sama); lastna mapa sme.
@Test func scratchBaseInsideOtherGatewayRefused() throws {
    let fm = FileManager.default
    let base = try tempBase()
    defer { try? fm.removeItem(at: base) }
    try put(base, "gw-other/" + ScratchOwner.ownerFile, "gw-other\n")
    try put(base, "gw-other/sub/x")
    try put(base, "gw-mine/" + ScratchOwner.ownerFile, "gw-mine\n")
    for bad in ["gw-other", "gw-other/sub"] {
        let why = RouterConfig.scratchProblem(base.appendingPathComponent(bad), allowedRoots: "", name: "gw-mine")
        #expect(why?.contains("another gateway's Scratch folder") == true, "\(bad): \(why ?? "nil")")
    }
    #expect(RouterConfig.scratchProblem(base, allowedRoots: "", name: "gw-mine") == nil)
    #expect(RouterConfig.scratchProblem(base.appendingPathComponent("gw-mine"), allowedRoots: "", name: "gw-mine") == nil)
}
