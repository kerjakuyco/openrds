#!/bin/sh
# Attach or release the fixed-size ext4 image behind one database volume.
# Docker's local driver can mount an existing ext4 device, but it cannot turn
# a file into a loop device itself. This helper does that in the host namespace.
set -eu

link_ok() {
  printf '%s' "${OPENRDS_LINK:-}" | grep -Eq '^openrds-db-[a-f0-9]{32}$'
}

image_ok() {
  link_ok || return 1
  printf '%s' "${OPENRDS_BYTES:-}" | grep -Eq '^[1-9][0-9]*$' || return 1
  case "${OPENRDS_IMAGE:-}" in
    *..*|[[:space:]]*) return 1 ;;
    */openrds_disk_images/_data/"$OPENRDS_LINK".img) ;;
    *) return 1 ;;
  esac
}

host() {
  nsenter -t 1 -m env OPENRDS_IMAGE="$OPENRDS_IMAGE" OPENRDS_LINK="$OPENRDS_LINK" sh -c "$1"
}

case "${1:-}" in
  check)
    if ! out=$(nsenter -t 1 -m losetup -f 2>&1); then
      echo "no free loop device on the host: ${out:-losetup failed}" >&2
      exit 1
    fi
    ;;
  attach)
    image_ok || { echo "invalid disk image request" >&2; exit 1; }
    file=$(basename "$OPENRDS_IMAGE")
    if [ ! -f "/images/$file" ]; then
      truncate -s "$OPENRDS_BYTES" "/images/$file"
      mkfs.ext4 -q -F -m 0 "/images/$file"
    fi
    actual=$(stat -c %s "/images/$file")
    [ "$actual" = "$OPENRDS_BYTES" ] || {
      echo "existing disk image is ${actual} bytes, not ${OPENRDS_BYTES}; it was left unchanged" >&2
      exit 1
    }
    sync
    host '
      set -eu
      loop=$(losetup -j "$OPENRDS_IMAGE" | sed -n "s/:.*//p" | head -n 1)
      if [ -z "$loop" ]; then
        if ! loop=$(losetup -f --show --direct-io=on "$OPENRDS_IMAGE" 2>/dev/null); then
          loop=$(losetup -f --show "$OPENRDS_IMAGE")
        fi
      fi
      mkdir -p /dev/openrds
      ln -sfn "$loop" "/dev/openrds/$OPENRDS_LINK"
    '
    ;;
  detach)
    image_ok || { echo "invalid disk image request" >&2; exit 1; }
    host '
      set -eu
      loop=$(losetup -j "$OPENRDS_IMAGE" | sed -n "s/:.*//p" | head -n 1)
      rm -f "/dev/openrds/$OPENRDS_LINK"
      if [ -n "$loop" ]; then
        losetup -d "$loop"
      fi
    '
    rm -f "/images/$(basename "$OPENRDS_IMAGE")"
    ;;
  resize)
    image_ok || { echo "invalid disk image request" >&2; exit 1; }
    file=$(basename "$OPENRDS_IMAGE")
    [ -f "/images/$file" ] || { echo "disk image not found" >&2; exit 1; }
    host '
      set -eu
      loop=$(losetup -j "$OPENRDS_IMAGE" | sed -n "s/:.*//p" | head -n 1)
      if [ -n "$loop" ] && grep -q "^$loop " /proc/mounts; then
        echo "Stop the database before changing its disk limit." >&2
        exit 1
      fi
      if grep -q "^/dev/openrds/$OPENRDS_LINK " /proc/mounts; then
        echo "Stop the database before changing its disk limit." >&2
        exit 1
      fi
      if [ -n "$loop" ]; then
        losetup -d "$loop"
      fi
      rm -f "/dev/openrds/$OPENRDS_LINK"
    '
    set +e
    e2fsck -f -p "/images/$file"
    status=$?
    set -e
    [ "$status" -le 1 ] || { echo "Filesystem check failed. The disk image was left unchanged." >&2; exit 1; }
    old=$(stat -c %s "/images/$file")
    kb=$((OPENRDS_BYTES / 1024))
    if [ "$OPENRDS_BYTES" -gt "$old" ]; then
      truncate -s "$OPENRDS_BYTES" "/images/$file"
      if ! resize2fs "/images/$file" "${kb}K"; then
        truncate -s "$old" "/images/$file"
        echo "Could not grow the filesystem. The previous size was restored." >&2
        exit 1
      fi
    elif [ "$OPENRDS_BYTES" -lt "$old" ]; then
      if ! resize2fs "/images/$file" "${kb}K"; then
        echo "The data does not fit in the smaller limit. The disk was left unchanged." >&2
        exit 1
      fi
      truncate -s "$OPENRDS_BYTES" "/images/$file"
    else
      resize2fs "/images/$file" "${kb}K"
    fi
    sync
    host '
      set -eu
      if ! loop=$(losetup -f --show --direct-io=on "$OPENRDS_IMAGE" 2>/dev/null); then
        loop=$(losetup -f --show "$OPENRDS_IMAGE")
      fi
      mkdir -p /dev/openrds
      ln -sfn "$loop" "/dev/openrds/$OPENRDS_LINK"
    '
    ;;
  *)
    echo "usage: disk-helper check|attach|detach" >&2
    exit 64
    ;;
esac
