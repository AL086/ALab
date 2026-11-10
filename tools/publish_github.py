#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把当前 git 仓库的内容发布到 GitHub（走 api.github.com，不依赖 git push）。

用法（PowerShell）：
    $env:GITHUB_TOKEN='ghp_xxx'      # fine-grained: Contents 读写；classic: repo
    python tools/publish_github.py
    python tools/publish_github.py --dry-run    # 只打印将要上传的文件，不写远端

为什么不用 git push：这台机器到 github.com:443 的连接时通时断
（Connection was reset / Empty reply），而 api.github.com 一直可用。
脚本用 Git Data API 一次性建 tree + commit，再更新 main 指针，等价于一次普通 push。

- 只上传 `git ls-files` 里的文件，所以 .gitignore 生效（.env、bin/、dist/ 不会外泄）
- 二进制文件自动走 blob 接口，文本文件直接进 tree
"""

import argparse
import base64
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

OWNER = os.environ.get("GH_OWNER", "AL086")
REPO = os.environ.get("GH_REPO", "ALab")
BRANCH = os.environ.get("GH_BRANCH", "main")
TOKEN = os.environ.get("GITHUB_TOKEN", "").strip()
API = "https://api.github.com"

TEXT_EXT = {
    ".go", ".mod", ".sum", ".sql", ".md", ".html", ".css", ".js", ".json",
    ".yml", ".yaml", ".sh", ".py", ".service", ".example", ".gitignore", ".txt", ".toml",
}


def api(method, path, body=None, tries=4):
    url = API + path
    data = json.dumps(body).encode() if body is not None else None
    headers = {
        "Authorization": "Bearer " + TOKEN,
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "blog-server-publisher",
        "Content-Type": "application/json",
    }
    last = None
    for i in range(tries):
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=45) as r:
                return r.status, json.loads(r.read().decode() or "{}")
        except urllib.error.HTTPError as e:
            txt = e.read().decode("utf-8", "replace")
            last = (e.code, txt)
            # 4xx 是参数/权限问题，重试没意义；5xx 与限流才值得再试
            if e.code < 500 and e.code != 429:
                break
        except (urllib.error.URLError, OSError) as e:
            last = (None, str(e))
        time.sleep(2 * (i + 1))
    raise SystemExit("调用 %s %s 失败：%s" % (method, path, last))


def tracked_files():
    out = subprocess.run(["git", "ls-files"], capture_output=True, text=True, check=True).stdout
    return [ln.strip() for ln in out.splitlines() if ln.strip()]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--message", default=None)
    args = ap.parse_args()

    files = tracked_files()
    if not files:
        raise SystemExit("git ls-files 为空，先 git add/commit")
    print("待上传 %d 个文件" % len(files))
    if args.dry_run:
        for f in files:
            print("  ", f)
        return 0

    if not TOKEN:
        raise SystemExit("缺少 GITHUB_TOKEN 环境变量")

    st, me = api("GET", "/user")
    print("token 身份：", me.get("login"), st)

    st, ref = api("GET", "/repos/%s/%s/git/ref/heads/%s" % (OWNER, REPO, BRANCH))
    head = ref["object"]["sha"]
    st, commit = api("GET", "/repos/%s/%s/git/commits/%s" % (OWNER, REPO, head))
    base_tree = commit["tree"]["sha"]
    print("远端 %s 当前提交 %s（%s）" % (BRANCH, head[:7], commit["message"].splitlines()[0][:40]))

    entries = []
    for f in files:
        raw = open(f, "rb").read()  # 项目里的文件都在仓库根之下
        ext = os.path.splitext(f)[1].lower()
        is_text = ext in TEXT_EXT or os.path.basename(f) in {".gitignore"}
        if is_text:
            try:
                text = raw.decode("utf-8")
            except UnicodeDecodeError:
                is_text = False
            else:
                entries.append({"path": f.replace("\\", "/"), "mode": "100644",
                                "type": "blob", "content": text})
        if not is_text:
            st, blob = api("POST", "/repos/%s/%s/git/blobs" % (OWNER, REPO),
                           {"content": base64.b64encode(raw).decode(), "encoding": "base64"})
            entries.append({"path": f.replace("\\", "/"), "mode": "100644",
                            "type": "blob", "sha": blob["sha"]})

    st, tree = api("POST", "/repos/%s/%s/git/trees" % (OWNER, REPO),
                   {"base_tree": base_tree, "tree": entries})
    print("新 tree：", tree["sha"][:7], "共", len(entries), "项")

    message = args.message or "同步本地仓库：%d 个文件" % len(entries)
    st, newc = api("POST", "/repos/%s/%s/git/commits" % (OWNER, REPO),
                   {"message": message, "tree": tree["sha"], "parents": [head]})
    print("新 commit：", newc["sha"][:7])

    st, upd = api("PATCH", "/repos/%s/%s/git/refs/heads/%s" % (OWNER, REPO, BRANCH),
                  {"sha": newc["sha"], "force": False})
    print("已更新 %s -> %s" % (BRANCH, upd["object"]["sha"][:7]))
    print("仓库：https://github.com/%s/%s" % (OWNER, REPO))
    return 0


if __name__ == "__main__":
    sys.exit(main())
