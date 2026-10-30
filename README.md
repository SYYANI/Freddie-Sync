# Freddie Sync Server

[Freddie](https://github.com/SYYANI/Freddie) 论文库的自托管同步服务。部署在你自己的服务器上，Mac / iPad 原生版和跨平台版（Windows / Linux / macOS）都能连上来，同步同一个库。

Freddie 不内置同步服务器，要用同步就得自己部署这个。

## 你需要

- 一台 Linux amd64 服务器（systemd + python3），能用 root SSH 登录
- 一个已经配好 HTTPS 的 nginx 站点，例如 `sync.example.com`
- 本机装有 Go

## 部署

```sh
sh deploy.sh root@vps sync.example.com
```

服务会挂在 `https://sync.example.com/freddie-sync/`，只给 nginx 加一个路径，不动现有站点和证书。升级时再跑一遍同样的命令。

## 添加设备

每台设备需要一份凭证文件。打开管理页面生成：

```sh
sh admin.sh root@vps
```

或者用命令行：

```sh
python3 provision.py --host root@vps --label "My Mac" --out .local/mac.json
```

在 Freddie 里打开“设置 → 同步 → 导入设备凭证并连接…”，选中这个文件即可。

给别人用时，在管理页面为对方新建一个库，或加 `--new-library "名字"`。不同库的数据互相隔离。

## 须知

- 数据只做传输加密，**没有端到端加密**，服务器管理员能看到库里的内容。
- 每天自动备份，保留 7 份，但都在同一块盘上。重要数据请另做异机备份。
- 设备丢了就在管理页面撤销它的凭证。

## 更多

部署细节、安全设计、同步协议、备份恢复等见 [docs/reference.md](docs/reference.md)。
