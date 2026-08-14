#!/usr/bin/env bash
# install.sh installs the latest buidl release.
#
#   curl -fsSL https://raw.githubusercontent.com/danecwalker/buidl/main/install.sh | bash
#
# This is the supported way to install a release binary. It detects the
# platform, verifies SHA-256 against checksums.txt from the same GitHub
# release, and writes `buidl` to BUIDL_INSTALL_DIR (default /usr/local/bin,
# or ~/.local/bin when that is not writable).
#
# Do not pipe this to `sudo bash`. A password prompt cannot work when stdin
# is the script; install to a writable directory instead.
set -euo pipefail

BASE_URL="${BUIDL_BASE_URL:-https://github.com/danecwalker/buidl}"
VERSION="${BUIDL_VERSION:-}"
INSTALL_DIR="${BUIDL_INSTALL_DIR:-}"
UA="buidl-install"

color=0
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-}" != "dumb" ]; then
  color=1
fi

if [ "$color" -eq 1 ]; then
  c_reset=$'\033[0m'
  c_bold=$'\033[1m'
  c_dim=$'\033[2m'
  c_red=$'\033[31m'
  c_green=$'\033[32m'
  c_cyan=$'\033[36m'
else
  c_reset=""
  c_bold=""
  c_dim=""
  c_red=""
  c_green=""
  c_cyan=""
fi

step()  { printf '  %s▸%s  %s%s%s\n' "$c_cyan" "$c_reset" "$c_bold" "$1" "$c_reset"; }
info()  { printf '     %s\n' "$1"; }
ok()    { printf '  %s✓%s  %s\n' "$c_green" "$c_reset" "$1"; }
fail()  { printf '  %s✗%s  %s\n' "$c_red" "$c_reset" "$1" >&2; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

fmt_bytes() {
  local n
  n=$1
  if [ "$n" -ge 1048576 ]; then
    awk -v n="$n" 'BEGIN { printf "%.1f MB", n / 1048576 }'
  elif [ "$n" -ge 1024 ]; then
    awk -v n="$n" 'BEGIN { printf "%.1f KB", n / 1024 }'
  else
    printf '%s B' "$n"
  fi
}

# progress_bar rewrites one line. A 50MB release would otherwise flood
# the terminal with wc -c samples.
progress_bar() {
  local have total width filled empty bar i pct
  have=$1
  total=$2
  width=28
  if [ -z "$total" ] || [ "$total" -le 0 ]; then
    printf '\r\033[K     %s downloaded' "$(fmt_bytes "$have")"
    return
  fi
  filled=$((have * width / total))
  if [ "$filled" -gt "$width" ]; then
    filled=$width
  fi
  empty=$((width - filled))
  bar=""
  i=0
  while [ "$i" -lt "$filled" ]; do
    bar="${bar}█"
    i=$((i + 1))
  done
  i=0
  while [ "$i" -lt "$empty" ]; do
    bar="${bar}░"
    i=$((i + 1))
  done
  pct=$((have * 100 / total))
  printf '\r\033[K     %s[%s]%s  %s / %s  %s%%' \
    "$c_cyan" "$bar" "$c_reset" \
    "$(fmt_bytes "$have")" "$(fmt_bytes "$total")" "$pct"
}

file_size() {
  if [ ! -f "$1" ]; then
    printf '0'
    return
  fi
  wc -c < "$1" | tr -d ' '
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  else
    fail "need sha256sum or shasum to verify the download"
  fi
}

detect_platform() {
  local os arch
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)
  case "$arch" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fail "unsupported architecture: $arch (want amd64 or arm64)" ;;
  esac
  case "$os" in
    linux|darwin) ;;
    *) fail "unsupported OS: $os (want linux or darwin)" ;;
  esac
  printf '%s %s' "$os" "$arch"
}

resolve_install_dir() {
  if [ -n "$INSTALL_DIR" ]; then
    printf '%s' "$INSTALL_DIR"
    return
  fi
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    printf '/usr/local/bin'
    return
  fi
  if [ -w /usr/local/bin/buidl ] 2>/dev/null; then
    printf '/usr/local/bin'
    return
  fi
  printf '%s/.local/bin' "${HOME:?}"
}

latest_tag() {
  # Follow /releases/latest and take the last path element. This is the
  # same redirect `buidl update` uses, so the installer and the CLI
  # cannot disagree about what "latest" means.
  local url tag
  url=$(curl -fsSLI -A "$UA" -o /dev/null -w '%{url_effective}' "$BASE_URL/releases/latest") ||
    fail "could not resolve $BASE_URL/releases/latest"
  tag=$(basename "${url%/}")
  if [ -z "$tag" ] || [ "$tag" = "latest" ]; then
    fail "could not determine the latest release tag from $url"
  fi
  printf '%s' "$tag"
}

content_length() {
  curl -fsSIL -A "$UA" "$1" | tr -d '\r' | awk 'tolower($1)=="content-length:" { print $2 }' | tail -1
}

download() {
  local url dest err total pid st detail
  url=$1
  dest=$2
  err=$3
  : >"$dest"
  total=$(content_length "$url" || true)

  set +e
  curl -fL --retry 3 --retry-delay 1 -A "$UA" --output "$dest" "$url" >/dev/null 2>"$err" &
  pid=$!
  if [ -t 1 ]; then
    while kill -0 "$pid" 2>/dev/null; do
      progress_bar "$(file_size "$dest")" "${total:-0}"
      sleep 0.1
    done
  fi
  wait "$pid"
  st=$?
  set -e
  if [ -t 1 ]; then
    progress_bar "$(file_size "$dest")" "${total:-$(file_size "$dest")}"
    printf '\n'
  fi
  if [ "$st" -ne 0 ]; then
    detail=$(tr '\n' ' ' <"$err" | sed 's/[[:space:]]*$//')
    fail "download failed ($url)${detail:+: $detail}"
  fi
}

need curl
need awk
need uname

printf '\n'
step "buidl installer"

platform=$(detect_platform)
os=${platform% *}
arch=${platform#* }
asset="buidl-${os}-${arch}"
info "platform     ${os}/${arch}"

if [ -z "$VERSION" ]; then
  VERSION=$(latest_tag)
fi
info "version      $VERSION"

dest_dir=$(resolve_install_dir)
mkdir -p "$dest_dir"
if [ ! -w "$dest_dir" ]; then
  fail "cannot write to $dest_dir (set BUIDL_INSTALL_DIR or pick a writable location)"
fi
dest="$dest_dir/buidl"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

step "downloading   $asset"
download "$BASE_URL/releases/download/${VERSION}/checksums.txt" "$tmp/checksums.txt" "$tmp/curl-sums.err"
download "$BASE_URL/releases/download/${VERSION}/${asset}" "$tmp/$asset" "$tmp/curl-bin.err"

want=$(awk -v f="$asset" '$2 == f || $2 == "*"f { print $1; exit }' "$tmp/checksums.txt")
if [ -z "$want" ]; then
  fail "checksums.txt has no entry for $asset"
fi
got=$(sha256_of "$tmp/$asset")
if [ "$got" != "$want" ]; then
  fail "checksum mismatch for $asset"
fi
info "checksum     ok"

# Same-directory rename so a crash cannot leave a half-written dest.
install_tmp="$dest_dir/.buidl-install-$$"
cp "$tmp/$asset" "$install_tmp"
chmod 755 "$install_tmp"
mv -f "$install_tmp" "$dest"

ok "installed    $dest"

if ver=$("$dest" --version 2>/dev/null); then
  info "$ver"
fi

case ":${PATH}:" in
  *":${dest_dir}:"*) ;;
  *)
    info "add ${dest_dir} to PATH"
    ;;
esac

info "next         buidl init && buidl deploy"
info "later        buidl update"
printf '\n'
