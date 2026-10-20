$ErrorActionPreference = 'Stop'
$version = '20261006'
$hostArch = if ($env:GOARCH -eq 'arm64') { 'aarch64' } else { 'x86_64' }
$expectedHash = if ($hostArch -eq 'aarch64') {
    '9a835d5179c9f3a5a783c11a7f14062a249b4765dd11add64a66786864e82ab2'
} else {
    '317492c456aa27ee607a5919f1d2d38dcdc1112516a24d0bf4b00d078f52d17a'
}
$asset = "llvm-mingw-$version-ucrt-$hostArch"
$toolchain = Join-Path $env:GITHUB_WORKSPACE ".tools/$asset"
$compiler = Join-Path $toolchain "bin/$hostArch-w64-mingw32-gcc.exe"
if (!(Test-Path $compiler)) {
    $archive = Join-Path $env:RUNNER_TEMP "$asset.zip"
    Invoke-WebRequest "https://github.com/mstorsjo/llvm-mingw/releases/download/$version/$asset.zip" -OutFile $archive
    $actualHash = (Get-FileHash $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne $expectedHash) {
        throw 'llvm-mingw archive checksum mismatch'
    }
    Expand-Archive $archive -DestinationPath (Join-Path $env:GITHUB_WORKSPACE '.tools') -Force
    Remove-Item $archive
}
if (!(Test-Path $compiler)) {
    throw 'llvm-mingw compiler missing after extraction'
}
"CC=$($compiler.Replace('\', '/'))" | Out-File -FilePath $env:GITHUB_ENV -Encoding utf8 -Append
"$toolchain/bin" | Out-File -FilePath $env:GITHUB_PATH -Encoding utf8 -Append
& $compiler --version
