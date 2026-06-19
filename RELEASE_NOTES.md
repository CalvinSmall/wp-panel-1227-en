# 更新说明

## v1.2.19

### 新增：面板自动更新

后台「面板设置」新增自动更新功能，默认关闭，管理员开启后按策略自动完成面板升级。

**更新模式**：
- `patch_only`（默认）：仅自动安装 patch 版本（如 v1.2.3 → v1.2.4）
- `all_stable`：自动安装所有正式版，包括 minor/major
- 不自动安装 prerelease、beta、rc

**安全约束**：
- 更新时间窗口（默认 03:00-05:00，可自定义）
- 发布延迟（默认 15 分钟，等待 `.sha256.sig` 签名文件上传）
- 签名等待超时（默认 120 分钟，超时转失败并触发冷却）
- 同一目标版本失败后冷却 24 小时，避免反复尝试
- 全局更新锁，与手动更新互斥

**执行流程**：
- 下载 → Ed25519 签名校验 → SHA256 校验 → 预检 → 磁盘空间检查
- VACUUM INTO 数据库热备 → 旧二进制备份 → 写入回滚计划
- systemd-run 独立 watchdog 进程执行健康检查
- 健康检查通过：清理状态，SMTP 配置存在时发送成功邮件
- 健康检查失败：自动恢复旧二进制和数据库，发送失败邮件

**新增端点**：
- `/healthz`：本机健康检查接口，校验 HTTP + SQLite schema + 核心表可读性，仅允许 loopback 访问

**设置项**（`security_settings`）：
- `panel_auto_update_enabled`、`panel_auto_update_mode`、`panel_auto_update_window`
- `panel_auto_update_release_delay_minutes`、`panel_auto_update_signature_timeout_minutes`
- 最近状态：`panel_auto_update_last_*`
