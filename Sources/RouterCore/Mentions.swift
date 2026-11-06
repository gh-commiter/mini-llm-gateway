import Foundation
import FoundationModels

/// Datoteke, omenjene v vprašanju za Siri (ki sama ne bere map): Apple FM izlušči imena, router jih poišče
/// v mapi seje in vsebino doda vprašanju. Vse ostane na tem Macu.
enum Mentions {
    @Generable
    struct Names {
        @Guide(description: "File or folder names mentioned in the text, exactly as written (for example CLAUDE.md, docs/kb, README). Empty if none.")
        var names: [String]
    }

    /// Imena s končnico ali potjo (rezerva, če Apple FM ni na voljo) + imena po Apple FM.
    static func names(in text: String) async -> [String] {
        let pattern = try! NSRegularExpression(pattern: "(?:\\./)?[\\w.-]+(?:/[\\w.-]+)*\\.[A-Za-z0-9]{1,6}\\b")
        var found = pattern.matches(in: text, range: NSRange(text.startIndex..., in: text))
            .compactMap { Range($0.range, in: text).map { String(text[$0]) } }
        if case .available = SystemLanguageModel.default.availability {
            let session = LanguageModelSession(instructions: "You list file and folder names that the user's text mentions. Never invent names.")
            if let r = try? await session.respond(to: String(text.prefix(4000)), generating: Names.self) { found += r.content.names }
        }
        var seen = Set<String>()
        return found.map { $0.trimmingCharacters(in: CharacterSet(charactersIn: " `'\"@")) }
            .filter { !$0.isEmpty && $0.count < 200 && seen.insert($0.lowercased()).inserted }
    }

    static let skipDirs: Set<String> = ["node_modules", "build", "dist", "target", "DerivedData", "Pods", "vendor"]

    /// Ujemanje po imenu (brez razlike v velikih črkah, tudi brez končnice) ali po delu poti; največ 4 ravni.
    /// Poleg imen iz vprašanja (`names`) tudi datoteke in mape, katerih ime je v `question` cela beseda (»kb«, »zapiski«).
    static func find(_ names: [String], question: String = "", in root: URL, limit: Int = 8) -> [URL] {
        let wanted = names.map { $0.lowercased().replacingOccurrences(of: "./", with: "") }
        let words = Set(question.lowercased().components(separatedBy: CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "._-")).inverted)
            .filter { $0.count >= 2 })
        guard !wanted.isEmpty || !words.isEmpty, let e = FileManager.default.enumerator(at: root, includingPropertiesForKeys: [.isDirectoryKey],
                                                                       options: [.skipsHiddenFiles, .skipsPackageDescendants]) else { return [] }
        var hits: [URL] = []
        let base = root.standardizedFileURL.path + "/"
        for case let url as URL in e {
            if e.level > 4 { e.skipDescendants(); continue }
            if skipDirs.contains(url.lastPathComponent) { e.skipDescendants(); continue }
            let rel = url.standardizedFileURL.path.replacingOccurrences(of: base, with: "").lowercased()
            let name = url.lastPathComponent.lowercased(), stem = url.deletingPathExtension().lastPathComponent.lowercased()
            if wanted.contains(where: { $0 == name || $0 == stem || $0 == rel || rel.hasSuffix("/" + $0) })
                || words.contains(name) || words.contains(stem) {
                hits.append(url)
                if hits.count >= limit { break }
            }
        }
        return hits
    }

    /// Vsebina besedilnih datotek (mapa → njene datoteke na prvi ravni), skupaj največ `budget` bajtov.
    static let header = "The user attached these files from their project. Answer only from them; do not search elsewhere.\n\n"

    static func attachments(_ urls: [URL], root: URL, budget: Int = 16_000) -> String {
        var files: [URL] = []
        for u in urls {
            var isDir: ObjCBool = false
            FileManager.default.fileExists(atPath: u.path, isDirectory: &isDir)
            if isDir.boolValue {
                files += ((try? FileManager.default.contentsOfDirectory(at: u, includingPropertiesForKeys: nil, options: .skipsHiddenFiles)) ?? [])
                    .sorted { $0.path < $1.path }.prefix(10)
            } else { files.append(u) }
        }
        var out = "", left = budget
        let base = root.standardizedFileURL.path + "/"
        for f in files where left > 200 {
            guard let d = FileManager.default.contents(atPath: f.path), !d.prefix(4096).contains(0),
                  let text = String(data: d, encoding: .utf8) else { continue }  // le besedilo (brez binarnih)
            let rel = f.standardizedFileURL.path.replacingOccurrences(of: base, with: "")
            let part = String(text.prefix(left))
            out += "Attached file \(rel)\(part.count < text.count ? " (truncated)" : ""):\n```\n\(part)\n```\n\n"
            left -= part.utf8.count
        }
        return out.isEmpty ? "" : header + out
    }
}
