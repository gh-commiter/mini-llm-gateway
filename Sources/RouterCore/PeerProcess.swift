import Darwin

/// Kdo je na drugi strani lokalne povezave TCP (skrbniški API). Agenti (Codex, Grok, Claude Code) tečejo kot otroci
/// aplikacije in v sandboxu: tudi z ukradeno skrivnostjo ali z zagonom minigw ne smejo spreminjati nastavitev.
/// LOCAL_PEERPID deluje le za vtičnice Unix, zato libproc: proces z vtičnico TCP <vrata odjemalca> → <vrata routerja>.
public enum PeerProcess {
    /// nil = dovoljeno; sicer razlog. Neznan proces (drug uporabnik, že končan) je zavrnjen.
    public static func refusal(peerPort: UInt16, routerPort: UInt16, appPID: pid_t = getpid()) -> String? {
        guard let pid = pid(localPort: peerPort, remotePort: routerPort) else {
            return "cannot identify the process on this Mac that sent the request"
        }
        if isSandboxed(pid) { return "a sandboxed process (an agent) may not use the admin API" }
        var p = pid
        for _ in 0..<64 {  // otrok, vnuk … aplikacije: agent ali ukaz agenta
            if p == appPID { return "a process started by Mini Gateway (an agent) may not use the admin API" }
            guard let pp = parent(p), pp > 1, pp != p else { return nil }
            p = pp
        }
        return nil
    }

    /// Proces (istega uporabnika) z vtičnico TCP lokalna → oddaljena vrata.
    static func pid(localPort: UInt16, remotePort: UInt16) -> pid_t? {
        let n = proc_listallpids(nil, 0)
        guard n > 0 else { return nil }
        var pids = [pid_t](repeating: 0, count: Int(n) + 64)
        let count = proc_listallpids(&pids, Int32(pids.count * MemoryLayout<pid_t>.size))
        let infoSize = Int32(MemoryLayout<socket_fdinfo>.size)
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
                   UInt16(bigEndian: UInt16(truncatingIfNeeded: ini.insi_fport)) == remotePort { return pid }
            }
        }
        return nil
    }

    static func parent(_ pid: pid_t) -> pid_t? {
        var info = proc_bsdinfo()
        let size = Int32(MemoryLayout<proc_bsdinfo>.size)
        guard proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, size) == size else { return nil }
        return pid_t(info.pbi_ppid)
    }

    /// sandbox_check(pid, NULL, 0) > 0: proces je v sandboxu (Seatbelt). Ostane tudi pri otrocih, ki se odcepijo
    /// od aplikacije (dvojni fork), zato dopolni preverjanje prednikov. Brez spremenljivih argumentov (varno na arm64).
    static func isSandboxed(_ pid: pid_t) -> Bool {
        typealias Check = @convention(c) (pid_t, UnsafePointer<CChar>?, Int32) -> Int32
        guard let f = dlsym(UnsafeMutableRawPointer(bitPattern: -2), "sandbox_check") else { return false }  // RTLD_DEFAULT
        return unsafeBitCast(f, to: Check.self)(pid, nil, 0) > 0
    }
}
