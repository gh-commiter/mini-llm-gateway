import Foundation

/// #82 (vmesno, kot lockFolder v client/share.go): branje (GET, HEAD, PROPFIND) deljeno, pisanje, sync in worktree
/// izključno — v procesu (pthread_rwlock) in med procesi (flock na datoteki, imenovani po istovetnosti korena, v
/// Application Support/minigw/locks, ne v projektu; ob sesutju ga sprosti jedro). Tako branje ne dirka z MOVE nadrejene
/// mape med preverbo poti in branjem, dve seji minigw na isti mapi pa ne pišeta hkrati. Isto ime datoteke kot Go.
/// ponytail: preveri-nato-uporabi ostane za lokalni proces brez zaklepa; popolno (operacije prek odprtega korena) 0.5.26.
public final class FolderLock: @unchecked Sendable {
    public static let busyText = "minigw: the shared folder is busy (another minigw session on this folder is using it); try again"
    public static let defaultDir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("minigw/locks").path
    let path: String, wait: TimeInterval
    private let rw: UnsafeMutablePointer<pthread_rwlock_t>
    private let warned = NSLock()
    private var said: Set<String> = []

    /// id: istovetnost korena (SharedFolders.fileID); wait: čakanje drugega procesa, nato nil (503), nikoli ne obvisi.
    public init(id: [UInt64], dir: String = FolderLock.defaultDir, wait: TimeInterval = 5) {
        path = dir + "/" + id.map { String($0, radix: 16) }.joined(separator: "-") + ".lock"
        self.wait = wait
        rw = .allocate(capacity: 1)
        pthread_rwlock_init(rw, nil)
    }

    deinit { pthread_rwlock_destroy(rw); rw.deallocate() }

    /// body pod zaklepom; nil = drug proces ga drži dlje od wait.
    public func with<T>(exclusive: Bool, _ body: () -> T) -> T? {
        _ = exclusive ? pthread_rwlock_wrlock(rw) : pthread_rwlock_rdlock(rw)
        defer { pthread_rwlock_unlock(rw) }
        try? FileManager.default.createDirectory(atPath: (path as NSString).deletingLastPathComponent, withIntermediateDirectories: true,
                                                 attributes: [.posixPermissions: 0o700])
        let fd = open(path, O_RDWR | O_CREAT | O_NONBLOCK | O_NOFOLLOW | O_CLOEXEC, 0o600)
        var st = stat()
        guard fd >= 0, fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFREG else {
            let e = errno
            if fd >= 0 { close(fd) }
            return unlocked(e == 0 ? "not a regular file" : String(cString: strerror(e)), body)
        }
        defer { close(fd) }
        let deadline = Date().addingTimeInterval(wait)
        while flock(fd, (exclusive ? LOCK_EX : LOCK_SH) | LOCK_NB) != 0 {
            let e = errno
            guard e == EWOULDBLOCK || e == EINTR else { return unlocked(String(cString: strerror(e)), body) }
            if Date() > deadline { once(Self.busyText); return nil }
            usleep(10_000)
        }
        defer { flock(fd, LOCK_UN) }  // LOCK_UN pred close (kot ScratchOwner): kopija v otroku posix_spawn ga ne drži
        return body()
    }

    /// Zaklep ni mogoč (npr. disk brez flock): kot 0.5.24, le znotraj procesa; opozorilo enkrat.
    private func unlocked<T>(_ why: String, _ body: () -> T) -> T {
        once("minigw: cannot lock the shared folder against other minigw sessions (\(why))")
        return body()
    }

    private func once(_ line: String) {
        let key = String(line.prefix(30))
        if warned.withLock({ said.insert(key).inserted }) { try? FileHandle.standardError.write(contentsOf: Data((line + "\n").utf8)) }
    }
}
