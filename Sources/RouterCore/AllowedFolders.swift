import Foundation
import Darwin
import os

/// M2: the real path and identity of an allowed folder, recorded when it is added (or at the first start of a version
/// that has records) and saved with the settings. Start compares and refuses a folder that differs; it never rewrites it.
/// B1: the identity is the volume UUID plus the inode, never st_dev (macOS assigns it at mount, so it changes for external
/// and network disks). `volume` nil = the volume has no UUID (some network filesystems): the inode alone is compared.
/// `ino` 0 = the folder did not exist when it was recorded: it adopts its identity at its first accepted use (B2).
public struct FolderRecord: Codable, Sendable, Equatable {
    public var real: String
    public var volume: String?
    public var ino: UInt64 = 0

    public init(real: String, volume: String? = nil, ino: UInt64 = 0) {
        self.real = real; self.volume = volume; self.ino = ino
    }

    public init(from decoder: Decoder) throws {  // an older record (dev, ino, no volume) still reads: its dev is ignored
        let c = try decoder.container(keyedBy: CodingKeys.self)
        real = try c.decode(String.self, forKey: .real)
        volume = try c.decodeIfPresent(String.self, forKey: .volume)
        ino = try c.decodeIfPresent(UInt64.self, forKey: .ino) ?? 0
    }

    /// The recorded identity matches `volume`/`ino` now; volumes are compared only when both are known.
    func matches(volume now: String?, ino now2: UInt64) -> Bool {
        ino == 0 || ino == now2 && (volume == nil || now == nil || volume == now)
    }
}

extension ClaudeCLIConfig {
    /// Records every allowed folder that has no record yet and drops the records of removed folders. Returns the new ones
    /// (folder, real path) for the log. An existing record is never changed.
    @discardableResult
    public mutating func recordFolders(volumeID: (String) -> String? = AllowedFolders.volumeUUID) -> [(folder: String, real: String)] {
        var records: [String: FolderRecord] = [:], added: [(String, String)] = []
        for p in AllowedFolders.paths(allowedRoots) {
            if let old = allowedRecords[p] { records[p] = old; continue }
            guard let real = Sync.realResolving(URL(fileURLWithPath: p))?.path else { continue }
            var rec = FolderRecord(real: real), st = stat()
            if lstat(real, &st) == 0 { rec.volume = volumeID(real); rec.ino = UInt64(st.st_ino) }
            records[p] = rec
            added.append((p, real))
        }
        allowedRecords = records
        return added
    }
}

/// Trust lasts until a folder is removed from the list, including across Router rebuilds.
public final class AllowedFolders: @unchecked Sendable {
    private struct Entry {
        var real: String?
        var missing: Bool
        var logged = false
        var record: FolderRecord?  // M2, A1: the recorded identity; compared on every check (ino 0: adopted at the first use, B2)
    }
    private let lock = NSLock()
    /// B2: called with a record that has just adopted its identity; the owner saves it (must not call back in).
    public var onAdopt: (@Sendable (String, FolderRecord) -> Void)?
    private let volumeID: (String) -> String?
    private var entries: [String: Entry] = [:]

    public init(volumeID: @escaping (String) -> String? = AllowedFolders.volumeUUID) { self.volumeID = volumeID }

    public static func volumeUUID(_ p: String) -> String? {
        let v = try? URL(fileURLWithPath: p).resourceValues(forKeys: [.volumeUUIDStringKey]).volumeUUIDString
        return v?.isEmpty == false ? v : nil
    }

    /// A comma-separated list as typed in settings; checked paths travel as arrays (a real path may contain a comma).
    static func list(_ roots: String) -> [String] {
        roots.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }
    }

    static func paths(_ roots: String) -> [String] {
        roots.split(separator: ",").compactMap {
            let p = ($0.trimmingCharacters(in: .whitespaces) as NSString).expandingTildeInPath
            guard p.hasPrefix("/"), p != "/" else { return nil }
            return URL(fileURLWithPath: p).standardized.path  // not standardizedFileURL: it drops /private
        }
    }

    public func record(_ roots: String, records: [String: FolderRecord] = [:]) {
        lock.withLock {
            let paths = Self.paths(roots)
            entries = entries.filter { paths.contains($0.key) }
            for p in paths {
                var st = stat()
                let missing = stat(p, &st) != 0 && errno == ENOENT
                if var e = entries[p] {
                    e.missing = e.missing || missing
                    entries[p] = e
                } else {
                    var e = Entry(real: Sync.realResolving(URL(fileURLWithPath: p))?.path, missing: missing)
                    if let rec = records[p] {  // M2: the saved record, not the current target
                        e.record = rec
                        e.real = rec.real
                    }
                    entries[p] = e
                }
            }
        }
    }

    struct Checked: Sendable {
        struct Folder: Sendable {
            let path: String
            let device: dev_t
            let inode: ino_t
        }
        var folders: [Folder] = []
        var paths: [String] { folders.map(\.path) }
        /// The paths for the agent's arguments without the cwd itself (it is passed already); checked paths are real paths.
        func paths(without cwd: URL) -> [String] {
            let real = Sync.real(cwd)?.path
            return paths.filter { $0 != cwd.path && $0 != real }
        }

        func open() throws -> [FileHandle] {
            var held: [FileHandle] = []
            do {
                for f in folders {
                    let fd = Darwin.open(f.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
                    guard fd >= 0 else { throw CocoaError(.fileReadNoPermission) }
                    let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
                    held.append(handle)
                    var st = stat()
                    guard fstat(fd, &st) == 0, st.st_dev == f.device, st.st_ino == f.inode,
                          st.st_uid == getuid() else { throw CocoaError(.fileReadNoPermission) }
                }
                return held
            } catch {
                for handle in held { try? handle.close() }
                throw error
            }
        }
    }

    func folders(_ roots: String, excluding cwd: String = "", scratch: URL? = nil,
                 inspect: (String) -> (directory: Bool, error: Int32) = AllowedFolders.inspect,
                 log: (String) -> Void = { Router.log.warning("\($0, privacy: .public)") }) -> [String] {
        checked(roots, excluding: cwd, scratch: scratch, inspect: inspect, log: log).paths
    }

    func checked(_ roots: String, excluding cwd: String = "", scratch: URL? = nil,
                 inspect: (String) -> (directory: Bool, error: Int32) = AllowedFolders.inspect,
                 log: (String) -> Void = { Router.log.warning("\($0, privacy: .public)") }) -> Checked {
        lock.withLock {
            let folders: [Checked.Folder] = Self.paths(roots).compactMap { p in
                guard p != cwd, var e = entries[p] else { return nil }
                defer { entries[p] = e }
                let st = inspect(p)
                guard st.directory else {
                    if st.error == ENOENT { e.missing = true }
                    return nil
                }
                guard !(scratch.map { RouterConfig.overlapsScratch(p, scratch: $0) } ?? false),
                      !RouterConfig.overlapsCopilotCache(p) else { return nil }
                let config = URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent("Library/Preferences")
                guard !RouterConfig.overlapsScratch(p, scratch: config) else { return nil }
                let r = Sync.real(URL(fileURLWithPath: p))?.path
                var identity = stat()
                let have = r.map { lstat($0, &identity) == 0 } ?? false
                var why = ""
                if r == nil { why = "cannot resolve its real path" }
                else if r != e.real { why = "its real path changed to " + r! }
                else if have, let rec = e.record, !rec.matches(volume: volumeID(r!), ino: UInt64(identity.st_ino)) { why = "its identity changed (another folder is there now)" }
                else if e.missing && r != p { why = "it is reached through a link to " + r! }
                else if !Self.owned(r!) || (e.missing && !Self.owned(p)) { why = "it is a link or owned by another user" }
                if !why.isEmpty {
                    if !e.logged {
                        e.logged = true
                        log("WARNING: allowed folder \(p) is not used: \(why); if you trust it, remove and add it again in the allowed folders")
                    }
                    return nil
                }
                guard have, identity.st_mode & S_IFMT == S_IFDIR,
                      identity.st_uid == getuid() else { return nil }
                if var rec = e.record, rec.ino == 0 {  // B2: recorded while missing: trust on first use
                    rec.volume = volumeID(r!)
                    rec.ino = UInt64(identity.st_ino)
                    e.record = rec
                    onAdopt?(p, rec)
                }
                return Checked.Folder(path: r!, device: identity.st_dev, inode: identity.st_ino)
            }
            return Checked(folders: folders)
        }
    }

    private static func inspect(_ p: String) -> (directory: Bool, error: Int32) {
        var st = stat()
        guard stat(p, &st) == 0 else { return (false, errno) }
        return (st.st_mode & S_IFMT == S_IFDIR, 0)
    }

    private static func owned(_ p: String) -> Bool {
        var st = stat()
        return lstat(p, &st) == 0 && st.st_mode & S_IFMT == S_IFDIR && st.st_uid == getuid()
    }
}
