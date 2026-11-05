import CryptoKit
import Foundation

/// Šifrirana povezava minigw ↔ router v slogu WireGuard: Noise_NK_25519_ChaChaPoly_SHA256.
/// Router ima stalni ključ (javni del dobi minigw skupaj z žetonom), odjemalec je anonimen in se overi
/// z žetonom v šifriranem HTTP. Vsaka TCP povezava: magic + rokovanje (1 RTT), nato okvirji, v katerih teče
/// navaden HTTP. Ključi seje so nova, zato je prejšnji promet varen tudi ob kraji ključa routerja.
public enum Noise {
    public static let magic = Data("MGW1".utf8)
    static let protocolName = Data("Noise_NK_25519_ChaChaPoly_SHA256".utf8)  // točno 32 bajtov = HASHLEN
    public static let handshakeLength = 48                                     // e (32) + prazen šifriran payload (16)
    public static let maxPlain = 65535 - 16

    public enum Failure: Error { case badMessage, decrypt }

    /// Smerni šifrirni ključ s števcem (nonce = 4 ničle + LE64 števec, kot v Noise).
    public final class Cipher: @unchecked Sendable {
        private let key: SymmetricKey
        private var n: UInt64 = 0
        private let lock = NSLock()
        /// Šifriranje + pošiljanje kot celota: sočasni zapisi (npr. ping med tokom) ne smejo zamenjati vrstnega reda okvirjev.
        public let sendLock = NSLock()
        init(_ key: SymmetricKey) { self.key = key }

        static func nonce(_ n: UInt64) -> ChaChaPoly.Nonce {
            var d = Data(count: 4)
            withUnsafeBytes(of: n.littleEndian) { d.append(contentsOf: $0) }
            return try! ChaChaPoly.Nonce(data: d)
        }

        public func seal(_ plain: Data, ad: Data = Data()) -> Data {
            lock.withLock {
                defer { n += 1 }
                let box = try! ChaChaPoly.seal(plain, using: key, nonce: Self.nonce(n), authenticating: ad)
                return box.ciphertext + box.tag
            }
        }

        public func open(_ c: Data, ad: Data = Data()) throws -> Data {
            try lock.withLock {
                guard c.count >= 16 else { throw Failure.decrypt }
                defer { n += 1 }
                let box = try ChaChaPoly.SealedBox(nonce: Self.nonce(n), ciphertext: c.dropLast(16), tag: c.suffix(16))
                do { return try ChaChaPoly.open(box, using: key, authenticating: ad) } catch { throw Failure.decrypt }
            }
        }
    }

    /// Simetrično stanje rokovanja (ck, h, k).
    struct Symmetric {
        var ck: Data, h: Data
        var k: Cipher?

        init(prologue: Data, responderStatic: Data) {
            h = Noise.protocolName; ck = h
            mixHash(prologue)
            mixHash(responderStatic)  // vnaprej znan ključ routerja (<- s)
        }

        mutating func mixHash(_ d: Data) { h = Data(SHA256.hash(data: h + d)) }

        mutating func mixKey(_ ikm: Data) {
            let (a, b) = Noise.hkdf(ck, ikm)
            ck = a; k = Cipher(SymmetricKey(data: b))
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
            return (Cipher(SymmetricKey(data: a)), Cipher(SymmetricKey(data: b)))
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
    }

    // MARK: odjemalec (minigw)

    public struct Initiator {
        var s: Symmetric
        let e = Curve25519.KeyAgreement.PrivateKey()
        let rs: Data

        public init(routerKey: Data) { rs = routerKey; s = Symmetric(prologue: Noise.magic, responderStatic: routerKey) }

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
            return Session(send: a, receive: b)
        }
    }

    // MARK: router

    /// <- s; -> e, es; <- e, ee. Vrne odgovor in sejo; napačen ključ ali poškodba → napaka.
    public static func respond(_ msg: Data, key: Curve25519.KeyAgreement.PrivateKey) throws -> (Data, Session) {
        guard msg.count == handshakeLength else { throw Failure.badMessage }
        var s = Symmetric(prologue: magic, responderStatic: key.publicKey.rawRepresentation)
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
        return (reply, Session(send: b, receive: a))
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
