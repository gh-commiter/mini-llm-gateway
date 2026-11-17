import Foundation

enum SessionHistory {
    static func collect(_ fetch: (String) throws -> [String: Any]) throws -> [String: Any] {
        var rows: [[String: Any]] = [], after = "", seen = Set<String>()
        repeat {
            let page = try fetch(after)
            guard let part = page["sessions"] as? [[String: Any]] else { throw ProjectError("the sessions answer is not understood") }
            rows += part
            after = page["next"] as? String ?? ""
            if !after.isEmpty && !seen.insert(after).inserted { throw ProjectError("repeated session page") }
        } while !after.isEmpty
        return ["sessions": rows]
    }

    static func text(_ page: [String: Any]) -> String {
        let rows = page["sessions"] as? [[String: Any]] ?? []
        if rows.isEmpty { return "no sessions\n" }
        let stamp = DateFormatter()
        stamp.locale = Locale(identifier: "en_US_POSIX")
        stamp.timeZone = TimeZone(secondsFromGMT: 0)
        stamp.dateFormat = "yyyy-MM-dd'T'HH:mm:ss'Z'"
        func metric(_ d: JDict, _ key: String) -> String {
            let m = d[key]?.dict ?? JDict()
            guard let n = m["value"]?.int else { return "unknown" }
            return String(n) + ((m["unknownEvents"]?.int ?? 0) > 0 ? "+?" : "")
        }
        return rows.map { row in
            let data = (try? JSONSerialization.data(withJSONObject: row)) ?? Data()
            let d = JSONValue.parse(data)?.dict ?? JDict()
            func s(_ k: String) -> String { d[k]?.string ?? "unknown" }
            func time(_ k: String) -> String { d[k]?.string.flatMap(ProjectText.parseTime).map(stamp.string) ?? "unknown" }
            let partial = (d["eventsWithoutCost"]?.int ?? 0) > 0 && d["costUsd"]?.string != nil ? "+?" : ""
            let models = (d["models"]?.array ?? []).compactMap(\.string).joined(separator: ",")
            let duration = (d["durationMs"]?.int).map { "\($0)ms" } ?? "unknown"
            var labels = ""
            for (name, key, src) in [("phase", "phase", ""), ("activity", "activity", ""), ("task", "task", "taskSource")] {
                if let v = d[key]?.string, !v.isEmpty { labels += "  " + name + " " + v + (src.isEmpty ? "" : (d[src]?.string == "branch" ? " (branch)" : "")) }
            }
            return ProjectText.clean("\(s("id"))  \(time("start"))  \(s("userName"))  \(s("deviceName"))  \(models)  \(s("outcome"))  \(d["requests"]?.int ?? 0) requests  \(metric(d,"tokensIn")) in  \(metric(d,"tokensOut")) out  \(metric(d,"cacheReadTokens")) cache-read  \(metric(d,"cacheWriteTokens")) cache-write  USD \(s("costUsd"))\(partial) (reported \(s("reportedCostUsd")), estimated \(s("estimatedCostUsd")), \(d["eventsWithoutCost"]?.int ?? 0) unknown)  \(d["projectId"]?.int ?? 0)  end \(time("end"))  wall \(duration)\(labels)") + "\n"
        }.joined()
    }

    static func json(_ page: [String: Any]) throws -> String {
        String(decoding: try JSONSerialization.data(withJSONObject: page, options: [.sortedKeys, .withoutEscapingSlashes]), as: UTF8.self)
    }
}
