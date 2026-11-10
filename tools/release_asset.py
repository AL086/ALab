#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 dist/blog-server-linux-amd64.tar.gz 作为 GitHub Release asset 传上去。

仓库里只放源码（dist/ 和 *.tar.gz 都在 .gitignore 里），
"解压即装"的二进制包走 Release，别人点一下就能下，不用装 Go。

用法：
    $env:GITHUB_TOKEN='...'
    python tools/release_asset.py --tag v1.0.0
"""

import argparse
import hashlib
import json
import os
import time
import urllib.error
import urllib.request

OWNER = os.environ.get("GH_OWNER", "AL086")
REPO = os.environ.get("GH_REPO", "ALab")
TOKEN = os.environ.get("GITHUB_TOKEN", "").strip()
API = "https://api.github.com"

BODY = """\
## 解压即装（Linux x86_64）

```bash
tar -xzf blog-server-linux-amd64.tar.gz
cd blog-server-linux-amd64
./install.sh --check      # 先体检：root / systemd / 端口 / 磁盘
sudo ./install.sh         # 装 PostgreSQL + MinIO + Meilisearch + 应用
```

装完脚本会打印一个地址（默认 `http://<本机IP>:8080`）。第一次打开网页会自动进入配置向导：
填数据库 / 对象存储 / 搜索服务的地址、端口、口令，每组点「测试连接」，三项全绿才允许「保存并启动」。
`install.sh` 已把随机口令写进 `/opt/kb/app/install.env`，向导会自动预填。

## 包里有什么

- `blog-server`：Linux amd64 静态二进制（后端 + 内嵌前端，含初始化向导）
- `install.sh`：一键安装脚本（含 `--check` 预检；已有 `blog-kb.service` 时拒绝覆盖，需 `FORCE=1`）
- `README.md`：依赖清单、两种安装方式、向导字段说明、常见问题
- `vendor/`：放离线 `minio` / `meilisearch` 二进制的位置（内网机器用）

## 校验

见 `sha256.txt`（也在本 Release 里）。
"""


def api(method, path, body=None, tries=4, raw=None, ctype="application/json"):
    url = path if path.startswith("http") else API + path
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    headers = {
        "Authorization": "Bearer " + TOKEN,
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "blog-server-publisher",
        "Content-Type": ctype,
    }
    last = None
    for i in range(tries):
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=180) as r:
                return r.status, json.loads(r.read().decode() or "{}")
        except urllib.error.HTTPError as e:
            txt = e.read().decode("utf-8", "replace")
            last = (e.code, txt)
            if e.code < 500 and e.code != 429:
                break
        except (urllib.error.URLError, OSError) as e:
            last = (None, str(e))
        time.sleep(3 * (i + 1))
    raise SystemExit("调用 %s %s 失败：%s" % (method, url, last))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tag", default="v1.0.0")
    ap.add_argument("--file", default=os.path.join("dist", "blog-server-linux-amd64.tar.gz"))
    args = ap.parse_args()

    if not TOKEN:
        raise SystemExit("缺少 GITHUB_TOKEN")
    if not os.path.isfile(args.file):
        raise SystemExit("找不到 %s，先跑 python tools/package_release.py" % args.file)

    raw = open(args.file, "rb").read()
    sha = hashlib.sha256(raw).hexdigest()
    size = len(raw)
    name = os.path.basename(args.file)
    print("包：%s  %.1f MB  sha256=%s" % (name, size / 1048576, sha))

    st, me = api("GET", "/user")
    print("token 身份：", me.get("login"), st)

    # 已存在同名 release 就复用，避免重复建 tag
    rel = None
    try:
        st, rel = api("GET", "/repos/%s/%s/releases/tags/%s" % (OWNER, REPO, args.tag), tries=1)
        print("复用已有 release", args.tag)
    except SystemExit:
        st, rel = api("POST", "/repos/%s/%s/releases" % (OWNER, REPO), {
            "tag_name": args.tag,
            "name": "解压即装包 " + args.tag,
            "body": BODY,
            "target_commitish": "main",
            "draft": False,
            "prerelease": False,
        })
        print("已建 release", args.tag, "→", rel["html_url"])

    # 已传过同名 asset 就先删掉（重跑不加 -1 后缀）
    for a in rel.get("assets", []):
        if a["name"] == name:
            api("DELETE", "/repos/%s/%s/releases/assets/%d" % (OWNER, REPO, a["id"]))
            print("删掉旧 asset", a["id"])

    upload = rel["upload_url"].split("{")[0] + "?name=" + name
    st, asset = api("POST", upload, raw=raw, ctype="application/gzip")
    print("已上传 asset：", asset["name"], asset["size"], asset["browser_download_url"])

    st, txt = api("POST", rel["upload_url"].split("{")[0] + "?name=sha256.txt",
                  raw=(sha + "  " + name + "\n").encode(), ctype="text/plain")
    print("已上传 sha256.txt")
    print("Release：", rel["html_url"])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
