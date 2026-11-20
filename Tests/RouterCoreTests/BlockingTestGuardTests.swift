import Foundation
import Testing

/// Static half of the guard for the #150/#147/#140 family (the run-time half is PoolLagGuardTests.swift). Swift Testing runs a test body, synchronous or
/// async, on a thread of the Swift concurrency pool; the pool has about one thread per core, and the router under test answers from a `Task` that needs
/// one of them. A test that blocks (waitUntilExit, a semaphore wait, a sleep, a blocking socket call, a call that runs git and waits) for seconds, a dozen
/// of them at once, left the router without a thread and stalled the whole suite.
///
/// The rule: a `@Test` whose body blocks (directly, or by a helper of the same file that blocks) carries the `.blocking` trait (BlockingTrait.swift: the
/// body runs on a thread of its own), unless the blocking part is inside `offPool { }`, a `Thread { }` or `Thread.detachNewThread { }`, which this scan
/// does not look into. Not on the list of primitives below? Then the run-time sampler is the net.
@Suite struct BlockingTestGuard {
    /// What blocks a thread, with the name used in a failure message. `.wait(timeout: .now())` is a poll and is not here.
    static let primitives: [(pattern: String, name: String)] = [
        (#"\.waitUntilExit\("#, "waitUntilExit"), (#"readDataToEndOfFile"#, "readDataToEndOfFile"), (#"(?<![\w.])usleep\("#, "usleep"),
        (#"Thread\.sleep"#, "Thread.sleep"), (#"(?<![\w.])sleep\("#, "sleep"),
        (#"\.wait\((?!timeout: *\.now\(\)\s*\))"#, "semaphore or group wait"),
        (#"Darwin\.(read|recv|connect|accept)\("#, "blocking socket call"), (#"(?<![\w.])(accept|connect|recv)\("#, "blocking socket call"),
        (#"ShareHub\.run\("#, "ShareHub.run"), (#"Worktree\.[a-z]\w*\("#, "Worktree call (runs git and waits)"),
        (#"Native\.(run|spawnGroup)\w*\("#, "Native process call"), (#"ClaudeSeatbelt\.capture"#, "ClaudeSeatbelt.capture"),
        (#"\.sync\s*[{(]"#, "DispatchQueue.sync"), (#"SO_RCVTIMEO"#, "blocking read with a timeout"), (#"AdminClient\("#, "AdminClient request"),
        (#"(?<![\w.])flock\("#, "flock"),
    ]
    /// Blocks that are not looked into: the work is on a thread of its own there.
    static let offThread = try! NSRegularExpression(pattern: #"offPool\s*\(?\s*\{|Thread\s*\{|Thread\.detachNewThread\s*\{|Thread\(block:\s*\{"#)

    /// test file name, test name, why it may block without the trait.
    static let allowed: [(file: String, test: String, why: String)] = [
        ("NonLoopbackListenerGuardTests.swift", "noTestSourceListensOrBindsOutsideLoopbackWithoutAReason", "the scan names connect( as text in a string literal; the test reads files and blocks on nothing"),
        ("NonLoopbackListenerGuardTests.swift", "plantedExamplesAreCaughtAndDataIsNot", "the planted examples are string literals; the test blocks on nothing"),
    ]

    static func strip(_ text: String) -> String {
        let chars = Array(text.utf16)
        var out = [UInt16](), i = 0
        let ns = text as NSString
        while true {
            guard let m = offThread.firstMatch(in: text, range: NSRange(location: i, length: ns.length - i)) else { out += chars[i...]; break }
            out += chars[i..<m.range.location]
            var j = m.range.location + m.range.length, depth = 1
            while j < chars.count && depth > 0 {
                if chars[j] == 123 { depth += 1 } else if chars[j] == 125 { depth -= 1 }
                j += 1
            }
            i = j
        }
        return String(decoding: out, as: UTF16.self)
    }

    static func hits(_ code: String) -> Set<String> {
        let text = strip(code).replacingOccurrences(of: #"//.*"#, with: "", options: .regularExpression)
        return Set(primitives.filter { text.range(of: $0.pattern, options: .regularExpression) != nil }.map(\.name))
    }

    /// The function starting at line `from`: its text up to the closing brace, and the line it ends on.
    static func body(_ lines: [Substring], from: Int) -> (text: String, end: Int) {
        var depth = 0, k = from, started = false, parts = [Substring]()
        while k < lines.count {
            for ch in lines[k] { if ch == "{" { depth += 1; started = true } else if ch == "}" { depth -= 1 } }
            parts.append(lines[k])
            if started && depth <= 0 { break }
            k += 1
        }
        return (parts.joined(separator: "\n"), k)
    }

    /// Tests of one source text that block but do not carry `.blocking`: (test name, what blocks).
    static func violations(in source: String) -> [(test: String, why: String)] {
        let lines = source.split(separator: "\n", omittingEmptySubsequences: false)
        var helpers = [String: (blocks: Set<String>, text: String)](), tests = [(name: String, attr: String, text: String)]()
        var i = 0
        while i < lines.count {
            let l = String(lines[i])
            if l.range(of: #"^\s*@Test\b"#, options: .regularExpression) != nil {
                var j = i
                while j < lines.count && !lines[j].contains("func ") { j += 1 }
                guard j < lines.count else { break }
                let b = body(lines, from: j)
                let name = lines[j].range(of: #"func (\w+)"#, options: .regularExpression).map { String(lines[j][$0].dropFirst(5)) } ?? "?"
                tests.append((name, String(lines[i]) + String(lines[j]), b.text))
                i = b.end + 1
                continue
            }
            if let r = l.range(of: #"\bfunc (\w+)"#, options: .regularExpression) {
                let name = String(l[r].dropFirst(5)), b = body(lines, from: i)
                helpers[name] = (hits(b.text), b.text)
            }
            i += 1
        }
        var blocking = helpers.compactMapValues { $0.blocks.isEmpty ? nil : $0.blocks }
        var changed = true
        while changed {
            changed = false
            for (name, h) in helpers where blocking[name] == nil {
                let text = strip(h.text)
                if let callee = blocking.first(where: { text.range(of: #"(?<![\w.])"# + NSRegularExpression.escapedPattern(for: $0.key) + #"\("#, options: .regularExpression) != nil }) {
                    blocking[name] = callee.value; changed = true
                }
            }
        }
        var found = [(test: String, why: String)]()
        for t in tests where !t.attr.contains(".blocking") {
            var why = hits(t.text)
            let text = strip(t.text)
            for (h, blocks) in blocking where text.range(of: #"(?<!\w)"# + NSRegularExpression.escapedPattern(for: h) + #"\("#, options: .regularExpression) != nil {
                why.insert("\(h)() -> " + blocks.sorted().joined(separator: ", "))
            }
            if !why.isEmpty { found.append((t.name, why.sorted().joined(separator: "; "))) }
        }
        return found
    }

    static var testsDir: URL { URL(fileURLWithPath: #filePath).deletingLastPathComponent() }
    /// This file and the trait name the primitives as text.
    static let selfNames = ["BlockingTestGuardTests.swift", "BlockingTrait.swift", "BlockingTraitTests.swift"]

    @Test func everyTestThatBlocksRunsOffThePool() throws {
        let names = try FileManager.default.contentsOfDirectory(atPath: Self.testsDir.path).filter { $0.hasSuffix(".swift") && !Self.selfNames.contains($0) }.sorted()
        for name in names {
            let text = try String(contentsOf: Self.testsDir.appendingPathComponent(name), encoding: .utf8)
            for v in Self.violations(in: text) {
                let ok = Self.allowed.contains { $0.file == name && $0.test == v.test }
                #expect(ok, "\(name): test \(v.test) blocks (\(v.why)) on a thread of the Swift concurrency pool: add the .blocking trait (@Test(.blocking)), or put the blocking part in offPool { } (#150); or list it in BlockingTestGuard.allowed with the reason")
            }
        }
    }

    @Test func plantedExamplesAreCaughtAndSafeOnesAreNot() {
        let planted = { (attr: String, body: String) in Self.violations(in: "\(attr) func t() {\n\(body)\n}\n").map(\.test) }
        #expect(planted("@Test", "    usleep(1000)") == ["t"])
        #expect(planted("@Test", "    try p.run(); p.waitUntilExit()") == ["t"])
        #expect(planted("@Test", "    let s = DispatchSemaphore(value: 0); s.wait()") == ["t"])
        #expect(planted("@Test(arguments: [1])", "    Thread.sleep(forTimeInterval: 1)") == ["t"])
        #expect(planted("@Test(.blocking)", "    usleep(1000)").isEmpty)  // the trait is the answer
        #expect(planted("@Test", "    await offPool { usleep(1000) }").isEmpty)  // on a thread of its own
        #expect(planted("@Test", "    Thread { usleep(1000) }.start()").isEmpty)
        #expect(planted("@Test", "    let polled = s.wait(timeout: .now()) == .success").isEmpty)  // a poll does not block
        let viaHelper = "private func slow() { usleep(10) }\nprivate func slower() { slow() }\n@Test func t() {\n    slower()\n}\n"
        #expect(Self.violations(in: viaHelper).map(\.test) == ["t"])  // a helper of the same file, one level removed
        #expect(Self.violations(in: "// usleep(10) in a comment\n@Test func t() { #expect(1 == 1) }\n").isEmpty)
    }

    @Test func everyAllowedEntryStillNamesATestThatBlocks() throws {
        for e in Self.allowed {
            let text = try String(contentsOf: Self.testsDir.appendingPathComponent(e.file), encoding: .utf8)
            #expect(!e.why.isEmpty && Self.violations(in: text).contains { $0.test == e.test }, "stale allow-list entry \(e.file) \(e.test): remove it")
        }
    }
}
