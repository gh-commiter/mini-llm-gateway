import Foundation

/// The request of the project and note commands (client/project_cmd.go realDo): like Profiles' other calls (no redirects), but with the
/// response header Retry-After. Status 0 = no answer. A reply is read at most 8 MiB.
public enum ProjectHTTP {
    public static func send(_ c: ProjectConn, _ method: String, _ path: String, _ headers: [String: String], _ body: Data?, clientInfo: String,
                            timeout: TimeInterval = 30) -> ProjectReply {
        guard let url = URL(string: c.url + path) else { return ProjectReply(status: 0) }
        var r = URLRequest(url: url, timeoutInterval: timeout)
        r.httpMethod = method
        r.httpBody = body
        r.setValue("Bearer " + c.token, forHTTPHeaderField: "authorization")
        r.setValue(clientInfo, forHTTPHeaderField: "X-Minigw-Client")
        if body != nil { r.setValue("application/json", forHTTPHeaderField: "content-type") }
        for (k, v) in headers { r.setValue(v, forHTTPHeaderField: k) }
        let done = DispatchSemaphore(value: 0)
        nonisolated(unsafe) var reply = ProjectReply(status: 0)
        Profiles.session.dataTask(with: r) { data, resp, _ in  // L2: no redirects
            if let h = resp as? HTTPURLResponse {
                reply = ProjectReply(status: h.statusCode, data: (data ?? Data()).prefix(8 << 20), retryAfter: h.value(forHTTPHeaderField: "Retry-After"))
            }
            done.signal()
        }.resume()
        done.wait()
        return reply
    }
}
