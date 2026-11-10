import Foundation

/// minigw plugin install|uninstall (0.5.20): vtičnik za Claude Code v ~/.claude/settings.json. Enaka pravila kot
/// odjemalec Go (client/plugin.go) in prej postflight v Casks/minigw.rb: dodano le, česar še ni (uporabnikov »false«
/// ostane), odstranjeno le, kar doda add; neveljaven JSON se ne prepiše.
public enum PluginSetup {
    public static let id = "minigw@mini-gateway"
    public static let market = "mini-gateway"
    /// #63: veja release (premika jo le skripta za izdajo) in autoUpdate: Claude Code tretjih marketplace sam ne posodablja.
    public static let marketplace: [String: Any] = ["source": ["source": "github", "repo": "gh-commiter/mini-llm-gateway", "ref": "release"], "autoUpdate": true]
    /// Vnos do 0.5.21 (brez ref in autoUpdate).
    public static let oldMarketplace: [String: Any] = ["source": ["source": "github", "repo": "gh-commiter/mini-llm-gateway"]]

    public struct Failure: Error {
        public let message: String
        public init(message: String) { self.message = message }
    }

    /// L3: mapa ali datoteka ni zapisljiva za druge, je last tega uporabnika ali root, za skupino pa le, če ji uporabnik
    /// zaupa (trustedGroup; Homebrew: /opt/homebrew/bin je drwxrwxr-x <user>:admin). Kot Go safeDir.
    public static func safePath(_ path: String) -> Bool {
        var st = stat()
        return stat(path, &st) == 0 && st.st_mode & 0o002 == 0 && (st.st_uid == getuid() || st.st_uid == 0)
            && (st.st_mode & 0o020 == 0 || trustedGroup(st.st_gid))
    }

    /// #48: vse nadrejene mape poti do / (kot je podana in prava pot) po pravilih safePath: kdor lahko zamenja katero koli,
    /// zamenja minigw. Višje mape (nad neposredno) smejo biti zapisljive le z bitom sticky (/tmp): tam drugi ne
    /// zamenjajo našega vnosa. Kot Go safePath.
    public static func safeAncestors(_ path: String) -> Bool {
        guard let real = realpath(path, nil) else { return false }
        defer { free(real) }
        for start in [path, String(cString: real)] {
            var d = start, strict = true
            repeat {
                d = (d as NSString).deletingLastPathComponent
                var st = stat()
                guard stat(d, &st) == 0, safePath(d) || !strict && stickyOK(st) else { return false }
                strict = false
            } while d != "/" && !d.isEmpty
        }
        return true
    }

    /// Višja mapa, zapisljiva za druge: le s sticky in v lasti root ali tega uporabnika (sicer jo lastnik zamenja).
    static func stickyOK(_ st: stat) -> Bool { st.st_mode & S_ISVTX != 0 && (st.st_uid == 0 || st.st_uid == getuid()) }

    /// macOS: le admin (80); staff (20) je vsak lokalni uporabnik (kot Go trustedGroup).
    public static func trustedGroup(_ gid: gid_t) -> Bool { gid == 80 }

    /// Le JSON true (ne 1).
    public static func isTrue(_ v: Any?) -> Bool {
        guard let n = v as? NSNumber else { return false }
        return CFGetTypeID(n) == CFBooleanGetTypeID() && n.boolValue
    }

    /// changed: kaj dodano; updated: le naš stari vnos (brez autoUpdate) zamenjan z novim (uporabnikov spremenjeni
    /// vnos ostane); off: vtičnik je tam izklopljen (uporabnikov false). Kot Go pluginAdd.
    public static func add(_ s: inout [String: Any]) throws -> (changed: Bool, updated: Bool, off: Bool) {
        func object(_ k: String) throws -> [String: Any] {
            switch s[k] {
            case nil, is NSNull: return [:]
            case let m as [String: Any]: return m
            default: throw Failure(message: "\(k) is not a JSON object")
            }
        }
        var m = try object("extraKnownMarketplaces"), e = try object("enabledPlugins"), changed = false, updated = false
        if m[market] == nil || m[market] is NSNull {
            m[market] = marketplace; changed = true
        } else if (m[market] as? NSDictionary)?.isEqual(to: oldMarketplace) == true {
            m[market] = marketplace; updated = true
        }
        if e[id] == nil { e[id] = true; changed = true }
        s["extraKnownMarketplaces"] = m
        s["enabledPlugins"] = e
        return (changed, updated && !changed, !isTrue(e[id]))
    }

    /// Naš vnos (nov ali do 0.5.21). autoUpdate le JSON true: NSDictionary.isEqual bi sprejel tudi 1 (kot Go DeepEqual).
    public static func ours(_ v: Any?) -> Bool {
        guard let d = v as? NSDictionary else { return false }
        return d.isEqual(to: oldMarketplace) || d.isEqual(to: marketplace) && isTrue(d["autoUpdate"])
    }

    public static func remove(_ s: inout [String: Any]) -> (changed: Bool, off: Bool) {
        var changed = false, off = false
        if var e = s["enabledPlugins"] as? [String: Any] {
            if isTrue(e[id]) { e[id] = nil; s["enabledPlugins"] = e; changed = true }
            off = e[id] != nil
        }
        if var m = s["extraKnownMarketplaces"] as? [String: Any], ours(m[market]) {
            m[market] = nil
            s["extraKnownMarketplaces"] = m
            changed = true
        }
        return (changed, off)
    }

    /// JSONSerialization sprejme vejico pred } ali ]; Go (in Claude Code) ne: zavrnemo jo enako.
    static func trailingComma(_ data: Data) -> Bool {
        var inString = false, escaped = false, comma = false
        for b in data {
            if inString {
                if escaped { escaped = false } else if b == 0x5c { escaped = true } else if b == 0x22 { inString = false }
                continue
            }
            switch b {
            case 0x22: inString = true; comma = false
            case 0x2c: comma = true
            case 0x7d, 0x5d: if comma { return true }
            case 0x20, 0x09, 0x0a, 0x0d: break
            default: comma = false
            }
        }
        return false
    }

    /// edit: settings.json skozi povezavo (dotfiles), atomarno in z isto pravico (lahko ima ključe v env; nova 0600).
    public static func edit(_ path: String, _ f: (inout [String: Any]) throws -> Bool) throws {
        let real = URL(fileURLWithPath: path).resolvingSymlinksInPath().path
        var s: [String: Any] = [:], mode = 0o600
        if let data = FileManager.default.contents(atPath: real) {
            guard !trailingComma(data), let o = try? JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed]) else {
                throw Failure(message: "\(path) is not valid JSON: not changed (fix it, or add the plugin in Claude Code: /plugin install \(id))")
            }
            guard let d = o as? [String: Any] else { throw Failure(message: "\(path) is not a JSON object: not changed") }
            s = d
            mode = (try? FileManager.default.attributesOfItem(atPath: real)[.posixPermissions] as? Int) ?? 0o600
        } else if FileManager.default.fileExists(atPath: real) {
            throw Failure(message: "cannot read \(path)")
        }
        do {
            guard try f(&s) else { return }
        } catch let e as Failure {
            throw Failure(message: "\(path): \(e.message): not changed")
        }
        var out = try JSONSerialization.data(withJSONObject: s, options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes])
        out.append(0x0a)
        try? FileManager.default.createDirectory(atPath: (real as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
        let tmp = real + ".minigw-tmp"
        unlink(tmp)
        let fd = open(tmp, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, mode_t(mode))
        guard fd >= 0 else { throw Failure(message: "cannot write \(tmp): \(String(cString: strerror(errno)))") }
        let ok = out.withUnsafeBytes { write(fd, $0.baseAddress, $0.count) } == out.count && fchmod(fd, mode_t(mode)) == 0 && fsync(fd) == 0
        close(fd)
        guard ok, rename(tmp, real) == 0 else {
            unlink(tmp)
            throw Failure(message: "cannot write \(path)")
        }
    }
}
