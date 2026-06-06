$ErrorActionPreference = 'Stop'

$env:GOCACHE = Join-Path (Get-Location) '.gocache'
$packages = go list ./... | Where-Object { $_ -notlike '*/scratch' }

go test $packages
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

go vet $packages
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

go build -o (Join-Path $env:GOCACHE 'wp-panel-verify.exe') .
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

git diff --check
exit $LASTEXITCODE
