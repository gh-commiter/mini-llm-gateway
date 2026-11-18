import Foundation
import Testing

/// The line labels of `minigw status`: the same fixture as client/status_parity_test.go (client/testdata/status-lines.json).
@Suite struct StatusParity {
    @Test func statusLinesMatchTheGoClient() throws {
        var repo = URL(fileURLWithPath: #filePath)
        for _ in 0..<3 { repo.deleteLastPathComponent() }
        let fx = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/status-lines.json"))) as! [String: Any]
        let want = Set(fx["labels"] as! [String])
        let src = try String(contentsOf: repo.appendingPathComponent("Sources/MiniGW/main.swift"), encoding: .utf8)
        let re = try NSRegularExpression(pattern: #""([A-Z][a-z]+):( +)"#)
        var got = Set<String>()
        for m in re.matches(in: src, range: NSRange(src.startIndex..., in: src)) {
            let label = String(src[Range(m.range(at: 1), in: src)!]), pad = m.range(at: 2).length
            #expect(label.count + 1 + pad == 11, "\(label) is not padded to column 11")
            got.insert(label)
        }
        #expect(got == want)
    }
}
