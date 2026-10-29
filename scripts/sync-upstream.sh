#!/usr/bin/env bash
# 同步上游并合并到本地开发分支。
#
#   ./scripts/sync-upstream.sh            # 同步 fork main + rebase + build/test
#   ./scripts/sync-upstream.sh --deploy   # 额外构建并重启本地服务
#
# 约定：
#   origin = 作者仓库（上游）  lowa9 = 自己的 fork
#   main 只镜像上游，不直接开发；开发都在 local/org-resource-package
set -euo pipefail
cd "$(dirname "$0")/.."

UPSTREAM="${UPSTREAM_REMOTE:-origin}"
FORK="${FORK_REMOTE:-lowa9}"
DEV="${DEV_BRANCH:-local/org-resource-package}"
DEPLOY=0
[[ "${1:-}" == "--deploy" ]] && DEPLOY=1

if [[ -n "$(git status --porcelain)" ]]; then
  echo "工作区有未提交改动，请先 commit 或 stash。" >&2
  exit 1
fi

echo "==> 拉取上游 $UPSTREAM"
git fetch "$UPSTREAM"
echo "    upstream/main: $(git log -1 --oneline "$UPSTREAM/main")"

echo "==> 同步 fork main（仅允许快进）"
if ! git push "$FORK" "$UPSTREAM/main:main"; then
  echo "fork main 无法快进（可能被直接改动过），请手动处理后重试。" >&2
  exit 1
fi

echo "==> 将 $DEV rebase 到 $UPSTREAM/main"
git checkout "$DEV"
if ! git rebase "$UPSTREAM/main"; then
  echo
  echo "rebase 有冲突：解决冲突后执行 'git rebase --continue'，" >&2
  echo "然后重新运行：./scripts/sync-upstream.sh" >&2
  exit 1
fi

echo "==> 构建与测试"
go build ./...
go test ./...

echo
echo "==> 完成，当前提交："
git log --oneline -3

echo
echo "fork 上的开发分支尚未更新，可执行："
echo "  git push $FORK $DEV"

if [[ "$DEPLOY" -eq 1 ]]; then
  echo "==> 部署：构建并重启本地服务"
  go build -ldflags="-s -w" -o qoder2api.new .
  OLD_PID="$(pgrep -f "qoder2api --web-port=3588" | head -1 || true)"
  if [[ -n "$OLD_PID" ]]; then
    kill "$OLD_PID"
    sleep 2
  fi
  mv qoder2api.new qoder2api
  chmod +x qoder2api
  nohup ./qoder2api --web-port=3588 --bridge-port=8963 \
    >> "$HOME/.qoder2api-cn/stdout.log" 2>&1 &
  sleep 3
  pgrep -fl "qoder2api --web-port=3588" || {
    echo "服务启动失败，查看 $HOME/.qoder2api-cn/stdout.log" >&2
    exit 1
  }
  echo "==> 服务已重启"
fi
