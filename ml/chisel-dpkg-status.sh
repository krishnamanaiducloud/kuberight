#!/bin/sh
# Writes dpkg status records (the status.d layout distroless images use) for the packages in a
# chiselled Ubuntu root filesystem, so image scanners can match them against Ubuntu's security
# data. Chisel's manifest records each package's name, version and architecture and the slice
# that owns every path; the source package name comes from the Ubuntu archive's metadata.
#
# Usage: chisel-dpkg-status.sh ROOTFS OUTPUT_DIR   (needs zstd, jq and `apt-get update`)
set -eu

rootfs=$1
out=$2
manifest=$(mktemp)
mkdir -p "$out"
zstd -qdc "$rootfs/var/lib/chisel/manifest.wall" > "$manifest"

tab=$(printf '\t')
jq -r 'select(.kind == "package") | [.name, .version, .arch] | @tsv' "$manifest" |
while IFS="$tab" read -r name version arch; do
  # The first stanza of the archive record, preferably for the installed version.
  record=$( { apt-cache show "$name=$version" 2>/dev/null || apt-cache show "$name" 2>/dev/null || true; } \
    | awk 'NF == 0 { exit } { print }')
  field() { printf '%s\n' "$record" | sed -n "s/^$1: //p" | head -n 1; }
  source=$(field Source)
  source=${source%% (*}
  {
    echo "Package: $name"
    echo "Status: install ok installed"
    for key in Priority Section Maintainer; do
      value=$(field "$key")
      if [ -n "$value" ]; then echo "$key: $value"; fi
    done
    echo "Architecture: $arch"
    echo "Source: ${source:-$name}"
    echo "Version: $version"
    description=$(field Description)
    echo "Description: ${description:-$(field Description-en)}"
  } > "$out/$name"
  # File list, so scanners attribute the package's binaries to it.
  jq -r --arg prefix "${name}_" \
    'select(.kind == "path" and any(.slices[]; startswith($prefix))) | .path' "$manifest" |
  while read -r path; do
    if [ -f "$rootfs$path" ] && [ ! -L "$rootfs$path" ]; then
      (cd "$rootfs" && md5sum "${path#/}")
    fi
  done > "$out/$name.md5sums"
done
rm -f "$manifest"
ls "$out" | grep -vc '\.md5sums$' | sed 's/^/dpkg status records: /'
