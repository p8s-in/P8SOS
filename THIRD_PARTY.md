# Third-party code

| Component | Version | License | Where |
|---|---|---|---|
| [noVNC](https://github.com/novnc/noVNC) | 1.6.0 (npm `@novnc/novnc`) | MPL-2.0 | `internal/web/novnc.min.js`, license in `internal/web/novnc-LICENSE.txt`, served at `/novnc-LICENSE.txt` |
| [xterm.js](https://github.com/xtermjs/xterm.js) | 5.5.0 (npm `@xterm/xterm`) + `@xterm/addon-fit` 0.10.0 | MIT | `internal/web/xterm.min.js`, `internal/web/xterm-fit.min.js` (unchanged `lib/` builds), CSS inlined in `internal/web/terminal.html`; license in `internal/web/xterm-LICENSE.txt`, served at `/xterm-LICENSE.txt` |

noVNC is bundled with esbuild from the package's `lib/` directory into one file. One line was changed:
in `lib/util/browser.js` the top-level `await _checkWebCodecsH264DecodeSupport()` became a
`.then(...)` assignment so the code can be bundled (H.264 decoding is not used with QEMU's VNC).
The unmodified source is at the link above.

Everything else in p8sd is the Go standard library.
