import Foundation
import Testing
@testable import RouterCore

// #111 (0.5.28): the Claude CLI without "Allow local tools" has no sandbox; the tool rules are the only fence.
// S1 names are trimmed, S2 only tools the path rules cover, S3 git and gateway folders denied, S4 only modes that enforce the rules, S5 rule-safe folders.

private func value(_ a: [String], _ flag: String) -> String { a.firstIndex(of: flag).map { a[$0 + 1] } ?? "" }
private let cwd = URL(fileURLWithPath: "/tmp/session")

@Test func untrimmedToolNamesAreTrimmedAndScoped() {  // S1: ` Grep` and ` Glob` went in bare (any path) — a real run read the outside folder with them
    var c = ClaudeCLIConfig(); c.tools = "Read, Grep , Glob,, "
    let a = c.agentArgs(cwd: cwd, allTools: [], folders: AllowedFolders(), checkedPaths: ["/w"])
    #expect(value(a, "--tools") == "Read,Grep,Glob")
    #expect(value(a, "--allowedTools") == "Read(./**),Read(//w/**),Grep(./**),Grep(//w/**),Glob(./**),Glob(//w/**)")
    #expect(!value(a, "--allowedTools").split(separator: ",").contains { !$0.contains("(") })  // nothing bare
    #expect(ClaudeCLIConfig.toolsTextProblem("Read, Grep,\tGlob\n") == nil)
    for bad in ["Read;rm", "Bash(git:*)", "Re ad", "Read,mcp-x"] { #expect(ClaudeCLIConfig.toolsTextProblem(bad) != nil, "\(bad)") }
    c.tools = " Edit , Write"
    #expect(value(c.agentArgs(cwd: cwd, allTools: [], folders: AllowedFolders(), checkedPaths: []), "--allowedTools") == "Edit(./**)")
}

@Test func onlyToolsThePathRulesCoverAreAllowedWithoutLocalTools() {  // S2
    var c = ClaudeCLIConfig(); c.tools = "Read,Bash,Task,mcp__x__run,Write,WebFetch"
    #expect(ClaudeCLIConfig.unsafeRestricted(c.tools) == ["Bash", "Task", "mcp__x__run"])
    #expect(c.restrictedProblem()?.contains("Bash") == true && c.restrictedProblem()?.contains("Allow local tools") == true)
    let a = c.agentArgs(cwd: cwd, allTools: [], folders: AllowedFolders(), checkedPaths: [])  // the last defence: filtered
    #expect(value(a, "--tools") == "Read,Write,WebFetch" && !value(a, "--allowedTools").contains("Bash"))
    c.allowLocalTools = true  // the sandbox confines Bash there
    #expect(c.restrictedProblem() == nil)
    c.allowLocalTools = false; c.tools = "Read,Grep,Glob,Edit,Write,MultiEdit,NotebookEdit,LS,WebSearch,WebFetch"
    #expect(c.restrictedProblem(mode: false, folders: false) == nil)
}

@Test func gitAndGatewayFoldersAreDeniedToTheEditTools() {  // S3
    var c = ClaudeCLIConfig(); c.tools = "Read,Edit,Write"
    let a = c.agentArgs(cwd: cwd, allTools: [], folders: AllowedFolders(), checkedPaths: ["/w"])
    let deny = value(a, "--disallowedTools").split(separator: ",").map(String.init)
    for d in ["/tmp/session", "/w"] {
        for g in ["config", "hooks", "info", "commondir", "worktrees", "config.worktree"] {
            #expect(deny.contains("Edit(//\(d.dropFirst())/.git/\(g)/**)") && deny.contains("Edit(//\(d.dropFirst())/**/.git/\(g)/**)") && deny.contains("Edit(//\(d.dropFirst())/.git/\(g))"), "\(d) \(g)")
        }
        #expect(deny.contains("Edit(//\(d.dropFirst())/.git)") && deny.contains("Edit(//\(d.dropFirst())/**/.git)"))
    }
    #expect(deny.allSatisfy { $0.hasPrefix("Edit(") || $0.hasPrefix("Read(") })  // only Edit and Read path rules are matched (Edit covers Write: real run)
    c.tools = "Read,Grep"  // nothing edits: no git rules needed
    #expect(!value(c.agentArgs(cwd: cwd, allTools: [], folders: AllowedFolders(), checkedPaths: ["/w"]), "--disallowedTools").contains(".git"))
    // the gateway's own app folder: Read and Edit denied (as --disallowedTools, or in --settings permissions.deny)
    var all = deny
    if let json = a.firstIndex(of: "--settings").map({ a[$0 + 1] }), let o = (try? JSONSerialization.jsonObject(with: Data(json.utf8))) as? [String: Any],
       let d = (o["permissions"] as? [String: Any])?["deny"] as? [String] { all += d }
    let appDir = GatewayHistory.defaultFile.deletingLastPathComponent().standardizedFileURL.path
    #expect(all.contains("Read(/\(appDir)/**)") && all.contains("Edit(/\(appDir)/**)"), "\(appDir)")
}

@Test func onlyModesThatEnforceTheRulesAreUsedWithoutLocalTools() {  // S4: real runs: auto let Read, Grep and Write outside through, plan let Read and Grep through
    for m in ["dontAsk", "acceptEdits"] {
        var c = ClaudeCLIConfig(); c.permissionMode = m
        #expect(c.restrictedProblem() == nil && value(c.agentArgs(cwd: cwd, allTools: [], folders: AllowedFolders(), checkedPaths: []), "--permission-mode") == m)
    }
    for m in ["auto", "plan", "bypassPermissions", ""] {
        var c = ClaudeCLIConfig(); c.permissionMode = m
        #expect(c.restrictedProblem()?.contains("does not enforce") == true, "\(m)")
        #expect(value(c.agentArgs(cwd: cwd, allTools: [], folders: AllowedFolders(), checkedPaths: []), "--permission-mode") == "dontAsk", "\(m)")  // a stored value is migrated at run
        c.allowLocalTools = true
        #expect(c.restrictedProblem() == nil)
    }
}

@Test func foldersThatCannotGoIntoARuleAreRefusedAndSkipped() {  // S5
    for ok in ["/Users/a/src", "/Users/a/my project", "/Users/a/x-y_z.1", "/Users/a/ä"] { #expect(ClaudeCLIConfig.ruleSafe(ok), "\(ok)") }
    for bad in ["/a/b)", "/a/(b", "/a/*", "/a/b?", "/a/[x]", "/a/{x}", "/a/ x", "/a/x ", "/a/b,c", "/a/b\nc", "/a/b\\c", "/a/b!", "", " /a"] { #expect(!ClaudeCLIConfig.ruleSafe(bad), "\(bad.debugDescription)") }
    var c = ClaudeCLIConfig(); c.allowedRoots = "~/ok, /Users/a/Bad (copy)"
    #expect(c.restrictedProblem()?.contains("Bad (copy)") == true)
    c.allowLocalTools = true
    #expect(c.restrictedProblem() == nil)  // the sandbox branch passes paths as JSON values
    c.allowLocalTools = false; c.tools = "Read,Edit"; c.allowedRoots = "~/ok"
    let a = c.agentArgs(cwd: cwd, allTools: [], folders: AllowedFolders(), checkedPaths: ["/w/ok", "/w/bad)", "/w/glob*"])  // run time: skipped
    #expect(value(a, "--allowedTools").contains("//w/ok/**") && !value(a, "--allowedTools").contains("bad") && !value(a, "--allowedTools").contains("glob"))
    #expect(a.filter { $0 == "--add-dir" }.count == 1 && !value(a, "--disallowedTools").contains("bad"))
    let unsafeCwd = c.agentArgs(cwd: URL(fileURLWithPath: "/tmp/p (1)"), allTools: [], folders: AllowedFolders(), checkedPaths: [])  // the deny rules for its .git cannot be written
    #expect(value(unsafeCwd, "--tools") == "Read" && value(unsafeCwd, "--allowedTools") == "Read(./**)")
}
