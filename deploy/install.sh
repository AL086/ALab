#!/usr/bin/env bash
# 知识库（Go + Gin + PostgreSQL + MinIO + Meilisearch）一键安装脚本
#
# 用法：
#   sudo ./install.sh              # 装依赖、建目录、写 systemd、启动，最后给一个配置向导地址
#   sudo SKIP_DEPS=1 ./install.sh  # 依赖已经自己装好了，只部署应用
#   ./install.sh --check           # 只体检、不写任何东西（可以非 root 跑）
#   sudo ./install.sh --app-only   # 只升级二进制并重启（配置/口令/依赖都不动）
#   sudo ./install.sh --uninstall  # 停服务、删单元，数据目录保留
#
# 设计口径：
#   - 依赖装成系统服务（postgresql / minio / meilisearch），应用只依赖它们，不内置；
#   - 账号口令由脚本随机生成，写进 /opt/kb/app/install.env（600），
#     配置向导读它预填，人不用手抄；
#   - 真正的"能不能用"由向导里的「测试连接」把关，脚本不假装配置成功。

set -euo pipefail

# 用法：（--check 只体检不写东西；--app-only 只换二进制；--uninstall 卸服务留数据）
CHECK_ONLY=0
APP_ONLY="${APP_ONLY:-0}"
UNINSTALL=0
for arg in "$@"; do
  case "$arg" in
    --check) CHECK_ONLY=1 ;;
    --app-only|--upgrade) APP_ONLY=1 ;;
    --uninstall) UNINSTALL=1 ;;
    --help|-h)
      sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) echo "未知参数：$arg（支持 --check / --app-only / --uninstall / --help）" >&2; exit 2 ;;
  esac
done

BASE_DIR="${BASE_DIR:-/opt/kb}"
APP_PORT="${APP_PORT:-8080}"
PG_PORT="${PG_PORT:-5432}"
MINIO_PORT="${MINIO_PORT:-9000}"
MINIO_CONSOLE_PORT="${MINIO_CONSOLE_PORT:-9001}"
MEILI_PORT="${MEILI_PORT:-7700}"
SKIP_DEPS="${SKIP_DEPS:-0}"

# MinIO 2025 起换了发布路径，旧地址已经 410；两个都试，别只留一个
MINIO_URL_NEW="https://dl.min.io/aistor/minio/release/linux-amd64/minio"
MINIO_URL_OLD="https://dl.min.io/server/minio/release/linux-amd64/minio"
MEILI_URL="https://github.com/meilisearch/meilisearch/releases/download/v1.4.0/meilisearch-linux-amd64"

APP_DIR="$BASE_DIR/app"
BIN_DIR="$BASE_DIR/bin"
MINIO_DIR="$BASE_DIR/minio"
MEILI_DIR="$BASE_DIR/meili"
LOG_DIR="$BASE_DIR/logs"

HINTS_FILE="$APP_DIR/install.env"
ENV_FILE="$APP_DIR/.env"

say() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33m[注意] %s\033[0m\n' "$*"; }
die() { printf '\033[1;31m[失败] %s\033[0m\n' "$*" >&2; exit 1; }

# ---------- 0. 基础环境 ----------
say "检查基础环境"
OS_ID=""; OS_LIKE=""
[ -r /etc/os-release ] && . /etc/os-release && OS_ID="${ID:-}" && OS_LIKE="${ID_LIKE:-}"
PKG=""
if command -v dnf >/dev/null; then PKG="dnf"
elif command -v apt-get >/dev/null; then PKG="apt"
elif command -v yum >/dev/null; then PKG="yum"
fi
echo "发行版: ${OS_ID:-unknown} (${OS_LIKE:-})   包管理: ${PKG:-未识别}"
[ -n "$PKG" ] || warn "没识别到包管理器，依赖需要你自己装（可 SKIP_DEPS=1 跳过）"

# --check：只体检，不建目录、不装包、不动 systemd
if [ "$CHECK_ONLY" = "1" ]; then
  say "预检（不写任何东西）"
  bad=0
  chk() {  # chk 判定 提示
    if [ "$1" = "1" ]; then printf '  [OK]   %s\n' "$2"; else printf '  [FAIL] %s\n' "$2"; bad=$((bad+1)); fi
  }
  opt() { printf '  [WARN] %s\n' "$1"; }

  [ "$(id -u)" -eq 0 ] && chk 1 "当前是 root" || opt "不是 root：正式安装要用 sudo 跑"
  sysd=0
  [ -d /run/systemd/system ] && sysd=1
  chk "$sysd" "systemd 在跑（/run/systemd/system 存在）"
  command -v systemctl >/dev/null && chk 1 "有 systemctl" || chk 0 "没有 systemctl，装不了系统服务"
  command -v curl >/dev/null && chk 1 "有 curl（下载 MinIO/Meilisearch 用）" || opt "没有 curl：需要自己把二进制放进 vendor/"
  command -v psql >/dev/null && opt "本机已有 psql" || opt "本机还没有 PostgreSQL 客户端（安装阶段会装）"

  # 端口占用：占了就提前说，免得装完启动失败
  for p in "$APP_PORT" "$PG_PORT" "$MINIO_PORT" "$MEILI_PORT"; do
    if command -v ss >/dev/null && ss -lnt 2>/dev/null | awk '{print $4}' | grep -q ":${p}$"; then
      opt "端口 $p 已被占用（如果是已有实例，改 APP_PORT 等变量）"
    fi
  done

  [ -f /etc/systemd/system/blog-kb.service ] && opt "已有 blog-kb.service：正式安装会拒绝覆盖（要加 FORCE=1）" || true
  [ -f ./vendor/minio ] && opt "包里带了 vendor/minio（离线可装）" || opt "没有 vendor/minio：安装时会尝试下载（新版地址优先）"
  [ -f ./vendor/meilisearch ] && opt "包里带了 vendor/meilisearch" || opt "没有 vendor/meilisearch：安装时会尝试从 GitHub Release 下载"
  hasbin=0
  for cand in ./blog-server ./blog-server-linux-amd64 ./dist/blog-server-linux-amd64; do
    [ -f "$cand" ] && hasbin=1 && break
  done
  chk "$hasbin" "找到应用二进制（$APP_DIR/blog-server）"

  avail="$(df -Pm / 2>/dev/null | awk 'NR==2{print $4}')"
  if [ -n "${avail:-}" ] && [ "$avail" -lt 500 ]; then
    opt "根分区剩余 ${avail}MB，建议 ≥ 500MB（数据目录 $BASE_DIR 可以另外挂盘）"
  else
    printf '  [OK]   磁盘剩余 %sMB\n' "${avail:-未知}"
  fi

  echo
  if [ "$bad" -eq 0 ]; then
    echo "预检通过，可以 sudo ./install.sh 了"
    exit 0
  fi
  echo "有 $bad 项硬性问题，先解决再装"
  exit 1
fi

[ "$(id -u)" -eq 0 ] || die "要用 root 跑：sudo $0（只想体检可以 ./install.sh --check）"
command -v systemctl >/dev/null || die "没有 systemd，这个脚本不支持该环境"

# ---------- 卸载（保留数据） ----------
if [ "$UNINSTALL" = "1" ]; then
  say "卸载服务（数据目录保留）"
  echo "将停止并删除：blog-kb.service、minio.service、meilisearch.service"
  echo "保留：$BASE_DIR（数据）、/var/lib/pgsql（PostgreSQL 数据）、/etc/default/minio"
  if [ -t 0 ] && [ "${FORCE:-0}" != "1" ]; then
    printf '确认卸载服务？输入 yes 继续：'
    read -r ans
    [ "$ans" = "yes" ] || die "已取消（要跳过确认用 FORCE=1）"
  fi
  for u in blog-kb minio meilisearch; do
    systemctl stop "$u" >/dev/null 2>&1 || true
    systemctl disable "$u" >/dev/null 2>&1 || true
    rm -f "/etc/systemd/system/$u.service"
  done
  systemctl daemon-reload
  echo "服务已删除。要连数据一起删：rm -rf $BASE_DIR /var/lib/pgsql/data（谨慎）"
  echo "要连 PostgreSQL 包一起卸：dnf remove postgresql-server 或 apt remove postgresql"
  exit 0
fi

random_hex() { tr -dc 'a-f0-9' </dev/urandom | head -c "${1:-32}"; }
random_pass() { tr -dc 'A-Za-z0-9' </dev/urandom | head -c "${1:-14}"; }

# 端口被占就往后找，别让安装卡在一个看得懂但没人管的报错上
free_port() {
  local p="${1:-8080}" i=0
  while [ "$i" -lt 20 ]; do
    if ! (command -v ss >/dev/null && ss -lnt 2>/dev/null | awk '{print $4}' | grep -q ":${p}$"); then
      echo "$p"; return 0
    fi
    p=$((p + 1)); i=$((i + 1))
  done
  echo "$1"
}
if [ "$APP_ONLY" != "1" ]; then
  NEWPORT="$(free_port "$APP_PORT")"
  if [ "$NEWPORT" != "$APP_PORT" ]; then
    warn "端口 $APP_PORT 被占用，改用 $NEWPORT（要固定就用 APP_PORT=xxxx 显式指定）"
    APP_PORT="$NEWPORT"
  fi
fi

# 小内存机器：Meilisearch + PostgreSQL + MinIO 挤在 1GB 会 OOM，先补 swap
if [ "$APP_ONLY" != "1" ] && [ "$SKIP_DEPS" != "1" ]; then
  MEM_MB="$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo 2>/dev/null || echo 0)"
  SWAP_MB="$(awk '/SwapTotal/{print int($2/1024)}' /proc/meminfo 2>/dev/null || echo 0)"
  if [ "$MEM_MB" -gt 0 ] && [ "$MEM_MB" -lt 1500 ] && [ "$SWAP_MB" -lt 500 ]; then
    say "内存只有 ${MEM_MB}MB，加 1GB swap 防止 OOM"
    if [ ! -f /swapfile ]; then
      dd if=/dev/zero of=/swapfile bs=1M count=1024 status=none 2>/dev/null || dd if=/dev/zero of=/swapfile bs=1M count=1024 2>/dev/null
      chmod 600 /swapfile
      mkswap /swapfile >/dev/null && swapon /swapfile
      grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
      echo "swap 已启用 1GB，并写进 /etc/fstab"
    fi
  fi
fi

mkdir -p "$APP_DIR" "$BIN_DIR" "$MINIO_DIR/data" "$MEILI_DIR/data" "$MEILI_DIR/dumps" "$LOG_DIR"
chmod 755 "$BASE_DIR"

# 下载并确认拿到的是 ELF 而不是 410 页/HTML 错误页
fetch_elf() {  # fetch_elf 目标文件 URL...
  local out="$1"; shift
  local url tmp
  tmp="$out.part"
  for url in "$@"; do
    rm -f "$tmp"
    if curl -fsSL --connect-timeout 10 --retry 2 -o "$tmp" "$url" 2>/dev/null; then
      if [ "$(head -c 4 "$tmp" | od -An -c | tr -d ' ')" = "177ELF" ]; then
        mv -f "$tmp" "$out"; chmod 755 "$out"
        echo "已下载: $url"
        return 0
      fi
      warn "下载到的不是 ELF（可能是 410/错误页）：$url"
    else
      warn "下载失败：$url"
    fi
  done
  rm -f "$tmp"
  return 1
}

# 读已有 .env：重装/升级时沿用原口令，别把线上服务改成连不上库
env_get() {
  [ -f "$ENV_FILE" ] || return 0
  grep -m1 -E "^$1=" "$ENV_FILE" 2>/dev/null | cut -d= -f2-
}
OLD_DSN="$(env_get DATABASE_URL)"
OLD_S3_KEY="$(env_get S3_ACCESS_KEY)"
OLD_S3_SECRET="$(env_get S3_SECRET_KEY)"
OLD_MEILI_KEY="$(env_get MEILI_API_KEY)"
OLD_ADMIN_PASS="$(env_get ADMIN_PASSWORD)"
OLD_PG_USER=""; OLD_PG_PASS=""
if [ -n "$OLD_DSN" ]; then
  creds="${OLD_DSN#*://}"; creds="${creds%%@*}"
  OLD_PG_USER="${creds%%:*}"
  [ "$creds" != "${creds#*:}" ] && OLD_PG_PASS="${creds#*:}"
fi

# ---------- 1. PostgreSQL ----------
if [ "$APP_ONLY" != "1" ]; then
if [ "$SKIP_DEPS" != "1" ]; then
  say "安装 PostgreSQL"
  case "$PKG" in
    dnf|yum)
      # openEuler / Rocky / CentOS：postgresql-server 提供 initdb 脚本
      "$PKG" install -y postgresql-server postgresql-contrib >/dev/null
      if [ ! -s /var/lib/pgsql/data/PG_VERSION ]; then
        if command -v postgresql-setup >/dev/null; then
          postgresql-setup --initdb >/dev/null
        else
          die "找不到 postgresql-setup，请手工 initdb 之后再跑本脚本"
        fi
      fi
      PG_SVC="postgresql"
      ;;
    apt)
      apt-get update -y >/dev/null
      apt-get install -y postgresql postgresql-contrib >/dev/null
      PG_SVC="postgresql"
      ;;
    *)
      warn "未知包管理器，跳过 PostgreSQL 安装"
      PG_SVC="postgresql"
      ;;
  esac
else
  say "跳过依赖安装（SKIP_DEPS=1）"
  PG_SVC="postgresql"
fi

systemctl enable --now "$PG_SVC" >/dev/null 2>&1 || warn "PostgreSQL 服务没起来，检查 systemctl status $PG_SVC"

# 允许本机用口令登录（默认 pg_hba 常是 peer/ident，应用连不上）
PG_HBA="$(su postgres -c 'psql -tAc "SHOW hba_file"' 2>/dev/null || true)"
if [ -n "$PG_HBA" ] && [ -f "$PG_HBA" ]; then
  if ! grep -q "127.0.0.1/32.*scram-sha-256" "$PG_HBA"; then
    cp "$PG_HBA" "$PG_HBA.bak.$(date +%s)"
    echo "host all all 127.0.0.1/32 scram-sha-256" >> "$PG_HBA"
    systemctl reload "$PG_SVC" >/dev/null 2>&1 || true
  fi
fi

PG_USER="${OLD_PG_USER:-kbuser}"
PG_DB="kb"
PG_PASS="${OLD_PG_PASS:-$(random_pass 14)}"
# 幂等：用户/库已存在就只改口令，保证 install.env 里的凭据永远是对的
su postgres -c "psql -v ON_ERROR_STOP=1 -tAc \"SELECT 1 FROM pg_roles WHERE rolname='$PG_USER'\"" >/dev/null 2>&1 || true
if su postgres -c "psql -tAc \"SELECT 1 FROM pg_roles WHERE rolname='$PG_USER'\"" | grep -q 1; then
  su postgres -c "psql -v ON_ERROR_STOP=1 -c \"ALTER USER $PG_USER WITH LOGIN PASSWORD '$PG_PASS'\"" >/dev/null
else
  su postgres -c "psql -v ON_ERROR_STOP=1 -c \"CREATE USER $PG_USER WITH LOGIN PASSWORD '$PG_PASS'\"" >/dev/null
fi
if ! su postgres -c "psql -tAc \"SELECT 1 FROM pg_database WHERE datname='$PG_DB'\"" | grep -q 1; then
  su postgres -c "psql -v ON_ERROR_STOP=1 -c \"CREATE DATABASE $PG_DB OWNER $PG_USER\"" >/dev/null
fi
PGPASSWORD="$PG_PASS" psql -U "$PG_USER" -d "$PG_DB" -h 127.0.0.1 -p "$PG_PORT" -tAc "SELECT 1" >/dev/null \
  || warn "本机口令登录没验通，可能是 pg_hba 还没生效（重启 $PG_SVC 再试）"
echo "PostgreSQL 就绪：$PG_USER / $PG_DB  @127.0.0.1:$PG_PORT"

# ---------- 2. MinIO ----------
say "准备 MinIO"
if [ ! -x "$BIN_DIR/minio" ]; then
  if [ -f "./vendor/minio" ]; then
    cp ./vendor/minio "$BIN_DIR/minio"
    echo "用离线包里的 minio"
  else
    fetch_elf "$BIN_DIR/minio" "$MINIO_URL_NEW" "$MINIO_URL_OLD" \
      || die "下载 minio 失败（官方旧地址已 410）。离线环境请把 minio 放到 ./vendor/minio 再跑"
  fi
fi
chmod 755 "$BIN_DIR/minio"

MINIO_USER="${OLD_S3_KEY:-kbminio}"
MINIO_PASS="${OLD_S3_SECRET:-$(random_pass 16)}"
cat > /etc/default/minio <<EOF
MINIO_ROOT_USER=$MINIO_USER
MINIO_ROOT_PASSWORD=$MINIO_PASS
MINIO_VOLUMES=$MINIO_DIR/data
MINIO_OPTS=--address 0.0.0.0:$MINIO_PORT --console-address 127.0.0.1:$MINIO_CONSOLE_PORT
EOF
chmod 600 /etc/default/minio

id -u minio-user >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin minio-user >/dev/null 2>&1 || true
chown -R minio-user:minio-user "$MINIO_DIR" 2>/dev/null || true

cat > /etc/systemd/system/minio.service <<EOF
[Unit]
Description=MinIO object storage
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/default/minio
ExecStart=$BIN_DIR/minio server \$MINIO_VOLUMES \$MINIO_OPTS
Restart=on-failure
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now minio >/dev/null 2>&1 || warn "MinIO 没起来，检查 journalctl -u minio"
echo "MinIO 就绪：$MINIO_USER / $MINIO_PASS  @127.0.0.1:$MINIO_PORT（控制台 $MINIO_CONSOLE_PORT，只绑本机）"

# ---------- 3. Meilisearch ----------
say "准备 Meilisearch"
if [ ! -x "$BIN_DIR/meilisearch" ]; then
  if [ -f "./vendor/meilisearch" ]; then
    cp ./vendor/meilisearch "$BIN_DIR/meilisearch"
    echo "用离线包里的 meilisearch"
  else
    fetch_elf "$BIN_DIR/meilisearch" "$MEILI_URL" \
      || die "下载 meilisearch 失败。离线环境请把它放到 ./vendor/meilisearch（官方 1.4.0 linux-amd64 静态包）"
  fi
fi
chmod 755 "$BIN_DIR/meilisearch"
MEILI_KEY="${OLD_MEILI_KEY:-$(random_pass 24)}"

id -u kbmeili >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin kbmeili >/dev/null 2>&1 || true
chown -R kbmeili:kbmeili "$MEILI_DIR" 2>/dev/null || true

cat > /etc/systemd/system/meilisearch.service <<EOF
[Unit]
Description=Meilisearch full-text search
After=network-online.target
Wants=network-online.target

[Service]
User=kbmeili
Group=kbmeili
ExecStart=$BIN_DIR/meilisearch --env production --master-key $MEILI_KEY \
  --http-addr 127.0.0.1:$MEILI_PORT --db-path $MEILI_DIR/data --dump-dir $MEILI_DIR/dumps
Restart=on-failure
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now meilisearch >/dev/null 2>&1 || warn "Meilisearch 没起来，检查 journalctl -u meilisearch"
echo "Meilisearch 就绪：@127.0.0.1:$MEILI_PORT"
fi  # 依赖阶段（APP_ONLY / SKIP_DEPS 时整段跳过）

# ---------- 4. 应用本体 ----------
say "部署应用"

# 已经有一套在跑的时候不要静默覆盖：单元名固定为 blog-kb，覆盖会让线上服务随时重启
if [ -f /etc/systemd/system/blog-kb.service ] && [ "${FORCE:-0}" != "1" ]; then
  die "这台机器上已有 blog-kb.service（可能是正式环境）。确认要重装请加 FORCE=1 再跑"
fi
BIN_SRC=""
for cand in ./blog-server ./blog-server-linux-amd64 ./dist/blog-server-linux-amd64; do
  [ -f "$cand" ] && BIN_SRC="$cand" && break
done
[ -n "$BIN_SRC" ] || die "当前目录找不到 blog-server 二进制（包里的 blog-server 要和本脚本放一起）"
install -m 755 "$BIN_SRC" "$APP_DIR/blog-server"
[ -f "$APP_DIR/server" ] && rm -f "$APP_DIR/server"

ADMIN_PASS="${OLD_ADMIN_PASS:-$(random_pass 12)}"
# 已有 .env 就一个字都不改：重跑只为修环境/换二进制，配置以线上为准
if [ -f "$ENV_FILE" ]; then
  echo "已有 $ENV_FILE，配置与口令保持不变（要重配置用 $APP_DIR/blog-server -setup）"
else
cat > "$HINTS_FILE" <<EOF
# 安装脚本生成的预填值：配置向导会读它，人不用手抄
# 这个文件只在本机，权限 600
PORT=$APP_PORT
ADMIN_USER=admin
ADMIN_PASSWORD=$ADMIN_PASS

PGHOST=127.0.0.1
PGPORT=$PG_PORT
PGDATABASE=$PG_DB
PGUSER=$PG_USER
PGPASSWORD=$PG_PASS

S3_ENDPOINT=http://127.0.0.1:$MINIO_PORT
S3_ACCESS_KEY=$MINIO_USER
S3_SECRET_KEY=$MINIO_PASS
S3_BUCKET=kb-files

MEILI_HOST=http://127.0.0.1:$MEILI_PORT
MEILI_API_KEY=$MEILI_KEY
MEILI_INDEX=kb_search

_DIRS=PostgreSQL 数据 $(su postgres -c 'psql -tAc "SHOW data_directory"' 2>/dev/null || echo /var/lib/pgsql/data)；MinIO 数据 $MINIO_DIR/data；Meilisearch 数据 $MEILI_DIR/data；应用 $APP_DIR
EOF
chmod 600 "$HINTS_FILE"
fi

cat > /etc/systemd/system/blog-kb.service <<EOF
[Unit]
Description=blog-server (Go + Gin, PostgreSQL + MinIO + Meilisearch)
After=network-online.target postgresql.service minio.service meilisearch.service
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$APP_DIR
EnvironmentFile=-$APP_DIR/.env
ExecStart=$APP_DIR/blog-server -env $APP_DIR/.env -install-hints $HINTS_FILE
Restart=always
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable blog-kb >/dev/null 2>&1 || true
systemctl restart blog-kb || warn "服务启动有问题，看 journalctl -u blog-kb"

# ---------- 5. 防火墙 ----------
if command -v firewall-cmd >/dev/null && systemctl is-active --quiet firewalld; then
  firewall-cmd --permanent --add-port="${APP_PORT}/tcp" >/dev/null 2>&1 || true
  firewall-cmd --reload >/dev/null 2>&1 || true
  echo "已放行 $APP_PORT/tcp"
fi

# ---------- 6. 自检：到底能不能打开 ----------
say "自检"
READY=""
for i in $(seq 1 30); do
  CODE="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$APP_PORT/readyz" 2>/dev/null || echo 000)"
  case "$CODE" in
    200) READY="ok"; break ;;
    503) READY="needsetup"; break ;;
    *) sleep 1 ;;
  esac
done
case "$READY" in
  ok) echo "应用已就绪：/readyz 返回 200，三项依赖都连上了" ;;
  needsetup) echo "应用在跑，但配置还没完成（/readyz 503）：去浏览器把向导走完就能用" ;;
  *) warn "30 秒内没起来，看日志：journalctl -u blog-kb -n 50 --no-pager" ;;
esac
for u in blog-kb postgresql minio meilisearch; do
  if systemctl list-unit-files 2>/dev/null | grep -q "^$u.service"; then
    printf '  %-12s %s\n' "$u" "$(systemctl is-active "$u" 2>/dev/null || echo unknown)"
  fi
done

# ---------- 7. 下一步 ----------
IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -n "$IP" ] || IP="127.0.0.1"
if [ -f "$ENV_FILE" ]; then
  say "安装完成（沿用原有配置，未重新生成口令）"
else
  say "安装完成，最后一步在浏览器里做"
fi
cat <<EOF

  打开： http://$IP:$APP_PORT/
$(if [ ! -f "$ENV_FILE" ]; then cat <<INNER
  这是配置向导（第一次没有 .env 时会自动出现）：
    1. 四组信息已经按 install.env 预填好了，逐项点「测试连接」；
    2. 全绿之后点「保存并启动」，它会写 $ENV_FILE、建表、建桶、建索引、建管理员；
    3. 页面自动跳转前台，用 admin / $ADMIN_PASS 登录后台。
INNER
else cat <<INNER
  直接就是前台页面，用 admin / $ADMIN_PASS 登录后台（口令见下方 install.env）。
INNER
fi)

  预填值文件：$HINTS_FILE（权限 600，含数据库/对象存储口令，别提交到仓库）
  正式配置：  $ENV_FILE（向导生成，同样 600）
  数据目录：  $BASE_DIR（PostgreSQL 数据在 /var/lib/pgsql）

  常用命令：
    看状态    systemctl status blog-kb
    看日志    journalctl -u blog-kb -f
    重启      systemctl restart blog-kb
    改配置    sudo $APP_DIR/blog-server -env $ENV_FILE -setup -setup-port $APP_PORT
    升级      sudo ./install.sh --app-only        # 只换二进制，配置不动
    重装      sudo FORCE=1 ./install.sh           # 口令沿用 .env 里的，不会打断线上
    卸载      sudo ./install.sh --uninstall       # 删服务，数据保留
    换端口    sudo APP_PORT=8090 FORCE=1 ./install.sh

EOF
