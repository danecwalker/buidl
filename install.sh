#!/usr/bin/env bash
# install.sh installs the latest buidl release.
#
#   curl -fsSL https://raw.githubusercontent.com/danecwalker/buidl/main/install.sh | bash
#
# This is the supported way to install a release binary. It detects the
# platform, verifies SHA-256 against checksums.txt from the same GitHub
# release, and writes `buidl` to BUIDL_INSTALL_DIR (default ~/.local/bin).
# That directory is user-owned, so later `buidl update` does not need sudo.
# If ~/.local/bin is not on PATH, the script links /usr/local/bin/buidl at
# it (sudo once). Set BUIDL_INSTALL_DIR to pick another location.
#
# Do not pipe this to `sudo bash`. The installer prompts itself when it
# needs a password; wrapping the whole script in sudo is unnecessary, and
# `sudo -S` would treat the rest of the script as the password.
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
  printf '%s/.local/bin' "${HOME:?HOME is not set}"
}

# True when this user cannot create or write dest without privilege.
needs_privilege() {
  local dir parent next
  dir=$1
  if [ "$(id -u)" -eq 0 ]; then
    return 1
  fi
  if [ -d "$dir" ]; then
    [ ! -w "$dir" ]
    return
  fi
  parent=$dir
  while [ ! -e "$parent" ]; do
    next=$(dirname "$parent")
    if [ "$next" = "$parent" ]; then
      break
    fi
    parent=$next
  done
  [ ! -w "$parent" ]
}

as_root() {
  if [ "${need_sudo:-0}" -eq 1 ]; then
    sudo -p "     password for %p: " -- "$@"
  else
    "$@"
  fi
}

# Default dest is ~/.local/bin so updates stay user-owned. When a dest
# (or the PATH symlink) is not writable, prompt via /dev/tty (stdin is
# the piped script). Passwordless sudo skips the prompt. Never sudo -S.
prepare_dest() {
  need_sudo=0
  if ! needs_privilege "$dest_dir"; then
    mkdir -p -- "$dest_dir"
    [ -w "$dest_dir" ] || fail "cannot write to $dest_dir (set BUIDL_INSTALL_DIR to a writable directory)"
    return
  fi

  command -v sudo >/dev/null 2>&1 ||
    fail "sudo is required to write $dest_dir (set BUIDL_INSTALL_DIR to a writable directory)"
  info "needs sudo to write $dest_dir"
  if ! sudo -n -v >/dev/null 2>&1; then
    if ! { true </dev/tty >/dev/tty; } 2>/dev/null; then
      fail "cannot write to $dest_dir and cannot prompt for sudo (set BUIDL_INSTALL_DIR to a writable directory)"
    fi
    sudo -p "     password for %p: " -v </dev/tty ||
      fail "sudo is required to write $dest_dir (set BUIDL_INSTALL_DIR to a writable directory)"
  fi
  need_sudo=1
  as_root mkdir -p -- "$dest_dir"
}

on_path() {
  case ":${PATH}:" in
    *":$1:"*) return 0 ;;
    *) return 1 ;;
  esac
}

print_path_hint() {
  local dir=$1
  info "add ${dir} to PATH"
  case ${SHELL##*/} in
    fish) info "  fish_add_path ${dir}" ;;
    zsh)  info "  echo 'export PATH=\"${dir}:\$PATH\"' >> ~/.zshrc && hash -r" ;;
    *)    info "  echo 'export PATH=\"${dir}:\$PATH\"' >> ~/.bashrc && hash -r" ;;
  esac
}

# Point a PATH directory at the user-owned binary so `buidl` works without
# putting ~/.local/bin on PATH. Best-effort: a failed link must not undo
# a successful install. Must not call fail.
install_path_link() {
  local link_dir link
  if on_path "$dest_dir"; then
    return 0
  fi

  link_dir="${BUIDL_PATH_LINK_DIR:-/usr/local/bin}"
  if ! on_path "$link_dir"; then
    return 1
  fi

  link="$link_dir/buidl"
  if ! prepare_link_dir "$link_dir"; then
    return 1
  fi

  # ln -sf will not replace a regular file (the previous default dest).
  as_root rm -f -- "$link" || return 1
  as_root ln -s -- "$dest" "$link" || return 1
  info "linked      $link -> $dest"
  return 0
}

# Like prepare_dest, but returns 1 instead of exiting so a PATH symlink
# is optional.
prepare_link_dir() {
  local dir=$1
  need_sudo=0
  if ! needs_privilege "$dir"; then
    mkdir -p -- "$dir" 2>/dev/null || return 1
    [ -w "$dir" ]
    return
  fi
  command -v sudo >/dev/null 2>&1 || return 1
  info "needs sudo to link $dir/buidl"
  if ! sudo -n -v >/dev/null 2>&1; then
    if ! { true </dev/tty >/dev/tty; } 2>/dev/null; then
      return 1
    fi
    sudo -p "     password for %p: " -v </dev/tty || return 1
  fi
  need_sudo=1
  as_root mkdir -p -- "$dir"
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
info "install dir  $dest_dir"
need_sudo=0
prepare_dest
dest="$dest_dir/buidl"

tmp=$(mktemp -d)
install_tmp=""
cleanup() {
  rm -rf "$tmp"
  if [ -n "$install_tmp" ]; then
    if [ "${need_sudo:-0}" -eq 1 ]; then
      sudo -n rm -f -- "$install_tmp" 2>/dev/null || true
    else
      rm -f -- "$install_tmp"
    fi
  fi
}
trap cleanup EXIT

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
as_root cp -- "$tmp/$asset" "$install_tmp"
as_root chmod 755 "$install_tmp"
as_root mv -f -- "$install_tmp" "$dest"
install_tmp=""

ok "installed    $dest"

if ver=$("$dest" --version 2>/dev/null); then
  info "$ver"
fi

# A PATH symlink is only for the default ~/.local/bin install. An explicit
# BUIDL_INSTALL_DIR is the dest the user asked for; do not also touch
# /usr/local/bin.
if [ -z "$INSTALL_DIR" ]; then
  install_path_link || print_path_hint "$dest_dir"
else
  case ":${PATH}:" in
    *":${dest_dir}:"*) ;;
    *) print_path_hint "$dest_dir" ;;
  esac
fi

info "next         buidl init && buidl deploy"
info "later        buidl update"
printf '\n'
