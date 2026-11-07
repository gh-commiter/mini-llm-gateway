import Foundation

/// Naprave, ki čakajo na odobritev (največ 20, kot minigw serve), in zadnji stik dovoljenih. Le v pomnilniku aplikacije:
/// preživi ponovni zagon routerja (sprememba nastavitev), ne aplikacije. `onChange`: aplikacija osveži meni in Settings.
public final class DeviceLog: @unchecked Sendable {
    public struct Pending: Sendable, Equatable, Identifiable {
        public let id: String, name: String, address: String
        public let time: Date
    }
    private let lock = NSLock()
    private var waiting: [Pending] = []
    private var seenAt: [String: Date] = [:]
    private var change: (@Sendable () -> Void)?

    public init() {}

    public var onChange: (@Sendable () -> Void)? {
        get { lock.withLock { change } }
        set { lock.withLock { change = newValue } }
    }

    public var pending: [Pending] { lock.withLock { waiting } }

    public func lastSeen(_ id: String) -> Date? { lock.withLock { seenAt[id] } }

    public func seen(_ id: String) { lock.withLock { seenAt[id] = Date() } }

    public func addPending(id: String, name: String, address: String) {
        lock.withLock {
            waiting.removeAll { $0.id == id }
            waiting.append(Pending(id: id, name: name, address: address, time: Date()))
            if waiting.count > 20 { waiting.removeFirst(waiting.count - 20) }
        }
        onChange?()
    }

    @discardableResult
    public func removePending(_ id: String) -> Pending? {
        let p: Pending? = lock.withLock {
            guard let i = waiting.firstIndex(where: { $0.id == id }) else { return nil }
            return waiting.remove(at: i)
        }
        if p != nil { onChange?() }
        return p
    }
}

extension Router {
    public static let deviceLog = DeviceLog()
}
