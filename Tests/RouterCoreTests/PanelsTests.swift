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
    var delays: [String: UInt64] = [:]
    func slow(_ ns: UInt64) { delay = ns }
    func slow(_ key: String, _ ns: UInt64) { delays[key] = ns }  // key: "METHOD path?query"
    func set(_ k: String, _ status: Int, _ body: String) { answers[k] = (status, body) }
    func handle(_ r: CoreRequest) async -> CoreResponse {
        log.append(r.method + " " + r.path + (r.query.isEmpty ? "" : "?" + r.query) + (r.body.isEmpty ? "" : " " + String(decoding: r.body, as: UTF8.self)))
        let full = r.method + " " + r.path + (r.query.isEmpty ? "" : "?" + r.query)
        if let d = delays[full] ?? (delay > 0 ? delay : nil) { try? await Task.sleep(nanoseconds: d) }
        let (s, b) = answers[full] ?? answers[r.method + " " + r.path] ?? (404, #"{"error":{"message":"nothing here"}}"#)
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
        #expect(reg["builtinSecret"] as? [String] == PanelRoutes.builtinSecret)
        #expect(reg["builtinConfirm"] as? [String] == PanelRoutes.builtinConfirm)
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
        guard case .ready(let d) = PanelValidator.parse(data(f["panel"]!), provider: f["provider"] as! String, routes: f["routes"] as? [String], secretRoutes: f["secretRoutes"] as? [String]) else { throw Err.bad }
        let orders = d.sections[1]
        #expect(d.sections[0].kind == "text" && orders.kind == "list")
        #expect(orders.columns!.map(\.kind) == ["text", "multiline", "bool", "enum", "ref", "time"])
        #expect(orders.paging != nil && orders.hide != nil && orders.filter != nil && orders.detail?.sections.count == 3)
        #expect(orders.scope?.param == "item" && orders.detail?.sections[2].kind == "record" && orders.detail?.sections[2].when?.field == "paid")
        #expect(orders.actions![0].fields!.map(\.type) == ["text", "ref", "choice", "toggle"])
        // a provider that registered fewer routes cannot show it
        let fewer = Array((f["routes"] as! [String]).dropFirst())
        if case .ready = PanelValidator.parse(data(f["panel"]!), provider: "acme", routes: fewer, secretRoutes: f["secretRoutes"] as? [String]) { Issue.record("accepted with a missing route") }
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
            let parsed = PanelValidator.parse(data(doc), provider: "acme", routes: f["routes"] as? [String], secretRoutes: f["secretRoutes"] as? [String], confirmRoutes: c["confirmRoutes"] as? [String] ?? [])
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
        guard case .ready(let d) = PanelValidator.parse(data(f["panel"]!), provider: "acme", routes: f["routes"] as? [String], secretRoutes: f["secretRoutes"] as? [String]) else { throw Err.bad }
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
        #expect(c.error == nil && c.entries.map(\.id) == ["builtin.users", "builtin.teams", "builtin.kb", "builtin.upstreams"])  // the order of the answer
        let usersEntry = c.entries[0]
        #expect(usersEntry.title(c.strings) == "Users and devices" && usersEntry.sidebarSymbol == "person.2" && c.strings.locale == "C")
        #expect(c.entries[2].title(c.strings) == "Knowledge base" && c.entries[2].sidebarSymbol == "book" && c.entries[1].title(c.strings) == "Teams and projects")
        for e in c.entries { guard case .ready = e.parse else { throw Err.bad } }
        let sl = await catalog(200, String(decoding: try fixture("panels.json"), as: UTF8.self), locale: "sl_SI")
        #expect(sl.entries[0].title(sl.strings) == "Uporabniki in naprave" && sl.entries[2].title(sl.strings) == "Baza znanja" && sl.entries[1].title(sl.strings) == "Ekipe in projekti" && sl.strings.locale == "sl_SI" && sl.stringsError == nil)
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
        rows[1]["path"] = "/core/{row.id}"  // another provider's route
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
        for bad in [("POST", "/core/restore"), ("POST", "/core/backup"), ("POST", "/import"), ("POST", "/export"), ("DELETE", "/teams/1"), ("PUT", "/projects/1"),
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
        guard case .ready(let d) = PanelValidator.parse(data(f["panel"]!), provider: "acme", routes: f["routes"] as? [String], secretRoutes: f["secretRoutes"] as? [String]) else { throw Err.bad }
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
                for f in a.fields ?? [] { keys.insert(f.label); keys.formUnion((f.options ?? []).map(\.label)) }
                if let r = a.result { if let t = r.title { keys.insert(t) }; for c in r.columns { keys.formUnion([c.label, c.yes, c.no].compactMap { $0 } + (c.values ?? [:]).values) } }
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
    ("GET /kb/submissions", 200, ##"{"submissions":[{"id":5,"title":"<script>alert(1)</script>","scopeName":"Acme","scopeKind":"project","class":"data","entryKind":"finding","authorName":"ana","received":"2026-11-17T10:00:00Z","isReplacement":false,"replaces":0,"reviewHash":"h5"},{"id":6,"title":"**b** [x](https://evil.example)","scopeName":"web","scopeKind":"team","class":"structure","entryKind":"faq","authorName":"a\u202eb","received":"2026-11-17T11:00:00Z","isReplacement":true,"replaces":77,"reviewHash":"h6"}],"next":0}"##),
    ("GET /kb/submissions/5/versions/h5", 200, #"{"id":5,"title":"t5","body":"text five","isReplacement":false}"#),
    ("GET /kb/submissions/6/versions/h6", 200, #"{"id":6,"title":"t6","body":"new text","isReplacement":true,"replaces":77}"#),
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

    @Test(.enabled(if: haveFixtures)) func anAnswerForAnEarlierChoiceOfThePickerIsDropped() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let notes = m.panel.sections[2]
        await fake.set("GET /kb/notes?project=1", 200, #"{"notes":[{"id":1,"title":"of one"}]}"#)
        await fake.set("GET /kb/notes?project=2", 200, #"{"notes":[{"id":2,"title":"of two"}]}"#)
        await fake.slow("GET /kb/notes?project=1", 150_000_000)   // the answer for the first choice arrives last
        await fake.slow("GET /kb/notes?project=2", 10_000_000)
        async let first: Void = m.select(.int(1), section: notes, path: "notes-project")
        try await Task.sleep(nanoseconds: 20_000_000)
        async let second: Void = m.select(.int(2), section: notes, path: "notes-project")
        _ = await (first, second)
        #expect(m.data("notes-project").rows.map(\.id) == ["2"] && m.selected[PanelListKey(path: "notes-project", parent: "")] == .int(2))
        #expect(!m.data("notes-project").loading)
    }

    @Test(.enabled(if: haveFixtures)) func twoQuickLoadMoreClicksSendTheCursorOnce() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let notes = m.panel.sections[2]
        await fake.set("GET /kb/notes?project=1&after=c2", 200, #"{"notes":[{"id":10,"title":"more"}]}"#)
        await fake.slow("GET /kb/notes?project=1&after=c2", 60_000_000)
        await fake.clear()
        async let a: Void = m.loadMore(notes, path: "notes-project")
        try await Task.sleep(nanoseconds: 15_000_000)
        async let b: Void = m.loadMore(notes, path: "notes-project")
        _ = await (a, b)
        #expect(await fake.log == ["GET /kb/notes?project=1&after=c2"])
        #expect(m.data("notes-project").rows.map(\.id) == ["9", "10"] && m.data("notes-project").error == nil)
    }

    @Test(.enabled(if: haveFixtures)) func exactRequestsOfAcceptAndReject() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let queue = m.panel.sections[1]
        let row = m.data("queue").rows[0]
        let accept = queue.rowActions![0], reject = queue.rowActions![1]
        #expect(try m.request(accept, row: row).get() == CoreRequest(method: "POST", path: "/kb/submissions/5/accept", body: Data(#"{"reviewHash":"h5"}"#.utf8)))  // the version on screen
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
        #expect(log.first == #"POST /kb/submissions/5/accept {"reviewHash":"h5"}"# && log.contains("GET /kb/submissions"))
    }

    @Test(.enabled(if: haveFixtures)) func theDetailPageReadsTheSubmissionAndOnlyAReplacementsOldNote() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let queue = m.panel.sections[1]
        let plain = m.data("queue").rows[0], replacement = m.data("queue").rows[1]
        await fake.clear()
        await m.openDetail(queue, row: plain)
        #expect(await fake.log == ["GET /kb/submissions/5/versions/h5"])  // the version of the row: what its Accept publishes  // not a replacement: the old note is not requested
        #expect(m.record("queue/replaced", parent: plain).values == nil)
        let sub = queue.detail!.sections[0], old = queue.detail!.sections[1]
        #expect(!m.recordVisible(old, parent: plain) && m.recordVisible(old, parent: replacement))
        await fake.clear()
        await m.openDetail(queue, row: replacement)
        #expect(await fake.log == ["GET /kb/submissions/6/versions/h6", "GET /kb/notes/77"])
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
        for (k, st, b) in [("GET /kb/notes", 200, "{}"), ("GET /kb/notes/3", 200, "{}"), ("GET /kb/faq", 200, "{}"), ("GET /kb/submissions", 200, "{}"), ("GET /kb/submissions/3", 200, "{}"), ("GET /kb/submissions/3/versions/ab", 200, "{}"),
                           ("POST /kb/submissions/3/accept", 200, "{}"), ("POST /kb/submissions/3/reject", 200, "{}"), ("GET /projects", 200, "{}"), ("GET /teams", 200, "{}")] { await fake.set(k, st, b) }
        let send = PanelTransport.send(admin: { await fake.handle($0) }, devices: { [] })
        for ok in [("GET", "/kb/notes"), ("GET", "/kb/notes/3"), ("GET", "/kb/faq"), ("GET", "/kb/submissions"), ("GET", "/kb/submissions/3"), ("GET", "/kb/submissions/3/versions/ab"), ("POST", "/kb/submissions/3/accept"),
                   ("POST", "/kb/submissions/3/reject"), ("GET", "/projects"), ("GET", "/teams")] {
            #expect(await send(CoreRequest(method: ok.0, path: ok.1)).status == 200, "\(ok)")
        }
        let before = await fake.log.count
        for bad in [("POST", "/kb/notes"), ("DELETE", "/kb/notes/3"), ("PUT", "/kb/notes/3"), ("POST", "/kb/search/rebuild"), ("PUT", "/projects/3"), ("DELETE", "/projects/3"), ("PUT", "/teams/1"),
                    ("POST", "/teams/1/members/2"), ("GET", "/kb/submissions/3/accept"), ("POST", "/kb/submissions/3/other"), ("GET", "/kb/notes/3/x"), ("GET", "/projects/3/grants"),
                    ("GET", "/kb/notes/%2e%2e"), ("PUT", "/grants"), ("GET", "/audit")] {
            #expect(await send(CoreRequest(method: bad.0, path: bad.1)).status == 404, "\(bad)")
        }
        #expect(await fake.log.count == before)
    }
}

private func teamsPanel() throws -> PanelDescription {
    guard case .ready(let d) = PanelValidator.parse(data(try description("builtin.teams")), provider: "builtin") else { throw Err.bad }
    return d
}

private let teamsAnswers: [(String, Int, String)] = [
    ("GET /users", 200, #"{"users":[{"id":2,"name":"ana","active":true,"owner":false},{"id":3,"name":"bor","active":true,"owner":false}]}"#),
    ("GET /projects", 200, ##"{"projects":[{"id":1,"slug":"acme-web","name":"Acme Web","archived":false,"teamName":"<b>web</b>","memberCount":2},{"id":8,"slug":"old","name":"Old","archived":true,"teamName":"","memberCount":0}]}"##),
    ("GET /teams", 200, ##"{"teams":[{"id":4,"name":"web [x](https://evil.example)","archived":false,"path":"acme/web","segment":1099511627780,"memberCount":2,"projectCount":1},{"id":5,"name":"old","archived":true,"path":"","segment":1099511627781,"memberCount":0,"projectCount":0}]}"##),
    ("GET /grants", 200, #"{"grants":[{"source":1,"reader":8,"class":"data","id":"1-8-data","sourceName":"Acme Web","readerName":"Old"}]}"#),
    ("GET /teams/4/members", 200, #"{"members":[{"user":2,"name":"ana"}]}"#),
    ("GET /teams/4/projects", 200, #"{"projects":[{"id":1,"slug":"acme-web","name":"Acme Web","archived":false}]}"#),
    ("GET /teams/4/assignments", 200, #"{"assignments":[{"project":1,"user":2,"role":"developer","id":"1-2","userName":"ana","projectName":"Acme Web"}]}"#),
    ("GET /projects/1/members", 200, #"{"members":[{"user":2,"name":"ana","role":"admin","directRole":"developer","fromTeams":"web (admin)"},{"user":3,"name":"bor","role":"reader","directRole":"none","fromTeams":"web (reader)"},{"user":9,"name":"eva","role":"admin","directRole":"admin","fromTeams":""}]}"#),
    ("GET /projects/1/remotes", 200, #"{"remotes":["host.example/a/b"],"entries":[{"remote":"host.example/a/b"}]}"#),
    ("GET /projects/1/depends", 200, #"{"depends":[{"id":8,"slug":"old","name":"Old"}],"usedBy":[]}"#),
]

@MainActor @Suite struct PanelTeamsTests {
    private func model(_ fake: Fake, locale: String = "C") async throws -> PanelModel {
        for (k, st, b) in teamsAnswers { await fake.set(k, st, b) }
        let m = PanelModel(panel: try teamsPanel(), strings: try strings(locale), routes: PanelRoutes.builtin, send: { await fake.handle($0) })
        await m.load()
        return m
    }

    @Test(.enabled(if: haveFixtures)) func everyKeyHasATextAndTheSidebarOrderIsGiven() throws {
        for loc in ["C", "sl_SI"] {
            let st = try strings(loc)
            let keys = panelKeys(try teamsPanel())
            #expect(keys.count > 60)
            for k in keys { #expect(st.table[k] != nil, "\(loc): no text for \(k)") }
        }
        #expect(try teamsPanel().sections.map(\.id) == ["teams", "projects", "grants"])
    }

    @Test(.enabled(if: haveFixtures)) func loadsRefsThenTheThreeLists() async throws {
        let fake = Fake()
        let m = try await model(fake)
        #expect(await fake.log == ["GET /projects", "GET /teams", "GET /users", "GET /teams", "GET /projects", "GET /grants"])
        // archived rows are hidden until the toggle is on; a hidden team segment is never a row (the project list is the admin list)
        #expect(m.visibleRows(m.panel.sections[0], path: "teams").map(\.id) == ["4"])
        m.setShowHidden(true, path: "teams")
        #expect(m.visibleRows(m.panel.sections[0], path: "teams").map(\.id) == ["4", "5"])
        #expect(m.visibleRows(m.panel.sections[1], path: "projects").map(\.id) == ["1"])
        #expect(m.refs["projects"]?.map(\.id) == ["1", "8"])
        #expect(m.panel.sections.allSatisfy { ($0.rowActions ?? []).count <= PanelLimits.actions })
    }

    @Test(.enabled(if: haveFixtures)) func exactRequestsOfTheTeamActions() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let teams = m.panel.sections[0]
        let web = m.visibleRows(teams, path: "teams")[0]
        let add = teams.actions![0]
        let byID = Dictionary(uniqueKeysWithValues: teams.rowActions!.map { ($0.id, $0) })
        #expect(try m.request(add, values: ["name": .string("acme")]).get() == CoreRequest(method: "POST", path: "/teams", body: Data(#"{"name":"acme"}"#.utf8)))
        #expect(try m.request(byID["rename"]!, row: web, values: ["name": .string("platform")]).get() == CoreRequest(method: "PATCH", path: "/teams/4", body: Data(#"{"name":"platform"}"#.utf8)))
        #expect(m.defaults(byID["path"]!, row: web) == ["path": .string("acme/web")])
        #expect(try m.request(byID["path"]!, row: web, values: ["path": .string("")]).get().body == Data(#"{"path":""}"#.utf8))  // empty clears the label
        #expect(try m.request(byID["archive"]!, row: web).get() == CoreRequest(method: "PATCH", path: "/teams/4", body: Data(#"{"archived":true}"#.utf8)))
        #expect(try m.request(byID["unarchive"]!, row: web).get().body == Data(#"{"archived":false}"#.utf8))
        let c = m.confirmation(byID["archive"]!, row: web)
        #expect(c?.destructive == true && c?.title == "Archive web [x](https://evil.example)?" && c?.message.contains("do not come back") == true && c?.message.contains("not stopped") == true)
        #expect(m.confirmation(byID["rename"]!, row: web) == nil && m.confirmation(byID["unarchive"]!, row: web) == nil)
        #expect(m.rowActions(teams, web).map(\.id) == ["rename", "path", "archive"])  // archive only for an active team
    }

    @Test(.enabled(if: haveFixtures)) func theTeamDetailPageAndItsRequests() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let teams = m.panel.sections[0]
        let web = m.visibleRows(teams, path: "teams")[0]
        await fake.clear()
        await m.openDetail(teams, row: web)
        #expect(await fake.log == ["GET /teams/4/members", "GET /teams/4/projects", "GET /teams/4/assignments"])
        let members = teams.detail!.sections[0], projects = teams.detail!.sections[1], assigns = teams.detail!.sections[2]
        let person = m.data("teams/members", parent: web).rows[0]
        #expect(try m.request(members.actions![0], parent: web, values: ["user": .int(3)]).get() == CoreRequest(method: "PUT", path: "/teams/4/members/3"))
        #expect(try m.request(members.rowActions![0], row: person, parent: web).get() == CoreRequest(method: "DELETE", path: "/teams/4/members/2"))
        #expect(m.confirmation(members.rowActions![0], row: person)?.message.contains("Direct roles stay") == true)
        let project = m.data("teams/projects", parent: web).rows[0]
        #expect(try m.request(projects.actions![0], parent: web, values: ["project": .int(8)]).get() == CoreRequest(method: "PUT", path: "/teams/4/projects/8"))
        #expect(try m.request(projects.rowActions![0], row: project, parent: web).get() == CoreRequest(method: "DELETE", path: "/teams/4/projects/1"))
        let a = m.data("teams/assignments", parent: web).rows[0]
        #expect(try m.request(assigns.actions![0], parent: web, values: ["user": .int(3), "project": .int(1), "role": .string("reader")]).get()
                == CoreRequest(method: "PUT", path: "/teams/4/assignments", body: Data(#"{"project":1,"role":"reader","user":3}"#.utf8)))
        #expect(try m.request(assigns.rowActions![0], row: a, parent: web, values: ["role": .string("admin")]).get()
                == CoreRequest(method: "PUT", path: "/teams/4/assignments", body: Data(#"{"project":1,"role":"admin","user":2}"#.utf8)))
        #expect(try m.request(assigns.rowActions![1], row: a, parent: web).get() == CoreRequest(method: "DELETE", path: "/teams/4/assignments", query: "project=1&user=2"))
        #expect(m.confirmation(assigns.actions![0])?.message.contains("Manage (admin) lets a person read every person's private notes") == true)
        #expect(m.confirmation(assigns.rowActions![0], row: a)?.title == "Change the role of ana?" && m.confirmation(assigns.rowActions![1], row: a)?.destructive == true)
    }

    @Test(.enabled(if: haveFixtures)) func exactRequestsOfTheProjectActionsAndTheRoleOrigins() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let projects = m.panel.sections[1]
        let p = m.visibleRows(projects, path: "projects")[0]
        #expect(try m.request(projects.actions![0], values: ["slug": .string("acme-web"), "name": .string("Acme Web")]).get()
                == CoreRequest(method: "POST", path: "/projects", body: Data(#"{"name":"Acme Web","slug":"acme-web"}"#.utf8)))
        #expect(m.problems(projects.actions![0], values: ["slug": .string(String(repeating: "a", count: 64)), "name": .string("x")]) == ["slug": .tooLong(63)])
        let archive = projects.rowActions!.first { $0.id == "archive" }!
        #expect(try m.request(archive, row: p).get() == CoreRequest(method: "PATCH", path: "/projects/1", body: Data(#"{"archived":true}"#.utf8)))
        #expect(m.confirmation(archive, row: p)?.message.contains("Nobody can write") == true && m.confirmation(archive, row: p)?.message.contains("access does not change") == true)
        await fake.clear()
        await m.openDetail(projects, row: p)
        #expect(await fake.log == ["GET /projects/1/members", "GET /projects/1/remotes", "GET /projects/1/depends", "GET /projects/1/depends"])
        let members = projects.detail!.sections[0]
        let rows = m.data("projects/members", parent: p).rows
        let (ana, bor, eva) = (rows[0], rows[1], rows[2])
        // where a role comes from is in the columns: effective role, direct role, teams
        #expect(members.columns!.map { m.cell($0, ana) } == ["ana", "Manage (admin)", "Develop (developer)", "web (admin)"])
        #expect(members.columns!.map { m.cell($0, bor) } == ["bor", "Read (reader)", "None", "web (reader)"])
        // the role actions depend on the direct role; only a LOWERING asks first
        #expect(m.rowActions(members, ana).map(\.id) == ["to-reader-from-developer", "to-admin-from-developer", "remove-developer"])
        #expect(m.rowActions(members, bor).map(\.id) == ["to-reader-from-none", "to-developer-from-none", "to-admin-from-none"])  // no direct role: nothing to remove
        #expect(m.rowActions(members, eva).map(\.id) == ["to-reader-from-admin", "to-developer-from-admin", "remove-admin"])
        let lower = m.rowActions(members, ana)[0], raise = m.rowActions(members, ana)[1], remove = m.rowActions(members, ana)[2]
        #expect(m.confirmation(lower, row: ana)?.title == "Lower the role of ana?")
        // raising to admin is security relevant: it asks first, says why, and is not a destructive (red) confirmation
        let toAdmin = m.confirmation(raise, row: ana)
        #expect(toAdmin?.destructive == false && toAdmin?.message.contains("private notes") == true && toAdmin?.message.contains("manage the notes of the team") == true)
        #expect(m.rowActions(members, bor).allSatisfy { ($0.id == "to-admin-from-none") == (m.confirmation($0, row: bor) != nil) })  // only the admin one asks
        #expect(m.confirmation(lower, row: ana)?.message.contains("not stopped") == true && m.confirmation(remove, row: ana)?.message.contains("If no team gives this person a role either") == true)
        #expect(try m.request(raise, row: ana, parent: p).get() == CoreRequest(method: "PUT", path: "/projects/1/members/2", body: Data(#"{"role":"admin"}"#.utf8)))
        #expect(try m.request(lower, row: ana, parent: p).get().body == Data(#"{"role":"reader"}"#.utf8))
        #expect(try m.request(remove, row: ana, parent: p).get() == CoreRequest(method: "DELETE", path: "/projects/1/members/2"))
        // giving a role (any, so also admin) and giving one through a team ask first
        #expect(m.confirmation(members.actions![0])?.message.contains("Manage (admin) lets a person read every person's private notes") == true)
        #expect(try m.request(members.actions![0], parent: p, values: ["user": .int(3), "role": .string("developer")]).get()
                == CoreRequest(method: "PUT", path: "/projects/1/members/3", body: Data(#"{"role":"developer"}"#.utf8)))
        // git identities and dependencies
        let remotes = projects.detail!.sections[1], deps = projects.detail!.sections[2]
        let remote = m.data("projects/remotes", parent: p).rows[0]
        #expect(try m.request(remotes.actions![0], parent: p, values: ["remote": .string("git@host.example:a/b.git")]).get()
                == CoreRequest(method: "POST", path: "/projects/1/remotes", body: Data(#"{"addRemote":"git@host.example:a/b.git"}"#.utf8)))
        #expect(try m.request(remotes.rowActions![0], row: remote, parent: p).get().body == Data(#"{"removeRemote":"host.example/a/b"}"#.utf8))
        #expect(m.confirmation(remotes.rowActions![0], row: remote)?.destructive == true)
        let dep = m.data("projects/depends", parent: p).rows[0]
        #expect(try m.request(deps.actions![0], parent: p, values: ["other": .int(8)]).get() == CoreRequest(method: "PUT", path: "/projects/1/depends/8"))
        #expect(try m.request(deps.rowActions![0], row: dep, parent: p).get() == CoreRequest(method: "DELETE", path: "/projects/1/depends/8"))
        #expect(m.confirmation(deps.rowActions![0], row: dep)?.message.contains("only a search hint") == true)
    }

    @Test(.enabled(if: haveFixtures)) func grantsListAndRemoveWithTheQuery() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let grants = m.panel.sections[2]
        let g = m.data("grants").rows[0]
        #expect(g.id == "1-8-data" && grants.columns!.map { m.cell($0, g) } == ["Acme Web", "Old", "Data"])
        #expect(try m.request(grants.actions![0], values: ["source": .int(1), "reader": .int(8), "class": .string("data")]).get()
                == CoreRequest(method: "POST", path: "/grants", body: Data(#"{"class":"data","reader":8,"source":1}"#.utf8)))
        #expect(try m.request(grants.rowActions![0], row: g).get() == CoreRequest(method: "DELETE", path: "/grants", query: "source=1&reader=8&class=data"))
        let c = m.confirmation(grants.rowActions![0], row: g)
        #expect(c?.message.hasPrefix("Old can no longer read the notes of Acme Web") == true && c?.message.contains("not stopped") == true)
        // a failed call shows the server's message and leaves the rows
        let before = m.data("grants").rows
        await fake.set("POST /grants", 400, #"{"error":{"message":"grant to the same project"}}"#)
        #expect(await !m.perform(grants.actions![0], section: "grants", values: ["source": .int(1), "reader": .int(1), "class": .string("data")]))
        #expect(m.actionError[PanelListKey(path: "grants", parent: "")] == .server("grant to the same project") && m.data("grants").rows == before)
    }

    @Test(.enabled(if: haveFixtures)) func namesWithMarkupAreShownAsTypedAndSlovenianHasNoCaseSlip() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let teams = m.panel.sections[0], projects = m.panel.sections[1]
        #expect(m.cell(teams.columns![0], m.visibleRows(teams, path: "teams")[0]) == "web [x](https://evil.example)")
        #expect(m.cell(projects.columns![2], m.visibleRows(projects, path: "projects")[0]) == "<b>web</b>")
        let sl = try await model(Fake(), locale: "sl_SI")
        let web = sl.visibleRows(teams, path: "teams")[0]
        #expect(sl.confirmation(teams.rowActions!.first { $0.id == "archive" }!, row: web)?.title == "Arhiviram ekipo web [x](https://evil.example)?")
        #expect(sl.panel.sections[0].columns!.map { sl.text($0.label) } == ["Ime", "Oznaka poti", "Osebe", "Projekti", "Stanje"])
    }
}

/// The shared table of exact requests (client/testdata/panels-requests.json): the web renderer's script test reads the same file.
@MainActor @Suite struct PanelRequestFixtureTests {
    @Test(.enabled(if: haveFixtures)) func aRowValueInAConfirmationIsShortPlainText() throws {
        let cases = try object("panels-requests.json")["confirmations"] as! [[String: Any]]
        #expect(cases.count >= 7)
        for c in cases {
            guard case .ready(let d) = PanelValidator.parse(data(try description(c["panel"] as! String)), provider: "builtin") else { throw Err.bad }
            let m = PanelModel(panel: d, strings: try strings("C"), routes: PanelRoutes.builtin, send: { _ in .unavailable })
            let a = d.sections.first { $0.id == c["section"] as! String }!.rowActions!.first { $0.id == c["action"] as! String }!
            let values = try JSONDecoder().decode([String: PanelJSON].self, from: data(c["row"] as! [String: Any]))
            #expect(m.confirmation(a, row: PanelRow(id: "", values: values))?.title == c["title"] as? String, "\(c["row"]!)")
        }
    }

    @Test(.enabled(if: haveFixtures)) func everyCaseOfTheSharedTableHasTheSameRequestAndConfirmation() throws {
        let cases = try object("panels-requests.json")["cases"] as! [[String: Any]]
        #expect(cases.count >= 60)
        for c in cases {
            let id = c["panel"] as! String
            guard case .ready(let d) = PanelValidator.parse(data(try description(id)), provider: "builtin") else { throw Err.bad }
            let m = PanelModel(panel: d, strings: PanelStrings(), routes: PanelRoutes.builtin, send: { _ in .unavailable })
            let parts = (c["section"] as! String).split(separator: "/").map(String.init)
            var sec = d.sections.first { $0.id == parts[0] }!
            if parts.count > 1 { sec = sec.detail!.sections.first { $0.id == parts[1] }! }
            let list = (c["kind"] as! String) == "actions" ? sec.actions : sec.rowActions
            let a = list!.first { $0.id == c["action"] as! String }!
            func row(_ k: String) throws -> PanelRow? {
                guard let o = c[k] as? [String: Any] else { return nil }
                return PanelRow(id: "", values: try JSONDecoder().decode([String: PanelJSON].self, from: data(o)))
            }
            let values = try (c["values"] as? [String: Any]).map { try JSONDecoder().decode([String: PanelJSON].self, from: data($0)) } ?? [:]
            let result = m.request(a, row: try row("row"), parent: try row("parent"), values: values)
            if (c["want"] as? String) == "refused" {
                if case .success(let r) = result { Issue.record("sent \(r) for \(c)") }
            } else {
                let w = c["want"] as! [String: String]
                guard case .success(let r) = result else { Issue.record("refused: \(c)"); continue }
                #expect(r.method == w["method"] && r.path == w["path"] && r.query == w["query"] && String(decoding: r.body, as: UTF8.self) == w["body"], "\(c)")
            }
            let conf = m.confirmation(a, row: try row("row"))
            #expect((conf == nil ? "none" : conf!.destructive ? "destructive" : "plain") == c["confirm"] as! String, "\(c)")
        }
    }
}

@MainActor @Suite struct PanelPickerTests {
    @Test(.enabled(if: haveFixtures)) func pickersOfferActiveEntriesOnlyButLabelsStillResolve() async throws {
        let fake = Fake()
        await fake.set("GET /projects", 200, #"{"projects":[{"id":1,"name":"Old","archived":true},{"id":2,"name":"Live","archived":false}]}"#)
        await fake.set("GET /teams", 200, #"{"teams":[{"id":4,"name":"gone","archived":true,"segment":1099511627780},{"id":5,"name":"web","archived":false,"segment":1099511627781}]}"#)
        await fake.set("GET /kb/submissions", 200, #"{"submissions":[]}"#)
        for (k, b) in [("GET /kb/notes", #"{"notes":[]}"#), ("GET /kb/faq", #"{"entries":[]}"#)] { await fake.set(k, 200, b) }
        let m = PanelModel(panel: try kb(), strings: try strings("C"), routes: PanelRoutes.builtin, send: { await fake.handle($0) })
        await m.load()
        #expect(m.refs["projects"]?.map(\.skipped) == [true, false] && m.pickerOptions("projects").map(\.id) == ["2"])
        #expect(m.pickerOptions("teams").map(\.id) == ["1099511627781"])
        #expect(m.selected[PanelListKey(path: "notes-project", parent: "")] == .int(2))  // the first entry that is offered
        let log = await fake.log
        #expect(log.contains("GET /kb/notes?project=2") && !log.contains("GET /kb/notes?project=1"))
        // the users panel: an inactive user is not offered for a binding but still names the binding that refers to it
        let f2 = Fake()
        for (k, st, b) in userAnswers { await f2.set(k, st, b) }
        await f2.set("GET /bindings", 200, #"{"bindings":[{"device":"dev-1","user":3}]}"#)
        let um = PanelModel(panel: try users(), strings: try strings("C"), routes: PanelRoutes.builtin, send: { await f2.handle($0) })
        await um.load()
        #expect(um.pickerOptions("users").map(\.id) == ["1", "2"])
        let bound = um.visibleRows(um.panel.sections[1], path: "bindings")[0]
        #expect(um.cell(um.panel.sections[1].columns![1], bound) == "old")
        #expect(um.refs["users"]?.first { $0.id == "3" }?.skipped == true)
    }

    @Test(.enabled(if: haveFixtures)) func anEmptyValueIsAnEnDashAndRoleNamesCarryTheTechnicalName() async throws {
        let m = try await usersModel(Fake())
        let col = PanelColumn(field: "x", kind: "text", label: "k", yes: nil, no: nil, values: nil, ref: nil)
        #expect(m.shown(col, PanelRow(id: "", values: [:])) == "\u{2013}" && m.shown(col, PanelRow(id: "", values: ["x": .string("v")])) == "v")
        let roles = try teamsPanel().sections[1].detail!.sections[0].columns!.first { $0.field == "role" }!.values!
        for (loc, want) in [("C", ["Read (reader)", "Develop (developer)", "Manage (admin)"]), ("sl_SI", ["Branje (reader)", "Razvijanje (developer)", "Upravljanje (admin)"])] {
            let st = try strings(loc)
            #expect(["reader", "developer", "admin"].map { st.text(roles[$0]) } == want)
        }
    }
}

private func upstreamsPanel() throws -> PanelDescription {
    guard case .ready(let d) = PanelValidator.parse(data(try description("builtin.upstreams")), provider: "builtin") else { throw Err.bad }
    return d
}

private let upstreamToken = "tok-1234#key-5678"
private let upstreamAnswers: [(String, Int, String)] = [
    ("GET /upstreams", 200, #"{"upstreams":[{"name":"hub","address":"hub.example","device":"DEV"}],"links":[{"id":"project-1-hub","place":"project","label":"acme-web","upstream":"hub","linkId":1,"projectId":1,"teamId":0,"team":false,"classesText":"structure, data","pending":2,"total":5,"lastError":""}]}"#),
    ("GET /projects", 200, #"{"projects":[{"id":1,"name":"Acme Web","archived":false},{"id":2,"name":"Old","archived":true}]}"#),
    ("GET /teams", 200, #"{"teams":[{"id":4,"name":"web","archived":false}]}"#),
    ("GET /upstreams/hub/probe", 200, #"{"name":"hub","address":"hub.example","device":"DEV","state":"waiting","probe":"waiting for approval on the upstream"}"#),
]

@MainActor @Suite struct PanelUpstreamTests {
    private func model(_ fake: Fake) async throws -> PanelModel {
        for (k, st, b) in upstreamAnswers { await fake.set(k, st, b) }
        let m = PanelModel(panel: try upstreamsPanel(), strings: try strings("C"), routes: PanelRoutes.builtin, send: { await fake.handle($0) })
        await m.load()
        return m
    }

    @Test(.enabled(if: haveFixtures)) func everyKeyHasATextAndTheRegistryMarksTheSecretRoute() throws {
        let d = try upstreamsPanel()
        for loc in ["C", "sl_SI"] {
            let st = try strings(loc)
            let keys = panelKeys(d)
            #expect(keys.count > 60)
            for k in keys { #expect(st.table[k] != nil, "\(loc): no text for \(k)") }
        }
        #expect(PanelRoutes.builtinSecret == ["PUT upstreams/{}"] && PanelRoutes.builtin.contains("PUT upstreams/{}"))
        // an action on a route that needs a confirmation has one: without its confirm the panel is refused
        #expect(PanelRoutes.builtinConfirm == ["POST upstream-push", "PUT upstreams/{}", "DELETE upstreams/{}", "DELETE upstreams/{}/links/{}", "PUT upstreams/{}/links/{}", "POST upstream-resolve"])
        let bare = String(decoding: data(try description("builtin.upstreams")), as: UTF8.self).replacingOccurrences(of: "\"confirm\":", with: "\"confirmx\":")
        if case .ready = PanelValidator.parse(Data(bare.utf8), provider: "builtin") { Issue.record("accepted without any confirm") }
        // without a registered secret route the panel is refused
        if case .ready = PanelValidator.parse(data(try description("builtin.upstreams")), provider: "builtin", secretRoutes: []) { Issue.record("accepted without a secret route") }
    }

    @Test(.enabled(if: haveFixtures)) func theSecretStaysOutOfStateAndOutOfErrors() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let up = m.panel.sections[1], add = up.actions![0]
        let values: [String: PanelJSON] = ["name": .string("hub"), "address": .string("hub.example:8765"), "token": .string(upstreamToken), "deviceName": .string("")]
        let req = try m.request(add, values: values).get()
        #expect(req.path == "/upstreams/hub" && req.query.isEmpty && String(decoding: req.body, as: UTF8.self).contains(upstreamToken))
        #expect(m.scrubbed(add, values)["token"] == nil && m.scrubbed(add, values)["name"] == .string("hub"))
        #expect(m.problems(add, values: ["name": .string("hub"), "address": .string("a"), "token": .string("  ")]) == ["token": .required])
        // a server that echoes its input: neither the value nor a part of the token line is shown; a plain message stays; 5xx is generic
        for (status, message, want) in [(400, "bad token \(upstreamToken)", PanelError.http(400)), (400, "unknown key key-5678", .http(400)), (403, "tok-1234 refused", .http(403)),
                                        (400, "an upstream named hub exists", .server("an upstream named hub exists")), (500, "internal error \(upstreamToken)", .serverFault(500))] {
            await fake.set("PUT /upstreams/hub", status, #"{"error":{"message":"\#(message)"}}"#)
            #expect(await !m.perform(add, section: "upstreams", values: values, confirmed: true))
            #expect(m.actionError[PanelListKey(path: "upstreams", parent: "")] == want, "\(message)")
        }
        // the server trims the token line: an echo of the trimmed value (no "#" in it) is scrubbed too
        await fake.set("PUT /upstreams/hub", 400, #"{"error":{"message":"bad token plainsecret99"}}"#)
        var spaced = values; spaced["token"] = .string("  plainsecret99\n")
        #expect(await !m.perform(add, section: "upstreams", values: spaced, confirmed: true))
        #expect(m.actionError[PanelListKey(path: "upstreams", parent: "")] == .http(400))
        // success: the answer is shown (the device ID to approve, the probe text) until the view closes it
        await fake.set("PUT /upstreams/hub", 200, #"{"name":"hub","address":"hub.example:8765","device":"DEVICE-ID","probe":"waiting for approval"}"#)
        #expect(await m.perform(add, section: "upstreams", values: values, confirmed: true))
        let r = m.results[PanelListKey(path: "upstreams", parent: "")]
        #expect(r?.rows.first?.values["device"] == .string("DEVICE-ID") && r?.columns.count == 4)
        m.closeResult(section: "upstreams")
        #expect(m.results.isEmpty)
    }

    @Test(.enabled(if: haveFixtures)) func theProbeIsReadWhenTheDetailOpensAndPickersSkipArchived() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let up = m.panel.sections[1]
        #expect(m.pickerOptions("projects").map(\.id) == ["1"])
        let hub = m.data("upstreams").rows[0]
        await fake.clear()
        await m.openDetail(up, row: hub)
        #expect(await fake.log == ["GET /upstreams/hub/probe"])
        let state = up.detail!.sections[0].columns![0]
        #expect(m.cell(state, PanelRow(id: "", values: m.record("upstreams/connection", parent: hub).values!)) == "Waiting for approval there")
    }

    @Test(.enabled(if: haveFixtures)) func pushAsksFirstAndSaysWhatLeaves() async throws {
        let fake = Fake()
        let m = try await model(fake)
        let links = m.panel.sections[2], link = m.data("links").rows[0]
        #expect(m.confirmation(links.rowActions![0], row: link)?.title == "Run a dry run for acme-web on hub?")  // every action on upstream-push asks
        let c = m.confirmation(links.rowActions![1], row: link)
        #expect(c?.title == "Push acme-web to hub?" && c?.destructive == false && c?.message.contains("title, class, kind and text") == true && c?.message.contains("No note ids") == true)
        await fake.set("POST /upstream-push", 200, #"{"pushes":[{"label":"acme-web","upstream":"hub","created":1,"skipped":0,"replaced":0,"refused":1,"dryRun":true,"more":false,"linesText":"create <img src=x onerror=alert(1)>\nrefused not allowed"}]}"#)
        #expect(await m.perform(links.rowActions![0], section: "links", row: link, confirmed: true))
        let rep = m.results[PanelListKey(path: "links", parent: "")]
        #expect(rep?.rows.count == 1 && rep?.rows[0].values["linesText"]?.scalarText?.contains("<img src=x onerror=alert(1)>") == true)
        let lines = rep!.columns.last!
        #expect(m.cell(lines, rep!.rows[0]).split(separator: "\n").count == 2)
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
    let titled = #"(?:\bButton|\bToggle|\bSecureField|\bTextField|\bPicker|\bLabeledContent|\bMenu|\bDisclosureGroup|\bSection|\bLabel|\bAlert|\.navigationTitle|\.navigationSubtitle|\.help|\.alert|\.confirmationDialog|\.accessibilityLabel|\.accessibilityHint|\.accessibilityValue|\.badge)(?:\.init)?\("#
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
