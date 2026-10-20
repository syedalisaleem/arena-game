#!/bin/bash
# Sync the canonical client (web/) into server/web/ for go:embed.
set -e
rm -rf server/web
mkdir -p server/web
cp -r web/. server/web/
echo "synced web/ -> server/web/"
