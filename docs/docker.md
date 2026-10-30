# 使用 Docker 运行

macOS 上的 Apple `container` 使用独立的 [部署脚本与说明](apple-container.md)。

适用于 Linux 上的 Docker Engine + Compose v2，支持 amd64 / arm64。使用普通的 rootful
Docker；宿主机需要已有 HTTPS nginx 站点。以下以项目位于 `/srv/freddie-sync-server`、
域名为 `sync.example.com` 为例，Docker 命令由有相应权限的用户运行。

容器使用 host 网络，服务仍只监听宿主机 `127.0.0.1:17483`，nginx 继续代理这个地址。
此配置面向 Linux 服务器，没有为 Docker Desktop 或 rootless Docker 配置网络与文件权限。
host 网络的行为见 [Docker 文档](https://docs.docker.com/engine/network/drivers/host/)。

## 启动

在服务器的项目目录运行：

```sh
cd /srv/freddie-sync-server
cp .env.example .env
# 编辑 .env，将 FREDDIE_SYNC_HOST 改成实际域名（不带协议、端口或路径）。
sudo install -d -m 0700 -o 10001 -g 10001 var var/data var/backups var/run
docker compose up -d --build --wait
curl -fsS http://127.0.0.1:17483/health
```

可在 `.env` 设置 `FREDDIE_SYNC_PUBLIC_URL` 修改凭证里的 HTTPS 地址；留空时使用
`https://FREDDIE_SYNC_HOST/freddie-sync/`。更改地址时还需同步调整 nginx 的路径。

镜像采用固定的 Go 1.26.10 工具链构建静态程序，运行阶段使用 `scratch`，不含 shell 或包管理器。
程序以 UID/GID `10001:10001` 运行，根文件系统只读，默认内存上限为 256 MiB。
健康检查通过本机管理 socket 确认数据库可用，不输出任何内容；上面的 `curl` 另外检查 HTTP 接口。

持久化目录均相对于项目目录。整个 `var/` 作为一个挂载点进入容器的 `/state/`，
数据和备份位于同一挂载点，备份可以硬链接不可变文件而不必整份复制：

| 宿主机 | 容器内 | 内容 |
|---|---|---|
| `var/data/` | `/state/data/` | 数据库和文件 |
| `var/backups/` | `/state/backups/` | 校验后的备份 |
| `var/run/` | `/run/freddie-sync/`（另一挂载） | 0600 管理 socket |

这些目录和 `.env` 不进入 Git，也不进入镜像构建上下文。`docker compose down` 保留这些目录。
Compose 拒绝自动创建缺失的挂载目录，避免用 root 属主的空目录启动后发生权限错误。
容器不挂载 Docker socket，不监听公网管理端口。

## 接入 nginx

将 [deploy/nginx-limits.conf](../deploy/nginx-limits.conf) 加载到 nginx 的 `http {}` 中。
如果已有这两个 `limit_*_zone`，直接复用，不要重复定义。

生成路径配置：

```sh
sed 's/@SERVER_NAME@/sync.example.com/g' deploy/nginx-location.conf | sudo tee /etc/nginx/freddie-sync-location.conf >/dev/null
```

在该域名已有的 **HTTPS** `server {}` 中增加：

```nginx
include /etc/nginx/freddie-sync-location.conf;
```

检查并重载，确认域名和证书可用：

```sh
sudo nginx -t
sudo systemctl reload nginx
curl -fsS https://sync.example.com/freddie-sync/health
```

此后客户端使用 `https://sync.example.com/freddie-sync/`。容器不会修改 nginx 或管理证书。

## 管理与设备凭证

从自己的电脑运行已有的管理脚本，第三个参数指定宿主机上的 socket 路径：

```sh
sh admin.sh root@vps 17484 /srv/freddie-sync-server/var/run/admin.sock
```

页面会在 `http://127.0.0.1:17484/` 打开，可以新建库、签发设备凭证、撤销设备。
SSH 用户需有权访问该 socket，例如服务器 root；Ctrl-C 关闭转发。

也可以在服务器的项目目录使用命令行：

```sh
docker compose exec -T sync /usr/local/bin/freddie-sync library-list
docker compose exec -T sync /usr/local/bin/freddie-sync device-list
```

把新设备凭证直接写入私有文件，文件已存在时拒绝覆盖：

```sh
(
    set -euC
    umask 077
    mkdir -p .local
    docker compose exec -T sync /usr/local/bin/freddie-sync device-add --label "My Mac" > .local/mac.json
)
```

将文件安全传到设备，在 Freddie 的“设置 → 同步 → 导入设备凭证并连接…”中导入。
`provision.py` 和 `smoke.py` 使用 systemd 部署的固定程序路径，容器部署使用上面的管理入口。

## 备份与恢复

容器自身不启动定时任务。宿主机装有 Python 3 时，可以手动执行：

```sh
sudo sh deploy/docker-backup.sh
```

脚本在线创建并校验快照，成功后保留最新 7 份。备份位于 `var/backups/backup-TIMESTAMP/`；
失败时不会清理已有快照。宿主机 root 的 crontab 可添加下面一行，每天服务器时区 03:40 执行：

```cron
40 3 * * * /bin/sh /srv/freddie-sync-server/deploy/docker-backup.sh >>/var/log/freddie-sync-backup.log 2>&1
```

手动校验某份备份：

```sh
docker compose exec -T sync /usr/local/bin/freddie-sync verify --data /state/backups/backup-TIMESTAMP
```

恢复时先停止服务，再在 `var/` 下生成一个尚不存在的新目录：

```sh
docker compose stop sync
docker compose run --rm --no-deps sync restore --from /state/backups/backup-TIMESTAMP --data /state/restored
```

确认恢复命令成功后，将旧目录留作回退，再把恢复目录移到 `var/data`：

```sh
(
    set -eu
    test ! -e var/data-before-restore
    sudo mv var/data var/data-before-restore
    sudo mv var/restored var/data
    docker compose up -d --force-recreate --wait
)
```

恢复会更换 storeID，客户端需要重新对账。备份包含可读取的库内容和认证数据，仍需妥善保管，
并另行做异机备份；同机 7 份快照不能应对整盘损坏。

## 升级与查看状态

更新服务器上的源码后，在同一个项目目录运行：

```sh
docker compose up -d --build --wait
docker compose ps
docker compose logs --tail 100 sync
```

升级前先做一次备份。数据保存在宿主机目录中，重建镜像不会删除它。
`.env` 中的 `FREDDIE_SYNC_VERSION` 可设为 Git 提交号，用于版本与备份记录。

这是独立于 systemd 的部署方式；默认使用新的 `var/data`，不会导入原有服务的数据。
从原有部署迁移时，应先停止旧服务，迁移数据并调整 UID/GID；同一数据库只能由一个实例使用，
同一个 `127.0.0.1:17483` 也只能运行一个后端。
