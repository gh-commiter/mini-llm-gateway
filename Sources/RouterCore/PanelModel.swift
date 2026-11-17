import Foundation

// The logic of the panel renderer, free of any view (the app target cannot be unit-tested): the list of panels, the data of
// one panel, the exact request each action sends, and the states a view shows. Calls go to `send`: in the app the admin
// context of the project helper (like UsageChart.load), in tests a fake. Every string that comes from a description or from
// data is passed on unchanged; the views show it as plain text.

public typealias PanelSend = @Sendable (CoreRequest) async -> CoreResponse

/// What went wrong, for the view to say in its own (localized) words, or verbatim for the server's message.
public enum PanelError: Error, Equatable, Sendable {
    case unavailable          // the project helper cannot be asked (503 core_unavailable)
    case notFound             // GET /panels is a 404: a stale helper, "update the app"
    case needsNewer           // the answer's format is above 1
    case server(String)       // the server's error.message, verbatim
    case http(Int)            // an error without a message
    case refused(String)      // this renderer did not send the request (a value that cannot be in a path, a missing confirmation …)
}

extension CoreResponse {
    /// The error of a failed call, nil for 2xx.
    var panelError: PanelError? {
        if (200..<300).contains(status) { return nil }
        let e = (try? JSONSerialization.jsonObject(with: body) as? [String: Any])?["error"] as? [String: Any]
        if self == .unavailable || (status == 503 && e?["type"] as? String == "core_unavailable") { return .unavailable }
        if let m = e?["message"] as? String, !m.isEmpty { return .server(m) }
        return .http(status)
    }
}

// MARK: the list of panels

public struct PanelEntry: Identifiable, Equatable, Sendable {
    public let id: String
    public let parse: PanelParse
    let title: PStr?, about: PStr?, symbol: String
    public func title(_ l: PanelLang) -> String { title?.text(l) ?? id }
    public func about(_ l: PanelLang) -> String { about?.text(l) ?? "" }
    public var sidebarSymbol: String { symbol }
}

/// GET /panels: every description validated by this side again; one that fails stays in the list as "cannot be shown".
@MainActor public final class PanelCatalog: ObservableObject {
    @Published public private(set) var entries: [PanelEntry] = []
    @Published public private(set) var error: PanelError?
    @Published public private(set) var loaded = false
    private let send: PanelSend
    public init(send: @escaping PanelSend) { self.send = send }

    public func load() async {
        let r = await send(CoreRequest(method: "GET", path: "/panels"))
        loaded = true
        if let e = r.panelError { error = r.status == 404 ? .notFound : e; entries = []; return }
        guard let top = (try? JSONSerialization.jsonObject(with: r.body)) as? [String: Any], let format = top["format"] as? Int, let panels = top["panels"] as? [Any] else {
            error = .refused("GET /panels: not the expected answer"); entries = []; return
        }
        if format > 1 { error = .needsNewer; entries = []; return }
        error = nil
        var seen = Set<String>(), out: [PanelEntry] = []
        for (i, p) in panels.enumerated() {
            guard let data = try? JSONSerialization.data(withJSONObject: p) else { continue }
            let entry = Self.entry(data, p as? [String: Any] ?? [:], index: i)
            if seen.insert(entry.id).inserted { out.append(entry) }
        }
        entries = out
    }

    /// An entry for a description that is valid, and a best-effort one (id and title if they are strings) for one that is not.
    static func entry(_ data: Data, _ raw: [String: Any], index: Int) -> PanelEntry {
        let parse = PanelValidator.parse(data)
        if case .ready(let d) = parse { return PanelEntry(id: d.id, parse: parse, title: d.title, about: d.about, symbol: d.symbol) }
        func s(_ k: String) -> PStr? {
            guard let o = raw[k] as? [String: Any], let en = o["en"] as? String, let sl = o["sl"] as? String else { return nil }
            return PStr(en: String(en.prefix(PanelLimits.label)), sl: String(sl.prefix(PanelLimits.label)))
        }
        let id = (raw["id"] as? String).map { String($0.prefix(64)) } ?? "invalid-\(index)"
        return PanelEntry(id: id, parse: parse, title: s("title"), about: nil, symbol: "")
    }
}

// MARK: one panel

public struct PanelRow: Identifiable, Equatable, Sendable {
    public let id: String
    public let values: [String: PanelJSON]
}

public struct PanelOptionValue: Identifiable, Equatable, Sendable {
    public let key: PanelJSON
    public let label: String
    public var id: String { key.scalarText ?? "" }
}

public enum FormProblem: Equatable, Sendable { case required, tooLong(Int) }

/// A list section's data; `path` is "orders" or, in a detail page, "orders/lines"; `parent` is the key of the opened row.
public struct PanelListKey: Hashable, Sendable { public let path: String; public let parent: String }

public struct PanelListData: Sendable {
    public var rows: [PanelRow] = []
    public var next: String?
    public var error: PanelError?
    public var loading = false
    public var search = ""
    public var showHidden = false
}

public struct PanelConfirmText: Equatable, Sendable { public let title: String, message: String, button: String, destructive: Bool }

@MainActor public final class PanelModel: ObservableObject {
    public let panel: PanelDescription
    public let lang: PanelLang
    private let send: PanelSend
    private let routes: [String]
    @Published public private(set) var refs: [String: [PanelOptionValue]] = [:]
    @Published public private(set) var refError: PanelError?
    @Published public private(set) var lists: [PanelListKey: PanelListData] = [:]
    @Published public private(set) var actionError: [PanelListKey: PanelError] = [:]
    @Published public private(set) var loading = false
    private var opened: [PanelListKey: (PanelSection, PanelRow)] = [:]  // detail pages that are open: reloaded with the panel

    /// `routes`: what the panel's provider registered (nil for an unknown provider: nothing is ever called).
    public init(panel: PanelDescription, lang: PanelLang, routes: [String]? = nil, send: @escaping PanelSend) {
        self.panel = panel; self.lang = lang; self.send = send
        self.routes = routes ?? PanelRoutes.registered(for: panel.provider) ?? []
    }

    // MARK: loading

    /// Reloads the whole panel: refs, every top-level list and every open detail page.
    public func load() async {
        loading = true
        defer { loading = false }
        await loadRefs()
        for s in panel.sections where s.kind == "list" { await loadList(s, path: s.id, parent: nil) }
        for (_, o) in opened { await loadDetail(o.0, o.1) }
    }

    func allowed(_ method: String, _ template: String) -> Bool { PanelTemplate.route(method, template).map { routes.contains($0) } ?? false }

    private func loadRefs() async {
        refError = nil
        for (name, r) in panel.refs.sorted(by: { $0.key < $1.key }) {
            guard allowed("GET", r.path) else { refError = .refused("route GET \(r.path) is not registered"); continue }
            let a = await send(CoreRequest(method: "GET", path: r.path))
            if let e = a.panelError { refError = e; refs[name] = []; continue }
            guard let items = Self.items(a.body, r.items) else { refError = .refused("the answer of \(r.path) has no \(r.items)"); refs[name] = []; continue }
            if items.count > PanelLimits.refEntries { refError = .refused("more than \(PanelLimits.refEntries) entries in \(r.path)"); refs[name] = []; continue }
            refs[name] = items.compactMap { o in
                guard let k = o[r.key], k.scalarText != nil else { return nil }
                return PanelOptionValue(key: k, label: o[r.label]?.scalarText ?? k.scalarText ?? "")
            }
        }
    }

    static func items(_ body: Data, _ key: String) -> [[String: PanelJSON]]? {
        guard let top = try? JSONDecoder().decode([String: PanelJSON].self, from: body), case .array(let a)? = top[key] else { return nil }
        return a.compactMap { if case .object(let o) = $0 { o } else { nil } }
    }

    func loadList(_ s: PanelSection, path: String, parent: PanelRow?, more: Bool = false) async {
        let key = PanelListKey(path: path, parent: parent?.id ?? "")
        var d = lists[key] ?? PanelListData()
        guard let source = s.source else { return }
        let template = source.path
        var req = CoreRequest(method: "GET", path: "")
        guard allowed("GET", template), let p = PanelTemplate.resolve(path: template, .init(parent: parent?.values ?? [:])) else {
            d.error = .refused("the path of \(s.id) cannot be built"); lists[key] = d; return
        }
        req.path = p
        if more {
            guard let paging = s.paging, let n = d.next, let e = PanelTemplate.encode(n) else { return }
            req.query = "\(paging.param)=\(e)"
        }
        d.loading = true; lists[key] = d
        let a = await send(req)
        d.loading = false
        if let e = a.panelError { d.error = e; lists[key] = d; return }
        guard let items = Self.items(a.body, source.items) else { d.error = .refused("the answer has no \(source.items)"); lists[key] = d; return }
        d.error = nil
        let k = s.key ?? ""
        let base = more ? d.rows.count : 0
        let rows = items.enumerated().map { PanelRow(id: $1[k]?.scalarText ?? "#\(base + $0)", values: $1) }
        d.rows = more ? d.rows + rows : rows
        d.next = nil
        if let paging = s.paging, case .object(let top)? = try? JSONDecoder().decode(PanelJSON.self, from: a.body),
           let n = top[paging.next]?.scalarText, !n.isEmpty, n != "0" { d.next = n }
        lists[key] = d
    }

    public func loadMore(_ s: PanelSection, path: String, parent: PanelRow? = nil) async { await loadList(s, path: path, parent: parent, more: true) }

    /// Opens a detail page of `row`: its sections load now and with every reload.
    public func openDetail(_ s: PanelSection, row: PanelRow) async {
        opened[PanelListKey(path: s.id, parent: row.id)] = (s, row)
        await loadDetail(s, row)
    }

    public func closeDetail(_ s: PanelSection, row: PanelRow) { opened[PanelListKey(path: s.id, parent: row.id)] = nil }

    private func loadDetail(_ s: PanelSection, _ row: PanelRow) async {
        for c in s.detail?.sections ?? [] where c.kind == "list" { await loadList(c, path: s.id + "/" + c.id, parent: row) }
    }

    // MARK: what a view shows

    public func data(_ path: String, parent: PanelRow? = nil) -> PanelListData { lists[PanelListKey(path: path, parent: parent?.id ?? "")] ?? PanelListData() }

    public func setSearch(_ text: String, path: String, parent: PanelRow? = nil) { mutate(path, parent) { $0.search = text } }
    public func setShowHidden(_ on: Bool, path: String, parent: PanelRow? = nil) { mutate(path, parent) { $0.showHidden = on } }
    private func mutate(_ path: String, _ parent: PanelRow?, _ f: (inout PanelListData) -> Void) {
        let k = PanelListKey(path: path, parent: parent?.id ?? "")
        var d = lists[k] ?? PanelListData(); f(&d); lists[k] = d
    }

    /// The rows after the "hide" rule (until its toggle is on) and the search over the filter fields (plain case-insensitive text).
    public func visibleRows(_ s: PanelSection, path: String, parent: PanelRow? = nil) -> [PanelRow] {
        let d = data(path, parent: parent)
        return d.rows.filter { row in
            if let h = s.hide, !d.showHidden, row.values[h.field] == h.equals { return false }
            let q = d.search.trimmingCharacters(in: .whitespacesAndNewlines)
            if q.isEmpty { return true }
            return (s.filter ?? []).contains { row.values[$0]?.scalarText?.localizedCaseInsensitiveContains(q) == true }
        }
    }

    /// The text of a cell. A ref that is not found, an enum value that is not known show the raw value.
    public func cell(_ c: PanelColumn, _ row: PanelRow) -> String {
        let v = row.values[c.field]
        let raw = v?.scalarText ?? ""
        switch c.kind {
        case "bool":
            guard case .bool(let b)? = v else { return raw }
            return (b ? c.yes : c.no)?.text(lang) ?? raw
        case "enum": return c.values?[raw]?.text(lang) ?? raw
        case "ref": return refs[c.ref ?? ""]?.first { $0.id == raw }?.label ?? raw
        case "time": return Self.time(raw).map { $0.formatted(date: .abbreviated, time: .shortened) } ?? raw
        default: return raw
        }
    }

    static func time(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }

    /// The actions of a row that may be shown: this renderer knows all their parts, and `when` holds.
    public func rowActions(_ s: PanelSection, _ row: PanelRow) -> [PanelAction] {
        (s.rowActions ?? []).filter { a in a.supported && (a.when.map { row.values[$0.field] == $0.equals } ?? true) }
    }

    public func actions(_ s: PanelSection) -> [PanelAction] { (s.actions ?? []).filter(\.supported) }

    /// How many parts of a section this renderer cannot show (unknown column kinds, forms with an unknown field type): the view says so.
    public func unsupportedCount(_ s: PanelSection) -> Int {
        (s.columns ?? []).filter { !$0.supported }.count + ((s.actions ?? []) + (s.rowActions ?? [])).filter { !$0.supported }.count
    }

    // MARK: forms and requests

    /// The initial values of a form: constants and values of the row, a toggle that is off, otherwise nothing.
    public func defaults(_ a: PanelAction, row: PanelRow? = nil) -> [String: PanelJSON] {
        var out: [String: PanelJSON] = [:]
        for f in a.fields ?? [] {
            if let c = f.default?.const { out[f.id] = c }
            else if let r = f.default?.row, let v = row?.values[r] { out[f.id] = v }
            else if f.type == "toggle" { out[f.id] = .bool(false) }
        }
        return out
    }

    /// What the form still lacks (only a spared round trip: the server decides).
    public func problems(_ a: PanelAction, values: [String: PanelJSON]) -> [String: FormProblem] {
        var out: [String: FormProblem] = [:]
        for f in a.fields ?? [] {
            let v = values[f.id]
            if f.required == true {
                switch (f.type, v) {
                case ("text", .string(let s)?) where !s.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty: break
                case ("text", _): out[f.id] = .required
                case (_, nil), (_, .null?): out[f.id] = .required
                default: break
                }
            }
            if f.type == "text", case .string(let s)? = v, let m = f.maxLength, m > 0, s.count > m { out[f.id] = .tooLong(m) }
        }
        return out
    }

    /// The exact request of an action, or why it cannot be sent. Placeholders take one checked, encoded path segment each.
    public func request(_ a: PanelAction, row: PanelRow? = nil, parent: PanelRow? = nil, values: [String: PanelJSON] = [:]) -> Result<CoreRequest, PanelError> {
        guard a.supported, allowed(a.method, a.path) else { return .failure(.refused("the route \(a.method) \(a.path) is not registered")) }
        if !problems(a, values: values).isEmpty { return .failure(.refused("the form is not complete")) }
        let v = PanelTemplate.Values(row: row?.values ?? [:], parent: parent?.values ?? [:], field: values)
        guard let path = PanelTemplate.resolve(path: a.path, v) else { return .failure(.refused("a value cannot be part of the path")) }
        var req = CoreRequest(method: a.method, path: path)
        if let q = a.query, !q.isEmpty {
            guard let rq = PanelTemplate.resolve(query: q, v) else { return .failure(.refused("a value cannot be part of the query")) }
            req.query = rq
        }
        var body: [String: Any] = [:]
        for (k, b) in a.body ?? [:] {
            if let f = b.field {
                guard let x = values[f], x != .null else { return .failure(.refused("no value for \(f)")) }
                body[k] = x.any
            } else if let r = b.row {
                guard let x = row?.values[r] else { return .failure(.refused("the row has no \(r)")) }
                body[k] = x.any
            } else if let c = b.const { body[k] = c.any }
        }
        if !body.isEmpty { req.body = (try? JSONSerialization.data(withJSONObject: body, options: [.sortedKeys, .withoutEscapingSlashes])) ?? Data() }
        return .success(req)
    }

    /// The confirmation of an action; `{field}` takes the row's value as plain text. nil when the action needs none.
    public func confirmation(_ a: PanelAction, row: PanelRow? = nil) -> PanelConfirmText? {
        guard a.needsConfirmation, let c = a.confirm else { return nil }
        func fill(_ s: PStr) -> String {
            var t = s.text(lang)
            let re = try! Regex(#"\{([A-Za-z][A-Za-z0-9_]{0,63})\}"#)
            for m in t.matches(of: re).reversed() {  // one pass: a value that looks like a placeholder is not filled again
                guard let name = m.output[1].substring.map(String.init) else { continue }
                t.replaceSubrange(m.range, with: row?.values[name]?.scalarText ?? "")
            }
            return t
        }
        return PanelConfirmText(title: fill(c.title), message: fill(c.message), button: fill(c.button), destructive: a.method == "DELETE" || a.destructive == true || c.destructive == true)
    }

    /// Sends an action and reloads the whole panel after success. An action that needs a confirmation is refused unless
    /// `confirmed`. A failure is kept in `actionError` for the section and leaves the rows as they were.
    @discardableResult
    public func perform(_ a: PanelAction, section path: String, row: PanelRow? = nil, parent: PanelRow? = nil, values: [String: PanelJSON] = [:], confirmed: Bool = false) async -> Bool {
        let key = PanelListKey(path: path, parent: parent?.id ?? "")
        actionError[key] = nil
        if a.needsConfirmation && !confirmed { actionError[key] = .refused("the action needs a confirmation"); return false }
        switch request(a, row: row, parent: parent, values: values) {
        case .failure(let e): actionError[key] = e; return false
        case .success(let req):
            let r = await send(req)
            if let e = r.panelError { actionError[key] = e; return false }
            await load()
            return true
        }
    }
}

// MARK: the app's transport

public enum PanelTransport {
    /// Admin routes of the core go to the project helper in the owner's context; `GET /devices`, the one other route a
    /// built-in panel uses, is answered from the app's own device list (the same shape as the gateway's answer). Nothing else.
    public static func send(core: CoreHelper, devices: @escaping @Sendable () async -> [(id: String, name: String)]) -> PanelSend {
        { req in
            if req.method == "GET", req.path == "/devices" {
                let list = await devices().map { ["id": $0.id, "name": $0.name] }
                let body = (try? JSONSerialization.data(withJSONObject: ["devices": list], options: [.sortedKeys])) ?? Data()
                return CoreResponse(status: 200, header: ["Content-Type": ["application/json"]], body: body)
            }
            guard CoreRoutes.isAdminPath(req.method, req.path) else { return CoreResponse(status: 404, body: Data(#"{"type":"error","error":{"type":"not_found_error","message":"Not found"}}"#.utf8)) }
            return await core.call(context: .admin, request: req)
        }
    }
}
