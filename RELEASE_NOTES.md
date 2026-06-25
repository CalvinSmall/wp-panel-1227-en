# 更新说明

## v1.2.22

### 新增：通用 PHP 网站支持 public 入口目录

- 创建通用 PHP 网站时新增“Web 入口目录”选项，可选择“项目根”或“public”。
- Laravel、Symfony 等入口文件位于 `public/index.php` 的程序，可选择 `public`，面板会将 Nginx root 指向站点项目根下的 `public` 目录。
- WordPress 站点保持原有行为，不显示该选项，也不会改变默认根目录配置。

### 改进：PHP 站点详情页可后续切换入口目录

- 通用 PHP 网站详情页新增 Web 入口目录设置，可在“项目根”和“public”之间切换。
- 切换到 `public` 时，面板会自动创建对应目录并设置站点用户权限。
- 网站根目录仍保留为项目根，用于文件管理、备份、权限加固和 PHP-FPM open_basedir，避免 Laravel 的 `.env`、`vendor`、`storage` 等项目文件直接暴露给公网。

### SSL 与 Nginx 联动

- 启用或续期 SSL 时，会使用当前有效 Web 入口目录写入 ACME 验证文件。
- 重建 Nginx 配置、切换缓存、修改 CDN 真实 IP、修改站点域名等会重新渲染 Nginx 的场景，会保留 PHP 站点的入口目录配置。
- 如果 `public` 目录被删除，相关流程会自动补建目录，降低站点配置失效风险。
- 修复创建网站时首次自动申请 Let's Encrypt 证书可能失败、稍后在详情页再次申请才成功的问题。
- 面板现在会持久化 ACME 账户 registration 信息；老服务器如果只有既有 `account.key`，会按账户私钥恢复 registration 并保存，避免后续 ACME 订单缺少账户上下文。
- 对 Let's Encrypt 偶发的订单 finalize 临时错误增加一次短重试，降低首次签发阶段的瞬时失败概率。

### 数据库升级

- `websites` 表新增 `document_root_subdir` 字段，用于记录通用 PHP 网站的 Web 入口目录。
- 老版本升级后默认值为空，所有已建站点继续使用项目根，不会自动切换到 `public`。
- 新装和升级路径均已处理，升级步骤支持重复执行。

### 安全与稳定性

- Web 入口目录仅支持留空（项目根）或 `public`，不接受任意路径，避免路径穿越和误指向系统目录。
- Nginx 对外访问目录和站点项目根分离，降低 Laravel 等框架项目根敏感文件被访问的风险。
- 新建站点失败时会正确回滚已创建的网站目录和日志目录，避免残留孤立目录。
- ACME 账户元数据固定保存在 `/www/server/panel/acme/account.json`，文件权限为 `0600`，不包含证书私钥或站点证书内容。

### 测试

- 增加 Web 入口目录标准化和有效 root 计算测试，覆盖 `public`、尾斜杠、路径穿越、嵌套目录和非 PHP 站点场景。
- 增加自动创建 `public` 目录测试。
- 增加数据库升级测试，覆盖 `document_root_subdir` 字段的新装和从旧版本升级场景。
