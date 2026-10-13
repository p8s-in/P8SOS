# P8S — brand asset pack

Vector rebuild of the supplied logo sheet, plus 8K raster exports.

## Contents

### /SVG — resolution-independent masters
| File | Use |
|---|---|
| p8s-primary-stacked-blue / -black / -white | Primary lockup (mark + tagline) |
| p8s-wordmark-blue / -black / -white | Mark only, no tagline |
| p8s-symbol-blue / -black / -white | Standalone 8 symbol |
| p8s-app-icon | Blue tile, white mark |
| p8s-app-icon-inverted | White tile, blue mark |
| p8s-favicon-symbol | Blue tile, symbol only |

### /PNG_8K — 7680 px on the long edge, transparent background
Same twelve variants. The `-white` files are white artwork on transparency —
they look empty in a white-background previewer. Drop them on a dark surface.

### /PNG_app_icons — 1024, 512, 256, 180, 128, 64, 32, 16 px
Ready for iOS (180), Android (512), PWA (192/512), favicon (32/16).

## Colour
- Brand blue `#014578`
- Reversed white `#FFFFFF`
- Monochrome black `#111111`
- Tagline grey (stacked lockup) `#5A5A5A`

## Notes on fidelity
- The P8S wordmark and tagline were auto-traced from the supplied raster, then
  smoothed. Curves are close to the original but not mathematically identical.
- The standalone 8 symbol was rebuilt as true geometry (two 130-unit circles,
  78-unit monoline stroke, round caps) because the source crop was too small to
  trace cleanly. It matches the original silhouette.
- The gradient / bevel shading of the source sheet was intentionally dropped.
  Flat vector is what a logo file should be; shading belongs in the mockup.
