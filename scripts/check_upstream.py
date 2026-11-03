#!/usr/bin/env python3
"""检查上游变更：新提交 + 模型清单变化。

- `git fetch origin`，对比上次记录的 origin/main；
- 通过本地 Bridge 拉取 /v1/models，对比价格/上下文/输出/档位变化；
- 状态保存在数据目录 upstream_check.json，首次运行只建立基线。

用法：
    ./scripts/check_upstream.py
    ./scripts/check_upstream.py --fail-on-change   # 有变化时退出码 2（适合告警）

定时任务示例（每天 09:17）：
    17 9 * * * cd /path/qoder2api && ./scripts/check_upstream.py >> ~/.qoder2api-cn/upstream-check.log 2>&1

环境变量：
    QODER2API_HOME  数据目录（默认 ~/.qoder2api-cn）
    Q2A_BRIDGE      Bridge 地址（默认 http://127.0.0.1:8963）
    Q2A_API_KEY     Bridge API Key（默认读取 settings.json / qccg）
"""
import argparse
import json
import os
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DATA_DIR = Path(os.environ.get("QODER2API_HOME", os.path.expanduser("~/.qoder2api-cn")))
STATE_PATH = DATA_DIR / "upstream_check.json"
BRIDGE = os.environ.get("Q2A_BRIDGE", "http://127.0.0.1:8963").rstrip("/")
MODEL_FIELDS = (
    "price_factor",
    "context_window",
    "max_output_tokens",
    "max_input_tokens",
    "is_reasoning",
    "context_tiers",
)
PROTOCOL_PATHS = (
    "internal/cosy/",
    "internal/bridge/",
    "account/region.go",
    "account/oauth.go",
    "baseprompt.json",
)


def api_key() -> str:
    if os.environ.get("Q2A_API_KEY"):
        return os.environ["Q2A_API_KEY"]
    try:
        settings = json.loads((DATA_DIR / "settings.json").read_text())
        return settings.get("bridge_token") or "qccg"
    except Exception:
        return "qccg"


def load_state() -> dict:
    try:
        return json.loads(STATE_PATH.read_text())
    except Exception:
        return {}


def save_state(state: dict) -> None:
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    tmp = STATE_PATH.with_suffix(".tmp")
    tmp.write_text(json.dumps(state, ensure_ascii=False, indent=2))
    os.chmod(tmp, 0o600)
    tmp.replace(STATE_PATH)


def git(*args: str) -> str:
    return subprocess.check_output(["git", "-C", str(ROOT), *args], text=True).strip()


def check_commits(last_sha: str) -> tuple[str, bool]:
    try:
        subprocess.check_output(["git", "-C", str(ROOT), "fetch", "origin", "--quiet"], text=True)
    except Exception as exc:
        print(f"[上游] git fetch 失败：{exc}")
        return last_sha, False
    head = git("rev-parse", "origin/main")
    if not last_sha:
        print(f"[上游] 首次检查，建立基线：origin/main = {head[:7]}")
        return head, False
    if head == last_sha:
        print(f"[上游] 无新提交（origin/main = {head[:7]}）")
        return head, False
    print(f"[上游] 发现新提交 {last_sha[:7]}..{head[:7]}")
    for line in git("log", "--oneline", f"{last_sha}..{head}").splitlines():
        print("  ", line)
    files = git("diff", "--name-only", f"{last_sha}..{head}").splitlines()
    hot = [f for f in files if any(f.startswith(p) or f == p for p in PROTOCOL_PATHS)]
    if hot:
        print("[上游] 涉及协议/端点的文件（可能需要合并或适配）：")
        for f in hot:
            print("  ", f)
    return head, True


def fetch_models() -> dict:
    req = urllib.request.Request(BRIDGE + "/v1/models", headers={"x-api-key": api_key()})
    with urllib.request.urlopen(req, timeout=20) as resp:
        data = json.loads(resp.read().decode())
    models = data if isinstance(data, list) else data.get("data", [])
    out = {}
    for m in models:
        key = m.get("id") or m.get("key")
        if not key:
            continue
        out[key] = {f: m.get(f) for f in MODEL_FIELDS}
    return out


def diff_models(old: dict, new: dict) -> bool:
    if not old:
        print(f"[模型] 首次建立基线（{len(new)} 个模型）")
        return False
    changed = False
    added = sorted(set(new) - set(old))
    removed = sorted(set(old) - set(new))
    if added:
        changed = True
        print("[模型] 新增：" + ", ".join(added))
    if removed:
        changed = True
        print("[模型] 下线：" + ", ".join(removed))
    for key in sorted(set(old) & set(new)):
        diffs = []
        for f in MODEL_FIELDS:
            a, b = old[key].get(f), new[key].get(f)
            if a != b:
                diffs.append(f"{f}: {a} -> {b}")
        if diffs:
            changed = True
            print(f"[模型] {key} 变化：" + "; ".join(diffs))
    if not changed:
        print(f"[模型] 无变化（{len(new)} 个模型）")
    return changed


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--fail-on-change", action="store_true", help="有变化时退出码 2")
    args = parser.parse_args()

    state = load_state()
    print(f"== 检查时间 {time.strftime('%Y-%m-%d %H:%M:%S')} ==")

    last_sha = state.get("upstream_main", "")
    head, commit_changed = check_commits(last_sha)

    models_changed = False
    try:
        new_models = fetch_models()
        models_changed = diff_models(state.get("models", {}), new_models)
    except Exception as exc:
        print(f"[模型] 拉取 /v1/models 失败（Bridge 未启动？）：{exc}")
        new_models = state.get("models", {})

    save_state({
        "checked_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "upstream_main": head or last_sha,
        "models": new_models,
    })

    changed = commit_changed or models_changed
    print("[结果] " + ("检测到变化，请评估是否需要合并上游或适配" if changed else "无变化"))
    return 2 if changed and args.fail_on_change else 0


if __name__ == "__main__":
    sys.exit(main())
