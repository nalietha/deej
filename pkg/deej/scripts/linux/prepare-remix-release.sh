#!/bin/sh

# builds every deej remix release artifact from linux (windows binaries are cross-compiled)
# usage: pkg/deej/scripts/linux/prepare-remix-release.sh <version>    (i.e. v0.9.10-remix.1)
# run from the root deej directory. output goes to releases/<version>/

if [ -z "$1" ]; then
    echo "usage: $0 <version>    (use semver i.e. v0.9.10-remix.1)"
    exit 1
fi

set -e

VERSION_TAG=$1
GIT_COMMIT=$(git rev-list -1 --abbrev-commit HEAD)
if [ -n "$(git status --porcelain)" ]; then
    GIT_COMMIT="$GIT_COMMIT-dirty"
fi

OUT="releases/$VERSION_TAG"
STAGE="$OUT/stage"
FLAGS="-X main.gitCommit=$GIT_COMMIT -X main.versionTag=$VERSION_TAG"

echo "Preparing deej remix release $VERSION_TAG (commit $GIT_COMMIT)..."

echo '- running tests'
go test ./pkg/...

rm -rf "$OUT"
mkdir -p "$STAGE/windows" "$STAGE/linux"

echo '- building windows release'
GOOS=windows GOARCH=amd64 go build -o "$STAGE/windows/deej.exe" -ldflags "-H=windowsgui -s -w $FLAGS -X main.buildType=release" ./pkg/deej/cmd

echo '- building windows debug (with console)'
GOOS=windows GOARCH=amd64 go build -o "$STAGE/windows/deej-debug.exe" -ldflags "$FLAGS -X main.buildType=dev" ./pkg/deej/cmd

echo '- building linux release'
GOOS=linux GOARCH=amd64 go build -o "$STAGE/linux/deej" -ldflags "-s -w $FLAGS -X main.buildType=release" ./pkg/deej/cmd

for platform in windows linux; do
    cp pkg/deej/scripts/misc/default-config.yaml "$STAGE/$platform/config.yaml"
    cp arduino/deej-remix/deej-remix.ino "$STAGE/$platform/"
    cp REMIX.md "$STAGE/$platform/README.md"
done

sed "s/__VERSION__/$VERSION_TAG/" pkg/deej/scripts/misc/release-notes.txt > "$OUT/notes.txt"

echo '- packaging'
(cd "$STAGE/windows" && zip -q -r "../../deej-remix-$VERSION_TAG-windows-amd64.zip" .)
(cd "$STAGE/linux" && tar -czf "../../deej-remix-$VERSION_TAG-linux-amd64.tar.gz" .)

# loose files too, for attaching individually to a github release
cp "$STAGE/windows/deej.exe" "$STAGE/windows/deej-debug.exe" "$STAGE/windows/config.yaml" "$STAGE/windows/deej-remix.ino" "$OUT/"
rm -rf "$STAGE"

(cd "$OUT" && sha256sum deej-remix-* deej.exe deej-debug.exe > SHA256SUMS)

echo
echo "Release created in $OUT:"
ls -lh "$OUT"
