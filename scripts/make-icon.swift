// Nariše ikono aplikacije in jo zapiše v Resources/AppIcon.icns.
// Zagon (iz mape MiniGateway): swift scripts/make-icon.swift
import AppKit

let root = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
let iconset = FileManager.default.temporaryDirectory.appendingPathComponent("AppIcon.iconset")
try? FileManager.default.removeItem(at: iconset)
try FileManager.default.createDirectory(at: iconset, withIntermediateDirectories: true)

func draw(_ px: Int) -> Data {
    let s = CGFloat(px)
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: px, pixelsHigh: px, bitsPerSample: 8,
                               samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
                               bytesPerRow: 0, bitsPerPixel: 0)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    let ctx = NSGraphicsContext.current!.cgContext

    // Mreža ikon macOS: ploščica 824/1024, polmer ~185/1024, rahla senca.
    let inset = s * 100 / 1024
    let tile = CGRect(x: inset, y: inset, width: s - 2 * inset, height: s - 2 * inset)
    let shape = CGPath(roundedRect: tile, cornerWidth: s * 185 / 1024, cornerHeight: s * 185 / 1024, transform: nil)
    ctx.saveGState()
    ctx.setShadow(offset: CGSize(width: 0, height: -s * 10 / 1024), blur: s * 24 / 1024,
                  color: NSColor.black.withAlphaComponent(0.35).cgColor)
    ctx.addPath(shape)
    ctx.setFillColor(NSColor.black.cgColor)
    ctx.fillPath()
    ctx.restoreGState()

    ctx.saveGState()
    ctx.addPath(shape)
    ctx.clip()
    // Temna osnova + barvni »orbi« v slogu Siri.
    let space = CGColorSpaceCreateDeviceRGB()
    let base = CGGradient(colorsSpace: space, colors: [NSColor(red: 0.10, green: 0.07, blue: 0.22, alpha: 1).cgColor,
                                                      NSColor(red: 0.03, green: 0.03, blue: 0.10, alpha: 1).cgColor] as CFArray,
                          locations: [0, 1])!
    ctx.drawLinearGradient(base, start: CGPoint(x: tile.minX, y: tile.maxY), end: CGPoint(x: tile.maxX, y: tile.minY), options: [])
    let orbs: [(NSColor, CGPoint, CGFloat)] = [
        (NSColor(red: 1.00, green: 0.30, blue: 0.55, alpha: 0.95), CGPoint(x: 0.30, y: 0.70), 0.55),
        (NSColor(red: 0.55, green: 0.30, blue: 1.00, alpha: 0.90), CGPoint(x: 0.72, y: 0.62), 0.50),
        (NSColor(red: 0.20, green: 0.60, blue: 1.00, alpha: 0.90), CGPoint(x: 0.50, y: 0.25), 0.55),
    ]
    for (color, c, r) in orbs {
        let g = CGGradient(colorsSpace: space, colors: [color.cgColor, color.withAlphaComponent(0).cgColor] as CFArray, locations: [0, 1])!
        let center = CGPoint(x: tile.minX + tile.width * c.x, y: tile.minY + tile.height * c.y)
        ctx.drawRadialGradient(g, startCenter: center, startRadius: 0, endCenter: center, endRadius: tile.width * r, options: [])
    }
    ctx.restoreGState()

    // Bel simbol razvejitve (usmerjevalnik).
    let config = NSImage.SymbolConfiguration(pointSize: s * 0.42, weight: .semibold)
        .applying(NSImage.SymbolConfiguration(paletteColors: [.white]))
    if let glyph = NSImage(systemSymbolName: "arrow.triangle.branch", accessibilityDescription: nil)?.withSymbolConfiguration(config) {
        let gs = glyph.size
        let rect = CGRect(x: (s - gs.width) / 2, y: (s - gs.height) / 2, width: gs.width, height: gs.height)
        ctx.saveGState()
        ctx.setShadow(offset: .zero, blur: s * 18 / 1024, color: NSColor.white.withAlphaComponent(0.45).cgColor)
        glyph.draw(in: rect)
        ctx.restoreGState()
    }
    NSGraphicsContext.restoreGraphicsState()
    return rep.representation(using: .png, properties: [:])!
}

for size in [16, 32, 128, 256, 512] {
    try draw(size).write(to: iconset.appendingPathComponent("icon_\(size)x\(size).png"))
    try draw(size * 2).write(to: iconset.appendingPathComponent("icon_\(size)x\(size)@2x.png"))
}
let p = Process()
p.executableURL = URL(fileURLWithPath: "/usr/bin/iconutil")
p.arguments = ["-c", "icns", iconset.path, "-o", root.appendingPathComponent("Resources/AppIcon.icns").path]
try p.run()
p.waitUntilExit()
try draw(1024).write(to: root.appendingPathComponent("Resources/AppIcon-preview.png"))
print(p.terminationStatus == 0 ? "Resources/AppIcon.icns" : "iconutil failed")
