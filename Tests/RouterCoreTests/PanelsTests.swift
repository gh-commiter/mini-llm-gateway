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
    var delay: UInt64 = 0
    func slow(_ ns: UInt64) { delay = ns }
    func set(_ k: String, _ status: Int, _ body: String) { answers[k] = (status, body) }
    func handle(_ r: CoreRequest) async -> CoreResponse {
        log.append(r.method + " " + r.path + (r.query.isEmpty ? "" : "?" + r.query) + (r.body.isEmpty ? "" : " " + String(decoding: r.body, as: UTF8.self)))
        if delay > 0 { try? await Task.sleep(nanoseconds: delay) }
        let (s, b) = answers[r.method + " " + r.path] ?? (404, #"{"error":{"message":"nothing here"}}"#)
        return CoreResponse(status: s, body: Data(b.utf8))
    }
    func clear() { log = [] }
}

private func strings(_ locale: String) throws -> PanelStrings {
    guard let t = PanelStrings.parse(try fixture("panels-strings-\(locale).json")) else { throw Err.bad }
    return t
}

/// GET /panels names the provider next to each description; the descriptions of the fixture, and the answer for some.
private func descriptions() throws -> [[String: Any]] { (try object("panels.json")["panels"] as! [[String: Any]]).map { $0["description"] as! [String: Any] } }
private func description(_ id: String) throws -> [String: Any] { try descriptions().first { $0["id"] as? String == id }! }
private func answer(_ ds: [[String: Any]], provider: String = "builtin") -> String {
    String(decoding: data(["format": 1, "panels": ds.map { ["provider": provider, "description": $0] }]), as: UTF8.self)
}

private func users() throws -> PanelDescription {
    guard case .ready(let d) = PanelValidator.parse(data(try description("builtin.users")), provider: "builtin") else { throw Err.bad }
    return d
}
private enum Err: Error { case bad }

private let userAnswers: [(String, Int, String)] = [
    ("GET /users", 200, #"{"users":[{"id":1,"name":"acme","active":true,"owner":true},{"id":2,"name":"shop","active":true,"owner":false},{"id":3,"name":"old","active":false,"owner":false}]}"#),
    ("GET /devices", 200, #"{"devices":[{"id":"dev-1","name":"Mac"},{"id":"dev-2","name":"PC"}]}"#),
    ("GET /bindings", 200, #"{"bindings":[{"device":"dev-1","user":1}]}"#),
]

@MainActor private func usersModel(_ fake: Fake, locale: String = "C") async throws -> PanelModel {
    for (k, s, b) in userAnswers { await fake.set(k, s, b) }
    let m = PanelModel(panel: try users(), strings: try strings(locale), routes: PanelRoutes.builtin, send: { await fake.handle($0) })
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
        #expect(d.id == "builtin.users" && d.title == "builtin.users.title")  // a key, not a text
        #expect(d.sections.count == 3 && d.refs.keys.sorted() == ["devices", "users"])
        // the user name limit is the server's: core.validName, 128 bytes
        let add = d.sections[0].actions![0]
        #expect(add.fields![0].maxLength == 128)
    }

    @Test(.enabled(if: haveFixtures)) func format1FixtureCoversTheWholeFormat() throws {
        let f = try object("panels-format1.json")
        guard case .ready(let d) = PanelValidator.parse(data(f["panel"]!), provider: f["provider"] as! String, routes: f["routes"] as? [String]) else { throw Err.bad }
        let orders = d.sections[1]
        #expect(d.sections[0].kind == "text" && orders.kind == "list")
        #expect(orders.columns!.map(\.kind) == ["text", "multiline", "bool", "enum", "ref", "time"])
        #expect(orders.paging != nil && orders.hide != nil && orders.filter != nil && orders.detail?.sections.count == 3)
        #expect(orders.scope?.param == "item" && orders.detail?.sections[2].kind == "record" && orders.detail?.sections[2].when?.field == "paid")
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
        let m = PanelModel(panel: try users(), strings: try strings("C"), routes: PanelRoutes.builtin, send: { await fake.handle($0) })
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
        #expect(m.actionError[PanelListKey(path: "users", parent: "")] == .serverFault(500))  // no body text of a 5xx reaches the pane
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
        #expect(PanelLocale.tag(["sl-SI", "en"]) == "sl_SI" && PanelLocale.tag(["sl"]) == "sl" && PanelLocale.tag(["en-US"]) == "en_US")
        #expect(PanelLocale.tag(["zh-Hans-CN"]) == "zh_CN" && PanelLocale.tag([]) == "C" && PanelLocale.tag(["Base"]) == "C" && PanelLocale.tag(["../x"]) == "C")
        let fake = Fake()
        let m = try await usersModel(fake, locale: "sl_SI")
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
        let m = PanelModel(panel: try users(), strings: try strings("C"), routes: PanelRoutes.builtin, send: { await fake.handle($0) })
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
        let m = PanelModel(panel: try users(), strings: try strings("C"), routes: PanelRoutes.builtin, send: { await fake.handle($0) })
        await m.load()
        #expect(m.refError == .refused("more than 1000 entries in /users") && m.refs["users"] == [])
    }

    @Test(.enabled(if: haveFixtures)) func pagingAndDetailPagesOfTheFormat1Fixture() async throws {
        let f = try object("panels-format1.json")
        guard case .ready(let d) = PanelValidator.parse(data(f["panel"]!), provider: "acme", routes: f["routes"] as? [String]) else { throw Err.bad }
        let tabs = try JSONSerialization.jsonObject(with: fixture("panels-format1-strings.json")) as! [String: [String: String]]
        let fake = Fake()
        await fake.set("GET /plugins/acme/items", 200, #"{"items":[{"sku":"s1","title":"Shirt"}]}"#)
        await fake.set("GET /plugins/acme/orders", 200, #"{"orders":[{"id":5,"name":"first","paid":true,"state":"new","item":"s1","created":"2026-11-17T10:00:00Z","note":"a\nb"}],"next":6}"#)
        await fake.set("GET /plugins/acme/orders/5/lines", 200, #"{"lines":[{"id":9,"title":"one"}]}"#)
        await fake.set("GET /plugins/acme/orders/5/summary", 200, #"{"title":"s","note":"long"}"#)
        let m = PanelModel(panel: d, strings: PanelStrings(locale: "C", table: tabs["C"]!), routes: f["routes"] as! [String], send: { await fake.handle($0) })
        await m.load()
        let orders = d.sections[1]
        #expect(m.data("orders").next == "6")
        await fake.set("GET /plugins/acme/orders", 200, #"{"orders":[{"id":6,"name":"second","state":"sent"}],"next":0}"#)
        await m.loadMore(orders, path: "orders")
        let log6 = await fake.log
        #expect(log6.last == "GET /plugins/acme/orders?item=s1&after=6")
        #expect(m.data("orders").rows.map(\.id) == ["5", "6"] && m.data("orders").next == nil)
        // cells: ref label, enum label, bool label, time shown in local time (any text that is not the raw one)
        let row = m.data("orders").rows[0]
        #expect(orders.columns!.map { m.cell($0, row) }.prefix(5) == ["first", "a\nb", "Paid", "New", "Shirt"])
        #expect(m.cell(orders.columns![5], row) != "2026-11-17T10:00:00Z")
        // the detail page loads its lists with the opened row as {parent.id}, and its actions use both rows
        await m.openDetail(orders, row: row)
        let lines = orders.detail!.sections[1]
        let log7 = await fake.log
        #expect(log7.suffix(2) == ["GET /plugins/acme/orders/5/lines", "GET /plugins/acme/orders/5/summary"])  // the summary: the row is paid
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
        let m = PanelModel(panel: d, strings: PanelStrings(), routes: ["GET users"], send: { await fake.handle($0) })
        await m.load()
        let log9 = await fake.log
        #expect(log9 == ["GET /users", "GET /users"])  // the ref and the list; devices and bindings are not registered
        #expect(m.request(d.sections[0].actions![0], values: ["name": .string("x")]) == .failure(.refused("the route POST /users is not registered")))
        // an unknown provider has no routes at all
        let none = PanelModel(panel: d, strings: PanelStrings(), routes: [], send: { await fake.handle($0) })
        await none.load()
        let log10 = await fake.log
        #expect(log10 == ["GET /users", "GET /users"])
    }
}

@MainActor @Suite struct PanelCatalogTests {
    private func catalog(_ status: Int, _ body: String, locale: String = "C") async -> PanelCatalog {
        let texts = (try? fixture("panels-strings-\(locale).json")) ?? Data(#"{"format":1,"locale":"C","strings":{}}"#.utf8)
        let c = PanelCatalog(send: { r in r.path == "/panels/strings" ? CoreResponse(status: 200, body: texts) : CoreResponse(status: status, body: Data(body.utf8)) }, locale: locale)
        await c.load()
        return c
    }

    @Test(.enabled(if: haveFixtures)) func theGoAnswerLoads() async throws {
        let c = await catalog(200, String(decoding: try fixture("panels.json"), as: UTF8.self))
        #expect(c.error == nil && c.entries.map(\.id) == ["builtin.users", "builtin.kb"])  // the order of the answer
        let usersEntry = c.entries[0]
        #expect(usersEntry.title(c.strings) == "Users and devices" && usersEntry.sidebarSymbol == "person.2" && c.strings.locale == "C")
        #expect(c.entries[1].title(c.strings) == "Knowledge base" && c.entries[1].sidebarSymbol == "book")
        for e in c.entries { guard case .ready = e.parse else { throw Err.bad } }
        let sl = await catalog(200, String(decoding: try fixture("panels.json"), as: UTF8.self), locale: "sl_SI")
        #expect(sl.entries[0].title(sl.strings) == "Uporabniki in naprave" && sl.entries[1].title(sl.strings) == "Baza znanja" && sl.strings.locale == "sl_SI" && sl.stringsError == nil)
    }

    @Test(.enabled(if: haveFixtures)) func aMissingKeyIsShownAsTheKeyAndTheLanguageCarriesTheRest() throws {
        let t = PanelStrings(locale: "sl_SI", table: ["acme.a": "A"])
        #expect(t.text("acme.a") == "A" && t.text("acme.missing") == "acme.missing" && t.text(nil) == "")
        #expect(PanelStrings.parse(Data(#"{"format":2,"locale":"C","strings":{}}"#.utf8)) == nil && PanelStrings.parse(Data("[]".utf8)) == nil)
        // the table of the language answers for every key of the built-in panel, and C does too
        let d = try users()
        for loc in ["C", "sl_SI"] {
            let st = try strings(loc)
            var keys = Set<String>()
            func walk(_ s: [PanelSection]) {
                for x in s {
                    keys.formUnion([x.title, x.body, x.empty, x.hide?.toggle, x.scope?.label].compactMap { $0 })
                    for c in x.columns ?? [] { keys.formUnion([c.label, c.yes, c.no].compactMap { $0 } + (c.values ?? [:]).values) }
                    for a in (x.actions ?? []) + (x.rowActions ?? []) {
                        keys.insert(a.label)
                        if let c = a.confirm { keys.formUnion([c.title, c.message, c.button]) }
                        for f in a.fields ?? [] { keys.insert(f.label) }
                    }
                    walk(x.detail?.sections ?? [])
                }
            }
            walk(d.sections); keys.formUnion([d.title, d.about])
            #expect(keys.count > 20)
            for k in keys { #expect(st.table[k] != nil, "\(loc): no text for \(k)") }
        }
    }

    @Test func theStringsCallCarriesTheLocaleAndAFailureLeavesTheKeys() async {
        let seen = Fake()
        await seen.set("GET /panels", 200, #"{"format":1,"panels":[]}"#)
        let c = PanelCatalog(send: { await seen.handle($0) }, locale: "sl_SI")
        await c.load()
        let log = await seen.log
        #expect(log.contains("GET /panels/strings?lang=sl_SI") && c.stringsError == .server("nothing here") && c.strings.text("x.y") == "x.y")
    }

    @Test(.enabled(if: haveFixtures)) func theOrderOfTheAnswerIsTheOrderOfTheSidebar() async throws {
        let ds = [try description("builtin.kb"), try description("builtin.users")]
        let c = await catalog(200, answer(ds))
        #expect(c.entries.map(\.id) == ["builtin.kb", "builtin.users"])
        let d = [try description("builtin.users"), try description("builtin.kb")]
        #expect(await catalog(200, answer(d)).entries.map(\.id) == ["builtin.users", "builtin.kb"])
    }

    @Test(.enabled(if: haveFixtures)) func aHigherFormatIsNotGuessed() async throws {
        let good = [try description("builtin.users")]
        var newer = good[0]; newer["format"] = 2; newer["id"] = "builtin.future"; newer["sections"] = [["kind": "hologram"]]
        let c = await catalog(200, String(decoding: data(["format": 1, "panels": [["provider": "builtin", "description": newer], ["provider": "builtin", "description": good[0]]]]), as: UTF8.self))
        #expect(c.entries.map(\.id) == ["builtin.future", "builtin.users"])
        #expect(c.entries[0].parse == .needsNewer(2) && c.entries[0].title(c.strings) == "Users and devices")  // still in the sidebar
        let top = await catalog(200, #"{"format":2,"panels":[]}"#)
        #expect(top.error == .needsNewer && top.entries.isEmpty)
    }

    @Test(.enabled(if: haveFixtures)) func anUnknownElementKindIsAVisiblePlaceholder() async throws {
        var p = try description("builtin.users")
        var s = p["sections"] as! [[String: Any]]
        s.append(["kind": "chart", "id": "later"])
        var cols = s[0]["columns"] as! [[String: Any]]
        cols[0]["kind"] = "markdown"
        s[0]["columns"] = cols
        p["sections"] = s
        let c = await catalog(200, answer([p]))
        guard case .ready(let d) = c.entries[0].parse else { throw Err.bad }
        #expect(!d.sections[3].supported && d.sections[3].kind == "chart")
        let m = PanelModel(panel: d, strings: PanelStrings(), routes: PanelRoutes.builtin, send: { _ in .unavailable })
        #expect(m.unsupportedCount(d.sections[0]) == 1)  // the markdown column: its cell is replaced, nothing is rendered as markdown
    }

    @Test(.enabled(if: haveFixtures)) func aPanelThatFailsValidationHereStaysAsCannotBeShown() async throws {
        var p = try description("builtin.users")
        var s = p["sections"] as! [[String: Any]]
        var rows = s[0]["rowActions"] as! [[String: Any]]
        rows[1]["path"] = "/projects/{row.id}"  // another provider's route
        s[0]["rowActions"] = rows
        p["sections"] = s
        let c = await catalog(200, answer([p]))
        guard case .refused(let why) = c.entries[0].parse else { throw Err.bad }
        #expect(why.contains("is not registered"))
    }

    @Test func errorsOfTheListCall() async {
        let c1 = await catalog(404, #"{"error":{"message":"Not found"}}"#)
        #expect(c1.error == .notFound)
        let c2 = await catalog(503, String(decoding: CoreResponse.unavailable.body, as: UTF8.self))
        #expect(c2.error == .unavailable)
        let c3 = await catalog(500, #"{"error":{"message":"boom"}}"#)
        #expect(c3.error == .serverFault(500))  // "boom" may be SQL text: only the status is kept
        let c3b = await catalog(400, #"{"error":{"message":"name already used"}}"#)
        #expect(c3b.error == .server("name already used"))
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

@MainActor @Suite struct PanelTrustTests {
    @Test(.enabled(if: haveFixtures)) func theRegistrysProviderIdDecidesNotTheDescriptions() async throws {
        let d = try description("builtin.users")  // says "builtin"
        // the registry says acme: refused visibly, the built-in routes are not used
        let c = PanelCatalog(send: { r in r.path == "/panels" ? CoreResponse(status: 200, body: Data(answer([d], provider: "acme").utf8)) : .unavailable }, locale: "C")
        await c.load()
        guard case .refused(let why) = c.entries[0].parse else { throw Err.bad }
        #expect(why.contains("registry says acme") && c.entries[0].provider == "acme")
        // a provider id this renderer does not know has no routes
        #expect(PanelRoutes.registered(for: "acme") == nil)
        if case .ready = PanelValidator.parse(data(d), provider: "acme") { Issue.record("accepted under acme") }
        // an entry without a provider is skipped, not guessed
        let none = PanelCatalog(send: { r in r.path == "/panels" ? CoreResponse(status: 200, body: Data(#"{"format":1,"panels":[{"description":{}}]}"#.utf8)) : .unavailable }, locale: "C")
        await none.load()
        #expect(none.entries.isEmpty)
    }

    @Test func theTransportForwardsOnlyTheRegisteredRoutes() async {
        let fake = Fake()
        await fake.set("GET /users", 200, #"{"users":[]}"#)
        await fake.set("GET /panels", 200, "{}")
        await fake.set("GET /panels/strings", 200, "{}")
        await fake.set("PATCH /users/7", 200, "{}")
        await fake.set("DELETE /bindings/dev-1", 200, "{}")
        let send = PanelTransport.send(admin: { await fake.handle($0) }, devices: { [("d1", "Mac")] })
        for ok in [("GET", "/users"), ("GET", "/panels"), ("GET", "/panels/strings"), ("PATCH", "/users/7"), ("DELETE", "/bindings/dev-1")] {
            #expect(await send(CoreRequest(method: ok.0, path: ok.1)).status == 200, "\(ok)")
        }
        let devices = await send(CoreRequest(method: "GET", path: "/devices"))
        #expect(devices.status == 200 && String(decoding: devices.body, as: UTF8.self) == #"{"devices":[{"id":"d1","name":"Mac"}]}"#)
        let before = await fake.log.count
        for bad in [("POST", "/core/restore"), ("POST", "/core/backup"), ("POST", "/import"), ("POST", "/export"), ("POST", "/teams"), ("POST", "/projects"),
                    ("DELETE", "/users/7"), ("PATCH", "/users"), ("PATCH", "/users/7/x"), ("POST", "/kb/submissions/1/purge"), ("GET", "/users/7"), ("PUT", "/users"), ("POST", "/panels"), ("GET", "/panels/x"),
                    ("PATCH", "/users/.."), ("PATCH", "/users/%2e%2e"), ("PATCH", "/users/a%2Fb"), ("PATCH", "/users/"), ("GET", "/kb/search/rebuild"), ("GET", "/usage/events"),
                    ("GET", "users"), ("DELETE", "/devices/abc")] {
            #expect(await send(CoreRequest(method: bad.0, path: bad.1)).status == 404, "\(bad)")
        }
        #expect(await fake.log.count == before)  // none of them reached the core
    }

    @Test(.enabled(if: haveFixtures)) func aSecondTriggerWhileOneIsInFlightIsIgnored() async throws {
        let fake = Fake()
        let m = try await usersModel(fake)
        let u = m.panel.sections[0]
        let shop = m.visibleRows(u, path: "users")[1]
        let on = u.rowActions![2]
        await fake.set("PATCH /users/2", 200, #"{"ok":true}"#)
        await fake.slow(80_000_000)
        async let first = m.perform(on, section: "users", row: shop)
        try await Task.sleep(nanoseconds: 20_000_000)
        let second = await m.perform(on, section: "users", row: shop)  // the quick second click
        #expect(!second && m.sending.contains(PanelListKey(path: "users", parent: "")))
        #expect(await first)
        let patches = await fake.log.filter { $0.hasPrefix("PATCH") }
        #expect(patches.count == 1 && m.sending.isEmpty)
        // once it is over, the next one goes out
        #expect(await m.perform(on, section: "users", row: shop))
    }

    @Test(.enabled(if: haveFixtures)) func aPagingCursorIsAQueryValueAndAnErrorWhenItCannotBeSent() async throws {
        let f = try object("panels-format1.json")
        guard case .ready(let d) = PanelValidator.parse(data(f["panel"]!), provider: "acme", routes: f["routes"] as? [String]) else { throw Err.bad }
        let tabs = try JSONSerialization.jsonObject(with: fixture("panels-format1-strings.json")) as! [String: [String: String]]
        let routes = f["routes"] as! [String]
        let orders = d.sections[1]
        for (cursor, want) in [("x/../y?z&w=1 é", "item=s1&after=x%2F..%2Fy%3Fz%26w%3D1%20%C3%A9"), ("..", "item=s1&after=..")] {
            let fake = Fake()
            await fake.set("GET /plugins/acme/items", 200, #"{"items":[{"sku":"s1","title":"x"}]}"#)
            await fake.set("GET /plugins/acme/orders", 200, #"{"orders":[{"id":1}],"next":\#(String(decoding: try JSONEncoder().encode(cursor), as: UTF8.self))}"#)
            let m = PanelModel(panel: d, strings: PanelStrings(locale: "C", table: tabs["C"]!), routes: routes, send: { await fake.handle($0) })
            await m.load()
            await m.loadMore(orders, path: "orders")
            #expect(await fake.log.last == "GET /plugins/acme/orders?" + want)
        }
        let fake = Fake()
        await fake.set("GET /plugins/acme/items", 200, #"{"items":[{"sku":"s1","title":"x"}]}"#)
        await fake.set("GET /plugins/acme/orders", 200, #"{"orders":[{"id":1}],"next":"a\u0007b"}"#)
        let m = PanelModel(panel: d, strings: PanelStrings(), routes: routes, send: { await fake.handle($0) })
        await m.load()
        await m.loadMore(orders, path: "orders")
        #expect(m.data("orders").error == .refused("the paging cursor cannot be sent") && m.data("orders").rows.count == 1)  // visible, rows kept
    }

    @Test(.enabled(if: haveFixtures)) func aListWithoutUniqueRowKeysIsNotShown() async throws {
        let fake = Fake()
        for (k, st, b) in userAnswers { await fake.set(k, st, b) }
        let m = PanelModel(panel: try users(), strings: PanelStrings(), routes: PanelRoutes.builtin, send: { await fake.handle($0) })
        await fake.set("GET /users", 200, #"{"users":[{"id":1,"name":"a","active":true},{"id":1,"name":"b","active":true}]}"#)
        await m.load()
        guard case .cannotShow? = m.data("users").error else { throw Err.bad }
        #expect(m.data("users").rows.isEmpty)
        await fake.set("GET /users", 200, ##"{"users":[{"name":"a","active":true},{"id":"#1","name":"b"}]}"##)  // no key, and one that looks like a made-up id
        await m.load()
        guard case .cannotShow? = m.data("users").error else { throw Err.bad }
        await fake.set("GET /users", 200, ##"{"users":[{"id":"#1","name":"a","active":true},{"id":2,"name":"b","active":true}]}"##)
        await m.load()
        #expect(m.data("users").error == nil && m.data("users").rows.map(\.id) == ["#1", "2"])
    }

    @Test func routePatternsMatchSegmentForSegment() {
        let r = PanelRoutes.builtin
        #expect(PanelRoutes.allows("PUT", "/bindings/dev%201", in: r) && !PanelRoutes.allows("PUT", "/bindings", in: r) && !PanelRoutes.allows("PUT", "/bindings/a/b", in: r))
        #expect(!PanelRoutes.allows("PUT", "/bindings/%2E", in: r) && !PanelRoutes.allows("PUT", "/bindings/a%5Cb", in: r) && !PanelRoutes.allows("PUT", "/bindings/a%0Ab", in: r))
    }

    @Test(.enabled(if: haveFixtures)) func aNameLimitCountsBytesLikeTheCore() async throws {
        let m = try await usersModel(Fake())
        let add = m.panel.sections[0].actions![0]
        #expect(m.problems(add, values: ["name": .string(String(repeating: "č", count: 64))]).isEmpty)  // 128 bytes
        #expect(m.problems(add, values: ["name": .string(String(repeating: "č", count: 65))]) == ["name": .tooLong(128)])
        #expect(m.problems(add, values: ["name": .string(String(repeating: "č", count: 100))]) == ["name": .tooLong(128)])
    }
}

private func kb() throws -> PanelDescription {
    guard case .ready(let d) = PanelValidator.parse(data(try description("builtin.kb")), provider: "builtin") else { throw Err.bad }
    return d
}

private func panelKeys(_ d: PanelDescription) -> Set<String> {
    var keys: Set<String> = [d.title, d.about]
    func walk(_ s: [PanelSection]) {
        for x in s {
            keys.formUnion([x.title, x.body, x.empty, x.hide?.toggle, x.scope?.label].compactMap { $0 })
            for c in x.columns ?? [] { keys.formUnion([c.label, c.yes, c.no].compactMap { $0 } + (c.values ?? [:]).values) }
            for a in (x.actions ?? []) + (x.rowActions ?? []) {
                keys.insert(a.label)
                if let c = a.confirm { keys.formUnion([c.title, c.message, c.button]) }
                for f in a.fields ?? [] { keys.insert(f.label) }
            }
            walk(x.detail?.sections ?? [])
        }
    }
    walk(d.sections)
    return keys
}

private let kbAnswers: [(String, Int, String)] = [
    ("GET /projects", 200, #"{"projects":[{"id":1,"slug":"acme","name":"Acme"},{"id":2,"slug":"shop","name":"Shop"}]}"#),
    ("GET /teams", 200, #"{"teams":[{"id":4,"name":"web","archived":false,"path":"","segment":1099511627780}]}"#),
    ("GET /kb/submissions", 200, ##"{"submissions":[{"id":5,"title":"<script>alert(1)</script>","scopeName":"Acme","scopeKind":"project","class":"data","entryKind":"finding","authorName":"ana","received":"2026-11-17T10:00:00Z","isReplacement":false,"replaces":0},{"id":6,"title":"**b** [x](https://evil.example)","scopeName":"web","scopeKind":"team","class":"structure","entryKind":"faq","authorName":"a\u202eb","received":"2026-11-17T11:00:00Z","isReplacement":true,"replaces":77}],"next":0}"##),
    ("GET /kb/submissions/5", 200, #"{"id":5,"title":"t5","body":"text five","isReplacement":false}"#),
    ("GET /kb/submissions/6", 200, #"{"id":6,"title":"t6","body":"new text","isReplacement":true,"replaces":77}"#),
    ("GET /kb/notes/77", 200, #"{"id":77,"title":"old","class":"data","updated":"2026-11-01T10:00:00Z","body":"old text"}"#),
    ("GET /kb/notes", 200, #"{"notes":[{"id":9,"title":"n9","class":"private","entryKind":"finding","author":"ana","updated":"2026-11-01T10:00:00Z"}],"next":"c2"}"#),
    ("GET /kb/faq", 200, #"{"entries":[{"id":3,"title":"Q?","answer":"A.","class":"data","updated":"2026-11-01T10:00:00Z"}],"next":""}"#),
]

@MainActor @Suite struct PanelKBTests {
    private func model(_ fake: Fake, locale: String = "C") async throws -> PanelModel {
        for (k, st, b) in kbAnswers { await fake.set(k, st, b) }
        let m = PanelModel(panel: try kb(), strings: try strings(locale), routes: PanelRoutes.builtin, send: { await fake.handle($0) })
        await m.load()
        return m
    }

    @Test(.enabled(if: haveFixtures)) func everyKeyOfTheKnowledgeBasePanelHasATextInCAndSl() throws {
        for loc in ["C", "sl_SI"] {
            let st = try strings(loc)
            for d in [try kb(), try users()] {
                let keys = panelKeys(d)
                #expect(keys.count > 20)
                for k in keys { #expect(st.table[k] != nil, "\(loc): no text for \(k)") }
            }
        }
    }

    @Test(.enabled(if: haveFixtures)) func loadsTheQueueTheRefsAndTheFirstScopeOfEachList() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let log = await fake.log
        #expect(log == ["GET /projects", "GET /teams", "GET /kb/submissions", "GET /kb/notes?project=1", "GET /kb/notes?project=1099511627780", "GET /kb/faq?project=1", "GET /kb/faq?project=1099511627780"])
        let queue = m.panel.sections[1], notesP = m.panel.sections[2], notesT = m.panel.sections[3]
        #expect(m.data("queue").rows.map(\.id) == ["5", "6"] && m.selected[PanelListKey(path: "notes-project", parent: "")] == .int(1))
        #expect(m.selected[PanelListKey(path: "notes-team", parent: "")] == .int(1099511627780))
        // the picker changes the query parameter and reloads only that list
        await fake.clear()
        await m.select(.int(2), section: notesP, path: "notes-project")
        #expect(await fake.log == ["GET /kb/notes?project=2"])
        // paging keeps the scope and adds the cursor as a query value
        await fake.clear()
        await m.loadMore(notesP, path: "notes-project")
        #expect(await fake.log == ["GET /kb/notes?project=2&after=c2"])
        _ = (queue, notesT)
    }

    @Test(.enabled(if: haveFixtures)) func exactRequestsOfAcceptAndReject() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let queue = m.panel.sections[1]
        let row = m.data("queue").rows[0]
        let accept = queue.rowActions![0], reject = queue.rowActions![1]
        #expect(try m.request(accept, row: row).get() == CoreRequest(method: "POST", path: "/kb/submissions/5/accept", body: Data("{}".utf8)))
        #expect(m.confirmation(accept, row: row) == nil)  // accepting asks nothing
        #expect(m.defaults(reject, row: row) == ["reason": .string("")])
        #expect(try m.request(reject, row: row, values: ["reason": .string("a duplicate")]).get() == CoreRequest(method: "POST", path: "/kb/submissions/5/reject", body: Data(#"{"reason":"a duplicate"}"#.utf8)))
        #expect(try m.request(reject, row: row, values: m.defaults(reject, row: row)).get().body == Data(#"{"reason":""}"#.utf8))
        let c = m.confirmation(reject, row: row)
        #expect(c?.destructive == true && c?.title == "Reject <script>alert(1)</script>?")  // the title is plain text, once
        #expect(m.problems(reject, values: ["reason": .string(String(repeating: "č", count: 513))]) == ["reason": .tooLong(1024)])  // bytes, like the core
        // reject is refused without the confirmation, accept goes out and the whole panel reloads
        #expect(await !m.perform(reject, section: "queue", row: row, values: ["reason": .string("x")]))
        await fake.clear()
        await fake.set("POST /kb/submissions/5/accept", 200, "{}")
        #expect(await m.perform(accept, section: "queue", row: row))
        let log = await fake.log
        #expect(log.first == "POST /kb/submissions/5/accept {}" && log.contains("GET /kb/submissions"))
    }

    @Test(.enabled(if: haveFixtures)) func theDetailPageReadsTheSubmissionAndOnlyAReplacementsOldNote() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let queue = m.panel.sections[1]
        let plain = m.data("queue").rows[0], replacement = m.data("queue").rows[1]
        await fake.clear()
        await m.openDetail(queue, row: plain)
        #expect(await fake.log == ["GET /kb/submissions/5"])  // not a replacement: the old note is not requested
        #expect(m.record("queue/replaced", parent: plain).values == nil)
        let sub = queue.detail!.sections[0], old = queue.detail!.sections[1]
        #expect(!m.recordVisible(old, parent: plain) && m.recordVisible(old, parent: replacement))
        await fake.clear()
        await m.openDetail(queue, row: replacement)
        #expect(await fake.log == ["GET /kb/submissions/6", "GET /kb/notes/77"])
        let text = PanelRow(id: "", values: m.record("queue/submission", parent: replacement).values!)
        let oldText = PanelRow(id: "", values: m.record("queue/replaced", parent: replacement).values!)
        #expect(sub.columns!.last.map { m.cell($0, text) } == "new text" && old.columns!.last.map { m.cell($0, oldText) } == "old text")
    }

    @Test(.enabled(if: haveFixtures)) func aLongTextIsCutWithAMarkerAndMarkupStaysPlain() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let queue = m.panel.sections[1]
        let nasty = ["<script>alert(1)</script>", "**b** [x](https://evil.example)", "a\u{202e}b"]
        #expect(m.data("queue").rows.map { m.cell(queue.columns![0], $0) } == Array(nasty.prefix(2)))
        #expect(m.cell(queue.columns![5], m.data("queue").rows[1]) == nasty[2])  // the bidi character is kept as it is: plain text, not interpreted
        let big = String(repeating: "x", count: PanelLimits.multiline + 5)
        let body = queue.detail!.sections[0].columns!.last!
        let row = PanelRow(id: "", values: ["body": .string(big)])
        #expect(m.cell(body, row).count == PanelLimits.multiline && m.cellIsTruncated(body, row))
        #expect(!m.cellIsTruncated(body, PanelRow(id: "", values: ["body": .string("short")])))
    }

    @Test(.enabled(if: haveFixtures)) func aFailedSubmissionCallShowsTheServerTextAndAFiveHundredOnlyTheStatus() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let queue = m.panel.sections[1]
        let row = m.data("queue").rows[0]
        await fake.set("POST /kb/submissions/5/accept", 400, #"{"error":{"message":"submission already reviewed"}}"#)
        #expect(await !m.perform(queue.rowActions![0], section: "queue", row: row))
        #expect(m.actionError[PanelListKey(path: "queue", parent: "")] == .server("submission already reviewed"))
        await fake.set("POST /kb/submissions/5/accept", 500, #"{"error":{"message":"internal error: sqlite: disk I/O error"}}"#)
        _ = await m.perform(queue.rowActions![0], section: "queue", row: row)
        #expect(m.actionError[PanelListKey(path: "queue", parent: "")] == .serverFault(500))
    }

    @Test func theTransportPassesTheKnowledgeBaseRoutesAndNothingNearThem() async {
        let fake = Fake()
        for (k, st, b) in [("GET /kb/notes", 200, "{}"), ("GET /kb/notes/3", 200, "{}"), ("GET /kb/faq", 200, "{}"), ("GET /kb/submissions", 200, "{}"), ("GET /kb/submissions/3", 200, "{}"),
                           ("POST /kb/submissions/3/accept", 200, "{}"), ("POST /kb/submissions/3/reject", 200, "{}"), ("GET /projects", 200, "{}"), ("GET /teams", 200, "{}")] { await fake.set(k, st, b) }
        let send = PanelTransport.send(admin: { await fake.handle($0) }, devices: { [] })
        for ok in [("GET", "/kb/notes"), ("GET", "/kb/notes/3"), ("GET", "/kb/faq"), ("GET", "/kb/submissions"), ("GET", "/kb/submissions/3"), ("POST", "/kb/submissions/3/accept"),
                   ("POST", "/kb/submissions/3/reject"), ("GET", "/projects"), ("GET", "/teams")] {
            #expect(await send(CoreRequest(method: ok.0, path: ok.1)).status == 200, "\(ok)")
        }
        let before = await fake.log.count
        for bad in [("POST", "/kb/notes"), ("DELETE", "/kb/notes/3"), ("PUT", "/kb/notes/3"), ("POST", "/kb/search/rebuild"), ("POST", "/projects"), ("PATCH", "/projects/3"), ("POST", "/teams"),
                    ("PUT", "/teams/1/members/2"), ("GET", "/kb/submissions/3/accept"), ("POST", "/kb/submissions/3/other"), ("GET", "/kb/notes/3/x"), ("GET", "/projects/3/members"),
                    ("GET", "/kb/notes/%2e%2e"), ("GET", "/grants"), ("GET", "/audit")] {
            #expect(await send(CoreRequest(method: bad.0, path: bad.1)).status == 404, "\(bad)")
        }
        #expect(await fake.log.count == before)
    }
}

// MARK: the renderer's source (the app target cannot be unit-tested, so its rules are checked in the text)

/// The code of a source text: comments cut (a `//` inside a string literal stays), then every rule below applied to EVERY
/// occurrence. Returns what breaks a rule.
func rendererViolations(_ source: String) -> [String] {
    func strip(_ line: Substring) -> String {
        var out = "", inString = false, prev: Character = " "
        var it = line.makeIterator(), pending: Character?
        while let c = pending ?? it.next() {
            pending = nil
            if c == "\"" && prev != "\\" { inString.toggle() }
            if !inString && c == "/" {
                if let n = it.next() { if n == "/" { break }; pending = n }
            }
            out.append(c); prev = c
        }
        return out
    }
    let code = source.split(separator: "\n", omittingEmptySubsequences: false).map(strip).joined(separator: "\n")
    var bad: [String] = []
    for banned in ["LocalizedStringKey", "AttributedString", "markdown", "Markdown", "Link(", "openURL", "NSWorkspace", "WebView", "LocalizedStringResource", "URL("] where code.contains(banned) {
        bad.append("uses " + banned)
    }
    func args(_ pattern: String) -> [String] {
        let re = try! NSRegularExpression(pattern: pattern + #"(?=(.{0,100}))"#)
        return re.matches(in: code, range: NSRange(code.startIndex..., in: code)).map { String(code[Range($0.range(at: 1), in: code)!]) }
    }
    func literal(_ rest: String) -> Bool { rest.range(of: #"^"[^"\\]*""#, options: .regularExpression) != nil }
    for rest in args(#"\bText(?:\.init)?\("#) where !(rest.hasPrefix("verbatim:") || rest.hasPrefix("String(localized: \"") || (literal(rest) && rest.range(of: #"^"[^"\\]*"\)"#, options: .regularExpression) != nil)) {
        bad.append("Text(" + rest)
    }
    let titled = #"(?:\bButton|\bToggle|\bTextField|\bPicker|\bLabeledContent|\bMenu|\bDisclosureGroup|\bSection|\bLabel|\bAlert|\.navigationTitle|\.navigationSubtitle|\.help|\.alert|\.confirmationDialog|\.accessibilityLabel|\.accessibilityHint|\.accessibilityValue|\.badge)(?:\.init)?\("#
    for rest in args(titled) {
        let labels = ["role:", "action:", "isOn:", "selection:", "text:", "Text(verbatim:", "String(localized: \""]
        if !(labels.contains { rest.hasPrefix($0) } || literal(rest)) { bad.append("title " + rest) }
    }
    return bad
}

/// The panel code: every Panel*.swift of the app, and the lines of Settings.swift that mention a panel.
private let rendererSources: [(name: String, text: String)] = {
    let dir = repo.appendingPathComponent("Sources/MiniGateway")
    let read = { (n: String) in (try? String(contentsOf: dir.appendingPathComponent(n), encoding: .utf8)) ?? "" }
    let names = ((try? FileManager.default.contentsOfDirectory(atPath: dir.path)) ?? []).filter { $0.hasPrefix("Panel") && $0.hasSuffix(".swift") }.sorted()
    var out = names.map { ($0, read($0)) }
    out.append(("Settings.swift (panel lines)", read("Settings.swift").split(separator: "\n").filter { $0.range(of: #"panelEntries|panelError|panelStrings|panelItems|panelSend|PanelPane|PanelsUnavailablePane|PanelCatalog|PanelEntry|PanelStrings|\.panel\("#, options: .regularExpression) != nil }.joined(separator: "\n")))
    return out
}()

private func matches(_ pattern: String, in text: String) -> [NSTextCheckingResult] {
    try! NSRegularExpression(pattern: pattern).matches(in: text, range: NSRange(text.startIndex..., in: text))
}

@Suite struct PanelRendererSourceTests {
    @Test func theRendererFilesExist() {
        #expect(rendererSources.map(\.name) == ["PanelView.swift", "Settings.swift (panel lines)"])
        #expect(rendererSources[0].text.count > 1000 && rendererSources[1].text.count > 300)
    }

    @Test func theScannerCatchesEveryBadPattern() {
        let bad = [
            "Text(verbatim: a) + Text(b)", "Text.init(b)", "Text(LocalizedStringKey(b))", "Text(\"a \\(b)\")", "Text(b)", "VStack { Text(verbatim: a); Text(b) }",
            "Button(title) { }", "Label(title, systemImage: \"x\")", "Label(\"a \\(b)\", systemImage: \"x\")", "x.help(title)", "x.navigationTitle(title)", "x.navigationSubtitle(t)",
            ".alert(title, isPresented: $p) {", "x.confirmationDialog(title, isPresented: $p)", "Alert(title: t)", "x.accessibilityLabel(t)", "Toggle(t, isOn: $b)", "TextField(t, text: $s)",
            "Section(t) {", "Picker(t, selection: $s) {", "Text(AttributedString(a))", "Link(\"a\", destination: u)", "NSWorkspace.shared.open(u)", "Text(try! AttributedString(markdown: a))",
            "let u = \"https://x\"; Text(b)", "let a = 1 // ok\nText(b)", "x.openURL(u)", "let u = URL(string: a)", "Menu(t) { }", "LabeledContent(t) { }",
        ]
        for line in bad { #expect(!rendererViolations(line).isEmpty, "not caught: \(line)") }
        let good = [
            "Text(verbatim: a) + Text(verbatim: b)", "Text(\"Literal\")", "Text(String(localized: \"At most \\(n) characters\"))", "Button(role: .destructive) { }", "Button(\"Load More\") { }",
            "// Text(b) and LocalizedStringKey in a comment", "let a = 1 // Text(b)", "x.help(String(localized: \"Open\"))", "x.confirmationDialog(Text(verbatim: t), isPresented: $p)",
            "Toggle(isOn: $b) { Text(verbatim: t) }", "TextField(text: $s) { Text(verbatim: t) }", "Label { Text(\"x\") } icon: { Image(systemName: \"a\") }", "let s = \"a // not a comment\"",
        ]
        for line in good { #expect(rendererViolations(line).isEmpty, "wrongly flagged: \(line) \(rendererViolations(line))") }
    }

    @Test func theRendererFollowsTheRules() {
        for (name, text) in rendererSources { #expect(rendererViolations(text).isEmpty, "\(name): \(rendererViolations(text))") }
    }

    @Test func everyFixedStringHasASlovenianEntry() throws {
        let cat = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("Resources/Localizable.xcstrings"))) as! [String: Any]
        let strings = cat["strings"] as! [String: Any]
        var keys = Set<String>()
        for (_, text) in rendererSources {
            for m in matches(#"(?:\bText|\bButton|\bTextField|String\(localized:)\(?\s*"((?:[^"\\]|\\.)*)""#, in: text) {
                keys.insert(String(text[Range(m.range(at: 1), in: text)!]).replacingOccurrences(of: #"\\\([^)]*\)"#, with: "%lld", options: .regularExpression))
            }
        }
        #expect(keys.count >= 14, "found \(keys.count) strings")
        for k in keys.sorted() {
            let sl = (((strings[k] as? [String: Any])?["localizations"] as? [String: Any])?["sl"] as? [String: Any])?["stringUnit"] as? [String: Any]
            #expect((sl?["value"] as? String)?.isEmpty == false && sl?["state"] as? String == "translated", "no Slovenian entry for \"\(k)\"")
        }
    }
}
