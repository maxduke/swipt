#!/usr/bin/env bash

set -euo pipefail

VERSION=$1
TAG=$2
npm install -g npm@latest
for cpu in arm64 x64; do
  mkdir -p "build/server-darwin-$cpu"
  install -m 755 "build/swipt-server-darwin-$cpu" "build/server-darwin-$cpu/swipt-server"
  jq -n --arg cpu "$cpu" --arg version "$VERSION" '{name: "@swipt/server-darwin-\($cpu)", version: $version, license: "GPL-3.0-or-later",
    repository: {type: "git", url: "https://github.com/swipt-automation/swipt.git", directory: "cmd/swipt-server"},
    os: ["darwin"], cpu: [$cpu], bin: {"swipt-server": "swipt-server"}, files: ["swipt-server"], publishConfig: {access: "public"}}' \
    > "build/server-darwin-$cpu/package.json"
done
npm pkg set "version=$VERSION" --prefix api
npm pkg set "version=$VERSION" "peerDependencies.@swipt/api=$VERSION" "optionalDependencies.@swipt/server-darwin-arm64=$VERSION" "optionalDependencies.@swipt/server-darwin-x64=$VERSION" --prefix cli
for package in api build/server-darwin-arm64 build/server-darwin-x64 cli; do
  cp LICENSE "$package/"
  PACKAGE=$(node -p "require('./$package/package.json').name")
  if npm view "$PACKAGE@$VERSION" version > /dev/null 2>&1; then
    echo "$PACKAGE@$VERSION is already published"
    npm dist-tag add "$PACKAGE@$VERSION" "$TAG"
  else
    npm publish "./$package" --tag "$TAG"
  fi
done
