# Freddie Sync Server

[Freddie](https://github.com/SYYANI/Freddie) 论文库的自托管同步服务。部署在你自己的服务器上，Mac / iPad 原生版和跨平台版（Windows / Linux / macOS）都能连上来，同步同一个库。

Freddie 不内置同步服务器，要用同步就得自己部署这个。

## 部署

### macOS Apple container

需要 Apple silicon Mac、macOS 26 和 Apple `container`（按 1.5.0 接口适配）：

```sh
container system start
sh container.sh build
sh container.sh up sync.example.com
```

Mac 上的服务入口为 `127.0.0.1:17483`，数据保存在 Linux 原生命名卷中。
HTTPS 接入、管理页面和备份见 [Apple container 部署说明](docs/apple-container.md)。

### Linux Docker/Compose

需要 Linux 服务器上的 Docker Engine + Compose v2，以及已有的 HTTPS nginx 站点。
在服务器的项目目录运行，先把 `.env` 中的域名改成自己的：

```sh
cp .env.example .env
# 编辑 .env，填写实际域名。
sudo install -d -m 0700 -o 10001 -g 10001 var var/data var/backups var/run
docker compose up -d --build --wait
```

容器以非 root 用户运行，使用主机网络监听 `127.0.0.1:17483`，数据保存在 `var/`。
nginx 接入、设备管理和定时备份见 [容器部署说明](docs/docker.md)。

### 直接部署

你需要：

- 一台 Linux amd64 服务器（systemd + python3），能用 root SSH 登录
- 一个已经配好 HTTPS 的 nginx 站点，例如 `sync.example.com`
- 本机装有 Go

```sh
sh deploy.sh root@vps sync.example.com
```

服务会挂在 `https://sync.example.com/freddie-sync/`，只给 nginx 加一个路径，不动现有站点和证书。升级时再跑一遍同样的命令。

## 添加设备

每台设备需要一份凭证文件。直接部署时，打开管理页面生成：

```sh
sh admin.sh root@vps
```

或者用命令行：

```sh
python3 provision.py --host root@vps --label "My Mac" --out .local/mac.json
```

容器部署的管理方式见 [Apple container](docs/apple-container.md#管理页面与设备凭证)
或 [Docker/Compose](docs/docker.md#管理与设备凭证)。

在 Freddie 里打开“设置 → 同步 → 导入设备凭证并连接…”，选中这个文件即可。

给别人用时，在管理页面为对方新建一个库，或加 `--new-library "名字"`。不同库的数据互相隔离。

## 须知

- 数据只做传输加密，**没有端到端加密**，服务器管理员能看到库里的内容。
- 直接部署每天自动备份、保留 7 份；容器部署按对应说明配置备份。同机备份之外，重要数据请另做异机备份。
- 设备丢了就在管理页面撤销它的凭证。

## 更多

部署细节、安全设计、同步协议、备份恢复等见 [docs/reference.md](docs/reference.md)。
