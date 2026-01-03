#!/usr/bin/env bash
set -euo pipefail

VERSION=${VERSION:-dev}
TARGETS=${TARGETS:-"windows/amd64 linux/amd64"}
CLEAN=${CLEAN:-false}
RACE=${RACE:-false}
VERBOSE=${VERBOSE:-false}
DIST=dist

log(){ if [ "$VERBOSE" = true ]; then echo "[build] $*"; fi }

if [ "$CLEAN" = true ] && [ -d "$DIST" ]; then rm -rf "$DIST"; fi
mkdir -p "$DIST"

commit=$(git rev-parse --short HEAD 2>/dev/null || echo nogit)
btime=$(date +%Y%m%d-%H%M%S)
ld="-s -w -X main.BuildVersion=$VERSION -X main.BuildCommit=$commit -X main.BuildTime=$btime"

for t in $TARGETS; do
  goos=${t%/*}; goarch=${t#*/}
  outdir="$DIST/$goos-$goarch"
  mkdir -p "$outdir"
  exe=andyapi
  [ "$goos" = windows ] && exe+=".exe"
  echo "Building $t -> $outdir/$exe"
  export GOOS=$goos GOARCH=$goarch CGO_ENABLED=0
  if [ "$RACE" = true ]; then raceFlag=-race; else raceFlag=; fi
  log "go build $raceFlag -ldflags '$ld' -o $outdir/$exe ."
  go build $raceFlag -ldflags "$ld" -o "$outdir/$exe" .
  sha256sum "$outdir/$exe" >> "$outdir/SHA256SUMS"
  # also create a tar.gz artifact
  ( cd "$outdir" && tar -czf "${exe%.*}.tar.gz" "$exe" SHA256SUMS )
done

echo "Build(s) complete. See ./dist"
