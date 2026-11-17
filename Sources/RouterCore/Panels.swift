import Foundation

// Admin panels, format 1 (docs/plans/plugin-panels-design.md): the description types, the validator and the path templates.
// The Go core validates the same rules (client/coreapi/panels.go); the renderer applies them again to what it receives,
// because a description from the network is not trusted even when the server validated it. The fixtures
// client/testdata/panels-*.json are decoded by both sides. The model (PanelModel.swift) builds the requests.

/// The locale the renderer asks the core's string tables for: the app's own ("sl_SI" from "sl-SI"), or just the language
/// ("sl") when the app has no region; a region is never made up. "C" (the base table) when there is no usable language.
public enum PanelLocale {
    public static func tag(_ preferred: [String]) -> String {
        guard let first = preferred.first else { return "C" }
        let l = Locale(identifier: first.replacingOccurrences(of: "-", with: "_"))
        guard let lang = l.language.languageCode?.identifier.lowercased(), lang.count == 2, lang.allSatisfy({ $0.isASCII && $0.isLetter }) else { return "C" }
        guard let r = l.region?.identifier.uppercased(), r.count == 2, r.allSatisfy({ $0.isASCII && $0.isLetter }) else { return lang }
        return lang + "_" + r
    }
    public static var current: String { tag(Bundle.main.preferredLocalizations) }
}

/// The texts of the descriptions: the merged table of GET /panels/strings (the core already applied exact locale, same
/// language, then C). A key that has no text is shown as the key itself, never empty and never guessed.
public struct PanelStrings: Equatable, Sendable {
    public var locale: String
    public var table: [String: String]
    public init(locale: String = "C", table: [String: String] = [:]) { self.locale = locale; self.table = table }
    public func text(_ key: String?) -> String { key.map { table[$0] ?? $0 } ?? "" }

    static func parse(_ data: Data) -> PanelStrings? {
        struct Answer: Decodable { let format: Int; let locale: String; let strings: [String: String] }
        guard let a = try? JSONDecoder().decode(Answer.self, from: data), a.format == 1, a.strings.count <= 2000 else { return nil }
        return PanelStrings(locale: a.locale, table: a.strings)
    }
}

/// A JSON value of a description or of an API answer. Numbers keep their kind (an id stays an integer).
public enum PanelJSON: Decodable, Equatable, Sendable {
    case null, bool(Bool), int(Int64), double(Double), string(String), array([PanelJSON]), object([String: PanelJSON])

    public init(from d: Decoder) throws {
        let c = try d.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let v = try? c.decode(Bool.self) { self = .bool(v) }
        else if let v = try? c.decode(Int64.self) { self = .int(v) }
        else if let v = try? c.decode(Double.self) { self = .double(v) }
        else if let v = try? c.decode(String.self) { self = .string(v) }
        else if let v = try? c.decode([PanelJSON].self) { self = .array(v) }
        else { self = .object(try c.decode([String: PanelJSON].self)) }
    }

    var any: Any {
        switch self {
        case .null: NSNull()
        case .bool(let b): b
        case .int(let i): i
        case .double(let d): d
        case .string(let s): s
        case .array(let a): a.map(\.any)
        case .object(let o): o.mapValues(\.any)
        }
    }

    /// The text a scalar contributes to a path, a query or a label; nil for null, objects, arrays and fractions.
    public var scalarText: String? {
        switch self {
        case .string(let s): s
        case .int(let i): String(i)
        case .bool(let b): b ? "true" : "false"
        case .double(let d): d == d.rounded() && abs(d) < 9e15 ? String(Int64(d)) : nil
        default: nil
        }
    }

    var isScalar: Bool { if case .string = self { true } else if case .int = self { true } else if case .double = self { true } else if case .bool = self { true } else { false } }
}

// MARK: description

public struct PanelDescription: Decodable, Equatable, Sendable {
    public let format: Int
    public let id: String
    public let provider: String
    public let title: String
    public let about: String
    public let symbol: String
    public let refs: [String: PanelRef]
    public let sections: [PanelSection]
}

public struct PanelRef: Decodable, Equatable, Sendable { public let path: String; public let items: String; public let key: String; public let label: String }

public struct PanelSection: Decodable, Equatable, Sendable {
    public let kind: String
    public let id: String
    public let title: String?
    public let body: String?
    public let source: PanelSource?
    public let key: String?
    public let columns: [PanelColumn]?
    public let filter: [String]?
    public let hide: PanelHide?
    public let paging: PanelPaging?
    public let empty: String?
    public let actions: [PanelAction]?
    public let rowActions: [PanelAction]?
    public let detail: PanelDetail?
    public let scope: PanelScope?   // list: a picker whose value is sent as a query parameter
    public let when: PanelWhen?     // record in a detail page: shown only while the opened row's field equals the value
    public var supported: Bool { kind == "list" || kind == "text" || kind == "record" }
}

public struct PanelSource: Decodable, Equatable, Sendable { public let path: String; public let items: String? }
public struct PanelScope: Decodable, Equatable, Sendable { public let ref: String; public let param: String; public let label: String }

public struct PanelColumn: Decodable, Equatable, Sendable {
    public let field: String
    public let kind: String
    public let label: String
    public let yes: String?
    public let no: String?
    public let values: [String: String]?
    public let ref: String?
    public var supported: Bool { ["text", "multiline", "bool", "enum", "ref", "time"].contains(kind) }
}

public struct PanelHide: Decodable, Equatable, Sendable { public let field: String; public let equals: PanelJSON; public let toggle: String }
public struct PanelPaging: Decodable, Equatable, Sendable { public let next: String; public let param: String }
public struct PanelDetail: Decodable, Equatable, Sendable { public let title: String; public let sections: [PanelSection] }

public struct PanelAction: Decodable, Equatable, Sendable {
    public let id: String
    public let label: String
    public let method: String
    public let path: String
    public let query: String?
    public let body: [String: PanelBodyValue]?
    public let fields: [PanelField]?
    public let when: PanelWhen?
    public let confirm: PanelConfirm?
    public let destructive: Bool?
    /// A form with a field type this renderer does not know is never shown, not even in part.
    public var supported: Bool { (fields ?? []).allSatisfy(\.supported) }
    public var needsConfirmation: Bool { method == "DELETE" || destructive == true || confirm != nil }
}

public struct PanelBodyValue: Decodable, Equatable, Sendable { public let field: String?; public let row: String?; public let `const`: PanelJSON? }
public struct PanelWhen: Decodable, Equatable, Sendable { public let field: String; public let equals: PanelJSON }
public struct PanelConfirm: Decodable, Equatable, Sendable { public let title: String; public let message: String; public let button: String; public let destructive: Bool? }

public struct PanelField: Decodable, Equatable, Sendable {
    public let id: String
    public let type: String
    public let label: String
    public let required: Bool?
    public let maxLength: Int?
    public let `default`: PanelDefault?
    public let options: [PanelOption]?
    public let ref: String?
    public var supported: Bool { ["text", "toggle", "choice", "ref"].contains(type) }
}

public struct PanelDefault: Decodable, Equatable, Sendable { public let `const`: PanelJSON?; public let row: String? }
public struct PanelOption: Decodable, Equatable, Sendable { public let value: String; public let label: String }

// MARK: parsing and validation

/// The routes a provider registered, "METHOD pattern" with {} for one path segment (as adminDo's switch). The renderer
/// knows the built-in provider itself; the fixture client/testdata/panels-registry.json is asserted against it.
public enum PanelRoutes {
    public static let builtin = ["GET users", "POST users", "PATCH users/{}", "GET bindings", "PUT bindings/{}", "DELETE bindings/{}", "GET devices",
                                 "GET kb/submissions", "GET kb/submissions/{}", "POST kb/submissions/{}/accept", "POST kb/submissions/{}/reject",
                                 "GET kb/notes", "GET kb/notes/{}", "GET kb/faq", "GET projects", "GET teams"]
    public static func registered(for provider: String) -> [String]? { provider == "builtin" ? builtin : nil }

    /// Does a concrete (escaped) path with its method match one of the registered patterns, segment for segment? A segment
    /// that is empty, ".", ".." or holds a "/" or "\\" once decoded never matches, not even a "{}".
    public static func allows(_ method: String, _ escapedPath: String, in patterns: [String]) -> Bool {
        guard escapedPath.hasPrefix("/") else { return false }
        let raw = escapedPath.dropFirst().split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        var segs: [String] = []
        for r in raw {
            guard let d = r.removingPercentEncoding, !d.isEmpty, d != ".", d != "..", !d.contains("/"), !d.contains("\\"), !PanelValidator.hasControl(d) else { return false }
            segs.append(d)
        }
        return patterns.contains { pat in
            guard pat.hasPrefix(method + " ") else { return false }
            let pp = pat.dropFirst(method.count + 1).split(separator: "/", omittingEmptySubsequences: false).map(String.init)
            return pp.count == segs.count && zip(pp, segs).allSatisfy { $0 == "{}" || $0 == $1 }
        }
    }
}

public enum PanelParse: Equatable, Sendable {
    case ready(PanelDescription)
    case needsNewer(Int)   // a format above 1: "This panel needs a newer Mini Gateway", nothing else
    case refused(String)   // the reason, for the "cannot be shown" state
}

public enum PanelLimits {
    public static let multiline = 20_000, bytes = 64 << 10, sections = 12, columns = 10, actions = 12, fields = 10, label = 120, message = 600
    public static let refs = 5, refEntries = 1000, options = 20, bodyKeys = 10, queryParams = 8, maxLength = 4096, filter = 5
}

public enum PanelValidator {
    static let symbols = ["person.2", "person.3", "folder", "book", "tray", "gearshape", "doc.text", "key", "list.bullet"]

    /// Decodes and validates one description with the routes of the provider that the REGISTRY (the answer of GET /panels)
    /// names, never the provider field inside the description: that one must equal it.
    public static func parse(_ data: Data, provider: String, routes: [String]? = nil) -> PanelParse {
        guard data.count <= PanelLimits.bytes else { return .refused("the description is larger than \(PanelLimits.bytes) bytes") }
        struct Probe: Decodable { let format: Int }
        guard let probe = try? JSONDecoder().decode(Probe.self, from: data) else { return .refused("not a JSON object with a format") }
        if probe.format > 1 { return .needsNewer(probe.format) }
        guard probe.format == 1 else { return .refused("format \(probe.format) is not supported") }
        let d: PanelDescription
        do { d = try JSONDecoder().decode(PanelDescription.self, from: data) } catch { return .refused("cannot decode: \(error)") }
        guard d.provider == provider else { return .refused("the description says provider \(d.provider), the registry says \(provider)") }
        guard let routes = routes ?? PanelRoutes.registered(for: provider) else { return .refused("provider \(provider) is not known here") }
        do { try Check(d: d, routes: routes).panel(); return .ready(d) } catch let r as Refusal { return .refused(r.reason) } catch { return .refused("\(error)") }
    }

    struct Refusal: Error { let reason: String; init(_ r: String) { reason = r } }

    static func m(_ s: String, _ pattern: String) -> Bool { s.range(of: pattern, options: .regularExpression) != nil }
    static func isID(_ s: String) -> Bool { m(s, #"^[a-z][a-z0-9-]{0,31}\z"#) }
    static func isName(_ s: String) -> Bool { m(s, #"^[A-Za-z][A-Za-z0-9_]{0,63}\z"#) }

    static func hasControl(_ s: String) -> Bool {
        s.unicodeScalars.contains {
            switch $0.properties.generalCategory { case .control, .format, .lineSeparator, .paragraphSeparator: true; default: false }
        }
    }

    static func scalar(_ v: PanelJSON?) -> Bool {
        guard let v, v.isScalar else { return false }
        if case .string(let s) = v { return s.unicodeScalars.count <= PanelLimits.label }
        return true
    }

    struct Check {
        let d: PanelDescription
        let routes: [String]

        /// A text of the description is a key of the panel's namespace: [a-z0-9._-], at most 120 characters, "<panel id>.…".
        func str(_ name: String, _ k: String?, _ max: Int = 0) throws {
            guard let k, m(k, #"^[a-z0-9._-]{1,120}\z"#), k.hasPrefix(d.id + "."), !k.contains(".."), !k.hasSuffix(".") else {
                throw Refusal("\(name): \(k.map { "\($0.prefix(60))" } ?? "nothing") is not a string key of panel \(d.id)")
            }
        }

        func panel() throws {
            guard isID(d.provider) else { throw Refusal("provider \(d.provider) is not valid") }
            guard d.id.hasPrefix(d.provider + "."), isID(String(d.id.dropFirst(d.provider.count + 1))) else { throw Refusal("id \(d.id) must be \(d.provider).<name>") }
            try str("title", d.title); try str("about", d.about)
            guard symbols.contains(d.symbol) else { throw Refusal("symbol \(d.symbol) is not in the list") }
            guard d.refs.count <= PanelLimits.refs else { throw Refusal("more than \(PanelLimits.refs) refs") }
            for (name, r) in d.refs {
                guard isID(name), isName(r.items), isName(r.key), isName(r.label) else { throw Refusal("ref \(name): bad name, items, key or label") }
                _ = try path("ref \(name)", "GET", r.path, Scope())
            }
            try sections(d.sections, nested: false)
        }

        struct Scope { var row = false, parent = false; var fields: Set<String> = [] }

        func sections(_ ss: [PanelSection], nested: Bool) throws {
            guard !ss.isEmpty, ss.count <= PanelLimits.sections else { throw Refusal("sections: 1 to \(PanelLimits.sections)") }
            var seen = Set<String>()
            for s in ss {
                guard isID(s.id), seen.insert(s.id).inserted else { throw Refusal("section: bad or repeated id \(s.id)") }
                switch s.kind {
                case "text":
                    guard s.body != nil, s.title == nil, s.source == nil, s.key == nil, s.columns == nil, s.filter == nil, s.hide == nil, s.paging == nil,
                          s.empty == nil, s.actions == nil, s.rowActions == nil, s.detail == nil, s.scope == nil, s.when == nil else { throw Refusal("a text section has only id and body") }
                    try str("body", s.body, PanelLimits.message)
                case "list": try list(s, nested: nested)
                case "record": try record(s, nested: nested)
                default: continue  // an unknown kind in a known format: the renderer shows a visible placeholder
                }
            }
        }

        func list(_ s: PanelSection, nested: Bool) throws {
            guard s.body == nil, s.when == nil, let source = s.source, s.title != nil, let key = s.key else { throw Refusal("list \(s.id): needs title, source and key, and has no body or when") }
            if let sc = s.scope {
                guard d.refs[sc.ref] != nil, m(sc.param, #"^[a-z][a-z0-9_]{0,31}\z"#) else { throw Refusal("list \(s.id): scope with an undeclared ref or a bad parameter name") }
                if s.paging?.param == sc.param { throw Refusal("list \(s.id): scope and paging use the same parameter") }
                try str("scope.label", sc.label)
            }
            try str("title", s.title)
            if let e = s.empty { try str("empty", e) }
            guard let items = source.items, isName(items), isName(key) else { throw Refusal("list \(s.id): bad items or key") }
            _ = try path("list \(s.id) source", "GET", source.path, Scope(parent: nested))
            let cols = s.columns ?? []
            guard !cols.isEmpty, cols.count <= PanelLimits.columns else { throw Refusal("list \(s.id): columns 1 to \(PanelLimits.columns)") }
            for c in cols { try column(c) }
            let filter = s.filter ?? []
            guard filter.count <= PanelLimits.filter, filter.allSatisfy(isName) else { throw Refusal("list \(s.id): bad filter") }
            if let h = s.hide { guard isName(h.field), scalar(h.equals) else { throw Refusal("list \(s.id): bad hide") }; try str("hide.toggle", h.toggle) }
            if let p = s.paging, !(isName(p.next) && isName(p.param)) { throw Refusal("list \(s.id): bad paging") }
            guard (s.actions ?? []).count <= PanelLimits.actions, (s.rowActions ?? []).count <= PanelLimits.actions else { throw Refusal("list \(s.id): too many actions") }
            for a in s.actions ?? [] { try action(a, Scope(parent: nested)) }
            for a in s.rowActions ?? [] { try action(a, Scope(row: true, parent: nested)) }
            if let det = s.detail {
                guard !nested else { throw Refusal("list \(s.id): detail pages nest only one level") }
                guard isName(det.title) else { throw Refusal("list \(s.id): bad detail title") }
                try sections(det.sections, nested: true)
            }
        }

        /// One object from the answer of `source` as label/value pairs; in a detail page `when` is checked on the opened row.
        func record(_ s: PanelSection, nested: Bool) throws {
            guard s.body == nil, let source = s.source, source.items == nil, s.key == nil, s.filter == nil, s.hide == nil, s.paging == nil, s.empty == nil,
                  s.actions == nil, s.rowActions == nil, s.detail == nil, s.scope == nil else { throw Refusal("record \(s.id): only title, source (a path), columns and when") }
            if let t = s.title { try str("title", t) }
            _ = try path("record \(s.id) source", "GET", source.path, Scope(parent: nested))
            let cols = s.columns ?? []
            guard !cols.isEmpty, cols.count <= PanelLimits.columns else { throw Refusal("record \(s.id): columns 1 to \(PanelLimits.columns)") }
            for c in cols { try column(c) }
            if let w = s.when { guard nested, isName(w.field), scalar(w.equals) else { throw Refusal("record \(s.id): when is for detail pages, with a field and a scalar") } }
        }

        func column(_ c: PanelColumn) throws {
            guard isName(c.field) else { throw Refusal("column: bad field \(c.field)") }
            try str("column label", c.label)
            guard c.supported else { return }
            func extras(_ yesNo: Bool, _ values: Bool, _ ref: Bool) throws {
                guard ((c.yes != nil || c.no != nil) == yesNo) && ((c.values != nil) == values) && ((c.ref != nil) == ref) else { throw Refusal("column \(c.field): wrong extra keys") }
            }
            switch c.kind {
            case "bool": try extras(true, false, false); try str("yes", c.yes); try str("no", c.no)
            case "enum":
                try extras(false, true, false)
                guard let v = c.values, !v.isEmpty, v.count <= PanelLimits.options else { throw Refusal("column \(c.field): enum values") }
                for (k, l) in v { guard !k.isEmpty, k.utf8.count <= 64 else { throw Refusal("column \(c.field): bad enum key") }; try str("value \(k)", l) }
            case "ref":
                try extras(false, false, true)
                guard let r = c.ref, d.refs[r] != nil else { throw Refusal("column \(c.field): ref is not declared") }
            default: try extras(false, false, false)
            }
        }

        func action(_ a: PanelAction, _ outer: Scope) throws {
            guard isID(a.id) else { throw Refusal("action: bad id \(a.id)") }
            try str("action label", a.label)
            guard ["POST", "PUT", "PATCH", "DELETE"].contains(a.method) else { throw Refusal("action \(a.id): method \(a.method)") }
            let fields = a.fields ?? []
            guard fields.count <= PanelLimits.fields else { throw Refusal("action \(a.id): too many fields") }
            guard a.supported else { return }  // never shown: nothing else to check
            var sc = outer
            for f in fields {
                try field(f, outer)
                guard sc.fields.insert(f.id).inserted else { throw Refusal("action \(a.id): field \(f.id) repeated") }
            }
            _ = try path("action \(a.id) path", a.method, a.path, sc)
            try query(a.query, sc)
            let body = a.body ?? [:]
            guard body.count <= PanelLimits.bodyKeys, !(a.method == "DELETE" && !body.isEmpty) else { throw Refusal("action \(a.id): body") }
            for (k, b) in body {
                var n = 0
                if let f = b.field { n += 1; guard sc.fields.contains(f) else { throw Refusal("action \(a.id): body \(k) has no form field \(f)") } }
                if let r = b.row { n += 1; guard sc.row, isName(r) else { throw Refusal("action \(a.id): body \(k) row") } }
                if let c = b.const { n += 1; guard scalar(c) else { throw Refusal("action \(a.id): body \(k) const") } }
                guard isName(k), n == 1 else { throw Refusal("action \(a.id): body \(k) needs exactly one of field, row, const") }
            }
            if let w = a.when { guard sc.row, isName(w.field), scalar(w.equals) else { throw Refusal("action \(a.id): when") } }
            if a.method == "DELETE" || a.destructive == true { guard a.confirm != nil else { throw Refusal("action \(a.id): a destructive action needs a confirmation") } }
            if let c = a.confirm { try str("confirm.title", c.title); try str("confirm.message", c.message); try str("confirm.button", c.button) }
        }

        func field(_ f: PanelField, _ sc: Scope) throws {
            guard isName(f.id) else { throw Refusal("field: bad id \(f.id)") }
            try str("field label", f.label)
            guard f.supported else { return }
            guard (0...PanelLimits.maxLength).contains(f.maxLength ?? 0) else { throw Refusal("field \(f.id): maxLength") }
            if let df = f.default {
                guard (df.const != nil) != (df.row != nil) else { throw Refusal("field \(f.id): default needs exactly one of const, row") }
                if let r = df.row { guard sc.row, isName(r) else { throw Refusal("field \(f.id): default row") } }
                if let c = df.const { guard scalar(c) else { throw Refusal("field \(f.id): default const") } }
            }
            guard (f.maxLength == nil || f.type == "text"), (f.options != nil) == (f.type == "choice"), (f.ref != nil) == (f.type == "ref") else {
                throw Refusal("field \(f.id): wrong extra keys")
            }
            if f.type == "choice" {
                guard let o = f.options, !o.isEmpty, o.count <= PanelLimits.options else { throw Refusal("field \(f.id): options") }
                for x in o { guard !x.value.isEmpty, x.value.utf8.count <= 64 else { throw Refusal("field \(f.id): option value") }; try str("option", x.label) }
            }
            if f.type == "ref" { guard let r = f.ref, d.refs[r] != nil else { throw Refusal("field \(f.id): ref is not declared") } }
        }

        /// A path template: whole segments, each a plain name or exactly one placeholder; the route it resolves to must be registered.
        func path(_ where_: String, _ method: String, _ p: String, _ sc: Scope) throws -> [PanelTemplate.Seg] {
            guard let segs = PanelTemplate.parse(p) else { throw Refusal("\(where_): not a path of whole segments") }
            var pat: [String] = []
            for s in segs {
                switch s {
                case .literal(let l): pat.append(l)
                case .ref(let kind, let name):
                    guard (kind == .row && sc.row) || (kind == .parent && sc.parent) || (kind == .field && sc.fields.contains(name)) else {
                        throw Refusal("\(where_): placeholder \(kind.rawValue).\(name) is not available here")
                    }
                    pat.append("{}")
                }
            }
            if d.provider != "builtin" && !p.hasPrefix("/plugins/\(d.provider)/") { throw Refusal("\(where_): the routes of provider \(d.provider) live under /plugins/\(d.provider)/") }
            guard routes.contains(method + " " + pat.joined(separator: "/")) else { throw Refusal("\(where_): route \(method) \(pat.joined(separator: "/")) is not registered by provider \(d.provider)") }
            return segs
        }

        func query(_ q: String?, _ sc: Scope) throws {
            guard let q, !q.isEmpty else { return }
            let parts = q.split(separator: "&", omittingEmptySubsequences: false).map(String.init)
            guard parts.count <= PanelLimits.queryParams else { throw Refusal("query: too many parameters") }
            for p in parts {
                guard let eq = p.firstIndex(of: "="), m(String(p[..<eq]), #"^[a-z][a-z0-9_]{0,31}\z"#) else { throw Refusal("query: \(p) is not name=value") }
                let v = String(p[p.index(after: eq)...])
                if let (kind, name) = PanelTemplate.placeholder(v) {
                    guard (kind == .row && sc.row) || (kind == .parent && sc.parent) || (kind == .field && sc.fields.contains(name)) else { throw Refusal("query: placeholder \(v) is not available here") }
                } else if !m(v, #"^[A-Za-z0-9_.-]{0,64}\z"#) { throw Refusal("query: value \(v) is neither plain nor exactly one placeholder") }
            }
        }
    }
}

// MARK: templates

/// Path and query templates. A placeholder fills exactly one whole path segment; its value is checked and percent-encoded.
public enum PanelTemplate {
    public enum Kind: String, Sendable { case row, field, parent }
    public enum Seg: Equatable, Sendable { case literal(String), ref(Kind, String) }

    /// `{row.f}`, `{field.f}`, `{parent.f}` as the whole of `s`.
    static func placeholder(_ s: String) -> (Kind, String)? {
        guard s.hasPrefix("{"), s.hasSuffix("}") else { return nil }
        let inner = s.dropFirst().dropLast().split(separator: ".", maxSplits: 1, omittingEmptySubsequences: false)
        guard inner.count == 2, let k = Kind(rawValue: String(inner[0])), PanelValidator.isName(String(inner[1])) else { return nil }
        return (k, String(inner[1]))
    }

    /// nil when a segment is neither a plain name ([A-Za-z0-9_-]) nor exactly one placeholder, or the path is not "/a/b".
    public static func parse(_ p: String) -> [Seg]? {
        guard p.utf8.count <= 200, p.hasPrefix("/"), !p.hasSuffix("/") else { return nil }
        var out: [Seg] = []
        for s in p.dropFirst().split(separator: "/", omittingEmptySubsequences: false).map(String.init) {
            if let (k, n) = placeholder(s) { out.append(.ref(k, n)) }
            else if PanelValidator.m(s, #"^[A-Za-z0-9_-]{1,64}\z"#) { out.append(.literal(s)) }
            else { return nil }
        }
        return out
    }

    /// A value that may become part of a path or a query. Empty, ".", anything with "..", "/", "\", "?", "#" or a control
    /// character is refused; everything else is percent-encoded (nothing but unreserved characters stays as it is).
    public static func encode(_ v: String) -> String? {
        guard !v.isEmpty, v.unicodeScalars.count <= 256, v != ".", !v.contains(".."), !PanelValidator.hasControl(v) else { return nil }
        guard !v.contains(where: { "/\\?#".contains($0) }) else { return nil }
        return v.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"))
    }

    /// A paging cursor is a query VALUE, not a path segment: any text without control characters, percent-encoded (so a "/", "?",
    /// "&" or ".." in it is data); nil when empty, over 512 characters or with a control character.
    public static func encodeQuery(_ v: String) -> String? {
        guard !v.isEmpty, v.unicodeScalars.count <= 512, !PanelValidator.hasControl(v) else { return nil }
        return v.addingPercentEncoding(withAllowedCharacters: CharacterSet(charactersIn: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"))
    }

    /// The values the placeholders take: `row` (the list's row), `parent` (the row of the opened detail page), `field` (the form).
    public struct Values: Sendable {
        public var row: [String: PanelJSON] = [:], parent: [String: PanelJSON] = [:], field: [String: PanelJSON] = [:]
        public init(row: [String: PanelJSON] = [:], parent: [String: PanelJSON] = [:], field: [String: PanelJSON] = [:]) { self.row = row; self.parent = parent; self.field = field }
        func get(_ k: Kind, _ n: String) -> PanelJSON? { switch k { case .row: row[n]; case .parent: parent[n]; case .field: field[n] } }
    }

    public static func resolve(path p: String, _ v: Values) -> String? {
        guard let segs = parse(p) else { return nil }
        var out = ""
        for s in segs {
            switch s {
            case .literal(let l): out += "/" + l
            case .ref(let k, let n):
                guard let t = v.get(k, n)?.scalarText, let e = encode(t) else { return nil }
                out += "/" + e
            }
        }
        return out
    }

    public static func resolve(query q: String, _ v: Values) -> String? {
        var out: [String] = []
        for part in q.split(separator: "&", omittingEmptySubsequences: false) {
            guard let eq = part.firstIndex(of: "=") else { return nil }
            let value = String(part[part.index(after: eq)...])
            if let (k, n) = placeholder(value) {
                guard let t = v.get(k, n)?.scalarText, let e = encode(t) else { return nil }
                out.append(part[..<eq] + "=" + e)
            } else { out.append(String(part)) }
        }
        return out.joined(separator: "&")
    }

    /// The registered pattern a path template resolves to, "METHOD users/{}".
    public static func route(_ method: String, _ p: String) -> String? {
        parse(p).map { method + " " + $0.map { if case .literal(let l) = $0 { l } else { "{}" } }.joined(separator: "/") }
    }
}
