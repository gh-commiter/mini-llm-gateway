import Foundation

/// The request of the project and note commands (client/project_cmd.go realDo): like Profiles' other calls (no redirects), but with the
/// response header Retry-After. Status 0 = no answer. A reply is read at most 8 MiB.
public enum ProjectHTTP {
    public static func send(_ c: ProjectConn, _ method: String, _ path: String, _ headers: [String: String], _ body: Data?, clientInfo: String,
                            timeout: TimeInterval = 30) -> ProjectReply {
        guard let url = URL(string: c.url + path) else { return ProjectReply(status: 0, data: Data("bad gateway address".utf8)) }
        var r = URLRequest(url: url, timeoutInterval: timeout)
        r.httpMethod = method
        r.httpBody = body
        r.setValue("Bearer " + c.token, forHTTPHeaderField: "authorization")
        r.setValue(clientInfo, forHTTPHeaderField: "X-Minigw-Client")
        if body != nil { r.setValue("application/json", forHTTPHeaderField: "content-type") }
        for (k, v) in headers { r.setValue(v, forHTTPHeaderField: k) }
        let done = DispatchSemaphore(value: 0)
        nonisolated(unsafe) var reply = ProjectReply(status: 0)
        Profiles.session.dataTask(with: r) { data, resp, error in  // L2: no redirects
            if let h = resp as? HTTPURLResponse {
                reply = ProjectReply(status: h.statusCode, data: (data ?? Data()).prefix(8 << 20), retryAfter: h.value(forHTTPHeaderField: "Retry-After"))
            } else if let error {
                reply = ProjectReply(status: 0, data: Data(cause(error, timeout: timeout).utf8))  // status 0 carries the short cause as its body
            }
            done.signal()
        }.resume()
        done.wait()
        return reply
    }

    /// Why there was no answer, in a few fixed words the Go client uses too (transportCause in client/project_cmd.go): never the URL, a header or the system's own text.
    /// "" = none of the known causes.
    static func cause(_ error: Error, timeout: TimeInterval) -> String {
        let ns = error as NSError
        // URLSession hides the errno in the stream keys (domain 1 = POSIX): -1004 with code 61 is ECONNREFUSED
        let stream = (ns.userInfo["_kCFStreamErrorDomainKey"] as? Int) == 1 ? (ns.userInfo["_kCFStreamErrorCodeKey"] as? Int).map { Int32($0) } : nil
        let posix = stream ?? (ns.userInfo[NSUnderlyingErrorKey] as? NSError).flatMap { $0.domain == NSPOSIXErrorDomain ? Int32($0.code) : nil }
            ?? (ns.domain == NSPOSIXErrorDomain ? Int32(ns.code) : nil)
        switch posix {
        case ECONNREFUSED?: return "connection refused"
        case ECONNRESET?, EPIPE?: return "connection reset"
        case ENETUNREACH?, EHOSTUNREACH?, ENETDOWN?: return "network unreachable"
        default: break
        }
        guard ns.domain == NSURLErrorDomain else { return "" }
        switch ns.code {
        case NSURLErrorTimedOut: return "timed out after \(Int(timeout)) s"
        case NSURLErrorCannotFindHost, NSURLErrorDNSLookupFailed: return "could not resolve the host"
        case NSURLErrorNetworkConnectionLost: return "connection reset"
        case NSURLErrorNotConnectedToInternet: return "network unreachable"
        case NSURLErrorServerCertificateUntrusted, NSURLErrorServerCertificateHasBadDate, NSURLErrorServerCertificateHasUnknownRoot, NSURLErrorServerCertificateNotYetValid: return "TLS: certificate not trusted"
        case NSURLErrorSecureConnectionFailed, NSURLErrorClientCertificateRejected, NSURLErrorClientCertificateRequired: return "TLS: handshake failed"
        default: return ""
        }
    }
}
