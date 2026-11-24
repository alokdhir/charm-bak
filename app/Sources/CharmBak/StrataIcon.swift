import AppKit

/// The strata mark: a top slab over two layers, snapshots you dig down
/// through. One geometry, drawn two ways: a one-color template image for
/// the menu bar, and the app icon (tile + Charm gradient), rendered at
/// build time by `--render-icon`.
enum StrataIcon {
    enum State { case idle, paused, failed }

    // Geometry on a 24×24 grid, y down.
    private static func slab() -> CGPath {
        let p = CGMutablePath()
        p.move(to: CGPoint(x: 12, y: 3.5))
        p.addLine(to: CGPoint(x: 20, y: 7.5))
        p.addLine(to: CGPoint(x: 12, y: 11.5))
        p.addLine(to: CGPoint(x: 4, y: 7.5))
        p.closeSubpath()
        return p
    }

    private static func layers() -> CGPath {
        let p = CGMutablePath()
        for y in [12.0, 16.5] {
            p.move(to: CGPoint(x: 4, y: y))
            p.addLine(to: CGPoint(x: 12, y: y + 4))
            p.addLine(to: CGPoint(x: 20, y: y))
        }
        return p
    }

    private static func pauseBars() -> CGPath {
        let p = CGMutablePath()
        for x in [10.4, 13.6] {
            p.move(to: CGPoint(x: x, y: 3.5))
            p.addLine(to: CGPoint(x: x, y: 8.5))
        }
        return p
    }

    private static func stroke(_ path: CGPath, _ ctx: CGContext, width: CGFloat) {
        ctx.addPath(path)
        ctx.setLineWidth(width)
        ctx.setLineCap(.round)
        ctx.setLineJoin(.round)
        ctx.strokePath()
    }

    /// The menu bar image, 18pt. Idle and paused are templates (macOS tints
    /// them for the menu bar); failed adds a coral dot, so it isn't one.
    static func menuBar(_ state: State = .idle) -> NSImage {
        let size: CGFloat = 18
        let image = NSImage(size: NSSize(width: size, height: size), flipped: true) { _ in
            guard let ctx = NSGraphicsContext.current?.cgContext else { return false }
            ctx.scaleBy(x: size / 24, y: size / 24)
            ctx.setStrokeColor(NSColor.black.cgColor)
            stroke(state == .paused ? pauseBars() : slab(), ctx, width: 1.7)
            stroke(layers(), ctx, width: 1.7)
            if state == .failed {
                ctx.setFillColor(NSColor(srgbRed: 1, green: 0.341, blue: 0.490, alpha: 1).cgColor)
                ctx.fillEllipse(in: CGRect(x: 15.6, y: 16, width: 7.2, height: 7.2))
            }
            return true
        }
        image.isTemplate = state != .failed
        return image
    }

    /// A small filled circle in a fixed color, for the menu's status line.
    /// Not a template, so the menu keeps its color.
    static func dot(_ color: NSColor) -> NSImage {
        let image = NSImage(size: NSSize(width: 10, height: 10), flipped: false) { rect in
            color.setFill()
            NSBezierPath(ovalIn: rect.insetBy(dx: 1, dy: 1)).fill()
            return true
        }
        image.isTemplate = false
        return image
    }

    /// The app icon at `px` pixels square: Apple's icon grid (an 824/1024
    /// tile with ~185 radius), near-black tile, strata stroked in the TUI's
    /// pink-to-purple gradient.
    static func appIcon(px: Int) -> CGImage? {
        let s = CGFloat(px)
        guard let ctx = CGContext(data: nil, width: px, height: px, bitsPerComponent: 8, bytesPerRow: 0,
                                  space: CGColorSpace(name: CGColorSpace.sRGB)!,
                                  bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else { return nil }
        // Flip to y-down so the 24-grid geometry reads the same as the menu bar.
        ctx.translateBy(x: 0, y: s)
        ctx.scaleBy(x: 1, y: -1)

        let inset = s * 100 / 1024
        let tile = CGRect(x: inset, y: inset, width: s - 2 * inset, height: s - 2 * inset)
        let tilePath = CGPath(roundedRect: tile, cornerWidth: s * 185 / 1024, cornerHeight: s * 185 / 1024, transform: nil)
        ctx.addPath(tilePath)
        ctx.setFillColor(CGColor(srgbRed: 0.055, green: 0.051, blue: 0.071, alpha: 1))
        ctx.fillPath()
        ctx.addPath(tilePath)
        ctx.setStrokeColor(CGColor(srgbRed: 0.165, green: 0.157, blue: 0.2, alpha: 1))
        ctx.setLineWidth(max(1, s / 512))
        ctx.strokePath()

        // Mark: the 24-grid scaled to ~60% of the tile, centered.
        let mark = tile.width * 0.62
        let t = CGAffineTransform(translationX: tile.midX - mark / 2, y: tile.midY - mark / 2 + mark * 0.02)
            .scaledBy(x: mark / 24, y: mark / 24)
        let outline = CGMutablePath()
        outline.addPath(slab(), transform: t)
        outline.addPath(layers(), transform: t)
        let stroked = outline.copy(strokingWithWidth: mark / 24 * 1.5, lineCap: .round, lineJoin: .round, miterLimit: 10)

        ctx.saveGState()
        ctx.addPath(stroked)
        ctx.clip()
        let colors = [CGColor(srgbRed: 1, green: 0.376, blue: 1, alpha: 1),      // Charmtone Dolly
                      CGColor(srgbRed: 0.42, green: 0.314, blue: 1, alpha: 1)]    // Charmtone Charple
        let gradient = CGGradient(colorsSpace: CGColorSpace(name: CGColorSpace.sRGB), colors: colors as CFArray, locations: [0, 1])!
        ctx.drawLinearGradient(gradient, start: CGPoint(x: tile.minX, y: tile.minY), end: CGPoint(x: tile.maxX, y: tile.maxY), options: [])
        ctx.restoreGState()
        return ctx.makeImage()
    }

    /// Writes an .iconset folder (for `iconutil -c icns`) with every size
    /// macOS asks for.
    static func renderIconset(to dir: URL) throws {
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        for pt in [16, 32, 128, 256, 512] {
            for scale in [1, 2] {
                guard let img = appIcon(px: pt * scale) else { continue }
                let name = scale == 1 ? "icon_\(pt)x\(pt).png" : "icon_\(pt)x\(pt)@2x.png"
                let rep = NSBitmapImageRep(cgImage: img)
                try rep.representation(using: .png, properties: [:])?.write(to: dir.appending(path: name))
            }
        }
    }
}
