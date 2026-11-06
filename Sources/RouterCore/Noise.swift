import CryptoKit
import Foundation

/// Šifrirana povezava minigw ↔ router v slogu WireGuard: Noise_NK_25519_<šifra>_SHA256.
/// Šifro izbere odjemalec s prvimi štirimi bajti (kot pri SSH router podpira več šifer): »MGW1« = ChaChaPoly
/// (macOS minigw), »MGW2« = AESGCM (minigw.exe za Windows/Linux, le standardna knjižnica Go). Ime protokola je
/// v hashu rokovanja, zato zamenjave šifre ni mogoče podtakniti.
/// Router ima stalni ključ (javni del dobi minigw skupaj z žetonom), odjemalec je anonimen in se overi
/// z žetonom v šifriranem HTTP. Vsaka TCP povezava: magic + rokovanje (1 RTT), nato okvirji, v katerih teče
/// navaden HTTP. Ključi seje so nova, zato je prejšnji promet varen tudi ob kraji ključa routerja.
public enum Noise {
    public static let magic = Data("MGW1".utf8)  // privzeta šifra (ChaChaPoly)

    public enum Suite: Sendable, CaseIterable {
        case chachaPoly, aesGCM
        public var magic: Data { Data((self == .chachaPoly ? "MGW1" : "MGW2").utf8) }
        /// h0 = ime protokola, z ničlami dopolnjeno na 32 bajtov (HASHLEN), kot v Noise.
        var protocolName: Data {
            var d = Data((self == .chachaPoly ? "Noise_NK_25519_ChaChaPoly_SHA256" : "Noise_NK_25519_AESGCM_SHA256").utf8)
            if d.count < 32 { d += Data(count: 32 - d.count) }
            return d
        }
        public static func from(magic m: Data) -> Suite? { allCases.first { $0.magic == m } }
    }
    public static let handshakeLength = 48                                     // e (32) + prazen šifriran payload (16)
    public static let maxPlain = 65535 - 16

    public enum Failure: Error { case badMessage, decrypt }

    /// Smerni šifrirni ključ s števcem. Nonce kot v Noise: 4 ničle + 64-bitni števec,
    /// pri ChaChaPoly little endian, pri AESGCM big endian.
    public final class Cipher: @unchecked Sendable {
        private let key: SymmetricKey
        private let suite: Suite
        private var n: UInt64 = 0
        private let lock = NSLock()
        /// Šifriranje + pošiljanje kot celota: sočasni zapisi (npr. ping med tokom) ne smejo zamenjati vrstnega reda okvirjev.
        public let sendLock = NSLock()
        init(_ key: SymmetricKey, _ suite: Suite = .chachaPoly) { self.key = key; self.suite = suite }

        func nonceBytes(_ n: UInt64) -> Data {
            var d = Data(count: 4)
            withUnsafeBytes(of: suite == .chachaPoly ? n.littleEndian : n.bigEndian) { d.append(contentsOf: $0) }
            return d
        }

        public func seal(_ plain: Data, ad: Data = Data()) -> Data {
            lock.withLock {
                defer { n += 1 }
                switch suite {
                case .chachaPoly:
                    let box = try! ChaChaPoly.seal(plain, using: key, nonce: ChaChaPoly.Nonce(data: nonceBytes(n)), authenticating: ad)
                    return box.ciphertext + box.tag
                case .aesGCM:
                    let box = try! AES.GCM.seal(plain, using: key, nonce: AES.GCM.Nonce(data: nonceBytes(n)), authenticating: ad)
                    return box.ciphertext + box.tag
                }
            }
        }

        public func open(_ c: Data, ad: Data = Data()) throws -> Data {
            try lock.withLock {
                guard c.count >= 16 else { throw Failure.decrypt }
                defer { n += 1 }
                do {
                    switch suite {
                    case .chachaPoly:
                        let box = try ChaChaPoly.SealedBox(nonce: ChaChaPoly.Nonce(data: nonceBytes(n)), ciphertext: c.dropLast(16), tag: c.suffix(16))
                        return try ChaChaPoly.open(box, using: key, authenticating: ad)
                    case .aesGCM:
                        let box = try AES.GCM.SealedBox(nonce: AES.GCM.Nonce(data: nonceBytes(n)), ciphertext: c.dropLast(16), tag: c.suffix(16))
                        return try AES.GCM.open(box, using: key, authenticating: ad)
                    }
                } catch { throw Failure.decrypt }
            }
        }
    }

    /// Simetrično stanje rokovanja (ck, h, k).
    struct Symmetric {
        var ck: Data, h: Data
        var k: Cipher?
        let suite: Suite

        init(suite: Suite, responderStatic: Data) {
            self.suite = suite
            h = suite.protocolName; ck = h
            mixHash(suite.magic)  // prologue
            mixHash(responderStatic)  // vnaprej znan ključ routerja (<- s)
        }

        mutating func mixHash(_ d: Data) { h = Data(SHA256.hash(data: h + d)) }

        mutating func mixKey(_ ikm: Data) {
            let (a, b) = Noise.hkdf(ck, ikm)
            ck = a; k = Cipher(SymmetricKey(data: b), suite)
        }

        mutating func encryptAndHash(_ p: Data) -> Data {
            let c = k!.seal(p, ad: h)
            mixHash(c)
            return c
        }

        mutating func decryptAndHash(_ c: Data) throws -> Data {
            let p = try k!.open(c, ad: h)
            mixHash(c)
            return p
        }

        func split() -> (Cipher, Cipher) {
            let (a, b) = Noise.hkdf(ck, Data())
            return (Cipher(SymmetricKey(data: a), suite), Cipher(SymmetricKey(data: b), suite))
        }
    }

    static func hkdf(_ ck: Data, _ ikm: Data) -> (Data, Data) {
        let temp = Data(HMAC<SHA256>.authenticationCode(for: ikm, using: SymmetricKey(data: ck)))
        let o1 = Data(HMAC<SHA256>.authenticationCode(for: Data([1]), using: SymmetricKey(data: temp)))
        let o2 = Data(HMAC<SHA256>.authenticationCode(for: o1 + Data([2]), using: SymmetricKey(data: temp)))
        return (o1, o2)
    }

    static func dh(_ priv: Curve25519.KeyAgreement.PrivateKey, _ pub: Data) throws -> Data {
        guard let p = try? Curve25519.KeyAgreement.PublicKey(rawRepresentation: pub) else { throw Failure.badMessage }
        guard let s = try? priv.sharedSecretFromKeyAgreement(with: p) else { throw Failure.badMessage }
        return s.withUnsafeBytes { Data($0) }
    }

    /// Promet po rokovanju: `send` šifrira proti drugi strani, `receive` dešifrira od nje.
    public struct Session: Sendable {
        public let send: Cipher, receive: Cipher
        public let hash: Data  // h po rokovanju: enak na obeh straneh, edinstven za povezavo (vezava dokaza naprave)
    }

    // MARK: odjemalec (minigw)

    public struct Initiator {
        var s: Symmetric
        let e = Curve25519.KeyAgreement.PrivateKey()
        let rs: Data

        public let suite: Suite

        public init(routerKey: Data, suite: Suite = .chachaPoly) {
            rs = routerKey; self.suite = suite; s = Symmetric(suite: suite, responderStatic: routerKey)
        }

        /// -> e, es
        public mutating func first() throws -> Data {
            let pub = e.publicKey.rawRepresentation
            s.mixHash(pub)
            s.mixKey(try Noise.dh(e, rs))
            return pub + s.encryptAndHash(Data())
        }

        /// <- e, ee
        public mutating func finish(_ msg: Data) throws -> Session {
            guard msg.count == Noise.handshakeLength else { throw Failure.badMessage }
            let re = Data(msg.prefix(32))
            s.mixHash(re)
            s.mixKey(try Noise.dh(e, re))
            _ = try s.decryptAndHash(Data(msg.dropFirst(32)))
            let (a, b) = s.split()
            return Session(send: a, receive: b, hash: s.h)
        }
    }

    // MARK: router

    /// <- s; -> e, es; <- e, ee. Vrne odgovor in sejo; napačen ključ ali poškodba → napaka.
    public static func respond(_ msg: Data, key: Curve25519.KeyAgreement.PrivateKey, suite: Suite = .chachaPoly) throws -> (Data, Session) {
        guard msg.count == handshakeLength else { throw Failure.badMessage }
        var s = Symmetric(suite: suite, responderStatic: key.publicKey.rawRepresentation)
        let re = Data(msg.prefix(32))
        s.mixHash(re)
        s.mixKey(try dh(key, re))
        _ = try s.decryptAndHash(Data(msg.dropFirst(32)))
        let e = Curve25519.KeyAgreement.PrivateKey()
        let pub = e.publicKey.rawRepresentation
        s.mixHash(pub)
        s.mixKey(try dh(e, re))
        let reply = pub + s.encryptAndHash(Data())
        let (a, b) = s.split()
        return (reply, Session(send: b, receive: a, hash: s.h))
    }

    // MARK: naprava (minigw)

    /// Prvi šifrirani podatki odjemalca: »DEV1« + javni ključ naprave (Ed25519, 32) + podpis (64) + dolžina imena (1) + ime.
    /// Podpis pokrije hash rokovanja te povezave, zato ga ni mogoče prestreči in uporabiti drugje.
    public static let deviceMagic = Data("DEV1".utf8)

    public struct Device: Sendable, Equatable {
        public let id: String    // base64url javnega ključa naprave
        public let name: String
    }

    static func deviceMessage(_ hash: Data, _ name: Data) -> Data { Data("minigw-device".utf8) + hash + name }

    public static func deviceProof(_ key: Curve25519.Signing.PrivateKey, name: String, hash: Data) -> Data {
        let n = Data(name.utf8.prefix(255))
        let sig = try! key.signature(for: deviceMessage(hash, n))
        return deviceMagic + key.publicKey.rawRepresentation + sig + Data([UInt8(n.count)]) + n
    }

    public enum DeviceRead: Equatable { case needMore, absent, invalid, device(Device) }

    /// Iz začetka dešifriranih podatkov prebere dokaz naprave (in ga odstrani). `absent`: starejši minigw, takoj HTTP.
    public static func readDevice(_ plain: inout Data, hash: Data) -> DeviceRead {
        let m = deviceMagic.count
        guard !plain.isEmpty else { return .needMore }
        guard plain.count >= m else { return deviceMagic.starts(with: plain) ? .needMore : .absent }
        guard plain.starts(with: deviceMagic) else { return .absent }
        let fixed = m + 32 + 64 + 1
        guard plain.count >= fixed else { return .needMore }
        let b = [UInt8](plain)
        let len = Int(b[fixed - 1])
        guard plain.count >= fixed + len else { return .needMore }
        let pub = Data(b[m..<m + 32]), sig = Data(b[m + 32..<m + 96]), name = Data(b[fixed..<fixed + len])
        plain = Data(b[(fixed + len)...])
        guard let key = try? Curve25519.Signing.PublicKey(rawRepresentation: pub),
              key.isValidSignature(sig, for: deviceMessage(hash, name)) else { return .invalid }
        return .device(Device(id: base64url(pub), name: String(decoding: name, as: UTF8.self)))
    }

    // MARK: okvirji (2 bajta dolžina, big endian, kot pri Noise)

    public static func frames(_ plain: Data, _ c: Cipher) -> Data {
        var out = Data()
        var i = plain.startIndex
        repeat {
            let j = plain.index(i, offsetBy: min(maxPlain, plain.endIndex - i))
            let ct = c.seal(plain.subdata(in: i..<j))
            out.append(UInt8(ct.count >> 8)); out.append(UInt8(ct.count & 0xff)); out.append(ct)
            i = j
        } while i < plain.endIndex
        return out
    }

    /// Izlušči cele okvirje iz `buf` (ostanek ostane v buf); vrne dešifrirano besedilo.
    public static func unframe(_ buf: inout Data, _ c: Cipher) throws -> Data {
        var out = Data()
        while buf.count >= 2 {
            let n = Int(buf[buf.startIndex]) << 8 | Int(buf[buf.startIndex + 1])
            guard buf.count >= 2 + n else { break }
            out.append(try c.open(buf.subdata(in: buf.startIndex + 2..<buf.startIndex + 2 + n)))
            buf = Data(buf.dropFirst(2 + n))
        }
        return out
    }

    // MARK: ključi

    public static func newKey() -> Curve25519.KeyAgreement.PrivateKey { .init() }

    public static func base64url(_ d: Data) -> String {
        d.base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }

    public static func fromBase64url(_ s: String) -> Data? {
        var b = s.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
        while b.count % 4 != 0 { b += "=" }
        return Data(base64Encoded: b)
    }
}
