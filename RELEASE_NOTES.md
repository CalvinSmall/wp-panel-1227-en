# 更新说明

## v1.2.14

### 新功能
- 配套插件 `wp-panel-optimizer` 新增 FastCGI 缓存预加载：支持手动预加载、清缓存后自动预加载、发布/评论变更后的局部预加载，并在 WordPress 后台显示队列进度。
- 新增网站级 CDN 真实 IP 配置：支持 Cloudflare、通用 CDN 兼容模式和自定义 CDN 配置组，单站点可绑定多个同 Header 配置组。

### 安全
- 修复 HTTPS 站点 Nginx 模板 FastCGI 缓存指令错位问题：缓存指令原本位于 `location ~ \.php$` 外部（server 级别），导致 `wp-login.php` 等独立 location 块继承缓存指令，可能被缓存。现已移回 `location ~ \.php$` 内部。
- CDN 真实 IP 配置组的可信 IP 仅加入 Web 防护白名单；Cloudflare 站点级配置在官方 IP 段未缓存时不再退化为全来源信任。

### 优化
- 补全 FastCGI 缓存绕过条件，与 CDN 规则保持一致：新增 `wordpress_sec_` cookie、`wp-settings-` cookie、`/wp-signup.php` 路径的绕过检查（仅 WordPress 类型站点模板）。
- 将 `$wp_skip_cache` 变量初始化从 `location ~ \.php$` 内部提升到 server 级别（`include .conf` 之前），用户可在面板"Nginx 自定义配置 → Location 级配置"中通过 `if ($request_uri ~* "/path") { set $wp_skip_cache 1; }` 自定义绕过缓存的路径。
- 丰富面板 Nginx 自定义配置区域的帮助说明，新增可折叠的配置说明与示例（含缓存绕过写法、自定义 location 块示例）。
- 数据库备份列表折叠展示，避免长期积累后界面过长。
