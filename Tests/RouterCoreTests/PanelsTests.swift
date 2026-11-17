import Foundation
import Testing
@testable import RouterCore

// Panels (format 1): the description fixtures that the Go core also reads (client/testdata/panels-*.json), the validator's
// refusals, and the model: the exact request of each action of "Users and devices", the placeholder rules, the states.

private let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
private let testdata = repo.appendingPathComponent("client/testdata")
private let haveFixtures = FileManager.default.fileExists(atPath: testdata.appendingPathComponent("panels.json").path)

private func fixture(_ name: String) throws -> Data { try Data(contentsOf: testdata.appendingPathComponent(name)) }
private func object(_ name: String) throws -> [String: Any] { try JSONSerialization.jsonObject(with: fixture(name)) as! [String: Any] }
private func data(_ o: Any) -> Data { try! JSONSerialization.data(withJSONObject: o) }

/// A fake core: answers per "METHOD path" and records every request as "METHOD path?query body".
private actor Fake {
    var log: [String] = []
    var answers: [String: (Int, String)] = [:]
    func set(_ k: String, _ status: Int, _ body: String) { answers[k] = (status, body) }
    func handle(_ r: CoreRequest) -> CoreResponse {
        log.append(r.method + " " + r.path + (r.query.isEmpty ? "" : "?" + r.query) + (r.body.isEmpty ? "" : " " + String(decoding: r.body, as: UTF8.self)))
        let (s, b) = answers[r.method + " " + r.path] ?? (404, #"{"error":{"message":"nothing here"}}"#)
        return CoreResponse(status: s, body: Data(b.utf8))
    }
    func clear() { log = [] }
}

private func users() throws -> PanelDescription {
    let panels = try object("panels.json")["panels"] as! [Any]
    guard case .ready(let d) = PanelValidator.parse(data(panels[0])) else { throw Err.bad }
    return d
}
private enum Err: Error { case bad }

private let userAnswers: [(String, Int, String)] = [
    ("GET /users", 200, #"{"users":[{"id":1,"name":"acme","active":true,"owner":true},{"id":2,"name":"shop","active":true,"owner":false},{"id":3,"name":"old","active":false,"owner":false}]}"#),
    ("GET /devices", 200, #"{"devices":[{"id":"dev-1","name":"Mac"},{"id":"dev-2","name":"PC"}]}"#),
    ("GET /bindings", 200, #"{"bindings":[{"device":"dev-1","user":1}]}"#),
]

@MainActor private func usersModel(_ fake: Fake, lang: PanelLang = .en) async throws -> PanelModel {
    for (k, s, b) in userAnswers { await fake.set(k, s, b) }
    let m = PanelModel(panel: try users(), lang: lang, send: { await fake.handle($0) })
    await m.load()
    await fake.clear()
    return m
}

@Suite struct PanelFixtureTests {
    @Test(.enabled(if: haveFixtures)) func registryMatchesGo() throws {
        let reg = try object("panels-registry.json")
        #expect(reg["builtin"] as? [String] == PanelRoutes.builtin)
    }

    @Test(.enabled(if: haveFixtures)) func builtinDescriptionDecodesAndValidates() throws {
        let d = try users()
        #expect(d.id == "builtin.users" && d.title.sl == "Uporabniki in naprave")
        #expect(d.sections.count == 3 && d.refs.keys.sorted() == ["devices", "users"])
        // the user name limit is the server's: core.validName, 128 bytes
        let add = d.sections[0].actions![0]
        #expect(add.fields![0].maxLength == 128)
    }

    @Test(.enabled(if: haveFixtures)) func format1FixtureCoversTheWholeFormat() throws {
        let f = try object("panels-format1.json")
        guard case .ready(let d) = PanelValidator.parse(data(f["panel"]!), provider: f["provider"] as? String, routes: f["routes"] as? [String]) else { throw Err.bad }
        let orders = d.sections[1]
        #expect(d.sections[0].kind == "text" && orders.kind == "list")
        #expect(orders.columns!.map(\.kind) == ["text", "multiline", "bool", "enum", "ref", "time"])
        #expect(orders.paging != nil && orders.hide != nil && orders.filter != nil && orders.detail?.sections.count == 2)
        #expect(orders.actions![0].fields!.map(\.type) == ["text", "ref", "choice", "toggle"])
        // a provider that registered fewer routes cannot show it
        let fewer = Array((f["routes"] as! [String]).dropFirst())
        if case .ready = PanelValidator.parse(data(f["panel"]!), provider: "acme", routes: fewer) { Issue.record("accepted with a missing route") }
    }

    @Test(.enabled(if: haveFixtures)) func hostileTableIsRefused() throws {
        let f = try object("panels-format1.json")
        let cases = try object("panels-hostile.json")["cases"] as! [[String: Any]]
        #expect(cases.count >= 40)
        for c in cases {
            var doc = f["panel"]!
            for (ptr, v) in (c["set"] as? [String: Any] ?? [:]) { doc = edit(doc, ptr) { _ in v } }
            for ptr in (c["remove"] as? [String] ?? []) { doc = edit(doc, ptr, remove: true) { $0 } }
            for (ptr, n) in (c["grow"] as? [String: Int] ?? [:]) { doc = edit(doc, ptr) { var a = $0 as! [Any]; while a.count < n { a.append(a[0]) }; return a } }
            for (ptr, n) in (c["pad"] as? [String: Int] ?? [:]) { doc = edit(doc, ptr) { _ in String(repeating: "x", count: n) } }
            let parsed = PanelValidator.parse(data(doc), provider: "acme", routes: f["routes"] as? [String])
            let name = c["name"] as! String
            switch c["swift"] as! String {
            case "refuse":
                if case .ready = parsed { Issue.record("accepted: \(name)") }
            case "accept":
                guard case .ready = parsed else { Issue.record("refused (this side ignores unknown keys): \(name): \(parsed)"); continue }
            default:  // placeholder: known format, unknown element
                guard case .ready(let d) = parsed else { Issue.record("refused: \(name): \(parsed)"); continue }
                let orders = d.sections.count > 1 ? d.sections[1] : d.sections[0]
                let bad = !d.sections[0].supported || (orders.columns ?? []).contains { !$0.supported } || (orders.actions ?? []).contains { !$0.supported }
                #expect(bad, "no visible placeholder: \(name)")
            }
        }
    }
}

/// A JSON pointer edit on a JSONSerialization tree: `f` gives the new value from the old one; `remove` deletes the key.
private func edit(_ doc: Any, _ ptr: String, remove: Bool = false, _ f: (Any) -> Any) -> Any {
    func go(_ node: Any, _ path: ArraySlice<String>) -> Any {
        let head = path.first!
        if var o = node as? [String: Any] {
            if path.count == 1 { if remove { o[head] = nil } else { o[head] = f(o[head] as Any) }; return o }
            o[head] = go(o[head]!, path.dropFirst()); return o
        }
        var a = node as! [Any]
        let i = Int(head)!
        if path.count == 1 { a[i] = f(a[i]); return a }
        a[i] = go(a[i], path.dropFirst()); return a
    }
    return go(doc, ArraySlice(ptr.dropFirst().split(separator: "/", omittingEmptySubsequences: false).map(String.init)))
}

@MainActor @Suite struct PanelModelTests {
    @Test(.enabled(if: haveFixtures)) func loadsRefsThenListsAndShowsLabels() async throws {
        let fake = Fake()
        for (k, s, b) in userAnswers { await fake.set(k, s, b) }
        let m = PanelModel(panel: try users(), lang: .en, send: { await fake.handle($0) })
        await m.load()
        let log1 = await fake.log
        #expect(log1 == ["GET /devices", "GET /users", "GET /users", "GET /bindings"])
        let bindings = m.panel.sections[1]
        let row = m.visibleRows(bindings, path: "bindings")[0]
        #expect(bindings.columns!.map { m.cell($0, row) } == ["Mac", "acme"])  // ids resolved through refs
        let u = m.panel.sections[0]
        #expect(m.visibleRows(u, path: "users").map(\.id) == ["1", "2"])  // inactive rows are hidden …
        m.setShowHidden(true, path: "users")
        #expect(m.visibleRows(u, path: "users").map(\.id) == ["1", "2", "3"])  // … until the toggle is on
        m.setSearch("SHO", path: "users")
        #expect(m.visibleRows(u, path: "users").map(\.id) == ["2"])
    }

    @Test(.enabled(if: haveFixtures)) func exactRequestsOfTheUsersAndDevicesActions() async throws {
        let fake = Fake()
        let m = try await usersModel(fake)
        let u = m.panel.sections[0], b = m.panel.sections[1]
        let shop = m.visibleRows(u, path: "users")[1]
        let add = u.actions![0], rename = u.rowActions![0], off = u.rowActions![1], on = u.rowActions![2]
        #expect(try m.request(add, values: ["name": .string("acme two")]).get() == CoreRequest(method: "POST", path: "/users", body: Data(#"{"name":"acme two"}"#.utf8)))
        #expect(try m.request(rename, row: shop, values: ["name": .string("web")]).get() == CoreRequest(method: "PATCH", path: "/users/2", body: Data(#"{"name":"web"}"#.utf8)))
        #expect(try m.request(off, row: shop).get() == CoreRequest(method: "PATCH", path: "/users/2", body: Data(#"{"active":false}"#.utf8)))
        #expect(try m.request(on, row: shop).get() == CoreRequest(method: "PATCH", path: "/users/2", body: Data(#"{"active":true}"#.utf8)))
        let bind = b.actions![0], unbind = b.rowActions![0]
        #expect(try m.request(bind, values: ["device": .string("dev-2"), "user": .int(2)]).get()
                == CoreRequest(method: "PUT", path: "/bindings/dev-2", body: Data(#"{"user":2}"#.utf8)))  // the id stays a number
        let bound = m.visibleRows(b, path: "bindings")[0]
        #expect(try m.request(unbind, row: bound).get() == CoreRequest(method: "DELETE", path: "/bindings/dev-1"))
        // "owner" is never sent, and the rename default is the row's name
        #expect(m.defaults(rename, row: shop) == ["name": .string("shop")])
        // an incomplete form is not sent
        #expect(m.request(add, values: ["name": .string("  ")]) == .failure(.refused("the form is not complete")))
        #expect(m.problems(add, values: ["name": .string(String(repeating: "a", count: 129))]) == ["name": .tooLong(128)])
        let log2 = await fake.log
        #expect(log2.isEmpty)
    }

    @Test(.enabled(if: haveFixtures)) func whenDecidesWhichRowActionsAreShown() async throws {
        let m = try await usersModel(Fake())
        let u = m.panel.sections[0]
        m.setShowHidden(true, path: "users")
        let rows = m.visibleRows(u, path: "users")
        #expect(m.rowActions(u, rows[0]).map(\.id) == ["rename", "deactivate"])
        #expect(m.rowActions(u, rows[2]).map(\.id) == ["rename", "activate"])
    }

    @Test(.enabled(if: haveFixtures)) func destructiveActionsNeedTheConfirmation() async throws {
        let fake = Fake()
        let m = try await usersModel(fake)
        let u = m.panel.sections[0], b = m.panel.sections[1]
        let shop = m.visibleRows(u, path: "users")[1]
        let off = u.rowActions![1], unbind = b.rowActions![0]
        #expect(m.confirmation(off, row: shop)?.title == "Deactivate shop?")
        #expect(m.confirmation(off, row: shop)?.destructive == true && m.confirmation(unbind, row: m.visibleRows(b, path: "bindings")[0])?.destructive == true)
        #expect(m.confirmation(u.rowActions![2], row: shop) == nil)
        let sent = await m.perform(off, section: "users", row: shop)  // not confirmed: nothing is sent
        let log3 = await fake.log
        #expect(!sent && m.actionError[PanelListKey(path: "users", parent: "")] == .refused("the action needs a confirmation") && log3.isEmpty)
        await fake.set("PATCH /users/2", 200, #"{"ok":true}"#)
        let ok = await m.perform(off, section: "users", row: shop, confirmed: true)
        #expect(ok)
        // the panel is reloaded after the action
        let log4 = await fake.log
        #expect(log4 == ["PATCH /users/2 {\"active\":false}", "GET /devices", "GET /users", "GET /users", "GET /bindings"])
    }

    @Test(.enabled(if: haveFixtures)) func aFailedActionShowsTheServersMessageAndKeepsTheRows() async throws {
        let fake = Fake()
        let m = try await usersModel(fake)
        let u = m.panel.sections[0]
        let before = m.data("users").rows
        await fake.set("POST /users", 400, #"{"type":"error","error":{"type":"invalid_request_error","message":"name already used"}}"#)
        #expect(await !m.perform(u.actions![0], section: "users", values: ["name": .string("acme")]))
        #expect(m.actionError[PanelListKey(path: "users", parent: "")] == .server("name already used"))
        let log5 = await fake.log
        #expect(m.data("users").rows == before && log5.count == 1)  // no reload
        await fake.set("POST /users", 503, String(decoding: CoreResponse.unavailable.body, as: UTF8.self))
        #expect(await !m.perform(u.actions![0], section: "users", values: ["name": .string("acme")]))
        #expect(m.actionError[PanelListKey(path: "users", parent: "")] == .unavailable)
        await fake.set("POST /users", 500, "garbage")
        _ = await m.perform(u.actions![0], section: "users", values: ["name": .string("acme")])
        #expect(m.actionError[PanelListKey(path: "users", parent: "")] == .http(500))
    }

    @Test(.enabled(if: haveFixtures)) func aValueNeverBecomesMoreThanOnePathSegment() async throws {
        let m = try await usersModel(Fake())
        let b = m.panel.sections[1]
        let unbind = b.rowActions![0], bind = b.actions![0]
        let hostile = ["a/b", "..", "a..b", ".", "a?x=1", "a#f", "", "a\nb", "a\u{0}b", "a\u{7f}b", "a\u{202e}b", "a\u{2028}b", "a\\b", "../users", "/users"]
        for v in hostile {
            let row = PanelRow(id: "x", values: ["device": .string(v), "user": .int(1)])
            if case .success(let r) = m.request(unbind, row: row) { Issue.record("a row value \(v.debugDescription) became \(r.path)") }
            if case .success(let r) = m.request(bind, values: ["device": .string(v), "user": .int(1)]) { Issue.record("a field value \(v.debugDescription) became \(r.path)") }
        }
        // null, objects and fractions are not text either
        for j: PanelJSON in [.null, .object([:]), .array([]), .double(1.5)] {
            if case .success(let r) = m.request(unbind, row: PanelRow(id: "x", values: ["device": j])) { Issue.record("\(j) became \(r.path)") }
        }
        // a plain value is encoded, one segment
        for (v, want) in [("dev 1", "dev%201"), ("a.b", "a.b"), ("é", "%C3%A9"), ("50%", "50%25"), ("a+b", "a%2Bb")] {
            #expect(try m.request(unbind, row: PanelRow(id: "x", values: ["device": .string(v)])).get().path == "/bindings/" + want)
        }
    }

    @Test(.enabled(if: haveFixtures)) func templateRulesMatchTheGoValidator() {
        #expect(PanelTemplate.parse("/users/{row.id}") == [.literal("users"), .ref(.row, "id")])
        for bad in ["users", "/users/", "/users//x", "/users/x{row.id}", "/users/{row.id}{row.id}", "/users/{id}", "/users/{row.}", "/users/..", "/users/%2e%2e", "/users/a b", "/users?x", "/users/{other.id}"] {
            #expect(PanelTemplate.parse(bad) == nil, "\(bad)")
        }
        #expect(PanelTemplate.route("PATCH", "/users/{row.id}") == "PATCH users/{}")
        #expect(PanelTemplate.resolve(query: "item={field.item}&mode=draft", .init(field: ["item": .string("a b")])) == "item=a%20b&mode=draft")
        #expect(PanelTemplate.resolve(query: "item={field.item}", .init(field: ["item": .string("a&b=c")])) == "item=a%26b%3Dc")
    }

    @Test(.enabled(if: haveFixtures)) func renderersUseTheSlovenianStringsWhenTheAppIsSlovenian() async throws {
        #expect(PanelLang.choose(["sl-SI", "en"]) == .sl && PanelLang.choose(["en-US"]) == .en && PanelLang.choose([]) == .en && PanelLang.choose(["de"]) == .en)
        let fake = Fake()
        let m = try await usersModel(fake, lang: .sl)
        let u = m.panel.sections[0]
        let row = m.visibleRows(u, path: "users")[0]
        #expect(m.cell(u.columns![1], row) == "Aktiven")
        #expect(m.confirmation(u.rowActions![1], row: row)?.title == "Deaktiviram uporabnika acme?")
        let en = try await usersModel(Fake())
        #expect(en.cell(u.columns![1], en.visibleRows(u, path: "users")[0]) == "Active")
    }

    @Test(.enabled(if: haveFixtures)) func untrustedTextStaysTheSameString() async throws {
        let fake = Fake()
        let nasty = ["<script>alert(1)</script>", "<img src=x onerror=alert(1)>", "[x](https://evil.example)", "**b** _i_ `c`", "\\*raw\\*", "{name} {row.id}", "%@ %d"]
        let rows = nasty.enumerated().map { #"{"id":\#($0.offset + 1),"name":\#(String(decoding: try! JSONEncoder().encode($0.element), as: UTF8.self)),"active":true}"# }
        await fake.set("GET /users", 200, #"{"users":[\#(rows.joined(separator: ","))]}"#)
        await fake.set("GET /devices", 200, #"{"devices":[]}"#); await fake.set("GET /bindings", 200, #"{"bindings":[]}"#)
        let m = PanelModel(panel: try users(), lang: .en, send: { await fake.handle($0) })
        await m.load()
        let u = m.panel.sections[0]
        let shown = m.visibleRows(u, path: "users")
        #expect(shown.map { m.cell(u.columns![0], $0) } == nasty)
        // a confirmation takes the value as plain text, once: a value with a placeholder in it is not filled again
        let t = m.confirmation(u.rowActions![1], row: shown[5])!.title
        #expect(t == "Deactivate {name} {row.id}?")
    }

    @Test(.enabled(if: haveFixtures)) func refsOverTheLimitAreAnErrorNotATruncation() async throws {
        let fake = Fake()
        for (k, s, b) in userAnswers { await fake.set(k, s, b) }
        let many = (1...1001).map { #"{"id":\#($0),"name":"u\#($0)"}"# }.joined(separator: ",")
        await fake.set("GET /users", 200, #"{"users":[\#(many)]}"#)
        let m = PanelModel(panel: try users(), lang: .en, send: { await fake.handle($0) })
        await m.load()
        #expect(m.refError == .refused("more than 1000 entries in /users") && m.refs["users"] == [])
    }

    @Test(.enabled(if: haveFixtures)) func pagingAndDetailPagesOfTheFormat1Fixture() async throws {
        let f = try object("panels-format1.json")
        guard case .ready(let d) = PanelValidator.parse(data(f["panel"]!), provider: "acme", routes: f["routes"] as? [String]) else { throw Err.bad }
        let fake = Fake()
        await fake.set("GET /plugins/acme/items", 200, #"{"items":[{"sku":"s1","title":"Shirt"}]}"#)
        await fake.set("GET /plugins/acme/orders", 200, #"{"orders":[{"id":5,"name":"first","paid":true,"state":"new","item":"s1","created":"2026-11-17T10:00:00Z","note":"a\nb"}],"next":6}"#)
        await fake.set("GET /plugins/acme/orders/5/lines", 200, #"{"lines":[{"id":9,"title":"one"}]}"#)
        let m = PanelModel(panel: d, lang: .en, routes: f["routes"] as? [String], send: { await fake.handle($0) })
        await m.load()
        let orders = d.sections[1]
        #expect(m.data("orders").next == "6")
        await fake.set("GET /plugins/acme/orders", 200, #"{"orders":[{"id":6,"name":"second","state":"sent"}],"next":0}"#)
        await m.loadMore(orders, path: "orders")
        let log6 = await fake.log
        #expect(log6.last == "GET /plugins/acme/orders?after=6")
        #expect(m.data("orders").rows.map(\.id) == ["5", "6"] && m.data("orders").next == nil)
        // cells: ref label, enum label, bool label, time shown in local time (any text that is not the raw one)
        let row = m.data("orders").rows[0]
        #expect(orders.columns!.map { m.cell($0, row) }.prefix(5) == ["first", "a\nb", "Paid", "New", "Shirt"])
        #expect(m.cell(orders.columns![5], row) != "2026-11-17T10:00:00Z")
        // the detail page loads its lists with the opened row as {parent.id}, and its actions use both rows
        await m.openDetail(orders, row: row)
        let lines = orders.detail!.sections[1]
        let log7 = await fake.log
        #expect(log7.last == "GET /plugins/acme/orders/5/lines")
        let line = m.data("orders/lines", parent: row).rows[0]
        #expect(try m.request(lines.actions![0], parent: row, values: ["title": .string("two")]).get() == CoreRequest(method: "POST", path: "/plugins/acme/orders/5/lines", body: Data(#"{"title":"two"}"#.utf8)))
        #expect(try m.request(lines.rowActions![0], row: line, parent: row).get() == CoreRequest(method: "DELETE", path: "/plugins/acme/orders/5/lines/9"))
        // the create form: a query with a field placeholder, a constant, a toggle that defaults to off
        let add = orders.actions![0]
        let created = try m.request(add, values: ["name": .string("n"), "item": .string("s1"), "kind": .string("web"), "paid": .bool(false)]).get()
        #expect(created.path == "/plugins/acme/orders" && created.query == "mode=draft&item=s1")
        #expect(created.body == Data(#"{"count":1,"kind":"web","name":"n","paid":false}"#.utf8))
        // the open detail page is reloaded with the panel
        await fake.clear()
        await m.load()
        let log8 = await fake.log
        #expect(log8.contains("GET /plugins/acme/orders/5/lines"))
    }

    @Test(.enabled(if: haveFixtures)) func aCallToARouteTheProviderDidNotRegisterIsNotSent() async throws {
        // a model built with fewer routes than the description uses never sends the unregistered ones
        let fake = Fake()
        let d = try users()
        let m = PanelModel(panel: d, lang: .en, routes: ["GET users"], send: { await fake.handle($0) })
        await m.load()
        let log9 = await fake.log
        #expect(log9 == ["GET /users", "GET /users"])  // the ref and the list; devices and bindings are not registered
        #expect(m.request(d.sections[0].actions![0], values: ["name": .string("x")]) == .failure(.refused("the route POST /users is not registered")))
        // an unknown provider has no routes at all
        let none = PanelModel(panel: d, lang: .en, routes: [], send: { await fake.handle($0) })
        await none.load()
        let log10 = await fake.log
        #expect(log10 == ["GET /users", "GET /users"])
    }
}

@MainActor @Suite struct PanelCatalogTests {
    private func catalog(_ status: Int, _ body: String) async -> PanelCatalog {
        let c = PanelCatalog(send: { _ in CoreResponse(status: status, body: Data(body.utf8)) })
        await c.load()
        return c
    }

    @Test(.enabled(if: haveFixtures)) func theGoAnswerLoads() async throws {
        let c = await catalog(200, String(decoding: try fixture("panels.json"), as: UTF8.self))
        #expect(c.error == nil && c.entries.map(\.id) == ["builtin.users"])
        #expect(c.entries[0].title(.sl) == "Uporabniki in naprave" && c.entries[0].title(.en) == "Users and devices" && c.entries[0].sidebarSymbol == "person.2")
        guard case .ready = c.entries[0].parse else { throw Err.bad }
    }

    @Test(.enabled(if: haveFixtures)) func aHigherFormatIsNotGuessed() async throws {
        let good = try object("panels.json")["panels"] as! [[String: Any]]
        var newer = good[0]; newer["format"] = 2; newer["id"] = "builtin.future"; newer["sections"] = [["kind": "hologram"]]
        let c = await catalog(200, String(decoding: data(["format": 1, "panels": [newer, good[0]]]), as: UTF8.self))
        #expect(c.entries.map(\.id) == ["builtin.future", "builtin.users"])
        #expect(c.entries[0].parse == .needsNewer(2) && c.entries[0].title(.en) == "Users and devices")  // still in the sidebar
        let top = await catalog(200, #"{"format":2,"panels":[]}"#)
        #expect(top.error == .needsNewer && top.entries.isEmpty)
    }

    @Test(.enabled(if: haveFixtures)) func anUnknownElementKindIsAVisiblePlaceholder() async throws {
        var p = (try object("panels.json")["panels"] as! [[String: Any]])[0]
        var s = p["sections"] as! [[String: Any]]
        s.append(["kind": "chart", "id": "later"])
        var cols = s[0]["columns"] as! [[String: Any]]
        cols[0]["kind"] = "markdown"
        s[0]["columns"] = cols
        p["sections"] = s
        let c = await catalog(200, String(decoding: data(["format": 1, "panels": [p]]), as: UTF8.self))
        guard case .ready(let d) = c.entries[0].parse else { throw Err.bad }
        #expect(!d.sections[3].supported && d.sections[3].kind == "chart")
        let m = PanelModel(panel: d, lang: .en, send: { _ in .unavailable })
        #expect(m.unsupportedCount(d.sections[0]) == 1)  // the markdown column: its cell is replaced, nothing is rendered as markdown
    }

    @Test(.enabled(if: haveFixtures)) func aPanelThatFailsValidationHereStaysAsCannotBeShown() async throws {
        var p = (try object("panels.json")["panels"] as! [[String: Any]])[0]
        var s = p["sections"] as! [[String: Any]]
        var rows = s[0]["rowActions"] as! [[String: Any]]
        rows[1]["path"] = "/projects/{row.id}"  // another provider's route
        s[0]["rowActions"] = rows
        p["sections"] = s
        let c = await catalog(200, String(decoding: data(["format": 1, "panels": [p]]), as: UTF8.self))
        guard case .refused(let why) = c.entries[0].parse else { throw Err.bad }
        #expect(why.contains("is not registered"))
    }

    @Test func errorsOfTheListCall() async {
        let c1 = await catalog(404, #"{"error":{"message":"Not found"}}"#)
        #expect(c1.error == .notFound)
        let c2 = await catalog(503, String(decoding: CoreResponse.unavailable.body, as: UTF8.self))
        #expect(c2.error == .unavailable)
        let c3 = await catalog(500, #"{"error":{"message":"boom"}}"#)
        #expect(c3.error == .server("boom"))
        let c4 = await catalog(200, "[]")
        #expect(c4.error == .refused("GET /panels: not the expected answer"))
        let down = PanelCatalog(send: { _ in .unavailable })
        await down.load()
        #expect(down.error == .unavailable && down.loaded)
    }

    @Test func panelsIsAnAdminPathAndNotAMemberPath() {
        #expect(CoreRoutes.isAdminPath("GET", "/panels") && CoreRoutes.isAdminPath("GET", "/%70anels"))
        #expect(!CoreRoutes.isAdminPath("GET", "/panels/x") && !CoreRoutes.isMemberPath("/minigw/v1/panels"))
    }
}
