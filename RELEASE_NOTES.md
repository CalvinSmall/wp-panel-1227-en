# 更新说明

## v1.2.16

- 修复软件管理中修改 PHP 参数后，WordPress 后台 Site Health 显示值与面板设置不一致的问题。
- PHP 的 `memory_limit`、`upload_max_filesize`、`post_max_size`、`max_execution_time`、`max_input_time` 现在会统一写入 WP Panel 运行时配置，并同步应用到所有站点的 PHP-FPM Pool。
- 软件管理新增 `max_input_time` 配置项，便于与 WordPress Site Health 的 “Max input time” 对齐。
- 保存会影响 PHP-FPM Pool 的 PHP 参数后，会立即重建所有站点的 PHP-FPM Pool 并重载 PHP-FPM；如部分站点重建失败，面板会返回明确错误提示。
- `max_input_vars` 保存时仅更新 PHP 运行时配置并重载 PHP-FPM，不再触发全站 PHP-FPM Pool 重建。
- 修复 PHP 配置读取时可能误读注释行中旧值的问题。
- 优化面板启动顺序，先确保 PHP 运行时基线配置完整，再批量重建站点 PHP-FPM Pool，降低启动阶段配置不一致风险。
