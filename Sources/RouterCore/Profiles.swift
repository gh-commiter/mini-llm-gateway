import CryptoKit
import Foundation
import Network

/// Profili v minigw (#17): en odjemalec, več prehodov. Pripona modela @<profil> izbere prehod; /v1/models združi
/// sezname vseh dosegljivih (privzeti profil še brez pripone). Enako kot odjemalec Go (client/gateways.go).
public enum Profiles {
    public static let defaultName = "default"

    /// L2: zahteve z žetonom prehoda nikoli ne sledijo preusmeritvi (npr. na proxy drugega profila na 127.0.0.1).
    final class NoRedirect: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
        func urlSession(_ s: URLSession, task: URLSessionTask, willPerformHTTPRedirection r: HTTPURLResponse, newRequest: URLRequest) async -> URLRequest? { nil }
    }
    public static let session = URLSession(configuration: .ephemeral, delegate: NoRedirect(), delegateQueue: nil)

    /// Modeli agentov, ki delajo v mapi odjemalca: le zanje se mapa deli z drugim prehodom.
    public static func isAgentModel(_ m: String) -> Bool {
        ["claude-cli-", "claude-codex", "claude-grok", "claude-copilot"].contains { m.hasPrefix($0) }
    }

    /// Male črke, števke in »-« (do 32): ime gre v pripono modela, ime poverilnice in datoteke.
    public static func valid(_ n: String) -> Bool {
        n.range(of: "^[a-z0-9][a-z0-9-]{0,31}$", options: .regularExpression) != nil
    }

    /// minigw reconnect: vrstica »Copy Token« (<žeton>#<ključ>) ali le žeton; brez ključa ostane shranjeni ključ prehoda.
    /// nil = prazen ali nepopoln žeton (»#«, »#kljuc«, »zeton#«), kot Go newToken.
    public static func newToken(_ line: String, key: String?) -> (token: String, key: String?)? {
        let p = line.split(separator: "#", maxSplits: 1, omittingEmptySubsequences: false).map(String.init)
        guard !p[0].isEmpty, p.count == 1 || !p[1].isEmpty else { return nil }
        return (p[0], p.count > 1 ? p[1] : key)
    }

    /// »claude-cli-opus@win« → (claude-cli-opus, win), če je win profil; sicer model ostane cel (claude-opus-4@20250514).
    public static func split(_ model: String, names: [String]) -> (model: String, profile: String?) {
        guard let at = model.lastIndex(of: "@"), at > model.startIndex else { return (model, nil) }
        let p = String(model[model.index(after: at)...])
        return names.contains(p) ? (String(model[..<at]), p) : (model, nil)
    }

    /// --model <model>@<profil>: profil izbere prehod za ves zagon (mapa, sync, worktree, native), kot --minigw-profile;
    /// model gre naprej brez pripone (#8). profile nil = noben --model ne konča s profilom.
    public static func modelProfile(_ args: [String], names: [String]) -> (args: [String], profile: String?, model: String) {
        var out = args, profile: String?, model = ""
        for (i, a) in args.enumerated() {
            let eq = a.hasPrefix("--model=")
            guard eq || (a == "--model" && i + 1 < args.count) else { continue }
            let v = eq ? String(a.dropFirst(8)) : args[i + 1]
            let (bare, p) = split(v, names: names)
            guard let p else { continue }
            profile = p; model = v
            if eq { out[i] = "--model=" + bare } else { out[i + 1] = bare }
        }
        return (out, profile, model)
    }

    /// Podedovano okolje, ki ga claude pod minigw ne dobi: ANTHROPIC_*, CLAUDECODE in CLAUDE_CODE_* (seja in prijava
    /// starševskega Claude Code ali Claude Desktop, npr. CLAUDE_CODE_OAUTH_TOKEN: sicer 401 na prehodu). minigw nato
    /// nastavi svoje; CLAUDE_CODE_GIT_BASH_PATH (nastavitev uporabnika) ostane (#7).
    public static func parentClaudeVar(_ key: String) -> Bool {
        let k = key.uppercased()
        return (k.hasPrefix("ANTHROPIC_") || k == "CLAUDECODE" || k.hasPrefix("CLAUDE_CODE_")) && k != "CLAUDE_CODE_GIT_BASH_PATH"
    }

    /// Možnosti claude, ki jih minigw pod Claude Code preda naprej (H3); true = ima vrednost. Druge (npr.
    /// --dangerously-skip-permissions, --permission-mode, --allowedTools, --settings, --mcp-config, --add-dir, --plugin-dir)
    /// bi gnezdenemu claude, ki ga vodi prehod, dale dovoljenja brez vprašanja uporabnika (kot Go claudeSafe). Brez
    /// --input-format (stream-json: nadzorne zahteve na stdin, N1) in -c/-r/--session-id (prepis druge seje gre prehodu).
    static let claudeSafe: [String: Bool] = ["-p": false, "--print": false, "--model": true, "--output-format": true,
        "--verbose": false, "--max-turns": true, "--append-system-prompt": true, "--fallback-model": true, "-h": false, "--help": false]

    /// Prva možnost claude, ki ni na seznamu claudeSafe (nil = vse dovoljene), ko minigw teče v Claude Code (CLAUDECODE),
    /// razen ob MINIGW_ALLOW_UNSAFE_FLAGS=1 uporabnika. Po »--« so le argumenti (kot Go unsafeClaudeFlag).
    public static func unsafeClaudeFlag(_ args: [String], env: [String: String]) -> String? {
        guard checkClaudeArgs(env) else { return nil }
        var i = 0
        while i < args.count {
            let a = args[i]
            if a == "--" { return nil }
            if a.hasPrefix("-") {
                let name = String(a.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)[0]), eq = a.contains("=")
                guard let value = claudeSafe[name], !(eq && !name.hasPrefix("--")) else { return name }
                if value && !eq { i += 1 }
            }
            i += 1
        }
        return nil
    }

    /// minigw teče v Claude Code (CLAUDECODE) in uporabnik ni nastavil MINIGW_ALLOW_UNSAFE_FLAGS=1 (kot Go checkClaudeArgs).
    public static func checkClaudeArgs(_ env: [String: String]) -> Bool {
        !(env["CLAUDECODE"] ?? "").isEmpty && env["MINIGW_ALLOW_UNSAFE_FLAGS"] != "1"
    }

    /// N8: pod Claude Code --minigw-work= z načinom, ki piše v mapo brez veje (direct, sync, native-direct), le samo za
    /// branje ali kadar ga mapa že ima shranjenega (share add --work=); MINIGW_WORK v okolju Claude Code vpraša (kot Go unsafeWork).
    public static func unsafeWork(_ flag: String?, saved: String?, readOnly: Bool, env: [String: String]) -> Bool {
        guard let flag, checkClaudeArgs(env), !readOnly, flag != saved else { return false }
        return ["direct", "sync", "native-direct"].contains(flag)
    }

    /// L-31 (kot Go noDirectFallback): pod Claude Code izrecen, neshranjen način (native, worktree) ne pade na direct
    /// (agent bi pisal v mapo brez veje), razen samo za branje.
    public static func noDirectFallback(_ tries: [String], flag: String?, saved: String?, readOnly: Bool, env: [String: String]) -> [String] {
        guard let flag, flag != "auto", flag != saved, !readOnly, checkClaudeArgs(env) else { return tries }
        return tries.filter { $0 != "direct" }
    }

    /// N6, N7: claude -p bere navodilo kot vnos uporabnika: »@pot« (na začetku besede, tudi za ( " ', U+FEFF in 。、？！ kot
    /// pri claude, N6b) priloži katero koli datoteko, »/ukaz« zažene ukaz s poševnico (npr. /config permissionMode=…).
    /// nil = dovoljeno (kot Go promptRefused). Le še varovalka: claude dobi CLAUDE_CODE_DISABLE_FILE_MENTIONS (promptGuard).
    /// mentionsOff: »@« dovoljen (claude ga zanesljivo ne bere, #44).
    static func promptRefused(_ p: String, mentionsOff: Bool = false) -> String? {
        if p.drop(while: { $0.isWhitespace }).hasPrefix("/") { return "a prompt that starts with / (a slash command)" }
        if !mentionsOff, atMention(p) { return "@ before a file name (claude would attach that file)" }
        return nil
    }

    /// »@« na začetku besede (kot Go atMention).
    public static func atMention(_ p: String) -> Bool {
        p.range(of: #"(^|[\s\x{FEFF}\x{3002}\x{3001}\x{FF1F}\x{FF01}("'])@\S"#, options: .regularExpression) != nil
    }

    /// Najstarejša različica Claude Code (izmerjeno 2026-11-10), ki s CLAUDE_CODE_DISABLE_FILE_MENTIONS=1 in --settings ne prebere nobene oblike »@pot«
    /// (0.5.19); mentionSettings ga postavi tudi mimo env v uporabnikovem settings.json (I17; kot Go).
    public static let minMentionGuard = "2.1.299"
    public static let mentionSettings = #"{"env":{"CLAUDE_CODE_DISABLE_FILE_MENTIONS":"1"}}"#

    /// Izpis »claude --version« (»2.1.299 (Claude Code)«) je vsaj minMentionGuard; neznan = ne (kot Go mentionGuardVersion).
    public static func mentionGuardVersion(_ out: String) -> Bool {
        let v = out.split(whereSeparator: \.isWhitespace).first.map(String.init) ?? ""
        return !v.isEmpty && v.allSatisfy { "0123456789.".contains($0) } && v.compare(minMentionGuard, options: .numeric) != .orderedAscending
    }

    /// #44: »@« na stdin gre naprej le, ko ga gnezdeni claude zanesljivo ne bere: varovalo, različica vsaj minMentionGuard
    /// (preveri le ob »@«); info (-v, -h: claude brez --settings) nikoli (kot Go stdinMentionsOff).
    public static func stdinMentionsOff(_ stdin: String, info: Bool, guard on: Bool, version: () -> String) -> Bool {
        !info && on && atMention(stdin) && mentionGuardVersion(version())
    }

    /// L1: prva absolutna mapa v path (vrstni red kot execvp) z navadno izvršljivo datoteko `name`; nil = je ni.
    public static func executable(_ name: String, path: String) -> String? {
        path.split(separator: ":").lazy.filter { $0.hasPrefix("/") }.map { String($0) + "/" + name }.first { p in
            var st = stat()
            return stat(p, &st) == 0 && st.st_mode & S_IFMT == S_IFREG && access(p, X_OK) == 0
        }
    }

    /// Opozorilo, ko minigw stdin (cev ali datoteka), ki v waitMs ni dal ničesar, ni poslal (nil = nič; kot Go stdinNote).
    public static func stdinNote(read: Data?, waitMs: Int32, mode: mode_t) -> String? {
        guard read == nil, waitMs > 0, mode & S_IFMT == S_IFIFO || mode & S_IFMT == S_IFREG else { return nil }
        return "minigw: stdin had no data within \(waitMs / 1000) s and was not sent"
    }

    /// #78 (kot Go streamArgs): claude -p brez izbranega --output-format teče s stream-json in deltami (nil = argumenti ostanejo).
    public static func streamArgs(_ args: [String]) -> [String]? {
        let opts = args.prefix { $0 != "--" }
        guard opts.contains(where: { $0 == "-p" || $0 == "--print" }),
              !opts.contains(where: { $0 == "--output-format" || $0.hasPrefix("--output-format=") }) else { return nil }
        return ["--output-format", "stream-json", "--verbose", "--include-partial-messages"] + args
    }

    /// #78 (kot Go helperModelEnv): model ponudnika (ne opus/sonnet/haiku/fable): pomožni klici Claude Code gredo k istemu
    /// modelu, ne k claude-haiku-…, ki ga prehod morda nima (404); agenti (Codex, Grok, Copilot, Siri AI, Apple FM) in
    /// modeli Claude ne. Uporabnikove vrednosti ostanejo.
    public static func helperModelEnv(_ args: [String], env: [String: String]) -> [String: String] {
        var model = ""
        let opts = Array(args.prefix { $0 != "--" })
        for (i, a) in opts.enumerated() {
            if a == "--model", i + 1 < opts.count { model = opts[i + 1] } else if a.hasPrefix("--model=") { model = String(a.dropFirst(8)) }
        }
        guard !model.isEmpty, ClaudeCLI.match(model.lowercased()) == nil, !isAgentModel(model), !Siri.ids.contains(model),
              !AppleFM.ids.contains(model) else { return [:] }
        return Dictionary(uniqueKeysWithValues: ["ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL"].map { k in
            (k, env[k].flatMap { $0.isEmpty ? nil : $0 } ?? model)
        })
    }

    /// #78 (kot Go streamText): dogodki claude --output-format stream-json → besedilo modela za stdout sproti (delte
    /// --include-partial-messages; brez njih celi bloki), prazna vrstica med bloki; uporaba orodja (tudi oznaka ⏺ agenta
    /// prehoda) kot kratka vrstica za stderr; končni result ni izpisan dvakrat.
    public struct StreamText {
        /// Najdaljša vrstica stream-json (enako kot Go streamLineMax).
        public static let lineMax = 64 << 20
        var n = 0, last = ""          // izpisani bloki, besedilo zadnjega
        var partial = false, open = false  // partial: delte namesto celih blokov; open: blok se izpisuje
        /// Nove vrstice, ki jih besedilo želi na koncu (največ ena prazna vrstica), in že izpisane: prva gre takoj, da je
        /// vrstica cela pred oznako orodja (kot Go pend, nl).
        var pend = 0, nl = 0
        var apiError = ""  // besedilo sporočila z napako API, dokler ga ne prinese result
        public private(set) var failed = false
        /// Dogodek result: zadnji dogodek claude -p, branje se konča.
        public private(set) var done = false
        public init() {}

        /// Besedilo brez nadzornih znakov terminala (C0 razen \n in \t, DEL, C1; kot Go printable).
        public static func printable(_ s: String) -> String {
            String(String.UnicodeScalarView(s.unicodeScalars.filter { c in
                c == "\n" || c == "\t" || !(c.value < 0x20 || (0x7f...0x9f).contains(c.value))
            }))
        }

        /// Oznaka orodja agenta prehoda v besedilu (StreamTranslator: »⏺ Bash«) kot cel delta z imenom validName (kot Go
        /// toolMark), sicer je delta besedilo odgovora.
        static func toolMark(_ s: String) -> String? {
            let m = s.hasPrefix("\n") ? s.dropFirst() : Substring(s)
            guard m.hasPrefix("⏺ "), m.hasSuffix("\n") else { return nil }
            let name = String(m.dropFirst(2).dropLast())
            return validName(name) ? name : nil
        }

        /// Ime orodja za vrstico minigw: · : le [A-Za-z0-9_.:@/-], 1–80 znakov (model ne ponaredi vrstice z U+2028, bidi).
        static func validName(_ s: String) -> Bool {
            (1...80).contains(s.unicodeScalars.count) && s.unicodeScalars.allSatisfy { $0.isASCII && (CharacterSet.alphanumerics.contains($0) || "_.:@/-".unicodeScalars.contains($0)) }
        }

        /// Besedilo bloka za opozorilo (agent prehoda pošlje vse v enem bloku; kot Go lastMax).
        static let lastMax = 64 << 10

        mutating func text(_ raw: String) -> String {
            let t = Self.printable(raw)
            var out = ""
            if !open {
                if t.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return "" }
                if n > 0 { pend = 2 }
                open = true; n += 1; last = ""
            }
            if last.utf8.count < Self.lastMax { last += t }
            var body = t
            while body.hasSuffix("\n") { body.removeLast() }
            if !body.isEmpty {
                out += String(repeating: "\n", count: max(pend - nl, 0)) + body
                pend = 0; nl = 0
            }
            pend = min(pend + t.count - body.count, 2)
            return out + lineEnd()
        }

        mutating func lineEnd() -> String {
            guard pend > 0, nl == 0 else { return "" }
            nl = 1
            return "\n"
        }

        static func tool(_ name: String) -> String {
            "minigw: · \(validName(name) ? name : "tool")\n"
        }

        /// Ena vrstica JSON → besedilo za stdout in stderr.
        public mutating func line(_ data: Data) -> (out: String, err: String) {
            guard let ev = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else { return ("", "") }
            var out = "", err = ""
            switch ev["type"] as? String {
            case "stream_event":
                partial = true
                guard ev["parent_tool_use_id"] == nil || ev["parent_tool_use_id"] is NSNull,  // notranji podagent
                      let e = ev["event"] as? [String: Any] else { break }
                if e["type"] as? String == "content_block_delta", let d = e["delta"] as? [String: Any],
                   d["type"] as? String == "text_delta", let t = d["text"] as? String {
                    if let name = Self.toolMark(t) {
                        if open { pend = 2; out += lineEnd() }  // vrstica pred oznako je cela, preden gre oznaka na stderr
                        err += Self.tool(name)
                    } else {
                        out += text(t)
                    }
                } else if e["type"] as? String == "content_block_stop" {
                    open = false; pend = 0
                }
            case "assistant":
                for c in ((ev["message"] as? [String: Any])?["content"] as? [[String: Any]]) ?? [] {
                    // sintetično sporočilo z napako API (is_api_error_message): isto besedilo da result, le enkrat kot claude: na stderr
                    if c["type"] as? String == "text", ev["is_api_error_message"] as? Bool == true {
                        apiError += c["text"] as? String ?? ""  // do result ali konca branja (end)
                    } else if c["type"] as? String == "text", !partial, let t = c["text"] as? String {
                        out += text(t)
                        open = false; pend = 0
                    } else if c["type"] as? String == "tool_use" {
                        err += Self.tool(c["name"] as? String ?? "")
                    }
                }
            case "result":
                done = true
                apiError = ""
                let r = ev["result"] as? String ?? ""
                if ev["is_error"] as? Bool == true {
                    failed = true
                    err += Self.claudeLines(r)
                } else if n == 0 {
                    out += text(r)
                }
                if n > 0, nl == 0 { out += "\n"; nl = 1 }
            default: break
            }
            return (out, err)
        }

        /// Konec branja: zaključna nova vrstica izpisanega besedila brez dogodka result in napaka API, ki je result ni
        /// prinesel (claude je končal brez njega; kot Go).
        public mutating func end() -> (out: String, err: String) {
            let o = n > 0 && !done && nl == 0 ? "\n" : ""
            guard !apiError.isEmpty else { return (o, "") }
            failed = true
            defer { apiError = "" }
            return (o, Self.claudeLines(apiError))
        }

        /// Vsaka vrstica s predpono claude: — model ne more ponarediti vrstice minigw:, ki ji skill zaupa (kot Go).
        static func claudeLines(_ s: String) -> String {
            var r = printable(s)
            while r.hasSuffix("\n") { r.removeLast() }
            return r.split(separator: "\n", omittingEmptySubsequences: false).map { "claude: \($0)\n" }.joined()
        }

        /// Opozorilo, ko se edino besedilo sklicuje na prejšnje (»above«, »zgoraj«), ki ga ni bilo (nil = ni).
        public var warning: String? {
            n == 1 && last.range(of: #"\babove\b|\bsee (the )?previous\b|zgoraj"#, options: [.regularExpression, .caseInsensitive]) != nil
                ? "minigw: warning: the answer refers to earlier text that the model did not send; it may be incomplete\n" : nil
        }
    }

    /// #80 (kot Go gate): izhod toka claude piše svoja nit, da stdout, ki ga nihče ne bere (Ctrl+S, cev brez bralca),
    /// ne ustavi branja in roka 2 s. Ena vrsta za stdout in stderr ohrani vrstni red; po prvi napaki pisanja (| head,
    /// zaprt stderr) ta ročaj ne piše več. Čakajoči izhod ima mejo (stdout 16 MB, stderr 1 MB): čez njo ročaj ne piše
    /// več, za stdout je dropped (Go namesto tega zadrži claude kot navadna cev). Po close se ne začne noben nov kos
    /// (64 KB), tisti v teku se lahko še konča.
    public final class OutputGate: @unchecked Sendable {
        static let chunk = 64 << 10
        private let queue = DispatchQueue(label: "minigw.output")
        private let handles: [FileHandle]
        private let caps: [Int]
        private let lock = NSLock()
        private var off: Set<Int> = [], closed = false, pending = [0, 0], isDropped = false
        /// stdout je bil čez mejo izpuščen (nihče ga ni bral).
        public var dropped: Bool { lock.withLock { isDropped } }

        public init(out: FileHandle, err: FileHandle, outCap: Int = 16 << 20, errCap: Int = 1 << 20) {
            handles = [out, err]
            caps = [outCap, errCap]
        }

        public func out(_ s: String) { write(Data(s.utf8), 0) }
        public func err(_ s: String) { write(Data(s.utf8), 1) }

        private func write(_ d: Data, _ i: Int) {
            guard !d.isEmpty, lock.withLock({ () -> Bool in
                guard !closed, !off.contains(i) else { return false }
                guard pending[i] + d.count <= caps[i] else {
                    off.insert(i)
                    if i == 0 { isDropped = true }
                    return false
                }
                pending[i] += d.count
                return true
            }) else { return }
            queue.async { [self] in
                defer { lock.withLock { pending[i] -= d.count } }
                var at = d.startIndex
                while at < d.endIndex, lock.withLock({ !closed && !off.contains(i) }) {
                    let end = min(at + Self.chunk, d.endIndex)
                    do { try handles[i].write(contentsOf: d[at..<end]) } catch { lock.withLock { _ = off.insert(i) } }
                    at = end
                }
            }
        }

        /// Počaka, da gre izhod ven, največ timeout; nato ne piše več (vrstice o vejah sledijo na stderr).
        public func close(timeout: TimeInterval) {
            let s = DispatchSemaphore(value: 0)
            queue.async { s.signal() }
            _ = s.wait(timeout: .now() + max(timeout, 0))
            lock.withLock { closed = true }
        }
    }

    /// N6b, N7: claude brez terminala na stdin ali z -p bere navodilo kot vnos uporabnika: tedaj dobi --disable-slash-commands
    /// in CLAUDE_CODE_DISABLE_FILE_MENTIONS=1. Ostane le pogovor v terminalu, kjer tipka človek (kot Go promptGuard).
    public static func promptGuard(_ args: [String], env: [String: String], stdinTerminal: Bool) -> Bool {
        if !(env["CLAUDECODE"] ?? "").isEmpty || !stdinTerminal { return true }
        return args.prefix { $0 != "--" }.contains { $0 == "-p" || $0 == "--print" }
    }

    /// Razlog za zavrnitev navodila pod Claude Code (nil = dovoljeno): besede, ki niso možnost ali njena vrednost, vse po
    /// »--« in stdin (kot Go unsafePrompt). stdinMentionsOff (#44): na stdin le »/« na začetku (diff ima »@@«).
    public static func unsafePrompt(_ args: [String], stdin: String, env: [String: String], stdinMentionsOff: Bool = false) -> String? {
        guard checkClaudeArgs(env) else { return nil }
        if let r = promptWords(args).lazy.compactMap({ promptRefused($0) }).first { return r }
        guard let r = promptRefused(stdin, mentionsOff: stdinMentionsOff) else { return nil }
        return r.hasPrefix("a prompt") ? r : stdinMention
    }

    /// Razlog za »@« na stdin; promptRefusal mu doda svoj nasvet (kot Go).
    static let stdinMention = "@ before a file name on stdin (claude would attach that file)"

    /// Sporočilo ob zavrnitvi navodila z razlogom r iz unsafePrompt (kot Go promptRefusal).
    public static func promptRefusal(_ r: String) -> String {
        r == stdinMention
            ? r + " is not accepted when minigw runs inside Claude Code; remove the @ before file names on stdin or update Claude Code to \(minMentionGuard) or newer"
            : r + " is not accepted when minigw runs inside Claude Code: paste the text itself into the prompt"
    }

    /// Besede navodila v argumentih claude: niso možnost ali njena vrednost, in vse po »--« (kot Go promptWords).
    public static func promptWords(_ args: [String]) -> [String] {
        var words: [String] = [], i = 0
        while i < args.count {
            let a = args[i]
            if a == "--" { return words + args[(i + 1)...] }
            if !a.hasPrefix("-") { words.append(a) } else if claudeSafe[a] == true { i += 1 }
            i += 1
        }
        return words
    }

    /// claude -p z navodilom v argumentih čaka na stdin 3 s in nato nadaljuje brez njega; minigw enako (kot Go readStdin),
    /// zato odprt stdin, ki se nikoli ne zapre (orodje Bash v Claude Code), ne blokira. waitMs 0 = ves stdin.
    /// nil = v waitMs ni prišlo nič (claude dobi prazen stdin).
    public static func readStdin(fd: Int32, waitMs: Int32) -> Data? {
        var p = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
        if waitMs > 0, poll(&p, 1, waitMs) <= 0 { return nil }
        return FileHandle(fileDescriptor: fd, closeOnDealloc: false).readDataToEndOfFile()
    }

    /// Besedilo prehoda v eni vrstici brez kontrolnih znakov, največ `max` znakov: gre v kontekst Claude (M2, M3).
    public static func line(_ s: String, max: Int) -> String {
        let l = String(String.UnicodeScalarView(s.unicodeScalars.compactMap { u -> Unicode.Scalar? in
            u == "\n" ? " " : (u.value < 0x20 || (0x7f...0x9f).contains(u.value) || u.properties.generalCategory == .format) ? nil : u
        }))
        return l.count > max ? l.prefix(max) + "…" : l
    }

    /// Vrstice »minigw models« (M3): id in opis prehoda v eni vrstici in omejene dolžine (kot Go modelLines).
    public static func modelLines(_ data: [[String: Any]]) -> [String] {
        let ids = data.map { line($0["id"] as? String ?? "?", max: 100) }
        let width = ids.map(\.count).max() ?? 0
        return zip(ids, data).map { $0.padding(toLength: width, withPad: " ", startingAt: 0) + "  " + line($1["description"] as? String ?? "", max: 200) }
    }

    /// M2: besedilo prehoda kot »minigw: gateway: …« v eni vrstici; vrstico o veji izpiše le minigw sam iz preverjenih
    /// podatkov, zato besedilo prehoda nikoli ni videti kot ona (kot Go gatewayNote).
    public static func gatewayNote(_ msg: String) -> String {
        let m = line(msg, max: 500)
        return "minigw: gateway: " + (m.lowercased().contains("agent work is on branch") ? "reported a branch that minigw could not verify; ignored" : m)
    }

    /// Telo zahteve z modelom brez pripone, ime profila in model brez pripone; sicer telo, kot je, in nil.
    public static func routeBody(_ body: Data, names: [String]) -> (body: Data, profile: String?, model: String) {
        guard var o = try? JSONSerialization.jsonObject(with: body) as? [String: Any], let model = o["model"] as? String else { return (body, nil, "") }
        let (bare, p) = split(model, names: names)
        guard let p else { return (body, nil, model) }
        o["model"] = bare
        guard let nb = try? JSONSerialization.data(withJSONObject: o) else { return (body, nil, model) }
        return (nb, p, bare)
    }

    /// Vsak model s pripono @profil, privzeti (prvi) še brez nje; nil = noben prehod ni odgovoril.
    public static func merge(_ lists: [[[String: Any]]?], names: [String]) -> [[String: Any]]? {
        guard lists.contains(where: { $0 != nil }) else { return nil }
        var out: [[String: Any]] = [], seen: Set<String> = []
        func add(_ m: [String: Any], _ id: String, _ suffix: String?) {
            guard seen.insert(id).inserted else { return }
            var c = m
            c["id"] = id
            if let s = suffix, let dn = m["display_name"] as? String, !dn.isEmpty { c["display_name"] = dn + " @" + s }
            out.append(c)
        }
        for (i, l) in lists.enumerated() {
            guard let l else { continue }
            let ms = l.filter { !(($0["id"] as? String) ?? "").isEmpty }  // vrstni red prehoda ostane
            if i == 0 { for m in ms { add(m, m["id"] as! String, nil) } }
            for m in ms { add(m, (m["id"] as! String) + "@" + names[i], names[i]) }
        }
        return out
    }
}

/// Lokalni usmerjevalnik za Claude Code pri več profilih: na 127.0.0.1 z lastnim žetonom; zahteva gre prehodu iz
/// pripone modela (prek njegovega šifriranega proxyja), z njegovim žetonom. Ena zahteva na povezavo (Connection: close).
public final class ProfileMux: @unchecked Sendable {
    public struct Target: Sendable {
        public let name: String, url: String, token: String  // url: lokalni proxy prehoda ali prehod na tem Macu
        public init(name: String, url: String, token: String) { self.name = name; self.url = url; self.token = token }
    }

    public let targets: [Target]
    public let secret = Noise.base64url(Data((0..<32).map { _ in UInt8.random(in: 0...255) }))
    private let shareHooks: [String: @Sendable () -> Bool]  // mapa pri drugem prehodu ob prvi zahtevi zanj
    private let lock = NSLock()
    private var shared: [String: Bool] = [:]
    private let queue = DispatchQueue(label: "dev.trojar.minigw.mux", attributes: .concurrent)
    private var listener: NWListener?
    public private(set) var localPort: UInt16 = 0
    public var localURL: String { "http://127.0.0.1:\(localPort)" }

    public init(targets: [Target], share: [String: @Sendable () -> Bool] = [:]) {
        self.targets = targets; shareHooks = share
    }

    /// Stalen port za ta nabor profilov (Claude Code hrani modele skupaj z ANTHROPIC_BASE_URL), sicer naključen.
    public var stablePort: UInt16 {
        let h = SHA256.hash(data: Data(targets.map { "\($0.name)=\($0.url);" }.joined().utf8)).prefix(2).reduce(0) { $0 << 8 | Int($1) }
        return UInt16(30000 + h % 10000)
    }

    public func start() throws {
        do { try listen(NWEndpoint.Port(rawValue: stablePort)!) } catch { try listen(.any) }
    }

    private func listen(_ lp: NWEndpoint.Port) throws {
        let params = NWParameters.tcp
        params.requiredLocalEndpoint = .hostPort(host: "127.0.0.1", port: lp)
        let l = try NWListener(using: params)
        let ready = DispatchSemaphore(value: 0)
        l.stateUpdateHandler = { s in  // I5: .waiting (npr. zaseden port) je napaka: naključen port, ne čakanje
            switch s {
            case .ready, .failed, .waiting, .cancelled: ready.signal()
            default: break
            }
        }
        l.newConnectionHandler = { [weak self] c in self?.accept(c) }
        l.start(queue: queue)
        ready.wait()
        guard l.state == .ready, let p = l.port?.rawValue, p != 0 else { l.cancel(); throw NWError.posix(.EADDRINUSE) }
        localPort = p; listener = l
    }

    public func stop() { listener?.cancel() }

    private func accept(_ c: NWConnection) {
        c.start(queue: queue)
        read(c, Data())
    }

    private func read(_ c: NWConnection, _ buf: Data) {
        c.receive(minimumIncompleteLength: 1, maximumLength: 1 << 20) { [weak self] data, _, end, error in
            guard let self else { return c.cancel() }
            let b = buf + (data ?? Data())
            switch HTTPParser.parse(b) {
            case .request(let r): self.queue.async { self.handle(r, c) }
            case .error(let st, let msg): self.respond(c, st, Self.error(msg))
            case .needMore: if end || error != nil { c.cancel() } else { self.read(c, b) }
            }
        }
    }

    static func error(_ msg: String) -> Data {
        (try? JSONSerialization.data(withJSONObject: ["type": "error", "error": ["type": "api_error", "message": msg]])) ?? Data()
    }

    private func respond(_ c: NWConnection, _ status: Int, _ body: Data) {
        c.send(content: HTTPResponse.head(status: status, headers: [("Content-Type", "application/json")], contentLength: body.count) + body,
               completion: .contentProcessed { _ in c.cancel() })
    }

    private func authorized(_ r: HTTPRequest) -> Bool {
        var got = r.header("authorization") ?? ""
        if got.hasPrefix("Bearer ") { got.removeFirst(7) }
        if got.isEmpty { got = r.header("x-api-key") ?? "" }
        let a = Array(got.utf8), b = Array(secret.utf8)
        return a.count == b.count && zip(a, b).reduce(0) { $0 | ($1.0 ^ $1.1) } == 0
    }

    private func handle(_ r: HTTPRequest, _ c: NWConnection) {
        guard authorized(r) else { return respond(c, 401, Self.error("invalid token for minigw's local proxy")) }
        // I4: brez kontrolnih znakov v cilju zahteve in vrednostih glav (se posredujejo prehodu)
        let bad = { (s: String) in s.unicodeScalars.contains { $0.value < 0x20 || $0.value == 0x7f } }
        guard !bad(r.path), !bad(r.query ?? ""), !r.headers.contains(where: { bad($0.key) || bad($0.value) }) else {
            return respond(c, 400, Self.error("control characters in the request"))
        }
        if r.method == "GET", r.path == "/v1/models" { return models(c) }
        var t = targets[0], body = r.body
        if r.method == "POST", r.path.hasPrefix("/v1/messages") {
            let (nb, p, model) = Profiles.routeBody(r.body, names: targets.map(\.name))
            if let p, let x = targets.first(where: { $0.name == p }) { t = x; body = nb }
            if t.name != targets[0].name, Profiles.isAgentModel(model), let hook = shareHooks[t.name] {  // le model agenta deli mapo
                lock.lock()  // eno registracijo; druge zahteve za ta prehod počakajo nanjo
                if shared[t.name] == nil { shared[t.name] = hook() }
                lock.unlock()
            }
        }
        forward(r, body, t, c)
    }

    private func isShared(_ name: String) -> Bool { lock.withLock { shared[name] == true } }

    private func forward(_ r: HTTPRequest, _ body: Data, _ t: Target, _ local: NWConnection) {
        guard let u = URLComponents(string: t.url), let h = u.host, let port = NWEndpoint.Port(rawValue: UInt16(u.port ?? 80)) else {
            return respond(local, 502, Self.error("gateway \(t.name): invalid address"))
        }
        let drop: Set<String> = ["host", "authorization", "x-api-key", "content-length", "connection", "keep-alive", "transfer-encoding"]
        var head = "\(r.method) \(r.path)\(r.query.map { "?" + $0 } ?? "") HTTP/1.1\r\nhost: \(h):\(port)\r\n"
        for (k, v) in r.headers where !drop.contains(k) && !(k == "x-claude-mini-share" && t.name != targets[0].name) {
            head += "\(k): \(v)\r\n"
        }
        if t.name != targets[0].name, isShared(t.name) { head += "x-claude-mini-share: 1\r\n" }
        head += "authorization: Bearer \(t.token)\r\ncontent-length: \(body.count)\r\nconnection: close\r\n\r\n"
        // Lokalni port izbere jedro (prost, kratkotrajen). Brez tega Network.framework za cilj, ki je bil nedavno odjemalec
        // povezave tega procesa (npr. prek tega usmerjevalnika), ponovno uporabi star lokalni port: par je še v TIME_WAIT,
        // povezava ostane .waiting(EADDRINUSE) do 30 s in zahteva dobi 502.
        let params = NWParameters.tcp
        params.requiredLocalEndpoint = .hostPort(host: "::", port: .any)
        let remote = NWConnection(host: NWEndpoint.Host(h), port: port, using: params)
        var started = false
        remote.stateUpdateHandler = { [weak self] s in
            switch s {
            case .ready where !started:
                started = true
                remote.send(content: Data(head.utf8) + body, completion: .contentProcessed { _ in })
                self?.pump(remote, local, first: true, t.name)
            case .failed, .waiting:
                if !started { started = true; remote.cancel(); self?.respond(local, 502, Self.error("gateway \(t.name) is not reachable (minigw status)")) }
            default: break
            }
        }
        local.stateUpdateHandler = { s in if case .cancelled = s { remote.cancel() }; if case .failed = s { remote.cancel() } }
        remote.start(queue: queue)
    }

    /// Prehod → Claude Code brez sprememb (tudi SSE); ob koncu zapre obe strani.
    private func pump(_ remote: NWConnection, _ local: NWConnection, first: Bool, _ name: String) {
        remote.receive(minimumIncompleteLength: 1, maximumLength: 256 * 1024) { [weak self] data, _, end, error in
            if let data, !data.isEmpty { local.send(content: data, completion: .contentProcessed { _ in }) }
            if end || error != nil {
                if first, data?.isEmpty ?? true { self?.respond(local, 502, Self.error("gateway \(name) is not reachable (minigw status)")); remote.cancel(); return }
                local.send(content: nil, contentContext: .finalMessage, isComplete: true, completion: .contentProcessed { _ in local.cancel() })
                remote.cancel()
                return
            }
            self?.pump(remote, local, first: false, name)
        }
    }

    private func models(_ c: NWConnection) {
        var lists = [[[String: Any]]?](repeating: nil, count: targets.count)
        let g = DispatchGroup(), l = NSLock()
        for (i, t) in targets.enumerated() {
            guard let url = URL(string: t.url + "/v1/models") else { continue }
            var req = URLRequest(url: url, timeoutInterval: 10)
            req.setValue("Bearer \(t.token)", forHTTPHeaderField: "authorization")
            g.enter()
            Profiles.session.dataTask(with: req) { d, resp, _ in
                defer { g.leave() }
                guard (resp as? HTTPURLResponse)?.statusCode == 200, let d,
                      let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else { return }
                l.withLock { lists[i] = o["data"] as? [[String: Any]] ?? [] }
            }.resume()
        }
        g.wait()
        guard let data = Profiles.merge(lists, names: targets.map(\.name)) else {
            return respond(c, 502, Self.error("no gateway is reachable (minigw status)"))
        }
        var o: [String: Any] = ["data": data, "has_more": false]
        if let f = data.first?["id"], let last = data.last?["id"] { o["first_id"] = f; o["last_id"] = last }
        respond(c, 200, (try? JSONSerialization.data(withJSONObject: o)) ?? Data())
    }
}
