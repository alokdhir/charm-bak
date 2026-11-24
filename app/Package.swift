// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "CharmBak",
    platforms: [.macOS(.v14)],
    targets: [
        // The wire types (mirroring internal/protocol) and the small client
        // the menu uses to talk to charm-bak-daemon. The daemon itself is Go.
        .target(name: "CharmBakCore", path: "Sources/CharmBakCore"),
        // The menu bar app itself: SwiftUI and AppKit glue.
        .executableTarget(name: "CharmBak", dependencies: ["CharmBakCore"], path: "Sources/CharmBak"),
        .testTarget(name: "CharmBakCoreTests", dependencies: ["CharmBakCore"], path: "Tests/CharmBakCoreTests"),
    ]
)
