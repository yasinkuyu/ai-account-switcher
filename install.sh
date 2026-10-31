#!/bin/sh
# aisw installer for macOS and Linux:
#   curl -fsSL https://raw.githubusercontent.com/yasinkuyu/ai-account-switcher/main/install.sh | sh
set -eu

REPO="yasinkuyu/ai-account-switcher"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  *) echo "Unsupported OS: $(uname -s). On Windows use install.ps1." >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "Unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac

dir="${AISW_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dir"
base="${AISW_DOWNLOAD_BASE:-https://github.com/$REPO/releases/latest/download}"
url="$base/aisw-$os-$arch"
tmp="$(mktemp)"

echo "Downloading aisw ($os/$arch)..."
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$url" -o "$tmp"
else
  wget -qO "$tmp" "$url"
fi
chmod +x "$tmp"
mv "$tmp" "$dir/aisw"

echo "Installed: $dir/aisw"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add it to your PATH:  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.$(basename "${SHELL:-sh}")rc" ;;
esac
echo "Run: aisw"
