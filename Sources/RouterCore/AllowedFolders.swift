import Foundation
import Darwin
import os

/// Trust lasts until a folder is removed from the list, including across Router rebuilds.
public final class AllowedFolders: @unchecked Sendable {
    private struct Entry {
        var real: String?
        var missing: Bool
        var logged = false
    }
    private let lock = NSLock()
    private var entries: [String: Entry] = [:]

    public init() {}

    static func paths(_ roots: String) -> [String] {
        roots.split(separator: ",").compactMap {
            let p = ($0.trimmingCharacters(in: .whitespaces) as NSString).expandingTildeInPath
            guard p.hasPrefix("/"), p != "/" else { return nil }
            return URL(fileURLWithPath: p).standardized.path  // not standardizedFileURL: it drops /private
        }
    }

    public func record(_ roots: String) {
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
                    entries[p] = Entry(real: Sync.realResolving(URL(fileURLWithPath: p))?.path, missing: missing)
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
                var why = ""
                if r == nil { why = "cannot resolve its real path" }
                else if r != e.real { why = "its real path changed to " + r! }
                else if e.missing && r != p { why = "it is reached through a link to " + r! }
                else if !Self.owned(r!) || (e.missing && !Self.owned(p)) { why = "it is a link or owned by another user" }
                if !why.isEmpty {
                    if !e.logged {
                        e.logged = true
                        log("WARNING: allowed folder \(p) is not used: \(why); if you trust it, remove and add it again in the allowed folders")
                    }
                    return nil
                }
                var identity = stat()
                guard lstat(r!, &identity) == 0, identity.st_mode & S_IFMT == S_IFDIR,
                      identity.st_uid == getuid() else { return nil }
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
