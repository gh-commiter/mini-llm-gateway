// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "MiniGateway",
    platforms: [.macOS("27.0")],
    products: [.executable(name: "mg-test-net", targets: ["MGTestNet"])],
    targets: [
        .target(name: "RouterCore"),
        .executableTarget(name: "MiniGateway", dependencies: ["RouterCore"]),
        .executableTarget(name: "MiniGW", dependencies: ["RouterCore"]),  // ukaz minigw (Contents/Helpers)
        .executableTarget(name: "MGTestNet"),  // mg-test-net: the signed utility for tests that need a socket outside 127.0.0.1 (scripts/test-net-install.sh)
        .testTarget(name: "RouterCoreTests", dependencies: ["RouterCore"]),
    ],
    swiftLanguageModes: [.v5]
)
