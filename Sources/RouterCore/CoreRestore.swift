import Foundation

/// minigw gw core restore <file|name> on the Mac: only with the app (and so its helper) not running. The restore itself is Go's
/// (`minigw-core restore`, client/cmd/minigw-core/restore.go, the code of minigw serve's restore): it takes the store's lock
/// exclusively, so a helper that is up makes it fail anyway; this is the clear message before that, and the command line.
public enum CoreRestore {
    public static let refusal = "Mini Gateway is running: quit the app first (a restore needs the core database to itself), then run minigw gw core restore again. Nothing was changed."

    /// nil = go on; the message when the app runs.
    public static func check(appRunning: Bool) -> String? { appRunning ? refusal : nil }

    /// The argv of `minigw-core restore` (it never starts a helper).
    public static func argv(binary: String, store: String, file: String, yes: Bool) -> [String] {
        [binary, "restore", "-store", store] + (yes ? ["-yes"] : []) + ["--", file]
    }
}
