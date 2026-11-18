import Foundation

/// ONE interface for "secret by name" in the app (plan docs/plans/secrets-out-of-files-plan.md, step 2): `UpstreamSecretStore`
/// (read says found, missing or unreadable; write and remove say whether they worked; nothing here ever asks the user) plus the
/// line `minigw gw status` shows. The Keychain is the only backend on the Mac: a store that fails is reported, never replaced by
/// a file. Tests use a double (MemorySecrets); they never touch the real Keychain.
public protocol SecretVault: UpstreamSecretStore {
    /// One line for the status: where secrets are kept. Never a value.
    var place: String { get }
}

extension Keychain: SecretVault {
    public var place: String { "Keychain (\(loginOnly ? "login keychain, " : "")service \(service))" }
}
