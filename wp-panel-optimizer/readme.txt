=== WP Panel Optimizer ===
Contributors: naibabiji
Requires at least: 5.0
Tested up to: 7.0
Requires PHP: 8.1
Stable tag: 1.1.5
License: GPL-2.0+
License URI: https://www.gnu.org/licenses/gpl-2.0.html

Works with WP Panel to manage FastCGI cache, disable update detection, disable file editing, and other optimizations from the WordPress backend, with two-way sync to panel settings.

== Description ==

WP Panel Optimizer is the companion plugin for [WP Panel](https://github.com/CalvinSmall/wp-panel-1227-en), syncing optimization settings in real-time with the server-side panel via the panel API.

Author: [naibabiji](https://blog.naibabiji.com) | Plugin: [GitHub](https://github.com/CalvinSmall/wp-panel-1227-en)

= Features =

* **FastCGI Cache Management**: Enable/disable Nginx FastCGI full-site cache from the WordPress backend, set cache TTL
* **Cache Preloading**: Manually or auto-visit public pages after cache clear to generate FastCGI cache files in the background
* **Disable Update Detection**: Completely block WordPress update detection (no dashboard red dots, no notifications, update buttons also disabled). To update, disable this switch first then check
* **Disable File Editing**: Writes DISALLOW_FILE_EDIT constant to wp-config.php
* **Admin Bar Quick Clear**: One-click Nginx cache clear from the WordPress admin bar
* **Auto Clear Cache**: Auto-clear cache on post publish/update/delete
* **Two-way Panel Sync**: Settings changes auto-push to panel, also auto-pull latest panel state

= Requirements =

* WP Panel v1.0.0-beta2+ installed
* Plugin auto-installed by panel (Site Details → WordPress Optimization → Install Companion Plugin), no manual upload needed

== Installation ==

1. In WP Panel, go to the website details page
2. In the "WordPress Optimization" card, check the optimizations you want to enable
3. Click the "Install Companion Plugin" button; the panel auto-deploys the plugin to the site's wp-content/plugins/
4. Activate the plugin in the WordPress backend, or the panel auto-activates it

After plugin installation, the panel writes a configuration file (containing panel address and API Key) to /var/wp-panel/site-secrets/<domain>/wp-panel-config.json outside the web directory; no manual credential entry needed.

== Changelog ==

= 1.1.5 =
* Added "Clear Nginx Cache" button on the plugin settings page for convenient mobile backend manual cache clearing

= 1.1.4 =
* Optimized cache preload scheduling: system Cron now proactively advances the queue on each WordPress trigger, preventing queue stalls from unstable single-event renewals

= 1.1.3 =
* Added FastCGI cache preloading: supports manual preloading, auto-preloading after cache clear, and background batch processing status display

= 1.1.2 =
* Fixed PHP Warning that could be triggered when open_basedir is enabled and www/bare domain config probing occurs
* Updated configuration file location description

= 1.0.0 =
* Initial version
* FastCGI cache management
* Disable update detection / disable file editing
* Admin bar cache clear button
* Auto-clear cache on post publish/update
* Two-way sync with panel API
