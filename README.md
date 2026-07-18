# 轻签

一个面向小内存 VPS 的私人网页书签。参考 Sun-Panel-v2 的分类导航和浏览器书签导入体验，使用 Go 标准库实现，不依赖数据库或前端框架。

## 功能

- 密码登录，登录状态使用签名 Cookie，不在服务端累积会话
- 添加、编辑、删除、搜索和分类筛选
- 为书签导入网站图片，或由 VPS 自动检测常见网站图标
- 由 VPS 异步检测网站是否可访问，显示 HTTP 状态与延迟
- 监控式状态筛选、异常通知、分类分组和统计面板
- 导入 Chrome、Edge、Firefox 导出的 HTML 书签
- JSON 备份导入与导出
- 数据保存到 VPS 卷，电脑和手机访问同一份书签
- 深色模式和移动端布局
- 设置中心统一管理站点名称、副标题、时区、毛玻璃颜色、操作和数据
- 首页双屏时间日期显示，以及登录后可用的 VPS 私人便签
- 可在设置中修改访问密码，密码加盐哈希后保存并自动注销旧会话
- 上传 JPG/PNG 背景图片并保存到 VPS 数据卷

## 一键安装（推荐）

支持使用 systemd 的 Linux x86_64 和 ARM64 VPS，不需要 Docker。使用 root 用户或带 sudo 权限的用户执行：

```bash
curl -fsSL https://raw.githubusercontent.com/WXD2233/lightmarks/main/install.sh | sudo sh
```

安装脚本会自动下载并校验最新发布包，创建低权限运行用户，将数据保存到 `/var/lib/lightmarks`，并注册 `lightmarks.service`。首次登录密码为 `123123`，请登录后立即在设置中修改。

默认端口为 `5856`。如果 VPS 启用了防火墙，需要放行该 TCP 端口，随后访问：

```text
http://你的VPS地址:5856
```

常用管理命令：

```bash
systemctl status lightmarks
journalctl -u lightmarks -f
systemctl restart lightmarks
```

重新执行一键安装命令即可更新程序，现有配置、密码、背景和书签不会被覆盖。

卸载程序但保留数据：

```bash
curl -fsSL https://raw.githubusercontent.com/WXD2233/lightmarks/main/uninstall.sh | sudo sh
```

## Docker 部署（可选）

1. 安装 Docker 和 Docker Compose，把整个项目目录上传到 VPS。
2. 创建配置：

   ```bash
   cp .env.example .env
   openssl rand -base64 36
   ```

3. 编辑 `.env`，设置强密码，并把上一步的随机字符串填入 `SESSION_SECRET`。
4. 启动：

   ```bash
   docker compose up -d --build
   ```

5. 将 `deploy/lightmarks.nginx.conf` 复制到 Nginx 站点配置，替换域名，然后用 Certbot 开启 HTTPS。

容器只监听 VPS 本机的 `127.0.0.1:5856`，公网访问应经过带 HTTPS 的 Nginx。

## 资源保护

- Docker 内存上限：64 MiB
- Go 垃圾回收目标：48 MiB
- 最多保存 2000 条书签
- 导入文件和请求体最大 1 MiB
- 背景图片最大 2 MiB、2000 万像素
- 单个网站图片最大 256 KiB、400 万像素，图片单独存盘
- 数据文件最大 4 MiB
- 页面每次最多渲染 96 条，避免大书签库一次性撑满 DOM
- 网站检测固定为 6 个并发，单站最多等待 6 秒
- 无网络壁纸、无远程图标请求、无常驻定时任务

## 网站检测安全

检测请求由 VPS 发起。默认的 `ALLOW_PRIVATE_TARGETS=false` 会阻止访问回环地址、私有网段、链路本地地址和云服务元数据地址，避免书签检测被滥用为内网探测。

如果这台 VPS 位于可信家庭网络，并且确实需要检测 NAS 的内网地址，可将 `.env` 中的 `ALLOW_PRIVATE_TARGETS` 改为 `true` 后重启容器。公网部署不建议开启。

数据位于 Docker 卷 `lightmarks-data`。网页中的“导出”会生成可恢复的 JSON 备份。

网页修改后的密码保存在数据卷的 `credentials.json` 中，环境变量 `BOOKMARK_PASSWORD` 只用于首次初始化。忘记密码时，可删除该文件并重启容器，密码将恢复为环境变量中的值。

## 源码运行

需要 Go 1.24 或更高版本：

```bash
cp .env.example .env
BOOKMARK_PASSWORD='your-strong-password' SESSION_SECRET='your-32-character-random-secret' go run .
```

打开 `http://127.0.0.1:5856`。
