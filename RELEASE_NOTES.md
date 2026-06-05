# 更新说明

## [Unreleased]

### 新功能

- **WordPress 安装包本地优先 + 后台管理**：建站时优先使用本地已缓存的安装包，在线下载作为兜底。后台设置页新增安装包管理卡片，显示本地包状态（大小、更新时间），支持手动上传 `.zip` 和在线下载。上传和下载均先写临时文件再原子替换，避免并发写入冲突。（`executor/deploy.go`、`handlers/settings.go`、`router.go`、`templates/settings.html`）

- **网站列表增加文件管理快捷入口**：网站列表操作列增加「文件」按钮，跳转到文件管理页面并自动选中对应网站及加载文件列表。（`templates/websites.html`、`templates/files.html`）

- **数据库表前缀同步 + 自动检测**：`FixWPConfigCredentials` 增加 `tablePrefix` 参数，支持替换 `$table_prefix`。后台网站详情页表前缀改为可编辑输入框 + 自动检测按钮，从 `SHOW TABLES` 读取实际前缀，搬家后可一键同步。同步数据库信息确认弹窗包含表前缀变更提示。（`executor/wpconfig.go`、`executor/mariadb.go`、`handlers/website.go`、`router.go`、`templates/website_detail.html`）

- **一键修改 WordPress 站点 URL (siteurl/home)**：后台网站详情页数据库区域新增「修改站点URL」按钮 + 弹窗，自动读取当前 siteurl/home，支持一键填入面板域名。提示用户文章内旧域名需用插件替换，以及修改后需重新保存固定链接。（`executor/mariadb.go`、`handlers/website.go`、`router.go`、`templates/website_detail.html`）

### 修复

- **同步数据库信息误报"未找到 DB_NAME"**：`FixWPConfigCredentials` 使用 `ReplaceAllString` 后比较字符串是否变化来判断正则是否匹配，当 wp-config.php 中的值已经和目标值一致时，替换后字符串不变，被误判为"未找到"。改为先用 `MatchString()` 判断正则是否匹配，再执行替换。（`executor/wpconfig.go`）

- **wp-config.php 双引号格式不兼容**：原正则仅匹配 `define('DB_NAME', '...')` 单引号格式，部分手动安装的 WordPress 使用 `define("DB_NAME", "...")` 双引号格式。现同时支持两种引号格式，DB_NAME、DB_USER、`$table_prefix` 均已覆盖。（`executor/wpconfig.go`）

- **错误信息缺少文件路径**：`FixWPConfigCredentials` 报错时未显示实际读取的 wp-config.php 路径，排查困难。错误信息现已包含完整路径。（`executor/wpconfig.go`）

- **下载安装包缺少 ZIP 完整性校验**：下载后若网络中断会留下不完整文件，导致后续建站失败。新增 `unzip -t` 校验，确保文件完整；恢复 `%w` 错误包装，保留 wget 失败的具体原因。（`executor/deploy.go`）

- **UpdateWPSiteURLs 空字符串覆盖**：原 SQL 使用 `CASE WHEN` 在字段为空时写入 `''`，导致 WordPress 白屏。改为按条件独立执行 UPDATE，仅非空字段才写入。（`executor/mariadb.go`）

- **自动检测表前缀后提示不明确**：检测成功后缺少提示用户需点击同步保存。（`templates/website_detail.html`）

- **文件管理 site_id 参数类型不匹配**：`URLSearchParams.get()` 返回字符串，而 Alpine.js `option :value` 绑定整数，严格比较导致下拉框无法选中。改用 `parseInt()` 匹配类型。（`templates/files.html`）

### 改进

- **域名变更后缓存清理优化**：`UpdateWPSiteURLs` 原实现使用 `find /var/cache/nginx/fastcgi -type f -delete` 全局删除所有站点的 Nginx 缓存，改为调用 `executor.ClearSiteCache(site.ID)` 精准清理当前站点（换 cache key + 重载 Nginx）。（`handlers/website.go`）

- **域名变更后 Redis Object Cache 清理**：域名变更后旧前缀的 Redis 缓存可能残留 `siteurl`/`home` 旧值，导致 WordPress 后台显示不一致。新增按域名前缀 `--scan` + 批量 `DEL` 清理逻辑，单次 `redis-cli DEL key1 key2 ...` 批量执行，避免逐 key 启动进程。（`handlers/website.go`）

### UI

- **WordPress 安装包和远程备份并排显示**：设置页布局调整，安装包管理卡片与远程备份卡片并排展示。（`templates/settings.html`）
