# Release Notes

## v1.2.27

### Full-Interface Internationalization

- Added and improved i18n coverage for all visible panel interfaces, including login page, dashboard, settings, alerts, scheduled tasks, security, software, file management, site list, site details, AI diagnostics, and other pages, all integrated into a unified text system.
- Chinese and English locale files now include common operation text, reducing the occurrence of untranslated English placeholders or prompts in the interface.
- Added i18n-related regression tests to ensure routing, templates, and frontend calls remain consistent with language keys.

### Visible Text Cleanup

- Unified wording for site details, file imports, AI diagnostics, and software management areas.
- Fixed several key prompts affecting user operations, such as CSRF, site ID, remote import address, AI settings, etc.

### English Project Documentation

- Added English version of the project documentation for non-Chinese users to quickly understand the project positioning, feature modules, installation methods, security model, and maintenance commands.
- Default README now includes an English documentation link for easy navigation.

### Verification

- This update focuses on text and documentation, with no database structure changes or install script adjustments.
- Completed `go test ./...`, `go vet ./...`, `go build .`, and `git diff --check` verification.
