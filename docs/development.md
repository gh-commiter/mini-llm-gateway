# Development notes

## Tests and the network

Tests listen and bind only on `127.0.0.1`. The macOS firewall asks the owner for permission for every unsigned program that accepts connections,
and a test process (`swiftpm-testing-helper`, a `go test` binary) is one. A test that really needs a socket outside loopback (a listener on every
address, a bind attempt on `0.0.0.0`) uses the signed utility instead:

- `Sources/MGTestNet` is the program `mg-test-net`: `listen <ipv4> <port> [--mode close|echo|hold] [--accept N] [--deadline S]` and
  `bind <ipv4> <port> [--reuseaddr] [--reuseport]`. It does network things only (no files, no commands), prints one JSON line per event and ends
  on stdin EOF or at the deadline.
- `scripts/test-net-install.sh` builds it, signs it with the Developer ID identity (as `build.sh`), bundles it as
  `Mini Gateway Test Net.app` with the fixed identifier `dev.trojar.minigateway.testnet` and installs it under
  `/Volumes/SAMSUNG990PRO/Mini Gateway Scratch/test-tools/`. Run it once per Mac (and after a change of `Sources/MGTestNet`); the firewall
  permission binds to the signed app and survives rebuilds. It refuses to run in CI.
- Swift tests use it through `TestNet` (`Tests/RouterCoreTests/TestNet.swift`): `TestNet.locate()` takes `MG_TEST_NET`, else the install
  location; on CI (`CI` is set; GitHub's runner has no firewall) the package's own unsigned build next to the test bundle. Without it the test fails
  with a hint naming the install script. A test never falls back to listening itself. Go tests can exec the same program.
- Guards: `NonLoopbackListenerGuardTests.swift` (Swift) and `client/nonloopback_guard_test.go` (Go) fail on a new `INADDR_ANY`, `"0.0.0.0"`,
  `"::"`, `getifaddrs` next to a bind, or a `net.Listen` without a loopback address in a test, unless the place is on the guard's allow-list with a
  reason.
