import Foundation

/// Naprave, ki čakajo na odobritev (največ 20, kot minigw serve), in zadnji stik dovoljenih. Le v pomnilniku aplikacije:
/// preživi ponovni zagon routerja (sprememba nastavitev), ne aplikacije. `onChange`: aplikacija osveži meni in Settings.
/// L1: poln seznam obdrži najstarejše čakajoče (novi ključi jih ne izrinejo); onChange največ enkrat na sekundo.
public final class DeviceLog: @unchecked Sendable {
    public struct Pending: Sendable, Equatable, Identifiable {
        public let id: String, name: String, address: String
        public let client: String  // OS in različica minigw (glava x-minigw-client); prazno pri starejših odjemalcih
        public let time: Date
    }
    public static let limit = 20
    private let lock = NSLock()
    private var waiting: [Pending] = []
    private var seenAt: [String: Date] = [:]
    private var clients: [String: String] = [:]  // OS in različica dovoljene naprave (zadnja neprazna glava)
    private var change: (@Sendable () -> Void)?
    private var lastNotify = Date.distantPast, notifyQueued = false

    public init() {}

    public var onChange: (@Sendable () -> Void)? {
        get { lock.withLock { change } }
        set { lock.withLock { change = newValue } }
    }

    public var pending: [Pending] { lock.withLock { waiting } }

    public func lastSeen(_ id: String) -> Date? { lock.withLock { seenAt[id] } }

    public func seen(_ id: String, client: String = "") {
        lock.withLock {
            seenAt[id] = Date()
            if !client.isEmpty { clients[id] = client }
        }
    }

    public func client(_ id: String) -> String? { lock.withLock { clients[id] } }

    public func addPending(id: String, name: String, address: String, client: String = "") {
        let changed: Bool = lock.withLock {
            if let i = waiting.firstIndex(where: { $0.id == id }) {  // ista naprava: osveži, mesto ostane
                waiting[i] = Pending(id: id, name: name, address: address, client: client.isEmpty ? waiting[i].client : client, time: Date())
                return true
            }
            guard waiting.count < Self.limit else { return false }
            waiting.append(Pending(id: id, name: name, address: address, client: client, time: Date()))
            return true
        }
        if changed { notify() }
    }

    @discardableResult
    public func removePending(_ id: String) -> Pending? {
        let p: Pending? = lock.withLock {
            guard let i = waiting.firstIndex(where: { $0.id == id }) else { return nil }
            return waiting.remove(at: i)
        }
        if p != nil { notify() }
        return p
    }

    /// Največ enkrat na sekundo; sprememba med tem pride z zakasnjenim klicem.
    private func notify() {
        let (now, f): (Bool, (@Sendable () -> Void)?) = lock.withLock {
            let wait = 1 - Date().timeIntervalSince(lastNotify)
            if wait <= 0 { lastNotify = Date(); return (true, change) }
            if notifyQueued { return (false, nil) }
            notifyQueued = true
            DispatchQueue.global().asyncAfter(deadline: .now() + wait) { [weak self] in
                guard let self else { return }
                let f = self.lock.withLock { () -> (@Sendable () -> Void)? in
                    self.notifyQueued = false
                    self.lastNotify = Date()
                    return self.change
                }
                f?()
            }
            return (false, nil)
        }
        if now { f?() }
    }
}

extension Router {
    public static let deviceLog = DeviceLog()
}
