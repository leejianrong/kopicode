#!/bin/sh
# Exercises scripts/install.sh end to end against a fake `curl`, so the
# checksum paths are tested without a real release: good sums, a tampered
# binary, a release with no SHA256SUMS, and a SHA256SUMS missing the asset.
# Plain POSIX sh, like the script under test.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/site"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; *) arch=arm64 ;; esac
asset="kopicode-${os}-${arch}"

# The fake curl serves files from $work/site by URL basename, and the API
# response from $work/site/api.json (with the status line install.sh asks for).
cat > "$work/bin/curl" <<'FAKE'
#!/bin/sh
out=""; url=""; want_status=0
while [ $# -gt 0 ]; do
    case "$1" in
        -o) out=$2; shift ;;
        -w) want_status=1; shift ;;
        -*) ;;
        *) url=$1 ;;
    esac
    shift
done
case "$url" in
    */releases/latest)
        cat "$SITE/api.json"
        [ "$want_status" = 1 ] && printf '\n200'
        exit 0 ;;
    *) f="$SITE/$(basename "$url")"
       [ -f "$f" ] || exit 22
       if [ -n "$out" ]; then cp "$f" "$out"; else cat "$f"; fi ;;
esac
FAKE
chmod +x "$work/bin/curl"

fail=0
run() { # name, expected exit (0|nonzero), expected stderr substring
    name=$1; want=$2; needle=$3
    dest="$work/install-$name"
    rc=0
    out=$(SITE="$work/site" PATH="$work/bin:$PATH" INSTALL_DIR="$dest" sh "$here/install.sh" 2>&1) || rc=$?
    ok=1
    if [ "$want" = 0 ] && [ "$rc" != 0 ]; then ok=0; fi
    if [ "$want" != 0 ] && [ "$rc" = 0 ]; then ok=0; fi
    printf '%s' "$out" | grep -q -- "$needle" || ok=0
    if [ "$want" != 0 ] && [ -e "$dest/kopicode" ]; then ok=0; echo "  binary was installed despite the failure"; fi
    if [ "$want" != 0 ] && [ -e "$dest/kopicode.download" ]; then ok=0; echo "  temp download left behind"; fi
    if [ "$ok" = 1 ]; then echo "ok   $name"; else echo "FAIL $name (exit $rc)"; echo "$out" | sed 's/^/  | /'; fail=1; fi
}

printf 'genuine binary\n' > "$work/site/$asset"
sum=$(if command -v sha256sum >/dev/null 2>&1; then sha256sum "$work/site/$asset"; else shasum -a 256 "$work/site/$asset"; fi | awk '{print $1}')

api() { # with|without SHA256SUMS listed
    extra=""
    [ "$1" = with ] && extra=',{"name": "SHA256SUMS"}'
    printf '{"tag_name": "v9.9.9", "assets": [{"name": "%s"}%s]}' "$asset" "$extra" > "$work/site/api.json"
}

api with;    printf '%s  %s\n' "$sum" "$asset" > "$work/site/SHA256SUMS"
run good 0 "verified $asset"

api with;    printf 'tampered\n' > "$work/site/$asset"
run tampered 1 "checksum mismatch"

api with;    printf '%s  %s\n' "$sum" "other-asset" > "$work/site/SHA256SUMS"
printf 'genuine binary\n' > "$work/site/$asset"
run no-entry 1 "has no entry"

api without
run no-sums 0 "publishes no SHA256SUMS"

exit "$fail"
