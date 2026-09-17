#!/bin/sh
# 验收编排：从空数据库出发，依次验证
#   1) 重复回执幂等  2) 工作时间边界  3) 材料阻塞  4) 越权决定
#   5) 期限重算      6) 原子失败回滚  7) 重启后队列顺序
# 用法：scripts/acceptance.sh
set -eu
cd "$(dirname "$0")/.."

echo "== 清理旧环境（空数据库出发）"
docker compose down -v --remove-orphans >/dev/null 2>&1 || true

echo "== 构建镜像"
docker compose build app acceptance

echo "== 启动 PostgreSQL 与应用"
docker compose up -d postgres app

echo "== 阶段一：重启前检查（1~6）"
docker compose run --rm acceptance -phase=before-restart

echo "== 重启应用容器"
docker compose restart app

echo "== 阶段二：重启后检查（7）"
docker compose run --rm acceptance -phase=after-restart

echo
echo "验收通过。清理环境：docker compose down -v"
