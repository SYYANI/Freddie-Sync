# Freddie Sync Server

[Freddie](https://github.com/SYYANI/Freddie)（ReadPaper）Mac / iPad 论文库的自托管同步后端。
服务代码与原生 App 独立，不改 SwiftData schema，不复制或共享正在使用的 `ReadPaper.store`。

**Freddie 不内置任何同步服务器。** 由部署方在自己的域名上部署本服务，为每台设备签发凭证
（凭证文件里带有服务器地址和设备 token），用户在 App 的“设置 → 同步”导入凭证后才能使用；
App 在第一次上传前会显示凭证里的服务器地址并要求确认。

`smoke.py` 会用两台模拟设备验证真实 HTTPS 请求、文件传输、冲突和重启恢复。

下文用以下占位符：

- `SSH_HOST`：能以 root 登录服务器的 SSH 目标或 `~/.ssh/config` 别名，例如 `root@vps`。
- `sync.example.com`：服务器的公网域名。

## 部署结构

```text
Mac / iPad / iPhone
    │ HTTPS + 独立设备 Bearer token
    ▼
sync.example.com/freddie-sync/     现有 nginx，仅增加该路径
    │ HTTP，仅本机回环
    ▼
127.0.0.1:17483                   Go 服务，freddie-sync 专用非 root 账号
    ├── /var/lib/freddie-sync/sync.db       bbolt 事务库
    ├── /var/lib/freddie-sync/blobs/        不可变 SHA-256 文件
    └── /run/freddie-sync/admin.sock        0600，本机管理接口

/var/backups/freddie-sync/         每日校验备份，保留最近 7 份
```

使用 Go 标准 HTTP 库和固定版本 bbolt。工具链固定为含漏洞修复的 Go 1.26.10。
bbolt 默认同步提交，不启用 `NoSync`；每次成功写入前文件与相关目录均 fsync。
数据记录、历史、请求回执在同一个事务中提交。二进制文件先落盘并登记，再允许记录引用。

## 安全与数据保护

- 公网只通过受信 HTTPS 访问，要求 TLS 1.2/1.3；后端仅监听回环地址。
- 每台设备使用随机 256 位 token，服务器只保存 SHA-256 摘要，可单独撤销。
  新增设备和撤销通过 SSH 后的本机 socket 完成，没有公网注册或管理接口。
- 库从认证结果取得，客户端不能用 URL 或请求体切换到其他库。
- 拒绝带 Origin 的浏览器请求，不提供 CORS，不使用 cookie 或 URL token。
- nginx 限制连接数和请求速率；服务另有设备速率、并发、请求体和存储配额。
- blob 按流写盘，校验长度和 SHA-256，原子发布；断线和校验失败不产生可引用的文件。
  下载支持 HEAD、Range 和校验摘要。文件始终以附件形式返回，不执行上传的 HTML。
- 记录更新必须带服务器的 baseRevision；旧版本返回 409，整个批次回滚。
  删除保留 tombstone，恢复需要明确标记，不会因离线设备重新上线自动复活。
- requestID 在设备范围内永久去重；超时或重启后重试不会重复写入。
- 服务账号没有 shell 或额外权限；systemd 限制文件写入范围、内存、任务数和网络。
  公网接口不记录内容、请求 URL 或凭证，nginx 的该路径关闭 access log。
- 默认每库 10 GiB 文件、100,000 个文件、512 MiB 记录和历史配额；单文件 512 MiB。
  上传保留至少 256 MiB 系统磁盘余量。配额用完明确返回 507，不丢弃历史。

这是**传输加密和服务器权限隔离**，尚未实现端到端加密；有服务器 root 权限的人可以
读取正文和附件。没有自动清理旧 blob 或历史，以便冲突恢复和备份保持可验证。
同盘备份使用不可变文件的硬链接，节省空间；它能应对误删除、逻辑故障，但不能应对
整盘损坏或服务器失陷。需要另行配置异机备份，不能把这 7 份当作异地灾备。

## 构建、测试与部署

部署前提：

- Linux amd64 服务器，使用 systemd，装有 `python3`，部署方能以 root 通过 SSH 登录。
- `/etc/nginx/nginx.conf` 中已有该域名的 HTTPS server（含 `server_name sync.example.com;`
  和有效证书）。本服务只在其中加一个 include，不签发或续期证书。

```sh
go test -race -count=1 ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
sh deploy.sh SSH_HOST sync.example.com
python3 smoke.py --host SSH_HOST --base-url https://sync.example.com/freddie-sync/
```

部署脚本在本机交叉编译 Linux amd64 静态程序，经已有 SSH 连接传输。远端不安装 Go，
不安装新 Python 依赖，不变更现有站点的根路由或证书。

`deploy/install.py` 要求 nginx 配置里恰好有一处 `server_name sync.example.com;`，否则拒绝修改。
域名写入 `/etc/freddie-sync/serve.env`，由 systemd 传给 `freddie-sync serve --host`；
服务只接受该 Host 的请求，签发的凭证里写入 `https://sync.example.com/freddie-sync/`
（需要别的路径时可给 `serve` 加 `--public-url`）。以后升级时用同样的两个参数再运行 `deploy.sh`。
安装会保留 nginx 原配置，先检查 nginx/systemd 配置，再切换版本和重启 Freddie 服务。
失败时回滚配置与旧程序。二进制按 SHA-256 放在 `/opt/freddie-sync/releases/`，
`/opt/freddie-sync/current` 指向运行版本。升级默认要求数据库 schema 向后兼容；未来有
不可逆迁移时应先做备份并单独设计迁移，不能只切回二进制。

nginx 复用现有证书；证书的续期仍由现有运维流程负责。

## 库与设备凭证

服务器没有用户账号，隔离单位是**库**：每份设备凭证生成时绑定一个库，服务器按 token 认出库，
客户端无法切换。不同库的记录、历史、文件目录和配额互相独立。给别人用时为其新建一个库；
服务器管理员仍能读取所有库的内容（见上文“安全与数据保护”）。

### 管理页面

```sh
sh admin.sh SSH_HOST      # 可选第二个参数指定本地端口，默认 17484
```

脚本用 SSH 把服务器上 0600 的管理 socket 转发到本机 `127.0.0.1:17484` 并打开浏览器，
Ctrl-C 关闭转发后页面即不可用。服务器不为此监听任何 TCP 端口，nginx 也不转发它。
手动转发：`ssh -N -L 127.0.0.1:17484:/run/freddie-sync/admin.sock SSH_HOST`。

页面可以新建库、重命名库、为库添加设备并下载凭证文件、撤销设备、删除已停用的库，并显示
各库的文件用量和设备最近活动（只记在内存里，服务重启后清空）。凭证只在生成时返回一次，页面不保存它。
管理接口只接受 Host 为本机回环地址的同源请求，其他网站无法通过浏览器调用它。

### 命令行

```sh
python3 provision.py --host SSH_HOST --label "My Mac" --out .local/mac.json
python3 provision.py --host SSH_HOST --label "Alice Mac" --new-library "Alice" --out .local/alice-mac.json
python3 provision.py --host SSH_HOST --label "Alice iPad" --library LIBRARY_ID --out .local/alice-ipad.json
```

不带 `--library` 时使用默认库（部署方自己的库）。`--library` 只接受已存在的库，写错 ID 不会
新建空库；新建用 `--new-library`，输出里会显示库 ID。

把凭证文件交给设备的使用者，在 Freddie 的“设置 → 同步 → 导入设备凭证并连接…”中选择它；
每台设备用自己的凭证。iPad 可通过“文件”App 选择。

本地凭证文件为 0600，`.local/` 已被 Git 忽略。生成时 token 不出现在参数或终端输出中，
文件已存在则拒绝覆盖。不要把文件放进论文库或备份到公共 Git 仓库。
导入原生客户端后，token 存入 Keychain，设备配置仅保存服务器地址和设备/库 ID。
LLM API Key 不经此服务传输。

```sh
ssh SSH_HOST '/opt/freddie-sync/current/freddie-sync device-list'      # 按库分组
ssh SSH_HOST '/opt/freddie-sync/current/freddie-sync library-list'     # JSON，含用量
ssh SSH_HOST '/opt/freddie-sync/current/freddie-sync library-add --name "Alice"'
ssh SSH_HOST '/opt/freddie-sync/current/freddie-sync device-revoke --id DEVICE_UUID'
ssh SSH_HOST '/opt/freddie-sync/current/freddie-sync library-delete --id LIBRARY_ID'
ssh SSH_HOST 'systemctl status freddie-sync.service --no-pager'
ssh SSH_HOST 'systemctl list-timers freddie-sync-backup.timer --no-pager'
ssh SSH_HOST 'journalctl -u freddie-sync.service -u freddie-sync-backup.service --since today --no-pager'
```

丢失设备时撤销其凭证，再生成新凭证。已经下载的数据无法通过撤销从离线设备删除。
设备全部撤销后，管理页面把库收进“已停用的库”，可以在那里删除。删除会同时移除库的记录、
历史、文件和已撤销的设备，不能恢复；默认库和仍有可用设备的库不能删除。已有的每日备份里
最多还保留 7 天。`smoke.py` 结束时会删除自己建的两个库。

## 协议 v1

API base URL 由凭证的 `baseURL` 给出，例如 `https://sync.example.com/freddie-sync/`。除 `GET health` 外均要求
`Authorization: Bearer <device-token>`。JSON 请求要求 `Content-Type: application/json`。
JSON 中服务器版本号使用安全整数；ID 为 1–128 个 ASCII 字母数字及 `_ . : -`，首字符
必须是字母或数字。现有论文、附件、笔记、写作稿应直接沿用原 UUID。

| 请求 | 用途 |
|---|---|
| `GET health` | 存活检查，只返回状态 |
| `GET v1/info` | 协议版本、storeID、libraryID、deviceID 与大小限制 |
| `POST v1/changes` | 原子提交一批增删改 |
| `GET v1/changes?after=0&limit=100` | 按服务器版本顺序取历史，包含删除 |
| `GET v1/changes?after=100&until=200&limit=100` | 读完固定上界的历史，避免翻页期间新增记录干扰 |
| `GET v1/records/{kind}/{id}` | 获取当前版本，包括 tombstone |
| `PUT v1/blobs/{sha256}` | 上传文件，需 Content-Length 和 application/octet-stream |
| `HEAD v1/blobs/{sha256}` | 检查同库文件是否已存在 |
| `GET v1/blobs/{sha256}` | 下载文件，可 Range 续传 |

支持的 kind：`paper`、`attachment`、`folder`、`folderMembership`、`itemType`、`note`、
`readingState`、`translationSegment`、`pdfAnnotation`、`writing`、`htmlState`。
payload 是客户端定义的 JSON 对象，服务不理解或执行其内容；每条最多 256 KiB。
正文、PDF、HTML、图片等较大内容放 blob，在 payload 里保存其角色和相对路径，
并在 mutations[].blobs 里完整列出所引用哈希。服务只接受已上传到本库的 blob。

```json
{
  "requestID": "bdaaa085-5db7-4ae9-849b-c3a9c6bf064e",
  "mutations": [
    {
      "kind": "paper",
      "id": "bde9e351-a7c6-4fc3-973f-fecb08bdab41",
      "baseRevision": 0,
      "deleted": false,
      "payload": {"schemaVersion": 1, "title": "Example paper"},
      "blobs": []
    }
  ]
}
```

返回 `{"requestID":"…","revisions":[1]}`。一个批次最多 100 条、总请求体最多 4 MiB。
创建用 baseRevision=0；更新用当前 record.revision，版本由服务器分配，与客户端时钟
或 `Paper.modifiedAt` 无关。收藏、移动文件夹、文献类型变更因此无需改论文的列表排序。

删除用 `deleted:true,payload:null,blobs:[]`。恢复用当前删除记录的版本，另加
`restore:true,deleted:false` 和恢复内容。对已删除记录普通写入总是返回 409。

同一设备重试成功请求必须保留相同 requestID 和内容；修改请求内容必须使用新 requestID。
不同设备可以用同一 ID，回执互不干扰。409 冲突响应的 error.current 带最新记录（若存在），
本次所有写入均未提交，客户端应合并或保留冲突副本，禁止盲目重试覆盖。

分页响应包含 changes、cursor、headRevision、hasMore。单页最多 500 条/约 4 MiB，
不保证总是达到 limit。客户端必须先持久化这一页，再推进 cursor；首次读到的 headRevision
可作为接下来各页的 until。未发生变化时 cursor 保持原值。

客户端必须把本地队列、游标和服务器已知版本绑定到 **baseURL + libraryID + storeID**。
首次连接先读取 info；storeID 改变或游标超过服务器历史时停止自动覆盖，保留本地内容，
重新拉取并对账，不能静默把游标清零后上传全部本地删除。

401 要求重新配对；409 要求合并；429/503 按 Retry-After 退避；网络中断/500 应指数退避
并加抖动。413/415/422 是请求或文件问题；507 需处理配额/磁盘问题。
上传中断从头重试该文件，下载支持 Range 续传；收到文件后仍必须校验完整 SHA-256。

## 客户端约定

Freddie 原生客户端（`ReadPaper/Services/LibrarySync/`）已实现持久化 outbox、拉取应用和冲突 UI；
修改客户端或实现其他客户端时遵守以下约定：

1. 从 SwiftData 导出可版本化 DTO，保留原始 UUID；禁止上传本机绝对路径或系统设置、密钥。
   附件本地路径由 PaperFileStore 在当前设备重建。同步状态单独持久化，不改已有 SwiftData
   模型，也不依赖 Paper.modifiedAt 检测收藏或文件夹变更。
2. 先暂存并校验文件，再把记录和本地变更游标一起应用；一个缺失的文件不能被当成已有附件。
   所有资源相对路径要拒绝 `..`、绝对路径、符号链接和越界写入。
3. 批注按单条 UUID 同步，不用整份 sidecar 的最后修改时间互相覆盖；写作稿冲突保留副本。
   更新已经打开的编辑器/阅读器前要与未保存的本地状态协调。
4. HTML 修改先等待 HTMLRegionWriter 落盘；同步时为可变 HTML/写作稿取稳定快照。
   增量译文 PDF 的 blob、附件 ID 和 translatedLastPage 必须在同一记录/批次保持一致。
5. 阅读位置降低上传频率，在用户打开论文时提供续读位置，避免远端更新打断当前滚动。
   同步批量写库，避免每段译文/每次滚动触发整个窗口的 @Query 刷新。

## 备份、校验与恢复

每日 UTC 03:40 后随机延迟不超过 10 分钟运行备份，保留最近 7 个成功快照。先使用 bbolt
在线事务快照，再链接不可变文件，检查数据库页和全部登记文件的 SHA-256，成功才发布目录。
失败的 `.partial-*` 目录由命令自行移除，不参与保留策略。手动运行：

```sh
ssh SSH_HOST 'systemctl start freddie-sync-backup.service'
ssh SSH_HOST '/opt/freddie-sync/current/freddie-sync verify --data /var/backups/freddie-sync/backup-TIMESTAMP'
```

恢复命令仅允许写入不存在的新目录，校验后复制内容并更换 storeID；不会覆盖现有数据。
先准备恢复目录，确认后再切换服务的数据目录，保留旧目录用于回退：

```sh
ssh SSH_HOST '/opt/freddie-sync/current/freddie-sync restore --from /var/backups/freddie-sync/backup-TIMESTAMP --data /var/lib/freddie-sync-restored'
```

正式切换前要停止服务，将恢复目录和旧目录做原子换名，并把恢复目录的属主改为
freddie-sync。客户端会发现 storeID 变化并需要重新对账。不要直接用 cp 覆盖正在使用的数据库。

实现依据：[bbolt 事务与在线备份](https://github.com/etcd-io/bbolt)、
[Go HTTP 服务器与大小限制](https://pkg.go.dev/net/http)、
[Go 官方漏洞检查工具](https://go.dev/doc/tutorial/govulncheck)。
