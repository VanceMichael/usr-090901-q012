#!/bin/sh
# 本地开发辅助：把源码同步到 /tmp/q012dev 并把 go 指令降到 1.24，
# 以便用本地 Go 1.24 工具链构建/测试（仓库 go.mod 保持 go 1.25，
# Docker 镜像 golang:1.25-alpine 使用真正的 1.25 构建）。
set -eu
DEV=/tmp/q012dev
rm -rf "$DEV"
mkdir -p "$DEV"
cd /workspace
tar cf - --exclude=.git --exclude=vendor --exclude=.claude . | (cd "$DEV" && tar xf -)
sed -i 's/^go 1\.25$/go 1.24/' "$DEV/go.mod"
sed -i '/^toolchain /d' "$DEV/go.mod"
if [ -d /workspace/vendor ]; then
  mkdir -p "$DEV/vendor"
  (cd /workspace/vendor && tar cf - .) | (cd "$DEV/vendor" && tar xf -)
fi
echo "synced -> $DEV"
