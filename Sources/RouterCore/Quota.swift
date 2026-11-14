import Foundation

// Quota and provider credit for the owner (admin GET /quota, minigw gw quota): a port of client/serve_quota.go. Every value says where it
// comes from (source), when it was observed, when its window resets and whether it is stale. Sources: the last rate_limits record in the
// session log of a Codex run this gateway started; `claude -p /usage` (OPT-IN, off by default); the documented endpoints of OpenRouter and
// LiteLLM with the key the gateway already holds. Nothing here is billed per request, nothing is shown to members, and no key or token is
// ever in a value or a log line. The parsers are proven against Go through client/testdata/quota-parsers.json (QuotaTests.swift).

struct QuotaError: Error, CustomStringConvertible {
    let description: String
    init(_ d: String) { description = d }
}

struct QuotaValue: Equatable, Sendable {
    var name: String
    var unit = ""
    var value: Double?
    var source: String
    var observedAt: Date?
    var resetsAt: Date?
    var resetsText = ""
    var stale = false
    var note = ""
    var reason = ""

    /// The JSON of Go's quotaValue.
    var json: [String: Any] {
        var o: [String: Any] = ["name": name, "unit": unit, "source": source, "stale": stale]
        if let value { o["value"] = value }
        if let observedAt { o["observedAt"] = QuotaTime.format(observedAt) }
        if let resetsAt { o["resetsAt"] = QuotaTime.format(resetsAt) }
        if !resetsText.isEmpty { o["resetsText"] = resetsText }
        if !note.isEmpty { o["note"] = note }
        if !reason.isEmpty { o["reason"] = reason }
        return o
    }

    static func unavailable(_ name: String, _ reason: String) -> QuotaValue { QuotaValue(name: name, source: "unavailable", stale: true, reason: reason) }
}

enum QuotaTime {
    /// time.Time's JSON in UTC (RFC 3339 with the fraction only when there is one, trailing zeros dropped).
    static func format(_ d: Date) -> String {
        let t = d.timeIntervalSince1970
        var whole = t.rounded(.down)
        var micros = Int(((t - whole) * 1e6).rounded())  // a Date holds about 0.2 microseconds of a 2026 time: microseconds are what it keeps (Go writes nanoseconds)
        if micros >= 1_000_000 { whole += 1; micros = 0 }
        var frac = micros == 0 ? "" : "." + String(format: "%06d", micros)
        while frac.hasSuffix("0") { frac.removeLast() }
        var c = Calendar(identifier: .gregorian)
        c.timeZone = TimeZone(identifier: "UTC")!
        let p = c.dateComponents([.year, .month, .day, .hour, .minute, .second], from: Date(timeIntervalSince1970: whole))
        return String(format: "%04d-%02d-%02dT%02d:%02d:%02d", p.year!, p.month!, p.day!, p.hour!, p.minute!, p.second!) + frac + "Z"
    }

    /// RFC 3339 with any number of fractional digits (Go writes nanoseconds); nil when it is none.
    static func parse(_ s: String) -> Date? {
        guard let m = s.wholeMatch(of: #/([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})/#) else { return nil }
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        guard let base = f.date(from: String(m.1) + String(m.3)) else { return nil }
        return base.addingTimeInterval(m.2.flatMap { Double("0" + $0) } ?? 0)
    }
}

// MARK: parsers

enum QuotaParse {
    /// A JSON number (not a boolean).
    static func number(_ x: Any?) -> Double? {
        guard let n = x as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID(), n.doubleValue.isFinite else { return nil }
        return n.doubleValue
    }

    /// The same bounds as Go (serve_quota.go): a reset beyond year 9999 is no time, and seconds are bounded so that no conversion can trap or wrap.
    static let maxEpoch = 253_402_300_799.0
    static let maxResetIn = 1e9

    /// The first value of `key` found in the document (maps in key order: Go's map walk is random, the Codex lines have one).
    static func findKey(_ v: Any, _ key: String) -> Any? {
        switch v {
        case let m as [String: Any]:
            if let r = m[key] { return r }
            for k in m.keys.sorted() { if let r = findKey(m[k]!, key) { return r } }
        case let a as [Any]:
            for c in a { if let r = findKey(c, key) { return r } }
        default: break
        }
        return nil
    }

    /// The rate_limits object of one session-log line: primary (5 hours) and secondary (the week); resets_at is epoch seconds, or
    /// resets_in_seconds from the observation.
    static func codexRateLimits(_ line: Data, observed: Date) -> [QuotaValue] {
        guard let v = try? JSONSerialization.jsonObject(with: line, options: [.fragmentsAllowed]), let m = findKey(v, "rate_limits") as? [String: Any] else { return [] }
        var out: [QuotaValue] = []
        for (key, fallback) in [("primary", "5h"), ("secondary", "week")] {
            guard let x = m[key] as? [String: Any], let used = number(x["used_percent"]) else { continue }
            var name = fallback
            if let mins = number(x["window_minutes"]), mins > 0, mins < 1500 {
                name = String(Int(mins / 60)) + "h"
            } else if let mins = number(x["window_minutes"]), mins >= 1500 {
                name = mins < 10000 ? String(Int(mins / 1440)) + "d" : "week"
            }
            var q = QuotaValue(name: "codex." + name, unit: "percent", value: used, source: "codex-session-log", observedAt: observed)
            if let at = number(x["resets_at"]), at > 0, at <= maxEpoch {
                q.resetsAt = Date(timeIntervalSince1970: Double(Int64(at)))
            } else if let inSec = number(x["resets_in_seconds"]), inSec >= 0, inSec <= maxResetIn {
                q.resetsAt = observed.addingTimeInterval(Double(Int64(inSec)))
            }
            out.append(q)
        }
        return out
    }

    /// The newest rate_limits record in the last 256 KiB of a session log (a path: opened without following a link, see openRegular).
    static func lastRateLimits(path: String, observed: Date) -> [QuotaValue] {
        guard let fd = openRegular(path) else { return [] }
        defer { close(fd) }
        return lastRateLimits(fd: fd, observed: observed)
    }

    /// The same through an open descriptor: only the last 256 KiB are read (pread), however large the file is.
    static func lastRateLimits(fd: Int32, observed: Date) -> [QuotaValue] {
        var st = stat()
        guard fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFREG else { return [] }
        let tail = Int64(256 << 10), off = max(0, Int64(st.st_size) - tail)
        let want = Int(min(tail, Int64(st.st_size) - off))
        var buf = [UInt8](repeating: 0, count: want)
        var got = 0
        while got < want {
            let n = buf.withUnsafeMutableBytes { pread(fd, $0.baseAddress! + got, want - got, off + Int64(got)) }
            if n <= 0 { break }
            got += n
        }
        let marker = Data(#""rate_limits""#.utf8)
        for line in Data(buf.prefix(got)).split(separator: 0x0a, omittingEmptySubsequences: false).reversed() where line.range(of: marker) != nil {
            let vals = codexRateLimits(Data(line), observed: observed)
            if !vals.isEmpty { return vals }
        }
        return []
    }

    /// A regular file of the gateway's user, opened WITHOUT following a link (a link to /dev/zero or to another file is refused) and without
    /// blocking (a FIFO is refused); nil otherwise. The caller closes the descriptor.
    static func openRegular(_ path: String, at dir: Int32 = AT_FDCWD) -> Int32? {
        let fd = openat(dir, path, O_RDONLY | O_NOFOLLOW | O_NONBLOCK | O_CLOEXEC)
        guard fd >= 0 else { return nil }
        var st = stat()
        guard fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFREG, st.st_uid == getuid() else { close(fd); return nil }
        return fd
    }

    /// Entries scanned per folder and in all: a planted folder of millions of names costs a bounded amount.
    static let maxEntries = 4096, maxScanned = 20000

    /// The names in a folder given by a descriptor (at most `cap`), nil when it is no folder of ours.
    private static func names(_ fd: Int32, cap: Int) -> [String]? {
        var st = stat()
        guard fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFDIR, st.st_uid == getuid(), let d = fdopendir(dup(fd)) else { return nil }
        defer { closedir(d) }
        var out: [String] = []
        while out.count < cap, let e = readdir(d) {
            let n = withUnsafeBytes(of: e.pointee.d_name) { String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) }
            if n != "." && n != ".." { out.append(n) }
        }
        return out.sorted()
    }

    private static func openDir(_ name: String, in parent: Int32) -> Int32? {
        let fd = openat(parent, name, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        return fd >= 0 ? fd : nil
    }

    /// The log of THIS run's thread: <home>/sessions/YYYY/MM/DD/rollout-<time>-<thread id>.jsonl (the newest, if several), as an open descriptor
    /// (the caller closes it). Every folder level is opened relative to the one above it without following links, so a linked year, month or
    /// day folder (a project CODEX_HOME is a place an agent run has touched) is not entered, and only files of the user are taken.
    static func codexSessionLog(home: String, thread: String) -> Int32? {
        guard !thread.isEmpty, !thread.contains(where: { "*?[]/\\".contains($0) }) else { return nil }
        var scanned = 0
        var best: (fd: Int32, at: timespec)?
        func newer(_ a: timespec, _ b: timespec) -> Bool { a.tv_sec != b.tv_sec ? a.tv_sec > b.tv_sec : a.tv_nsec > b.tv_nsec }
        let root = open(home + "/sessions", O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard root >= 0 else { return nil }
        defer { close(root) }
        func walk(_ fd: Int32, _ depth: Int) {
            guard let list = names(fd, cap: maxEntries) else { return }
            scanned += list.count
            for n in list where scanned <= maxScanned {
                if depth < 3 {
                    if let sub = openDir(n, in: fd) { walk(sub, depth + 1); close(sub) }
                } else if n.hasPrefix("rollout-"), n.hasSuffix("-" + thread + ".jsonl"), let f = openRegular(n, at: fd) {
                    var st = stat()
                    if fstat(f, &st) == 0, best == nil || newer(st.st_mtimespec, best!.at) {
                        if let old = best { close(old.fd) }
                        best = (f, st.st_mtimespec)
                    } else { close(f) }
                }
            }
        }
        walk(root, 0)
        return best?.fd
    }

    /// The numbers of the newest log of this run's thread.
    static func lastRateLimits(home: String, thread: String, observed: Date) -> [QuotaValue] {
        guard let fd = codexSessionLog(home: home, thread: thread) else { return [] }
        defer { close(fd) }
        return lastRateLimits(fd: fd, observed: observed)
    }

    static let usageLine = try! NSRegularExpression(
        pattern: #"^[\t\n\f\r ]*(Current [^:\n]+?):[\t\n\f\r ]*([0-9]+(?:\.[0-9]+)?)% used(?:[\t\n\f\r ]*[·|-][\t\n\f\r ]*resets[\t\n\f\r ]+(.+?))?[\t\n\f\r ]*$"#,
        options: [.anchorsMatchLines])

    static let errInference = "claude /usage ran inference; it is switched off until the gateway restarts"

    /// The result text of `claude -p /usage --output-format json`. The envelope is checked too: the command must have done no inference
    /// (0 turns, no cost, no tokens), else the result is refused (QuotaError(errInference)).
    static func claudeUsage(_ data: Data, now: Date, zone: TimeZone = .current) throws -> [QuotaValue] {
        let top = try? JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed])
        guard let env = top as? [String: Any] ?? (top is NSNull ? [:] : nil) else { throw QuotaError("unexpected output") }  // Go: null decodes into an empty struct
        func int(_ k: String) throws -> Int64 {  // Go decodes into int fields: a fraction or a string is "unexpected output"
            guard let x = env[k], !(x is NSNull) else { return 0 }
            guard let n = x as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID(), n.doubleValue == Double(n.int64Value) else { throw QuotaError("unexpected output") }
            return n.int64Value
        }
        let usage = env["usage"] as? [String: Any] ?? [:]
        if let x = env["usage"], !(x is NSNull), !(x is [String: Any]) { throw QuotaError("unexpected output") }
        func intIn(_ k: String) throws -> Int64 {
            guard let x = usage[k], !(x is NSNull) else { return 0 }
            guard let n = x as? NSNumber, CFGetTypeID(n) != CFBooleanGetTypeID(), n.doubleValue == Double(n.int64Value) else { throw QuotaError("unexpected output") }
            return n.int64Value
        }
        if let r = env["result"], !(r is NSNull), !(r is String) { throw QuotaError("unexpected output") }
        if let c = env["total_cost_usd"], !(c is NSNull), number(c) == nil { throw QuotaError("unexpected output") }
        if let e = env["is_error"], !(e is NSNull), !((e as? NSNumber).map { CFGetTypeID($0) == CFBooleanGetTypeID() } ?? false) { throw QuotaError("unexpected output") }
        let turns = try int("num_turns"), inTok = try intIn("input_tokens"), outTok = try intIn("output_tokens")
        if turns != 0 || (number(env["total_cost_usd"]) ?? 0) != 0 || inTok != 0 || outTok != 0 { throw QuotaError(errInference) }
        if (env["is_error"] as? NSNumber)?.boolValue == true { throw QuotaError("claude reported an error") }
        let text = env["result"] as? String ?? ""
        var out: [QuotaValue] = []
        let ns = text as NSString
        for m in usageLine.matches(in: text, range: NSRange(location: 0, length: ns.length)) {
            let used = Double(ns.substring(with: m.range(at: 2))) ?? 0
            guard used.isFinite else { continue }  // 400 digits parse as infinity, which no JSON can carry
            var name = ns.substring(with: m.range(at: 1)).lowercased()
            if name.hasPrefix("current ") { name.removeFirst(8) }
            name = "claude." + slug(name)
            var q = QuotaValue(name: name, unit: "percent", value: used, source: "claude-usage", observedAt: now)
            if m.range(at: 3).location != NSNotFound {
                let when = ns.substring(with: m.range(at: 3))
                if let t = claudeReset(when, now: now, zone: zone) { q.resetsAt = t } else { q.resetsText = when }
            }
            out.append(q)
        }
        if out.isEmpty { throw QuotaError("no usage lines in the output (another Claude Code version?)") }
        return out
    }

    /// strings.NewReplacer(" (", ".", ")", "", " ", "-"): the first pair that matches at a position wins.
    private static func slug(_ s: String) -> String {
        var o = "", rest = Substring(s)
        while let c = rest.first {
            if rest.hasPrefix(" (") { o += "."; rest = rest.dropFirst(2) }
            else if c == ")" { rest = rest.dropFirst() }
            else if c == " " { o += "-"; rest = rest.dropFirst() }
            else { o.append(c); rest = rest.dropFirst() }
        }
        return o
    }

    private static let months = ["jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"]

    /// "Nov 19 at 12am (Europe/Ljubljana)", "Nov 19 at 3:30pm (UTC)", "Nov 19, 2026 at 3pm" (a run of spaces counts as one, as in Go's Parse): the named zone (Foundation's TimeZone), else
    /// `zone`; a year-less date is the next one at or after yesterday. nil = not understood (the text is shown as it is).
    static func claudeReset(_ text: String, now: Date, zone: TimeZone = .current) -> Date? {
        var s = text, loc = zone
        if let r = s.range(of: " (", options: .backwards), s.hasSuffix(")") {
            let name = String(s[r.upperBound...].dropLast())
            switch name {
            case "", "UTC": loc = TimeZone(identifier: "UTC")!
            case "Local": loc = zone
            default:
                guard let z = TimeZone(identifier: name) else { return nil }
                loc = z
            }
            s = String(s[..<r.lowerBound])
        }
        guard let m = s.wholeMatch(of: #/([A-Za-z]{3}) +([0-9]{1,2})(?:, +([0-9]{4}))? +at +([0-9]{1,2})(?::([0-9]{2}))?(am|pm)/#),
              let month = months.firstIndex(of: m.1.lowercased()).map({ $0 + 1 }), let day = Int(m.2), var hour = Int(m.4), hour <= 12 else { return nil }
        let minute = m.5.flatMap { Int($0) } ?? 0
        guard minute < 60 else { return nil }
        if m.6 == "pm" && hour < 12 { hour += 12 } else if m.6 == "am" && hour == 12 { hour = 0 }
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = loc
        var utc = Calendar(identifier: .gregorian)
        utc.timeZone = TimeZone(identifier: "UTC")!
        func daysIn(_ y: Int, _ mo: Int) -> Int { [31, (y % 4 == 0 && (y % 100 != 0 || y % 400 == 0)) ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][mo - 1] }
        /// Go's Date: a day beyond the month carries into the next one.
        func date(_ y: Int, _ mo: Int, _ d: Int) -> Date? {
            guard let first = cal.date(from: DateComponents(year: y, month: mo, day: 1, hour: hour, minute: minute)) else { return nil }
            return cal.date(byAdding: .day, value: d - 1, to: first)
        }
        if let ys = m.3 {
            guard let y = Int(ys), day >= 1, day <= daysIn(y, month) else { return nil }
            return date(y, month, day)
        }
        guard day >= 1, day <= daysIn(0, month) else { return nil }  // Go parses a year-less date in the year 0 (a leap year)
        guard var t = date(utc.component(.year, from: now), month, day) else { return nil }
        if t < now.addingTimeInterval(-86400) {  // AddDate(1, 0, 0)
            let c = cal.dateComponents([.year, .month, .day], from: t)
            guard let n = date(c.year! + 1, c.month!, c.day!) else { return nil }
            t = n
        }
        return t
    }

    /// {"data":{"total_credits":…,"total_usage":…}}: both zero is an unexpected answer.
    static func openRouterCredits(_ data: Data, now: Date) throws -> [QuotaValue] {
        guard let o = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else { throw QuotaError("unexpected answer") }
        var d: [String: Any] = [:]
        if let x = o["data"], !(x is NSNull) { guard let m = x as? [String: Any] else { throw QuotaError("unexpected answer") }; d = m }
        func f(_ k: String) throws -> Double {
            guard let x = d[k], !(x is NSNull) else { return 0 }
            guard let n = number(x) else { throw QuotaError("unexpected answer") }
            return n
        }
        let total = try f("total_credits"), used = try f("total_usage")
        if (total == 0 && used == 0) || !(total - used).isFinite { throw QuotaError("unexpected answer") }
        return [QuotaValue(name: "openrouter.credits-remaining", unit: "usd", value: total - used, source: "openrouter-api", observedAt: now),
                QuotaValue(name: "openrouter.credits-total", unit: "usd", value: total, source: "openrouter-api", observedAt: now)]
    }

    /// {"info":{"spend":…,"max_budget":…,"budget_reset_at":…}}: the spend must be there.
    static func liteLLMKeyInfo(_ data: Data, now: Date) throws -> [QuotaValue] {
        guard let o = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else { throw QuotaError("unexpected answer") }
        var info: [String: Any] = [:]
        if let x = o["info"], !(x is NSNull) { guard let m = x as? [String: Any] else { throw QuotaError("unexpected answer") }; info = m }
        func opt(_ k: String) throws -> Double? {
            guard let x = info[k], !(x is NSNull) else { return nil }
            guard let n = number(x) else { throw QuotaError("unexpected answer") }
            return n
        }
        let spend = try opt("spend"), budget = try opt("max_budget")
        if let r = info["budget_reset_at"], !(r is NSNull), !(r is String) { throw QuotaError("unexpected answer") }
        guard let spend else { throw QuotaError("unexpected answer") }
        let resets = (info["budget_reset_at"] as? String).flatMap(QuotaTime.parse)
        var out = [QuotaValue(name: "litellm.spend", unit: "usd", value: spend, source: "litellm-api", observedAt: now, resetsAt: resets)]
        if let budget { out.append(QuotaValue(name: "litellm.max-budget", unit: "usd", value: budget, source: "litellm-api", observedAt: now, resetsAt: resets)) }
        return out
    }
}

/// An observation older than a day, or one whose window has reset since, is stale.
func quotaMarkStale(_ vals: [QuotaValue], now: Date) -> [QuotaValue] {
    vals.map { v in
        var v = v
        if let obs = v.observedAt { v.stale = now.timeIntervalSince(obs) > 24 * 3600 || (v.resetsAt.map { now > $0 } ?? false) }
        return v
    }
}
