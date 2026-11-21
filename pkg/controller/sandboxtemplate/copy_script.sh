set -euo pipefail
temp=$(mktemp -d /tmp/template-copy.XXXXXXXX)
trap 'chmod -R u+rwx -- "$temp" || printf "%s\n" "COPY cleanup could not reset temporary directory permissions" >&2; rm -rf -- "$temp"' EXIT
mkdir -p -- "$temp/unpack"
tar -xzf "$archive" -C "$temp/unpack"
sourcePath="$temp/unpack$sourcePrefix"
if [[ "$owner" != *:* ]]; then
    owner="$owner:$(id -g "$owner")"
fi
sourceFolder="$(dirname "$sourcePath")"
cd "$sourceFolder"
entry="$(basename "$sourcePath")"
if [ ! -e "$entry" ] && [ ! -L "$entry" ]; then
    exit 1
fi
if [ -L "$entry" ]; then
    if [[ "$targetPath" == */ ]]; then
        mkdir -p -- "$targetPath"
    else
        mkdir -p -- "$(dirname "$targetPath")"
    fi
    chown -h -- "$owner" "$entry"
    mv -- "$entry" "$targetPath"
elif [ -f "$entry" ]; then
    chown -- "$owner" "$entry"
    if [ -n "$permissions" ]; then
        chmod -- "$permissions" "$entry"
    fi
    if [[ "$targetPath" == */ ]]; then
        mkdir -p -- "$targetPath"
    else
        mkdir -p -- "$(dirname "$targetPath")"
    fi
    mv -- "$entry" "$targetPath"
elif [ -d "$entry" ]; then
    chown -R -- "$owner" "$entry"
    if [ -n "$permissions" ]; then
        chmod -R -- "$permissions" "$entry"
    fi
    mkdir -p -- "$targetPath"
    (cd "$entry" && tar -cf - .) | tar -xf - -C "$targetPath" --keep-directory-symlink --no-overwrite-dir
else
    exit 1
fi
