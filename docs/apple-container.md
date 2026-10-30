# 在 macOS 上使用 Apple container

适用于 Apple silicon Mac、macOS 26 和 Apple `container`。以下命令按 `container` 1.5.0
的接口编写，需要支持 `--publish-socket` 和命名卷；无需 Docker 或 Compose。
安装和 CLI 选项见 [Apple 官方文档](https://github.com/apple/container/blob/1.5.0/docs/command-reference.md)。

## 构建与启动

在项目目录运行：

```sh
container system start
sh container.sh build
sh container.sh up sync.example.com
```

`sync.example.com` 是设备连接的公开域名。脚本签发的凭证默认使用
`https://sync.example.com/freddie-sync/`；使用其他 HTTPS 路径时传入第三个参数：

```sh
sh container.sh up sync.example.com https://sync.example.com/custom-sync/
```

脚本不读取 Docker Compose 的 `.env`。镜像构建复用项目中的 Dockerfile；可用
`FREDDIE_SYNC_VERSION=COMMIT sh container.sh build` 设置版本标识。

默认创建名为 `freddie-sync` 的容器和 `freddie-sync-state` 的持久化卷。首次启动时，
脚本使用一个临时 BusyBox 容器准备卷目录及其权限，随后以 UID/GID `10001:10001`
运行服务。应用根文件系统只读，内存为 256 MiB，分配 2 个虚拟 CPU。

服务在虚拟机内部监听 `0.0.0.0:17483`，并显式传入 `--allow-non-loopback`。
Mac 上的端口发布地址固定为 `127.0.0.1:17483`，但容器自身的 IP（通常为 `192.168.64.x`）
同样可以从这台 Mac 和其他容器直接访问，这条路径不经过反向代理的限流。
同步接口仍要求设备 token；不要在同一台 Mac 上运行不受信任的容器。
直接部署和 Linux Compose 仍默认使用回环监听。

## 接入 HTTPS

在 Mac 上运行的 HTTPS 反向代理应连接 `http://127.0.0.1:17483/`。使用 nginx 时，
复用 [nginx 路径配置](../deploy/nginx-location.conf) 和
[限流配置](../deploy/nginx-limits.conf)，将 `@SERVER_NAME@` 替换为实际域名，
分别放入 HTTPS `server {}` 和 `http {}`。重载 nginx 的方式取决于它在 Mac 上的安装方式。

客户端地址必须是可访问且证书受信任的 HTTPS 地址。容器脚本不配置域名、证书或反向代理。

## 管理页面与设备凭证

管理 socket 通过 Apple container 的 `--publish-socket` 转发到项目目录中的
`.local/container-freddie-sync/admin.sock`，其父目录权限为 0700。
Unix socket 不通过共享文件系统挂载，也没有对外发布管理 TCP 端口。

安装有 Python 3 时，可以打开管理页面：

```sh
sh container.sh admin
# 或指定另一个本机端口：
sh container.sh admin 17485
```

脚本只在 Mac 的 `127.0.0.1` 上启动转发，并打开浏览器；Ctrl-C 关闭转发。
页面可以新建库、签发凭证、撤销设备，仍保留原有的 Host、Origin 和同源检查。

命令行管理无需 Python：

```sh
sh container.sh exec library-list
sh container.sh exec device-list
sh container.sh exec library-add --name "Alice"
sh container.sh exec device-revoke --id DEVICE_UUID
```

为设备生成私有凭证文件，文件已存在时拒绝覆盖：

```sh
(
    set -euC
    umask 077
    mkdir -p .local
    sh container.sh exec device-add --label "My Mac" > .local/mac.json
)
```

可添加 `--library LIBRARY_ID` 指定已有库。在 Freddie 的“设置 → 同步 → 导入设备凭证并连接…”
中导入文件即可。`provision.py` 使用 systemd 的程序路径，不用于这套部署。

## 数据与备份

数据库与文件保存在命名卷的 `/state/data/`，备份保存在 `/state/backups/`。
卷使用 Linux 原生文件系统，不将 bbolt 数据库直接放到 macOS 共享目录上。
这些数据不在项目的 `var/` 中，删除或重建容器会保留命名卷。

手动创建并校验在线备份：

```sh
sh container.sh backup
```

命令返回备份路径，例如 `/state/backups/backup-20261030T140000Z`。可以再次校验并导出到 Mac：

```sh
sh container.sh exec verify --data /state/backups/backup-TIMESTAMP
mkdir -p .local
container cp freddie-sync:/state/backups/backup-TIMESTAMP .local/backup-TIMESTAMP
```

Apple container 脚本不自动调度备份，也不删除旧快照；按需要配置 macOS 的定时任务和保留策略。
备份含认证数据与可读取的库内容，导出后应妥善保管，并复制到另一台机器。

准备恢复时，可先把备份恢复到卷中的新目录；命令不会覆盖现有数据：

```sh
sh container.sh exec restore --from /state/backups/backup-TIMESTAMP --data /state/restored
```

确认恢复成功后，停止并删除服务容器，再通过临时工具容器切换目录，保留旧数据用于回退：

```sh
sh container.sh stop
sh container.sh rm
container run --rm --user 10001:10001 --read-only --cap-drop ALL \
    --mount type=volume,source=freddie-sync-state,target=/state \
    docker.io/library/busybox:1.37 sh -ec '
        test -d /state/restored
        test ! -e /state/data-before-restore
        mv /state/data /state/data-before-restore
        mv /state/restored /state/data
    '
sh container.sh up sync.example.com
```

恢复会更换 storeID，客户端需要重新对账。整个命名卷必须保留；删除卷会删除数据和其中的备份。

## 停止、升级与状态

```sh
sh container.sh status
sh container.sh logs
sh container.sh stop
sh container.sh start
```

停止操作给服务最多 40 秒完成退出。升级前先备份，再使用相同卷重建：

```sh
sh container.sh backup
sh container.sh stop
sh container.sh rm
sh container.sh build
sh container.sh up sync.example.com
```

如果使用了自定义公开 URL，重建时也要传入。`up` 不会自动替换已有容器；停止后可用
`start` 重开，或先 `rm` 再 `up`。这套脚本没有配置开机自启或崩溃后的自动重启。

需要修改默认名称时，可设置以下环境变量；后续命令使用相同的值：

| 变量 | 默认值 |
|---|---|
| `FREDDIE_SYNC_CONTAINER_NAME` | `freddie-sync` |
| `FREDDIE_SYNC_CONTAINER_VOLUME` | `<容器名称>-state` |
| `FREDDIE_SYNC_CONTAINER_IMAGE` | `freddie-sync-server:local` |
| `FREDDIE_SYNC_CONTAINER_INIT_IMAGE` | `docker.io/library/busybox:1.37` |

上面的导出与恢复示例使用默认名称；自定义后需替换示例中的容器名和卷名。
