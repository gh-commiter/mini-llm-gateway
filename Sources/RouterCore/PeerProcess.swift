import Foundation
import Darwin

/// Kdo je na drugi strani lokalne povezave TCP (skrbniški API). Agenti (Codex, Grok, Claude Code) tečejo kot otroci
/// aplikacije in v sandboxu: tudi z ukradeno skrivnostjo ali z zagonom minigw ne smejo spreminjati nastavitev.
/// LOCAL_PEERPID deluje le za vtičnice Unix, zato libproc: proces z vtičnico TCP <vrata odjemalca> → <vrata routerja>.
public enum PeerProcess {
    /// nil = dovoljeno; sicer razlog. Obramba v globino (za dokazom MGW-Admin): zavrne, če proces ni natanko eden
    /// (drug uporabnik, že končan, deljena vtičnica), če je v sandboxu ali potomec aplikacije, ali če veriga staršev
    /// ni preverljiva do PID 1.
    public static func refusal(peerPort: UInt16, routerPort: UInt16, appPID: pid_t = getpid()) -> String? {
        let pids = Set(self.pids(localPort: peerPort, remotePort: routerPort))
        guard pids.count == 1, let pid = pids.first else {
            return pids.isEmpty ? "cannot identify the process on this Mac that sent the request"
                : "several processes share the connection to the admin API"
        }
        if isSandboxed(pid) { return "admin requests from agent processes are not allowed (sandboxed process)" }
        var p = pid
        for _ in 0..<64 {  // otrok, vnuk … aplikacije: agent ali ukaz agenta
            if p == appPID { return "admin requests from agent processes are not allowed" }
            if p == 1 { return nil }
            guard let pp = parent(p), pp != p else { return "cannot check the parent processes of the client" }
            p = pp
        }
        return "cannot check the parent processes of the client"
    }

    /// Pot izvršljive datoteke procesa (jedro, ni mogoče ponarediti).
    static func path(_ pid: pid_t) -> String? {
        var buf = [CChar](repeating: 0, count: 4096)
        return proc_pidpath(pid, &buf, UInt32(buf.count)) > 0 ? String(cString: buf) : nil
    }

    /// Sistemski webdavfs_agent (pod SIP): edini, ki sme priklopiti mapo odjemalca.
    static func isWebDAVAgent(_ pid: pid_t) -> Bool {
        guard let p = path(pid) else { return false }
        return (p.hasPrefix("/System/") || p.hasPrefix("/usr/libexec/") || p.hasPrefix("/usr/sbin/")) && p.split(separator: "/").last == "webdavfs_agent"
    }

    /// Ali ima ta proces vtičnico TCP lokalna → oddaljena vrata (hitro preverjanje enega procesa).
    static func owns(_ pid: pid_t, localPort: UInt16, remotePort: UInt16) -> Bool {
        let size = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, nil, 0)
        guard size > 0 else { return false }
        var fds = [proc_fdinfo](repeating: proc_fdinfo(), count: Int(size) / MemoryLayout<proc_fdinfo>.stride + 8)
        let got = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, &fds, Int32(fds.count * MemoryLayout<proc_fdinfo>.stride))
        let infoSize = Int32(MemoryLayout<socket_fdinfo>.size)
        for fd in fds.prefix(max(0, Int(got)) / MemoryLayout<proc_fdinfo>.stride) where fd.proc_fdtype == PROX_FDTYPE_SOCKET {
            var si = socket_fdinfo()
            guard proc_pidfdinfo(pid, fd.proc_fd, PROC_PIDFDSOCKETINFO, &si, infoSize) == infoSize, si.psi.soi_kind == SOCKINFO_TCP else { continue }
            let ini = si.psi.soi_proto.pri_tcp.tcpsi_ini
            if UInt16(bigEndian: UInt16(truncatingIfNeeded: ini.insi_lport)) == localPort,
               UInt16(bigEndian: UInt16(truncatingIfNeeded: ini.insi_fport)) == remotePort { return true }
        }
        return false
    }

    /// Foundry Local (F2): vsi procesi, ki poslušajo na tem portu TCP, so tega uporabnika (procesov drugih uporabnikov
    /// libproc ne pokaže, zato tak port ostane brez najdenega poslušalca in velja za tujega).
    /// `installDir` (L3): poslušalec mora biti tudi program iz te mape (proc_pidpath, kanonično), ne le istega uporabnika:
    /// agent istega uporabnika bi lahko poslušal na vratih Foundry.
    public static func listenerIsOurs(port: UInt16, installDir: String? = nil) -> Bool {
        let n = proc_listallpids(nil, 0)
        guard n > 0 else { return false }
        var pids = [pid_t](repeating: 0, count: Int(n) + 64)
        let count = proc_listallpids(&pids, Int32(pids.count * MemoryLayout<pid_t>.size))
        let infoSize = Int32(MemoryLayout<socket_fdinfo>.size)
        var found = false
        for pid in pids.prefix(max(0, Int(count))) where pid > 0 {
            let size = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, nil, 0)
            guard size > 0 else { continue }
            var fds = [proc_fdinfo](repeating: proc_fdinfo(), count: Int(size) / MemoryLayout<proc_fdinfo>.stride + 8)
            let got = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, &fds, Int32(fds.count * MemoryLayout<proc_fdinfo>.stride))
            for fd in fds.prefix(max(0, Int(got)) / MemoryLayout<proc_fdinfo>.stride) where fd.proc_fdtype == PROX_FDTYPE_SOCKET {
                var si = socket_fdinfo()
                guard proc_pidfdinfo(pid, fd.proc_fd, PROC_PIDFDSOCKETINFO, &si, infoSize) == infoSize, si.psi.soi_kind == SOCKINFO_TCP,
                      si.psi.soi_proto.pri_tcp.tcpsi_state == TSI_S_LISTEN,
                      UInt16(bigEndian: UInt16(truncatingIfNeeded: si.psi.soi_proto.pri_tcp.tcpsi_ini.insi_lport)) == port else { continue }
                var bsd = proc_bsdinfo()
                guard proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &bsd, Int32(MemoryLayout<proc_bsdinfo>.size)) == Int32(MemoryLayout<proc_bsdinfo>.size),
                      bsd.pbi_uid == getuid() else { return false }
                if let installDir {
                    var buf = [CChar](repeating: 0, count: Int(MAXPATHLEN) * 4)
                    guard proc_pidpath(pid, &buf, UInt32(buf.count)) > 0,
                          let real = Sync.real(URL(fileURLWithPath: String(cString: buf)))?.path,
                          real.hasPrefix(installDir.hasSuffix("/") ? installDir : installDir + "/") else { return false }
                }
                found = true
            }
        }
        return found
    }

    static func pid(localPort: UInt16, remotePort: UInt16) -> pid_t? {
        let all = Set(pids(localPort: localPort, remotePort: remotePort))
        return all.count == 1 ? all.first : nil
    }

    /// Vsi procesi (istega uporabnika) z vtičnico TCP na loopbacku: lokalna → oddaljena vrata, oba naslova loopback.
    static func pids(localPort: UInt16, remotePort: UInt16) -> [pid_t] {
        let n = proc_listallpids(nil, 0)
        guard n > 0 else { return [] }
        var pids = [pid_t](repeating: 0, count: Int(n) + 64)
        let count = proc_listallpids(&pids, Int32(pids.count * MemoryLayout<pid_t>.size))
        let infoSize = Int32(MemoryLayout<socket_fdinfo>.size)
        var out: [pid_t] = []
        for pid in pids.prefix(max(0, Int(count))) where pid > 0 {
            let size = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, nil, 0)
            guard size > 0 else { continue }
            var fds = [proc_fdinfo](repeating: proc_fdinfo(), count: Int(size) / MemoryLayout<proc_fdinfo>.stride + 8)
            let got = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, &fds, Int32(fds.count * MemoryLayout<proc_fdinfo>.stride))
            for fd in fds.prefix(max(0, Int(got)) / MemoryLayout<proc_fdinfo>.stride) where fd.proc_fdtype == PROX_FDTYPE_SOCKET {
                var si = socket_fdinfo()
                guard proc_pidfdinfo(pid, fd.proc_fd, PROC_PIDFDSOCKETINFO, &si, infoSize) == infoSize,
                      si.psi.soi_kind == SOCKINFO_TCP else { continue }
                let ini = si.psi.soi_proto.pri_tcp.tcpsi_ini
                if UInt16(bigEndian: UInt16(truncatingIfNeeded: ini.insi_lport)) == localPort,
                   UInt16(bigEndian: UInt16(truncatingIfNeeded: ini.insi_fport)) == remotePort,
                   loopback(ini, local: true), loopback(ini, local: false) { out.append(pid) }
            }
        }
        return out
    }

    /// Naslov vtičnice je loopback v pravi družini (insi_vflag): 127/8, ::1 ali ::ffff:127/104.
    static func loopback(_ ini: in_sockinfo, local: Bool) -> Bool {
        let v4 = local ? ini.insi_laddr.ina_46.i46a_addr4.s_addr : ini.insi_faddr.ina_46.i46a_addr4.s_addr
        let v6 = local ? ini.insi_laddr.ina_6 : ini.insi_faddr.ina_6
        if ini.insi_vflag & UInt8(INI_IPV4) != 0 {
            return UInt32(bigEndian: v4) >> 24 == 127
        }
        if ini.insi_vflag & UInt8(INI_IPV6) != 0 {
            let b = withUnsafeBytes(of: v6) { Array($0) }
            if b[0..<15].allSatisfy({ $0 == 0 }) && b[15] == 1 { return true }  // ::1
            return b[0..<10].allSatisfy({ $0 == 0 }) && b[10] == 0xff && b[11] == 0xff && b[12] == 127
        }
        return false
    }

    /// Starš iz sysctl KERN_PROC_PID (kp_eproc.e_ppid): deluje tudi za procese drugih uporabnikov (npr. root »login«,
    /// ki ga Terminal zažene pred vsako lupino), kar proc_pidinfo ne.
    static func parent(_ pid: pid_t) -> pid_t? {
        var info = kinfo_proc()
        var size = MemoryLayout<kinfo_proc>.stride
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, pid]
        guard sysctl(&mib, 4, &info, &size, nil, 0) == 0, size == MemoryLayout<kinfo_proc>.stride else { return nil }
        return info.kp_eproc.e_ppid
    }

    /// sandbox_check(pid, NULL, 0) > 0: proces je v sandboxu (Seatbelt). Ostane tudi pri otrocih, ki se odcepijo
    /// od aplikacije (dvojni fork), zato dopolni preverjanje prednikov. Brez spremenljivih argumentov (varno na arm64).
    static func isSandboxed(_ pid: pid_t) -> Bool {
        typealias Check = @convention(c) (pid_t, UnsafePointer<CChar>?, Int32) -> Int32
        guard let f = dlsym(UnsafeMutableRawPointer(bitPattern: -2), "sandbox_check") else { return false }  // RTLD_DEFAULT
        return unsafeBitCast(f, to: Check.self)(pid, nil, 0) > 0
    }
}
