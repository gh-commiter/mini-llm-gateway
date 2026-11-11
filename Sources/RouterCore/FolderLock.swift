import Foundation

/// #82 (vmesno, kot lockFolder v client/share.go): branje (GET, HEAD, PROPFIND, SYNC-PUSH) deljeno, pisanje, SYNC-PULL in
/// worktree izključno — v procesu (pthread_rwlock) in med procesi (flock na datoteki, imenovani po istovetnosti korena, v
/// Application Support/minigw/locks, ne v projektu; ob sesutju ga sprosti jedro). Tako branje ne dirka z MOVE nadrejene
/// mape med preverbo poti in branjem, dve seji minigw na isti mapi pa ne pišeta hkrati. Isto ime datoteke kot Go.
/// ponytail: preveri-nato-uporabi ostane za lokalni proces brez zaklepa; popolno (operacije prek odprtega korena) 0.5.26.
public final class FolderLock: @unchecked Sendable {
    public static let busyText = "minigw: the shared folder is busy (another minigw session on this folder is using it); try again"
    public static let defaultDir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("minigw/locks").path
    /// Sync in worktree čakajo skoraj do roka posla prehoda (WORKTREE-PULL 300 s, drugi 900 s): 5 s bi delo agenta pustilo na prehodu.
    nonisolated(unsafe) public static var syncWait: TimeInterval = 880, worktreePullWait: TimeInterval = 280
    let path: String, wait: TimeInterval
    private let rw: UnsafeMutablePointer<pthread_rwlock_t>
    private let warned = NSLock()
    private var cantLock = false, lastBusy = Date.distantPast

    /// Ime datoteke zaklepa (kot folderLockName v Go): fileID ima st_dev že kot UInt32.
    public static func name(_ id: [UInt64]) -> String { id.map { String($0, radix: 16) }.joined(separator: "-") + ".lock" }

    /// id: istovetnost korena (SharedFolders.fileID); wait: privzeto čakanje posla WebDAV, nato nil (503), nikoli ne obvisi.
    public init(id: [UInt64], dir: String = FolderLock.defaultDir, wait: TimeInterval = 5) {
        path = dir + "/" + Self.name(id)
        self.wait = wait
        rw = .allocate(capacity: 1)
        pthread_rwlock_init(rw, nil)
    }

    deinit { pthread_rwlock_destroy(rw); rw.deallocate() }

    /// body pod zaklepom; nil = zaklepa ni bilo v roku (wait, sicer self.wait), tudi v tem procesu (L3).
    public func with<T>(exclusive: Bool, wait: TimeInterval? = nil, _ body: () -> T) -> T? {
        let deadline = Date().addingTimeInterval(wait ?? self.wait)
        while (exclusive ? pthread_rwlock_trywrlock(rw) : pthread_rwlock_tryrdlock(rw)) != 0 {
            if Date() > deadline { return busy() }
            usleep(5_000)
        }
        defer { pthread_rwlock_unlock(rw) }
        let dir = (path as NSString).deletingLastPathComponent
        try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        var ds = stat()
        guard lstat(dir, &ds) == 0, ds.st_mode & S_IFMT == S_IFDIR, ds.st_uid == getuid(), ds.st_mode & 0o022 == 0 else {  // L1
            return unlocked("\(dir) is not a private folder of this user", body)
        }
        let fd = open(path, O_RDWR | O_CREAT | O_NONBLOCK | O_NOFOLLOW | O_CLOEXEC, 0o600)
        var st = stat()
        guard fd >= 0, fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFREG, st.st_uid == getuid() else {
            let e = errno
            if fd >= 0 { close(fd) }
            return unlocked(e == 0 || fd >= 0 ? "\(path) is not a regular file of this user" : String(cString: strerror(e)), body)
        }
        defer { close(fd) }
        while flock(fd, (exclusive ? LOCK_EX : LOCK_SH) | LOCK_NB) != 0 {
            let e = errno
            guard e == EWOULDBLOCK || e == EINTR else { return unlocked(String(cString: strerror(e)), body) }
            if Date() > deadline { return busy() }
            usleep(10_000)
        }
        defer { flock(fd, LOCK_UN) }  // LOCK_UN pred close (kot ScratchOwner): kopija v otroku posix_spawn ga ne drži
        return body()
    }

    /// Vrstica ob vsaki novi zasedenosti, največ enkrat na 30 s.
    private func busy<T>() -> T? {
        let say = warned.withLock { let s = Date().timeIntervalSince(lastBusy) >= 30; lastBusy = Date(); return s }
        if say { try? FileHandle.standardError.write(contentsOf: Data((Self.busyText + "\n").utf8)) }
        return nil
    }

    /// Zaklep ni mogoč (npr. disk brez flock, tuja mapa): kot 0.5.24, le znotraj procesa; opozorilo enkrat.
    private func unlocked<T>(_ why: String, _ body: () -> T) -> T {
        if warned.withLock({ let first = !cantLock; cantLock = true; return first }) {
            try? FileHandle.standardError.write(contentsOf: Data("minigw: cannot lock the shared folder against other minigw sessions (\(why))\n".utf8))
        }
        return body()
    }
}
