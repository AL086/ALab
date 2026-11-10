# blog-server

个人代码文档仓库管理网页应用（GitHub：`AL086/ALab`）。Go 单体服务，按 `个人博客系统.md` 的架构落地。

## 已安装环境

| 组件 | 版本 | 位置 |
|---|---|---|
| Go | 1.27.3 | `C:\Program Files\Go` |
| Git | 2.56.0.4 | `C:\Program Files\Git` |
| VSCode | 1.145.2 | `%LOCALAPPDATA%\Programs\Microsoft VS Code` |
| gopls / dlv | v0.24.0 / 1.27.2 | `C:\Users\Administrator\go\bin` |

GOPROXY 已指向 `https://goproxy.cn,direct`，拉包走国内镜像。

## 常用命令

```bash
go run ./cmd/server        # 启动，端口取自 .env 的 PORT
go build -o bin/server.exe ./cmd/server
go test ./... -v
go mod tidy                # 加完 import 后执行
```

启动后：

- `GET /healthz` → 只证明进程活着
- `GET /readyz` → 依次探测 PostgreSQL / Meilisearch / MinIO，全通才返回 200，任一失败返回 503 并列出每项状态

启动时会自动执行 `internal/store/migrations/` 下未跑过的迁移（goose），幂等，重复启动不会重复执行。

## 在 VSCode 里开工

```bash
code C:\Users\Administrator\Desktop\blog-server
```

装好后直接按 F5 即可断点调试（`.vscode/launch.json` 已配好，会自动读取 `.env`）。

## 目录

```
cmd/server/main.go                     入口：读配置 → 迁移 → 连库 → 挂路由 → 优雅关闭
internal/config/config.go              所有环境变量集中在这里解析
internal/response/response.go          统一响应 {code,message,data,requestId}
internal/store/db.go                   pgx 连接池
internal/store/migrate.go              goose 迁移执行器（embed SQL）
internal/store/s3.go / s3core.go       MinIO 客户端（高层 Client + 分片用的 Core）
internal/store/key.go                  CAS 存储 key 的唯一拼法（api 与清理任务共用）
internal/store/migrations/*.sql        迁移脚本，按序号命名
internal/api/                          全部 HTTP handler，按业务分文件
                                        （files.go 文件/版本，ftext.go 在线编辑读存，dist.go 打包与只读分发）
internal/auth/auth.go                  argon2id + JWT
internal/search/search.go              Meilisearch REST 客户端 + outbox worker
internal/maintenance/maintenance.go    回收站清理 / 孤儿对象 GC / 僵死上传作废
internal/web/app.html                  后台页面（单文件原生 JS，编进二进制，路由 /app）
deploy/deploy.py                       一条命令发到 192.168.10.11（交叉编译 + 上传 + 重启）
deploy/blog-kb.service                 服务器上的 systemd 单元
deploy/env.prod.example                服务器 .env 模板（中间件都走 127.0.0.1）
.env.example                           本机开发配置模板，复制成 .env
.vscode/                               调试、任务、编辑器设置
```

## 加一张表 / 改表结构

**不要用 AutoMigrate**。DDL 以 SQL 为准，因为下面这些 Postgres 特性 ORM 表达不出来：

- 部分索引：`CREATE INDEX ... WHERE deleted_at IS NULL`
- `INET`、`TEXT[]` 字段
- `UNIQUE NULLS NOT DISTINCT`（NULL 参与唯一比较）

步骤：新建 `internal/store/migrations/002_xxx.sql`，写 `-- +goose Up` / `-- +goose Down` 两段，重启服务即自动执行。

## 已建表

- 001_init：`users`、`categories`、`tags`、`posts`、`post_revisions`
- 002_files：`file_nodes`、`file_versions`、`objects`、`download_logs`
- 003_search_outbox：`search_outbox`（事务性 outbox）
- 004_tags_uploads：`post_tags`（文章↔标签多对多，带 `idx_post_tags_tag` 反查索引）、
  `upload_sessions`（分片上传会话，`parts` 是 JSONB 数组）
- 005_posts_trash：`posts.deleted_at`（文章也进回收站）+ `idx_posts_deleted`
- 006_posts_cover：`posts.cover_url`（文章封面，存可直接进 `<img src>` 的地址；空 = 没封面）

另有 goose 自己的 `goose_db_version`。

文件仓库的引用计数口径（代码与迁移里都写了，别改）：

- `objects.ref_count` == `file_versions` 里该 sha256 的行数
- `file_nodes.storage_key` 只是"当前版本"的冗余指针，不参与计数
- 因此**替换版本、回滚版本都不改 ref_count**（版本行本来就在），只有"彻底删掉某个版本"才 -1
- 唯一约束是部分索引 `idx_files_unique_name ... NULLS NOT DISTINCT WHERE deleted_at IS NULL`：
  既挡住了根目录重名（NULL 参与比较），又允许软删除后复用同名

## 前端（前台 + 后台，两个单文件页面）

站点分成两页，都编进同一个二进制、都和 API 同源，**不需要 CORS、不需要 Node、不需要构建**：

| 路径 | 页面 | 定位 |
| --- | --- | --- |
| `/` | `internal/web/home.html` | **公开前台**：文章列表 / 文章阅读 / 搜索 / 公开文件区，匿名可看 |
| `/app` | `internal/web/app.html` | **后台管理**：登录后写文章、管文件、跑运维 |

前台的口径：

- 文章只显示 `status=published` 且 `visibility=public` 的；点进去按 Markdown 渲染（内联渲染器，不依赖外网 CDN）
- 搜索走 Meilisearch，匿名过滤条件同样是"已发布 + 公开"
- 文件仓库分两种视角：**登录后**看到自己完整的目录树（私有文件也在，下载/预览带登录态取）；
  **匿名**只看到一个平铺的"公开文件区"，每行带完整路径（`文档/notes.txt`）
- 文件默认都是 `private`，想让某个文件出现在前台，在后台文件行点 **设为公开**（走 `PATCH /files/:id {visibility}`）
- 文章封面：`cover_url` 有值就是左图右文的卡片，**没有封面就不给缩略图位**（不留灰框）；
  整张卡片是一个 `<a>`，点标题、摘要、封面或卡片空白都进文章详情。封面地址接受 `/raw/<id>` 或 `http(s)` 外链，
  后台编辑器里有 **选图上传**（上传后自动 `PATCH visibility=public`，否则前台匿名取不到图）+ 预览 + 清除

后台（`/app`）已串起来的功能：登录、文章列表（**按标签点选筛选**）/新建草稿/编辑器（分类+标签多选）/保存/发布/版本回滚/
**归档与退回草稿**/**删除到回收站与恢复**、文件仓库（目录浏览、新建目录、单文件上传、
**>8MB 自动分片带进度且可中途取消**、替换版本、**版本列表与回滚**、**重命名/移动**、**公开/私有切换**、
软删、按 id 恢复、下载、**打包下载本页/整目录**、**批量勾选（每行勾选框 + 表头全选 + Shift 连续区间）后打包下载**、**分享链接与在线预览**、
**在线编辑（md 双栏实时预览 / txt 等纯文本 / PDF 图片只能下载）**、
**md 插图（Ctrl+V 粘贴、拖拽或按钮，自动上传到同级 `assets/`，正文插 `![](/raw/<id>)`）**）、
分类树与标签管理（新建/改名/换父/删除）、全文搜索与重建索引、运维页手工跑清理与依赖自检

改页面：编辑对应的 `.html` 后 `go build` 重新生成 `bin/server.exe` 即可
（embed 在编译期打包，改完不重新编译看不到变化）。

## 认证

- 口令用 **argon2id**（m=64MB, t=1, p=4），散列形如 `$argon2id$v=19$m=65536,t=1,p=4$salt$hash`
- 登录换 **JWT（HS256，24 小时）**；`JWT_SECRET` 少于 16 字符会直接拒绝启动
- 首次启动时若 `users` 表为空，按 `.env` 的 `ADMIN_USER` / `ADMIN_PASSWORD` 建管理员；
  之后改 `.env` **不会**覆盖已有口令，要改得动库里那条记录
- 读写分组的约定：**GET 公开、写操作一律要 token**；`visibility=private` 的文件下载也要 token
- 取 token 两种方式：`Authorization: Bearer <token>`，或 `?token=<token>`（方便浏览器直接下载）

```bash
curl -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"your-ADMIN_PASSWORD"}'
# 返回 data.token，后续请求带 Authorization: Bearer <token>
```

## 搜索同步

用 **outbox** 而不是直连 Meilisearch：发布 / 编辑 / 回滚在**同一个数据库事务**里写一条
`search_outbox`，后台 worker 每 2 秒扫一批（`processed_at IS NULL AND attempts < 5`）推给
Meilisearch，推成功才标 `processed_at`。所以不会出现"库里改了但搜索没同步"。

- 只有 **published 且非 private** 的文章进索引；草稿和私有文章会被显式删掉文档
- 可搜字段：`title` / `summary` / `content`（Markdown 去标记后的纯文本）/ `slug`
- 可过滤：`status` / `visibility` / `categoryId`
- 全量重建：`POST /api/v1/admin/reindex`（要 token），只往 outbox 塞 upsert，不阻塞请求
- **Meilisearch 保持服务器上的 1.4.0，不升级**：服务器 dnf 源不可达，换二进制风险大于收益，
  1.4 已支持中文分词。客户端因此不用官方 SDK，改用原生 REST（`internal/search/search.go`），
  以后换任何版本的 Meilisearch 都不用改这层代码

## 分片上传

大于 8MB 的文件走分片，四个端点都在 `/files/upload` 前缀下：

| 步骤 | 调用 | 说明 |
|---|---|---|
| 1 | `POST /files/upload/init` 🔒 | `{name,size,sha256?,parentId?,mime?}`，返回 `uploadId` / `partSize`(8MB) / `totalParts` |
| 2 | `PUT /files/upload/:uploadId/part/:index` 🔒 | body 是**裸字节**（不是 multipart），序号从 1 开始 |
| 3 | `POST /files/upload/:uploadId/complete` 🔒 | 合并 + 算整体 sha256 + 落节点与版本 |
| 4 | `DELETE /files/upload/:uploadId` 🔒 | 主动放弃，释放远端分片 |

- **秒传**：init 时带上 `sha256`，若 `objects` 里已有该内容，直接返回
  `{complete:true, dedup:true, id}`，一个字节都不用传
- 单片 8MB（S3 要求除最后一片外不小于 5MB），单文件上限 **5GB**、最多 **10000** 片
- complete 时先把临时对象在**服务端复制**到 CAS 路径（`CopyObject`，不经过应用内存），
  再流式读一遍算 sha256 校验内容一致；命中已有对象就跳过复制
- 临时对象放在 `uploads/<sessionId>/blob`，合流成功或 abort 后立即删掉；忘了 abort 的
  会话由后台清理任务在 24 小时后作废

## 后台清理任务

`internal/maintenance` 一个 Job 四个任务，**启动先跑一轮**，之后每 12 小时一次：

1. **PurgeTrash**：物理删除软删超过 **30 天**的节点**连同整棵子树**（递归 CTE 找过期根
   再向下展开，别担心父目录过期而子目录没满 30 天的情况），删版本时按 sha 分组把
   `objects.ref_count` 减回去（`GREATEST(...,0)`），每批 500 个节点
2. **PurgePostTrash**：物理删除进回收站超过 30 天的文章；`post_revisions` / `post_tags`
   是 `ON DELETE CASCADE`，会跟着一起走（索引在上一次删除时已经摘过）
3. **AbortStaleUploads**：作废超过 24 小时仍是 `pending` 的分片会话并撤销远端 multipart
4. **GCObjects**：先**自愈**（把 `ref_count` 按 `file_versions` 实际行数校正一遍），
   再删"没有版本引用、且创建超过 1 小时"的对象（1 小时缓冲是给"刚上传还没写版本行"的竞态）

手工触发（运维与测试用）：`POST /api/v1/admin/run-maintenance` 🔒，返回各任务的处理数量。

## 已实现接口

统一前缀 `/api/v1`，返回 `{code,message,data,requestId}`。🔒 = 需要 `Authorization: Bearer`。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/auth/login` | 换 token `{username,password}` |
| GET 🔒 | `/auth/me` | 回显当前身份 |
| GET | `/search?q=&page=&pageSize=` | 全文检索（匿名只搜得到公开已发布） |
| POST 🔒 | `/admin/reindex` | 全部文章重新入队，重建索引 |
| POST 🔒 | `/posts` | 建草稿（slug 留空按标题生成，冲突自动加序号） |
| GET | `/posts?status=&tag=&trash=&page=&pageSize=` | 文章列表（匿名自动只返回已发布 + 公开）。`tag=Go,PG` 按标签 slug 或名字筛选，多个取交集；登录后 `trash=1` 列回收站 |
| GET | `/posts/:id` | 文章详情（匿名读草稿/私有统一回 404） |
| PUT 🔒 | `/posts/:id` | 改标题/正文/摘要/**封面**（草稿自动保存走这里，不产生版本）。`coverUrl` 传 `""` 表示清空封面；合法值只有 `/` 开头的站内路径和 `http(s)` 外链 |
| POST 🔒 | `/posts/:id/publish` | 发布，产生一条 revision |
| GET 🔒 | `/posts/:id/revisions` | 版本列表 |
| POST 🔒 | `/posts/:id/rollback` | 回滚（生成新版本，不改历史） |
| PUT 🔒 | `/posts/:id/tags` | 覆盖式设置文章标签 `{tagIds:[1,2]}`（先清后插，同一事务） |
| PUT 🔒 | `/posts/:id/status` | 下线用 `{status:"archived"\|"draft"}`，顺带把索引记录删掉；重新上线必须走 `/publish` |
| DELETE 🔒 | `/posts/:id` | 移到回收站（软删），同时摘掉搜索索引 |
| POST 🔒 | `/posts/:id/restore` | 从回收站恢复；原来是已发布的会重新进索引 |
| POST 🔒 | `/categories` | 建分类 `{name,slug?,parentId?,sort?}` |
| GET | `/categories?tree=1` | 分类列表；`tree=1` 返回嵌套树 |
| PUT 🔒 | `/categories/:id` | 改名/换父（换父前用递归 CTE 查环，不能挂到自己子树下） |
| DELETE 🔒 | `/categories/:id` | 删分类；**有子分类时拒绝**，只能从叶子往上删 |
| POST 🔒 | `/tags` | 建标签 `{name,slug?}` |
| GET | `/tags` | 标签列表（带使用数） |
| PUT 🔒 | `/tags/:id` | 改标签名 |
| DELETE 🔒 | `/tags/:id` | 删标签（`post_tags` 级联清理） |
| POST 🔒 | `/files/upload/init` | 分片上传第 1 步，带 `sha256` 可秒传 |
| PUT 🔒 | `/files/upload/:uploadId/part/:index` | 分片上传第 2 步，裸字节 body |
| POST 🔒 | `/files/upload/:uploadId/complete` | 分片上传第 3 步，合并落库 |
| DELETE 🔒 | `/files/upload/:uploadId` | 放弃上传，释放远端分片 |
| POST 🔒 | `/admin/run-maintenance` | 手工跑一次清理（回收站过期 / 孤儿对象 / 僵死会话） |
| POST 🔒 | `/files/dir` | 建目录 `{parentId,name}` |
| GET 🔒 | `/files?parent=&page=&pageSize=` | 列目录，目录在前 |
| POST 🔒 | `/files/upload` | 单文件上传（multipart：`file`、`parentId`、`name`） |
| GET | `/files/:id/download` | 流式下载，带 `X-Content-Sha256`、`nosniff`；私有文件要 token |
| GET 🔒 | `/files/zip?ids=1,2` \| `?dir=10` | 流式打包成 zip，不落临时文件；`dir` 递归子目录并保留相对路径，一次最多 100 个文件 |
| GET | `/s/:id` | 只读分发短链，语义同 download（公开匿名可下，私有要 token） |
| GET | `/raw/:id` | 在线预览：扩展名白名单（txt/md/json/csv/图片/PDF），`text/plain` + `nosniff`，类型不在名单回 415；**根路径也挂了一份**，这样 Markdown 里写 `![](/raw/<id>)` 能直接取到（`/s/:id`、`/files/:id/download` 同理） |
| PATCH 🔒 | `/files/:id` | 重命名 / 移动 / 改可见性 `{name?,parentId?,visibility?}`，移动前查环 |
| GET | `/pub/files` | 前台匿名用：只返回 `visibility=public` 的文件，平铺带 `path`，不暴露目录树 |
| GET 🔒 | `/files/:id/text` | 读文本内容给在线编辑器（只开放 md/txt/json/yaml/sh… 白名单，其他 415；>512KB 回 413） |
| PUT 🔒 | `/files/:id/text` | 按文本保存成新版本 `{content,note}`，走 CAS + 版本号 + ref_count，可回滚；同时把正文里 `/raw/<id>` 引用的图片可见性同步成与本文档一致 |
| POST 🔒 | `/files/:id/assets` | 给 Markdown 文件传一张插图（multipart `file`、`name?`）：自动在该文件同级建 `assets/` 子目录，图片可见性跟随源文件；只收 png/jpg/jpeg/gif/webp，>8MB 回 413，其他类型 415 |
| GET 🔒 | `/files/:id/versions` | 版本列表（`versionNo` / `sha256` / `size` / `note` / `createdAt`） |
| POST 🔒 | `/files/:id/versions` | 替换内容，新增版本 |
| POST 🔒 | `/files/:id/rollback` | 回滚到某版本 `{versionNo}` |
| DELETE 🔒 | `/files/:id` | 软删（含子树） |
| POST 🔒 | `/files/:id/restore` | 从回收站恢复 |

手工试一下（bash）：

```bash
TOKEN=$(curl -s -X POST http://127.0.0.1:8080/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"your-ADMIN_PASSWORD"}' \
  | grep -o '"token":"[^"]*' | cut -d'"' -f4)

curl -X POST http://127.0.0.1:8080/api/v1/files/dir \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" -d '{"name":"脚本"}'

curl -X POST http://127.0.0.1:8080/api/v1/files/upload \
  -H "Authorization: Bearer $TOKEN" \
  -F "file=@deploy.sh" -F "parentId=1" -F "name=deploy.sh"
```

上传响应里的 `dedup:true` 表示这份内容之前已经传过，只加了引用计数、没重复写对象存储。

## 还没做

- 多用户与权限细分（现在是单 owner，`role` 字段已预留）
- 正式域名 + HTTPS（现在是 `http://192.168.10.11` 走 nginx 反代，没上证书）
- 前端换独立仓库（现在定稿是内嵌单文件，改完 `go build` 即生效）
- 限流用的是进程内固定窗口（单机够用，多实例要换 Redis）
- `/raw/` 的中文分词/语法高亮之类纯展示增强（现在是纯文本直出）

## 装到一台新机器（解压 → 装依赖 → 网页配置 → 能用）

> 这一段是给"别人拿到仓库/发布包、从零开始"用的。装完不是直接进后台，而是先弹配置向导：
> 数据库、对象存储、搜索三样都要当场点「测试连接」通过，才会写配置并启动，
> 避免出现"看起来装好了、其实连不上库"的假成功。

**拿现成的包（推荐，不用装 Go）**：

```bash
wget https://github.com/AL086/ALab/releases/download/v1.0.0/blog-server-linux-amd64.tar.gz
tar -xzf blog-server-linux-amd64.tar.gz && cd blog-server-linux-amd64
./install.sh --check          # 只体检，不写任何东西
sudo ./install.sh             # 装依赖 + 起服务
```

Release 页：<https://github.com/AL086/ALab/releases/tag/v1.0.0>
（同一个 Release 里还有 `sha256.txt` 可以核对）。

### 需要哪些软件

| 软件 | 版本 | 作用 | 是否必须 | 拿不到时 |
|---|---|---|---|---|
| Linux + systemd | 任意主流发行版（openEuler / Rocky / Ubuntu / Debian） | 跑服务 | 必须 | — |
| **blog-server** | 包里自带（Go 静态二进制，`CGO_ENABLED=0`） | 应用本体 | 必须 | 本机 `go build` 交叉编译，见下文 |
| **PostgreSQL** | ≥ 13（本项目在 17.11 上验证） | 文章、文件元数据、标签 | 必须 | 包管理器装：`dnf install postgresql-server` / `apt install postgresql` |
| **MinIO** | 任意近期版本（官方静态包，新地址见下） | 文件原件（CAS 存储） | 必须 | `vendor/minio` 放离线二进制 |
| **Meilisearch** | 1.4.0（本项目用它验证；更高版本也兼容，代码走纯 REST） | 全文搜索 | 必须 | `vendor/meilisearch` 放官方 `meilisearch-linux-amd64` |
| nginx | 任意 | 可选，反代 80 → 应用端口 | 可选 | 直接访问应用端口也能用 |
| Go | ≥ 1.21 | 只有"自己编译"才需要 | 可选 | 用发布包里的二进制就不用装 |

服务器**不需要装 Node**：前端是两个 HTML 文件，`//go:embed` 编进二进制。

下载地址（网络不通时按这一列找离线包）：

- MinIO：`https://dl.min.io/aistor/minio/release/linux-amd64/minio`
  （**注意**：旧地址 `https://dl.min.io/server/...` 现在返回 410 Gone，脚本会先试新地址再试旧地址，
  并且校验下载到的是 ELF 而不是错误页）
- Meilisearch：`https://github.com/meilisearch/meilisearch/releases/download/v1.4.0/meilisearch-linux-amd64`
- PostgreSQL：发行版软件源（`dnf` / `apt`）

装之前可以先体检，不写任何东西：

```bash
./install.sh --check
```

它会报：是不是 root、systemd 在不在跑、有没有 curl、端口有没有被占、已经有没有 blog-kb 单元、
包里带没带离线二进制、磁盘剩多少。硬性问题会返回非 0。

### 方式一：一键脚本（推荐）

发布包里 `install.sh` 和 `blog-server` 在同一层目录：

```bash
tar -xzf blog-server-linux-amd64.tar.gz
cd blog-server-linux-amd64
sudo ./install.sh
```

脚本按顺序做这些事（每步都幂等，可以重复跑）：

1. 识别包管理器，装 PostgreSQL 并 `initdb`、启动；补一条
   `host all all 127.0.0.1/32 scram-sha-256` 到 `pg_hba.conf`（默认的 peer/ident 应用连不上）
2. 随机生成 `kbuser` 口令 → 建角色、建库 `kb`
3. 装 MinIO：二进制放 `/opt/kb/bin/minio`，数据 `/opt/kb/minio/data`，随机 root 口令，
   systemd 单元 `minio.service`（控制台 9001 只绑 `127.0.0.1`）
4. 装 Meilisearch：二进制 `/opt/kb/bin/meilisearch`，数据 `/opt/kb/meili/data`，
   随机 master key，只绑 `127.0.0.1:7700`，单元 `meilisearch.service`
5. 放应用 `/opt/kb/app/blog-server`，把上面这些凭据写进 `/opt/kb/app/install.env`（**600**），
   建单元 `blog-kb.service`（`Restart=always`），启动
6. 放行应用端口（firewalld 在跑才做），最后打印访问地址和预填好的管理员口令

装完打开 `http://<服务器IP>:8080/` 就是配置向导，四组值已经按 `install.env` 预填好，
逐项点「测试连接」→ 全绿后点「保存并启动」。

离线/内网：把 `minio`、`meilisearch` 二进制放到解包目录下的 `vendor/` 里，脚本会优先用它们；
依赖都自己装好了可以 `sudo SKIP_DEPS=1 ./install.sh`。

可覆盖的变量：`BASE_DIR`（默认 `/opt/kb`）、`APP_PORT`（8080）、`PG_PORT`（5432）、
`MINIO_PORT`（9000）、`MEILI_PORT`（7700）。

### 方式二：手动（理解每一步在干什么）

```bash
# 1) PostgreSQL：建角色和库
sudo -u postgres psql -c "CREATE USER kbuser WITH LOGIN PASSWORD '你的口令'"
sudo -u postgres psql -c "CREATE DATABASE kb OWNER kbuser"

# 2) MinIO（前台跑一下验证，正式用 systemd 托管）
MINIO_ROOT_USER=admin MINIO_ROOT_PASSWORD=你的口令 \
  ./minio server /opt/kb/minio/data --address :9000 --console-address 127.0.0.1:9001

# 3) Meilisearch
./meilisearch --env production --master-key 你的Key \
  --http-addr 127.0.0.1:7700 --db-path /opt/kb/meili/data

# 4) 应用：直接起二进制，没有 .env 就会进配置向导
PORT=8080 ./blog-server -env /opt/kb/app/.env
```

### 配置向导填什么

| 分组 | 字段 | 说明 |
|---|---|---|
| 应用 | 端口 / 公开访问地址 / 管理员账号 / 管理员口令 | 公开地址用于生成分享链接和正文插图；口令 ≥ 6 位 |
| PostgreSQL | 主机 / 端口 / 库名 / 用户 / 口令 | 库不存在会自动建（需要该用户有 `CREATE DATABASE`）；云库要 `sslmode=require` 时填下面的完整 DSN |
| 对象存储 | Endpoint / Access Key / Secret Key / 桶名 / 是否 HTTPS | `Endpoint` 要带 `http://`；桶不存在会自动建；桶名只能小写字母数字点短横线 |
| Meilisearch | 地址 / API Key / 索引名 | 索引不存在会自动建，字段配置每次启动对齐 |

每个分组都有独立的「测试连接」，底部还有「全部检测」；**三项都通过之后**「保存并启动」
才会写 `.env` → 跑迁移建表 → 建桶 → 建索引 → 建管理员，然后把监听交还给正式服务，
页面轮询 `/readyz` 成功后自动跳到前台。

失败提示是给人看的，不是驱动原文：端口没人监听、口令不对、主机名解析不了、超时、
Access Key 不对、证书问题都会单独翻译（见 `internal/setup/setup.go` 的 `friendly`）。

### 端口与目录

| 用途 | 默认端口 | 数据目录 |
|---|---|---|
| 应用（含配置向导） | 8080 | `/opt/kb/app`（二进制、`.env`、`install.env`） |
| PostgreSQL | 5432 | `/var/lib/pgsql/data`（apt 下是 `/var/lib/postgresql/<ver>/main`） |
| MinIO S3 | 9000 | `/opt/kb/minio/data` |
| MinIO 控制台 | 9001（只绑 127.0.0.1） | — |
| Meilisearch | 7700（只绑 127.0.0.1） | `/opt/kb/meili/data` |

### 装完怎么验

```bash
systemctl status blog-kb minio meilisearch postgresql
curl -s http://127.0.0.1:8080/readyz          # 三项都 ok 才算真的可用
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/
```

### 常见问题

- **向导里数据库"口令不对"**：`pg_hba.conf` 还是 peer/ident。补
  `host all all 127.0.0.1/32 scram-sha-256` 后 `systemctl reload postgresql`
- **对象存储"连不上"**：MinIO 只监听 9000，先 `ss -lntp | grep 9000`；`Endpoint` 少了 `http://` 也会报错
- **Meilisearch "API Key 不对"**：1.4 之后 master key 必须 ≥ 16 字节；换 key 要同步改向导里的值
- **想改配置**：`sudo /opt/kb/app/blog-server -env /opt/kb/app/.env -setup -setup-port 8080`，
  会再开一次向导（原 `.env` 的值会预填），改完自动回到正式服务
- **SELinux Enforcing**：不要把日志写成 `StandardOutput=append:/opt/kb/logs/x.log`，
  本机实测 systemd 打开该文件会 `Permission denied`，用 journald 就好
- **`.env` 和 `install.env` 都含明文口令**，权限 600，已在 `.gitignore` 里，别提交



服务器是 openEuler 26.09 + SELinux Enforcing，**没装 Go、没装 Node、dnf 源不可达**，
所以部署形态固定为「Windows 交叉编译 → 传二进制 → systemd 托管」：

- 程序：`/opt/kb/app/server`（静态链接的 amd64 ELF，`CGO_ENABLED=0`）
- 配置：`/opt/kb/app/.env`（模板见 `deploy/env.prod.example`，中间件全走 `127.0.0.1`）
- 单元：`blog-kb.service`（模板见 `deploy/blog-kb.service`，`Restart=on-failure`，日志进 journal）
- 入口：nginx 的 `www.conf` 已把 `location /` 反代到 `127.0.0.1:8080`，所以直接访问
  **<http://192.168.10.11/app>** 就是后台，`/api/v1/...` 同源，不需要 CORS

一条命令发布（本机执行，服务器不用装任何东西）：

```bash
pip install paramiko
export DEPLOY_PASS='服务器 root 口令'      # Windows PowerShell: $env:DEPLOY_PASS='...'
python deploy/deploy.py
```

口令一律从环境变量读（`DEPLOY_HOST` / `DEPLOY_USER` 可选），**仓库里不存任何明文口令**。

它做四件事：交叉编译 → SFTP 上传并原子替换二进制 → 首次部署才写 `.env`（已存在就保留，
不会冲掉正式 JWT 密钥）→ `systemctl restart blog-kb` 并轮询 `/readyz`。

常用运维命令：

```bash
systemctl status blog-kb            # 状态
journalctl -u blog-kb -f            # 实时日志（gin 访问日志也在这）
curl http://127.0.0.1:8080/readyz   # postgres / minio / meilisearch 三项自检
```

注意两点（都是踩过的）：

1. **不要把日志写成 `StandardOutput=append:/opt/kb/logs/...`** —— 这台机器的 SELinux
   会让 systemd 打开那个文件时 `Permission denied`（进程 209/STDOUT 直接起不来），用 journal 就好
2. **本地和服务器共用同一套 PostgreSQL / MinIO / Meilisearch**，所以别两边同时长期跑：
   outbox 推送和 12 小时清理任务会互相抢。部署验证完把本机进程停掉，本机要开发时再起
   `bin/server.exe` 即可（`go build -o bin/server.exe ./cmd/server`）。

## 新增依赖

```bash
go get github.com/jackc/pgx/v5          # PostgreSQL（已引入）
go get github.com/pressly/goose/v3      # 迁移（已引入）
go get github.com/joho/godotenv         # 读 .env（已引入）
go get github.com/minio/minio-go/v7     # MinIO（已引入）
go get github.com/golang-jwt/jwt/v5     # JWT（已引入）
go get golang.org/x/crypto              # argon2id（已引入）
```

搜索**没有**引入 `meilisearch-go`，理由见上面"搜索同步"一节。

