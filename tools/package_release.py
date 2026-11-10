#!/usr/bin/env python3
"""打发布包：交叉编译 linux/amd64 二进制，连同 install.sh、README 一起塞进 tar.gz。

用法（在项目根执行）：
    python tools/package_release.py
产物：
    dist/blog-server-linux-amd64.tar.gz
    解压后同层有 blog-server / install.sh / README.md / vendor/（可选离线二进制）
"""
import hashlib
import os
import shutil
import subprocess
import sys
import tarfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
NAME = "blog-server-linux-amd64"
OUT_DIR = ROOT / "dist"
STAGE = OUT_DIR / "release" / NAME


def run(cmd, env=None):
    print("$", " ".join(cmd))
    subprocess.run(cmd, cwd=ROOT, check=True, env=env, shell=False)


def main():
    go = shutil.which("go") or r"C:\Program Files\Go\bin\go.exe"
    if not Path(go).exists() and not shutil.which("go"):
        print("找不到 go，无法交叉编译", file=sys.stderr)
        return 1

    if STAGE.exists():
        shutil.rmtree(STAGE)
    STAGE.mkdir(parents=True, exist_ok=True)

    env = dict(os.environ,
               GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
    run([go, "build", "-trimpath", "-ldflags", "-s -w",
         "-o", str(STAGE / "blog-server"), "./cmd/server"], env=env)

    # 需要的文件：脚本、文档、配置模板
    shutil.copy2(ROOT / "deploy" / "install.sh", STAGE / "install.sh")
    os.chmod(STAGE / "install.sh", 0o755)
    shutil.copy2(ROOT / "README.md", STAGE / "README.md")
    (STAGE / "deploy").mkdir(exist_ok=True)
    shutil.copy2(ROOT / "deploy" / "env.prod.example", STAGE / "deploy" / "env.prod.example")
    shutil.copy2(ROOT / "deploy" / "install.sh", STAGE / "deploy" / "install.sh")

    # 离线二进制：项目根下 vendor/ 里有什么就带上什么（没有就留个说明）
    vendor_src = ROOT / "vendor"
    vendor_dst = STAGE / "vendor"
    vendor_dst.mkdir(exist_ok=True)
    got = []
    for name in ("minio", "meilisearch"):
        p = vendor_src / name
        if p.is_file():
            shutil.copy2(p, vendor_dst / name)
            os.chmod(vendor_dst / name, 0o755)
            got.append(name)
    (vendor_dst / "README.txt").write_text(
        "把离线二进制放这里，install.sh 会优先使用：\n"
        "  minio        https://dl.min.io/server/minio/release/linux-amd64/minio\n"
        "  meilisearch  https://github.com/meilisearch/meilisearch/releases/download/v1.4.0/meilisearch-linux-amd64\n"
        "（当前包里已有：" + ("、".join(got) if got else "无") + "）\n",
        encoding="utf-8")

    tarball = OUT_DIR / f"{NAME}.tar.gz"
    if tarball.exists():
        tarball.unlink()

    # Windows 上 os.chmod 只改只读位，tar 里的权限得手工写，否则到 Linux 全是 666
    def add(tf, path: Path, arcname: str):
        info = tf.gettarinfo(str(path), arcname)
        if path.is_dir():
            info.mode = 0o755
        elif path.name in ("install.sh", "blog-server", "minio", "meilisearch"):
            info.mode = 0o755
        else:
            info.mode = 0o644
        if path.is_file():
            with open(path, "rb") as fh:
                tf.addfile(info, fh)
        else:
            tf.addfile(info)

    with tarfile.open(tarball, "w:gz") as tf:
        add(tf, STAGE, NAME)
        for p in sorted(STAGE.rglob("*")):
            add(tf, p, str(Path(NAME) / p.relative_to(STAGE)).replace("\\", "/"))

    sha = hashlib.sha256(tarball.read_bytes()).hexdigest()
    size = tarball.stat().st_size
    print(f"\n产物: {tarball}  ({size/1024/1024:.1f} MB)")
    print(f"sha256: {sha}")
    print("\n目标机用法：tar -xzf %s.tar.gz && cd %s && sudo ./install.sh"
          % (NAME, NAME))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
