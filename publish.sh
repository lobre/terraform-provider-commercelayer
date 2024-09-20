#!/bin/sh

version=$1

if [ -z "$version" ]; then
    echo "missing version as argument"
    exit 1
fi

bin="terraform-provider-commercelayer_v${version}"
archive="terraform-provider-commercelayer_${version}_linux_amd64.zip"
sha="terraform-provider-commercelayer_${version}_SHA256SUMS"
manifest="terraform-provider-commercelayer_${version}_manifest.json"
sig="terraform-provider-commercelayer_${version}_SHA256SUMS.sig"

dist="dist/$version"


echo "remove old dist $dist"
rm -rf "$dist"

echo "create $dist"
mkdir -p "$dist"

echo "build go code"
CGO_ENABLED=0 go build -o "$bin"

echo "create archive $archive"
zip "$archive" "$bin"

echo "move archive to dist"
mv "$archive" "$dist/"

echo "remove binary"
rm "$bin"

echo "create manifest"
cp terraform-registry-manifest.json "$dist/$manifest"

echo "create shasum"
shasum -a 256 "$dist/$archive" | sed "s|$dist/||" > "$dist/$sha"
shasum -a 256 "$dist/$manifest" | sed "s|$dist/||" >> "$dist/$sha"

echo "add signature"
gpg --output "$dist/$sig" --detach-sign "$dist/$sha"
