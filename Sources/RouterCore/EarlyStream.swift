import Foundation

/// #4: odgovor s pretokom, ki se začne pred prenosom mape. Takoj pošlje glavo 200 (SSE) in komentarje »: keep-alive«
/// (vsak razčlenjevalnik SSE jih prezre). Ko ponudnik začne pisati: njegovo glavo 200 izpusti, napako (glava ne-200 in
/// JSON) pa pretvori v dogodek SSE »error«; vse drugo gre naprej nespremenjeno.
final class EarlyStreamWriter: ResponseWriter, @unchecked Sendable {
    private let inner: ResponseWriter
    private let lock = NSLock()
    private var started = false
    private var pinger: Task<Void, Never>?
    nonisolated(unsafe) static var interval: Duration = .seconds(10)  // testi: krajši

    init(_ inner: ResponseWriter) { self.inner = inner }

    func begin(rid: String) async {
        try? await inner.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "text/event-stream"), ("Cache-Control", "no-cache"),
                                                                         ("request-id", rid)]) + Data(": syncing the folder\n\n".utf8))
        pinger = Task { [inner] in
            while !Task.isCancelled {
                try? await Task.sleep(for: Self.interval)
                if Task.isCancelled { break }
                try? await inner.write(Data(": keep-alive\n\n".utf8))
            }
        }
    }

    func write(_ data: Data) async throws {
        let first = lock.withLock { () -> Bool in defer { started = true }; return !started }
        guard first else { return try await inner.write(data) }
        pinger?.cancel()
        guard data.starts(with: Data("HTTP/1.1 ".utf8)), let end = data.firstRange(of: Data("\r\n\r\n".utf8)) else {
            return try await inner.write(data)
        }
        let head = String(decoding: data[..<end.lowerBound], as: UTF8.self)
        let rest = data[end.upperBound...]
        if head.hasPrefix("HTTP/1.1 200") {
            if !rest.isEmpty { try await inner.write(Data(rest)) }
            return
        }
        let err = (try? JSONSerialization.jsonObject(with: Data(rest)) as? [String: Any])?["error"] as? [String: Any]
        try await inner.write(StreamTranslator.sse("error", ["type": "error", "error": [
            "type": err?["type"] as? String ?? "api_error", "message": err?["message"] as? String ?? "request failed"]]))
    }

    func close() { pinger?.cancel(); inner.close() }
}
