# Finn Desktop Daemon - Tray Icon Assets

## Required Files

### icon.png (System Tray/Menu Bar Icon)
- **Size**: 22x22 pixels (macOS standard) or 16x16 (Windows)
- **Format**: PNG with transparency
- **Content**: Just the "F" letterform (no white background square)
- **Color**: Teal/aqua gradient from your logo, OR white for template-style

## macOS Menu Bar Guidelines

macOS menu bar icons should be:
- 16-22 pixels in height (22px recommended for Retina clarity)
- Simple, recognizable at small size
- Ideally "template" style (single color that adapts to light/dark)
- If colored: ensure it's visible on both light and dark menu bars

## Windows System Tray Guidelines

Windows system tray icons:
- 16x16 pixels (standard DPI)
- 32x32 pixels (high DPI) - optional
- PNG or ICO format

## Export Instructions

From your F logo design:

1. Export JUST the "F" letterform (without the white square background)
2. Size: 22x22 pixels
3. Transparent background
4. Keep the teal gradient, or use solid white for template-style

### For Template-Style (Recommended for macOS):
- Export the F in solid white (#FFFFFF)
- macOS will automatically invert for light menu bars
- Looks professional and native

### For Colored Icon:
- Keep the teal gradient
- Works on both platforms
- More branded but less "native" feeling

## Current Implementation

The icon is embedded at compile time using Go's `//go:embed` directive.
Place your `icon.png` file in this directory and rebuild.
