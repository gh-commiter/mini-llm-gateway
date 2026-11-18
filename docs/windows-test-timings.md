# Windows test runtime investigation

Part 1 is implemented: the password dispatcher prints one stdout line after the
credential and revocation audit have been saved. Both input paths share it. It
reports sessions signed out and the configured listener state, without printing
passwords, lengths, or credential records. Missing config means listener off;
an unreadable or malformed config fails before replacing the credential rather
than reporting a guessed state. `docs/commands.md` describes the confirmation.

## Measurement limits

Local machine: macOS arm64, Go 1.27.4. No Windows execution was available.
These are single runs, not benchmark distributions. Tables rank top-level tests
by elapsed time from `go test -json ./...`; parent times include subtests, so
subtests are not counted again. Windows multipliers below are hypotheses, not
measurements or predictions with statistical confidence.

The first invocation could not use the default Go build cache (exit 1). With
`GOCACHE` pointing to a writable task directory and `GOPROXY=off`, the first full
JSON run completed with exit 1: minigw 124.329 s (failed), minigw/core 33.999 s
(passed). It inherited gateway-agent environment variables, which contaminated
some CLI tests. The controlled repeat cleared all `MINIGW_AGENT*` variables and
set `TMPDIR` to the writable task directory: exit 1, minigw 135.069 s (failed),
minigw/core 34.676 s (passed). Other packages passed in that repeat:
coreapi 8.194 s, minigw-core 2.310 s, minigw-tui 3.593 s.

The controlled repeat is the baseline used for ranking below. The first run is
included for transparency, not as evidence of a performance change. Only the
password confirmation and its tests changed between runs; no speed optimization
was made. Some tests fail early in this sandbox, so their timings underestimate
the successful path. F marks a failed test; unmarked rows passed. Test events
round elapsed times to hundredths of a second.

The sandbox cannot create the short `/tmp/mgw-*` roots required by worktree
execution. Setting a long writable TMPDIR cannot solve this: `tempBase()` falls
back to `/tmp` to preserve socket path limits. Tests also exercise inaccessible
keychain/config locations, a macOS firmlink outside this worktree, and hdiutil
image creation unavailable here. None of these tests or checks were weakened.

## Ranked hypotheses and runner confirmation

1. **Durable SQLite writes and filesystem latency (highest confidence for core).**
   `client/core/core.go:openDB` explicitly uses WAL and `synchronous(FULL)`;
   WAL already is enabled. Each `Store.write` commits separately. State changes
   also sync and rename a sidecar; audit append, backups and restore staging sync
   files. `newFx` seeds six users, device bindings, three projects, memberships
   and grants through separate public calls for every fixture. Bulk-note tests
   add more than 1,000 notes individually. NTFS flush latency and scanning can
   multiply these costs. Confirm with Windows JSON timings and an ETW/WPR file
   I/O trace, counting FlushBuffers/WriteFile operations and durations for
   core.db-wal, core.state, audit files and fixture directories. Capture the
   actual temp path and disk type; compare against macOS operations per test.
2. **Executable copies, process creation and Git (highest confidence for root).**
   `client/fake_test.go:fakeCLI` hard-links the running test executable on Unix,
   but reads and writes the whole executable on Windows for each new fake path.
   Its comment explains that a hard link to a running executable prevents
   TempDir cleanup on Windows. Each fake then starts another test process via
   TestMain. `gitT` starts Git per call, and nested worktree/bundle tests call it
   repeatedly. Defender scanning a new executable or many Git files is a
   plausible amplifier, not an observed cause. Confirm with process/file I/O
   tracing: count copied bytes, CreateProcess events and scan activity per
   fixture. A shared binary would need correct sidecar dispatch and lifecycle
   ownership; replacing copies with hard links is not a safe fix.
3. **Fixture creation and cleanup accumulate serially.** Static call-site counts:
   568 `t.TempDir()` calls in root tests and 138 in core tests (not runtime counts;
   loops/subtests expand them). Neither package currently uses `t.Parallel()`.
   Global environment, cwd, migration lists, rate limits, hooks and counters make
   blanket parallelization unsafe. Confirm with traces around fixture setup,
   the test body and cleanup, retaining each phase's elapsed time. Cleanup is
   included in the Go test elapsed event.
4. **Real deadline waits set a floor; they do not explain a sixfold increase.**
   The slow-body wire test waits for the real 10-second product deadline. Gateway
   discovery tests intentionally wait about two seconds. Streaming orphan tests
   exercise bounded pipe draining. Automatic-upstream integration tests use real
   debounce/backoff ticks. Preserve those assertions. Compare their Windows
   elapsed times with their fixed wait plus fixture/process overhead.
5. **Timer granularity is a secondary candidate.** The writer-preference test
   polls at 1 ms; the parent-swap race sleeps for 50 microseconds and 20 ms, with
   four readers each doing 300 pairs of requests. Windows timer quantization
   could amplify these loops, but it is not demonstrated on this runner and
   cannot explain core: core tests generally advance a fake clock and upstream
   tests inject `noSleep`. Confirm by isolating these tests with `-count=5`,
   measuring actual sleep durations and comparing with flush/process totals.
   Replacing the race sleeps changes scheduling coverage; no such edit was made.

No `go build` helper invocation was found in the root/core tests. The notices
test runs `go list -deps` for five OS/architecture targets; it can still incur
Go tool/process/cache scanning costs but does not build a helper per test.
`protectDir` uses native Windows security APIs, not an icacls process per call.
Windows-only tests do not appear in a Mac top-25 and need a separate ranking.

## Changes considered

No test-only performance change met the safety threshold from the available
evidence. There is no existing SQLite durability fixture hook in `core.Options`;
it exposes only file/directory protection callbacks. Disabling sync, sharing
mutable stores, reducing fixture coverage or extending deadlines would undermine
the task's constraints. Thus there are no before/after optimization timings to
claim. The two full-run timings above measure unchanged performance behavior and
environment differences.

Product-side ideas, requiring separate design and tests: batch fixture-equivalent
public operations into transactions where atomic semantics permit; reduce
redundant state/audit flushes only with an explicit durability contract; profile
Windows ownership/ACL and file replacement operations. None was implemented.

## Proposed Windows workflow instrumentation (not applied)

Replace the test step with the following, retaining the 30-minute timeout.
Keep the existing working-directory default of `client`. Upload runs even when
tests fail. Use the repository's approved pinned upload-artifact revision when
adopting this proposal.

```yaml
- name: Go test timings
  shell: pwsh
  run: |
    go test -count=1 -timeout 30m -json ./... 2> go-test-stderr.log |
      Tee-Object -FilePath go-test.json
    $testExit = $LASTEXITCODE
    exit $testExit
- name: Upload Go test timings
  if: always()
  uses: actions/upload-artifact@v4
  with:
    name: windows-go-test-timings
    path: |
      client/go-test.json
      client/go-test-stderr.log
```

Rank completed top-level `pass`/`fail` events by `Elapsed`, grouped by Package;
exclude events without Test and names containing `/`. Retain subtest events for
drilling into a slow parent. Preserve stderr and package fail events so a timed
out or crashed test cannot silently disappear from the report. For targeted
repeats use `go test -count=5 -timeout 30m -json -run '^TestName$' .` (or `./core`).
Compare the root/core runs separately from `./...` to identify cross-package disk
contention. Do not infer per-test time from wall-clock build duration.

## Final verification and delivery

Commands ran in `client/` with a writable `GOCACHE` and `GOPROXY=off` (no
dependency downloads). Full test runs cleared inherited `MINIGW_AGENT*` and used
a writable TMPDIR as described above.

| Command | Exit | Result |
|---|---:|---|
| `go test -run 'TestRemotePasswordCLI' -count=1 .` | 0 | Final password tests passed in 1.525 s, including missing config, on/off stdout, unchanged short refusal, no credential on refusal, and invalid config. |
| `go test ./...` | 1 | Root 187.530 s; the same 17 sandbox-limited tests as the controlled JSON run failed. All other packages passed: core 73.270 s, coreapi 15.183 s, minigw-core 5.203 s, minigw-tui 4.850 s. |
| `GOOS=windows GOARCH=amd64 go vet ./...` | 0 | Passed after the final Go test edits. |
| `GOOS=linux GOARCH=amd64 go vet ./...` | 0 | Passed after the final Go test edits. |
| `git diff --check` | 0 | Passed. |

The final full run overlapped other verification work and is not a speed
benchmark. The variation between runs precludes attributing a performance change
to the password confirmation. No Windows runtime or interactive terminal test
was possible here; both input paths use the same dispatcher confirmation.
No Swift files changed, so Swift build/test were not run.

The 17 failures shared by the controlled JSON and final full run were:

```text
TestWorktreeContinueConversation
TestPullRefusedBusyFailsRequest
TestAutoShare
TestBoxedClaudeWorktreeGitWrap
TestCodexTreeProfile
TestCopilotKeyringDetection
TestServeWorktreeRoundTrip
TestServeWorktreeSwapStopsUnpinnedClaude
TestServeWorktreeNestedRoundTrip
TestRunTempDir
TestTempRootRefreshed
TestWorktreeNestedSkippedTopDelivered
TestServeWorktreePlantedGitOnCwdPath
TestRunTempLink
TestShareRefusalMacFirmlinks
TestUninstallPurgeRemovesUpstreamCredentials
TestWorktreeFetchRepoOnAnotherVolume
```

In addition to the filesystem limits above, TestCodexTreeProfile cannot apply its
OS sandbox in this execution sandbox. These are not evidence that an unrestricted
macOS suite or Windows runtime passes.

Changed files: `client/serve_remote_auth.go`, `client/serve_router.go`,
`client/serve_remote_auth_test.go`, `docs/commands.md`, and this report.
No workflow, version, published release note, durability/security setting or
performance-related product behavior changed. No release scripts or push ran.
Staging/commit was attempted but the Git index lock is outside the writable
sandbox and was denied. Files remain unstaged for the lead to commit with:

```text
gw settings password: confirm success and listener state

Co-Authored-By: Claude Code <noreply@anthropic.com>
```

## Release note suggestion

Leave published sections untouched. Suggested future Unreleased/Fixed line:
“`minigw gw settings password` now confirms success, session sign-out and the
configured remote settings listener state for terminal and stdin input.”

## Slowest 25 tests per package

First = initial inherited-environment run. Baseline = controlled repeat.
Windows cost factors apply to a successful equivalent test path, relative to
this local baseline; failed local paths are unsuitable for numeric extrapolation.

### minigw

| Test | First (s) | Baseline (s) | Windows cost hypothesis (unmeasured) |
|---|---:|---:|---|
| TestUpstreamAutoPushBetweenTwoGateways | 12.34 | 12.28 | 2–4× plausible: real debounce/backoff, 1.2 s explicit sleep, two SQLite stores, 40 durable edits and loopback HTTP; the measured 12 s is not all fixed waiting. |
| TestRemoteSettingsSlowBodyWire | 10.22 | 10.23 | About 10 s plus TLS/auth fixture setup; timer floor should stay similar, not scale 6×. |
| TestWorktreeNestedSkippedTopDelivered | 3.91 F | 3.14 F | High but unquantifiable from failed path: four nested Git cases, fake executable copies, bundles and 70 MB random payload on successful path. |
| TestRemoteTrustedSourceSkipsOnlyGlobalLockout | 2.68 | 2.77 | 1–3×: repeated full-cost Argon2, lockout/audit file writes and Windows ACL checks. |
| TestRemoteDistributedBackoffPersisted | 2.42 | 2.42 | 1–3×: many password derivations plus durable lockout/audit persistence; backoff uses fake time. |
| TestRunClaudeStalledOutput | 2.32 | 2.32 | About 2 s drain floor plus process/copy overhead; Windows pipe and process behavior warrants direct measurement. |
| TestRunClaudeStream | 2.19 | 2.16 | About 2 s drain floor plus multiple fake process launches and an executable copy; orphan pipe lifetime is intentional. |
| TestCoreVisibilityBeforePayloadChecks | 2.09 | 2.15 | 3–10× plausible: real database fixture, repeated HTTP authorization/validation and durable writes. |
| TestServeGatewayDeadDoesNotDelayCLI | 2.04 | 2.04 | About 2 s floor plus setup: intentional discovery timeout; slow fake executable creation adds overhead. |
| TestServeGatewayReadyAfterKeyAdded | 2.05 | 2.04 | About 2 s discovery refresh plus polling/setup; 200 ms sleeps are not 1 ms timer amplification. |
| TestWorktreePushNestedNeedsGateway | 1.36 | 1.95 | 2–8×: repeated Git creation, snapshots/bundles and nested-repository checks. |
| TestProjectParityCommands | 1.60 | 1.76 | 2–8×: table of CLI subprocesses, per-case directories and project state writes. |
| TestFolderImportHeldRootAndTraversalLimits | 1.85 | 1.74 | Skipped on Windows: this test is in kb_files_unix_test.go; cannot explain Windows slowdown. |
| TestServeWorktreeNestedRoundTrip | 1.26 F | 1.69 F | High but unquantifiable from failed path: nested Git clone/bundle round trips and fake executable launches. |
| TestRunEpochChangeBetweenReads | 1.19 | 1.65 | 3–10× plausible: repeated core fixture setup and writes, plus simulated agent/request interleaving. |
| TestServeWorktreeNestedRefusedAtStart | 0.83 | 1.46 | 2–8×: nested Git setup and refusal cases; refusing before agent execution limits launch cost. |
| TestPullRefusedBusyFailsRequest | 1.21 F | 1.32 F | High but unquantifiable from failed path: sync/worktree variants, fake agent execution and lock/busy coordination. |
| TestWorktreeFetchChecksBundleAndKeepsUserCommits | 0.48 | 1.29 | 2–8×: many Git commands, bundles and filesystem operations; Windows process startup/scanning amplifies them. |
| TestServeWorktreeCodexSandbox | 0.71 | 1.21 | 2–8× where exercised: fake executable and Git fixture; platform-specific sandbox branches require runner comparison. |
| TestRemoteSessionInvalidation | 1.20 | 1.19 | 1–3×: multiple credential/token/disable/restart cases; Argon2 and persistent auth records dominate setup. |
| TestServeInstallFiles | 1.03 | 1.18 | 2–5× plausible: executable/file installation fixture and platform-specific branches; OS commands/startup hooks are stubbed. |
| TestShutdownDrainsThenCuts | 1.04 | 1.06 | About 1 s fixed waits plus setup; not a likely sixfold multiplier by itself. |
| TestWorktreeNestedRepoFilterDoesNotRun | 0.64 | 1.06 | 2–8×: nested Git fixtures/filter refusal verification; file/process cost rather than timer quantization. |
| TestServeWorktreeRoundTrip | 0.51 F | 1.05 F | High but unquantifiable from failed path: full fake-agent/Git bundle round trips, executable copies and cleanup. |
| TestWorktreeFetchRepoOnAnotherVolume | 1.03 F | 1.04 F | Skipped on Windows: macOS hdiutil disk-image test. |

### minigw/core

| Test | First (s) | Baseline (s) | Windows cost hypothesis (unmeasured) |
|---|---:|---:|---|
| TestMoreThanAThousandRefusedNotesAreNotResentEveryRun | 1.10 | 2.14 | 3–15× plausible: >1,000 separate note writes plus refusal bookkeeping; fake clock, no real hour-long wait. |
| TestAMoreRunGoesOnFromTheCursor | 0.95 | 1.11 | 3–15× plausible: >1,000 separate note commits and push receipt/cursor writes; no real 61-second sleep. |
| TestUpstreamPushOfManyNotesGetsPastTheLimit | 0.82 | 1.00 | 3–15× plausible: bulk notes created via public writes and per-note push/receipt processing. |
| TestSearchIndexedScale | 0.89 | 0.89 | 1–4×: 30,000 rows inserted in one transaction, FTS rebuild and seven queries; less per-note fsync than bulk public-create tests. |
| TestRestoreCrashAtEveryStage | 0.58 | 0.58 | 3–10×: five crash stages each build/backup/restore/reopen, sync audit/state and retry restore. |
| TestRestoreRefusesExtraSchemaObjects | 0.58 | 0.53 | 3–10×: schema-object cases each create fixtures, backups and validation/restore files. |
| TestCase10SequentialCorePart | 0.48 | 0.49 | 3–10×: multiple policy/epoch scenarios repeat full seeded fixtures and durable mutations. |
| TestBundleRefusalsAreAtomic | 0.62 | 0.45 | 3–10×: malformed-bundle cases, import rollback checks and repeated source/destination fixtures. |
| TestBundleWithoutAndWithBadNewFields | 0.39 | 0.44 | 3–10×: bundle variants and seeded stores, archive creation/import and validation. |
| TestRelativeAndOddPaths | 0.43 | 0.44 | 2–8×: backup/restore/reopen per path; Windows runs three names versus five on Unix, partly offsetting I/O cost. |
| TestDependsBundleCapDryRunMatchesImport | 0.36 | 0.41 | 3–10×: 20 projects, team/dependency writes, export and dry-run/import destination fixtures. |
| TestRestoreComparesTriggerBodies | 0.38 | 0.40 | 3–10×: trigger/schema validation variants plus backup/restore fixtures and durable file writes. |
| TestMembershipOrigins | 0.35 | 0.35 | 3–10×: source/destination membership, bundle import and origin changes with full seeded fixtures. |
| TestBundleArchiveLimits | 0.31 | 0.31 | 2–8×: archive/filesystem limit cases, database fixture and zip validation; partly CPU-bound. |
| TestSearchDeadline | 0.31 | 0.31 | About 250 ms query deadline plus fixture I/O; query portion should not scale with timer granularity alone. |
| TestTeamDependsAdminLimits | 0.37 | 0.31 | 3–10×: >15 projects/dependencies and team mutations, each public call commits. |
| TestAMoreRunWithNoProgressBacksOff | 0.37 | 0.31 | 3–10×: fixture/push bookkeeping and repeated AutoPush calls; backoff advances fake time. |
| TestBundleReceiptBackupRestore | 0.28 | 0.29 | 3–10×: export/import receipts plus backup, restore, sync and reopen. |
| TestRestoreInstallsWhatItValidatedWhenTheSourceIsSwapped | 0.34 | 0.29 | 3–10×: backup/restore fixture, second database, source replacement after staging and restore validation. |
| TestAnEditStormIsOnePush | 0.39 | 0.29 | 3–10×: repeated note edits commit individually; fake-clock debounce, not real waiting. |
| TestNotesNoLongerHereAreListedNotPropagated | 0.26 | 0.29 | 3–10×: local/upstream note state changes, receipts and public-write commits. |
| TestConcurrentWriters | 0.26 | 0.28 | 3–10×: serialized Store.write commits under concurrent goroutines; durability still serial. |
| TestBundleDisplayAuthorHasNoOwnership | 0.28 | 0.27 | 3–10×: multiple stores/import metadata and ownership checks with durable fixture setup. |
| TestDependsAdmin | 0.25 | 0.27 | 3–10×: dependency graph writes and policy checks on a seeded store. |
| TestMigrationSnapshotAndPrune | 0.25 | 0.26 | 3–10×: repeated migration/open/snapshot/prune operations, backups and durable state writes. |
