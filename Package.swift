// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "MiniGateway",
    platforms: [.macOS("27.0")],
    targets: [
        .target(name: "RouterCore"),
        .executableTarget(name: "MiniGateway", dependencies: ["RouterCore"]),
        .executableTarget(name: "MiniGW"),  // ukaz minigw (Contents/Helpers)
        .testTarget(name: "RouterCoreTests", dependencies: ["RouterCore"]),
    ],
    swiftLanguageModes: [.v5]
)
