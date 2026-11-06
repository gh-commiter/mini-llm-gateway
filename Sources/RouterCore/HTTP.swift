import Foundation

public struct HTTPRequest: Sendable {
    public var method: String
    public var path: String
    public var query: String?
    public var headers: [String: String]  // imena v malih črkah
    public var body: Data
    public var peer = ""  // IP odjemalca
    public var device: Noise.Device?  // minigw z dokazom naprave (šifrirana povezava)

    public func header(_ name: String) -> String? { headers[name.lowercased()] }
}

public enum ParseResult: Sendable {
    case needMore
    case request(HTTPRequest)
    case error(Int, String)
}

/// HTTP/1.1 brez keep-alive: ena zahteva na povezavo, odgovor se zaključi z zaprtjem.
public enum HTTPParser {
    public static let maxHead = 64 * 1024
    public static let maxBody = 64 * 1024 * 1024

    public static func parse(_ buf: Data) -> ParseResult {
        guard let sep = buf.range(of: Data("\r\n\r\n".utf8)) else {
            return buf.count > maxHead ? .error(431, "Request header too large") : .needMore
        }
        guard sep.lowerBound - buf.startIndex <= maxHead,
              let head = String(data: buf.subdata(in: buf.startIndex..<sep.lowerBound), encoding: .utf8)
        else { return .error(400, "Bad request header") }

        var lines = head.components(separatedBy: "\r\n")
        let first = lines.removeFirst().split(separator: " ")
        guard first.count == 3, first[2].hasPrefix("HTTP/1.") else { return .error(400, "Bad request line") }

        var headers: [String: String] = [:]
        for line in lines {
            guard let colon = line.firstIndex(of: ":") else { return .error(400, "Bad header line") }
            headers[line[..<colon].trimmingCharacters(in: .whitespaces).lowercased()] =
                line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
        }
        if headers["transfer-encoding"] != nil { return .error(411, "Chunked request bodies are not supported") }
        guard let length = Int(headers["content-length"] ?? "0"), length >= 0 else { return .error(400, "Bad Content-Length") }
        guard length <= maxBody else { return .error(413, "Request body too large") }

        let bodyStart = sep.upperBound
        guard buf.endIndex - bodyStart >= length else { return .needMore }

        let target = first[1].split(separator: "?", maxSplits: 1)
        return .request(HTTPRequest(
            method: String(first[0]),
            path: String(target.first ?? ""),
            query: target.count > 1 ? String(target[1]) : nil,
            headers: headers,
            body: buf.subdata(in: bodyStart..<bodyStart + length)
        ))
    }
}

public enum HTTPResponse {
    public static func head(status: Int, headers: [(String, String)], contentLength: Int? = nil) -> Data {
        var s = "HTTP/1.1 \(status) \(reason(status))\r\n"
        for (k, v) in headers { s += "\(k): \(v)\r\n" }
        if let n = contentLength { s += "Content-Length: \(n)\r\n" }
        s += "Connection: close\r\n\r\n"
        return Data(s.utf8)
    }

    static func reason(_ status: Int) -> String {
        [200: "OK", 201: "Created", 204: "No Content", 206: "Partial Content", 207: "Multi-Status",
         400: "Bad Request", 401: "Unauthorized", 403: "Forbidden", 404: "Not Found", 405: "Method Not Allowed", 409: "Conflict", 412: "Precondition Failed", 426: "Upgrade Required",
         411: "Length Required", 413: "Payload Too Large", 429: "Too Many Requests", 431: "Request Header Fields Too Large",
         500: "Internal Server Error", 502: "Bad Gateway", 503: "Service Unavailable", 504: "Gateway Timeout"][status] ?? "Status"
    }
}

public protocol ResponseWriter: Sendable {
    func write(_ data: Data) async throws
    func close()
}
