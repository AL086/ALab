#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 blog-server 发布到 192.168.10.11（交叉编译产物 + .env + systemd 单元）。

在 Windows 开发机上跑，不需要服务器装 Go：

    python deploy/deploy.py

做的事：
  1) 本机 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`
  2) SFTP 上传二进制到 /opt/kb/app/server.new，原子替换
  3) 首次部署时上传 .env 与 systemd 单元（已存在则不覆盖 .env，避免冲掉正式密钥）
  4) systemctl daemon-reload + restart blog-kb，然后等 /readyz 变 ok

前置：pip install paramiko；服务器上 nginx 已把 / 反代到 127.0.0.1:8080。
"""
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

import paramiko

HOST = os.environ.get("DEPLOY_HOST", "192.168.10.11")
USER = os.environ.get("DEPLOY_USER", "root")
# 服务器口令不要写进仓库：先设环境变量，再跑脚本
#   PowerShell: $env:DEPLOY_PASS='...'; python deploy/deploy.py
PASSWORD = os.environ.get("DEPLOY_PASS", "").strip()
APP_DIR = "/opt/kb/app"
UNIT_PATH = "/etc/systemd/system/blog-kb.service"
SERVICE = "blog-kb"

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BINARY = os.path.join(ROOT, "dist", "blog-server-linux-amd64")
ENV_SRC = os.path.join(ROOT, "deploy", "env.prod.example")
UNIT_SRC = os.path.join(ROOT, "deploy", "blog-kb.service")


def build():
    env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
    cmd = ["go", "build", "-trimpath", "-ldflags", "-s -w", "-o", BINARY, "./cmd/server"]
    print("[1/4] 交叉编译：", " ".join(cmd))
    subprocess.run(cmd, cwd=ROOT, env=env, check=True)
    print("      产物", BINARY, os.path.getsize(BINARY), "bytes")


def upload(sftp):
    print("[2/4] 上传二进制 -> %s/server.new" % APP_DIR)
    sftp.put(BINARY, APP_DIR + "/server.new")

    remote_env = APP_DIR + "/.env"
    try:
        sftp.stat(remote_env)
        print("      远端已有 .env，保留不覆盖（想换配置请手工改）")
    except IOError:
        print("      首次部署：上传 .env")
        sftp.put(ENV_SRC, remote_env)

    sftp.put(UNIT_SRC, UNIT_PATH)
    print("      上传 systemd 单元 ->", UNIT_PATH)


def switch(client):
    print("[3/4] 替换二进制并重启服务")
    script = """
set -e
chmod 755 %(app)s/server.new
mv -f %(app)s/server.new %(app)s/server
chmod 600 %(app)s/.env
restorecon -R %(app)s 2>/dev/null || true
systemctl daemon-reload
systemctl enable %(svc)s >/dev/null 2>&1 || true
systemctl restart %(svc)s
""" % {"app": APP_DIR, "svc": SERVICE}
    _, out, err = client.exec_command(script, timeout=120)
    out.channel.recv_exit_status()
    e = err.read().decode("utf-8", "replace").strip()
    if e:
        print("      stderr:", e)


def wait_ready(url, seconds=20):
    print("[4/4] 等 /readyz 就绪：", url)
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=3) as r:
                body = r.read().decode("utf-8", "replace")
            print("      ", r.status, body)
            if r.status == 200 and '"postgres":"ok"' in body:
                return True
        except (urllib.error.URLError, OSError) as exc:
            print("       还没起来：", exc)
        time.sleep(1.5)
    return False


def main():
    if not PASSWORD:
        print("缺少服务器口令：请先设置环境变量 DEPLOY_PASS，例如")
        print("  PowerShell:  $env:DEPLOY_PASS='你的root口令'; python deploy/deploy.py")
        return 2
    build()
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(HOST, 22, USER, PASSWORD, timeout=25, banner_timeout=25)
    try:
        with client.open_sftp() as sftp:
            upload(sftp)
        switch(client)
        # 走服务器本机端口检查，避免受 nginx 缓存/配置影响
        _, out, _ = client.exec_command("curl -s -m 8 http://127.0.0.1:8080/readyz", timeout=30)
        print("      readyz:", out.read().decode("utf-8", "replace").strip())
        _, out2, _ = client.exec_command("systemctl is-active %s" % SERVICE, timeout=30)
        print("      systemctl:", out2.read().decode("utf-8", "replace").strip())
    finally:
        client.close()
    ok = wait_ready("http://%s/readyz" % HOST)
    if not ok:
        print("部署完成，但公网入口还没就绪，去看 journalctl -u %s" % SERVICE)
        return 1
    print("部署成功：http://%s/app" % HOST)
    return 0


if __name__ == "__main__":
    sys.exit(main())
