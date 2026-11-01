// Windows the window server has ON SCREEN for one process: the OS's answer, not Electron's.
import CoreGraphics
import Foundation
let pid = Int(CommandLine.arguments[1])!
let list = CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as! [[String: Any]]
var out: [[String: Any]] = []
for w in list where (w[kCGWindowOwnerPID as String] as? Int) == pid {
  let b = w[kCGWindowBounds as String] as? [String: Any] ?? [:]
  out.append(["id": w[kCGWindowNumber as String] ?? 0, "layer": w[kCGWindowLayer as String] ?? 0, "w": b["Width"] ?? 0, "h": b["Height"] ?? 0, "x": b["X"] ?? 0, "y": b["Y"] ?? 0])
}
print(String(data: try! JSONSerialization.data(withJSONObject: out), encoding: .utf8)!)
