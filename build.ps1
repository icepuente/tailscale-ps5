# Build a Go program for freebsd/amd64 and wrap it in a PS5 payload.
# Output: build/<package name>.elf, ready for tools/send.py.
param(
    [string]$Sdk = "$PSScriptRoot/../ps5_emulator/KytyPS5/_Build/ps5-diagnostics/ps5-payload-sdk",
    [string]$Llvm = 'C:/Program Files (x86)/Microsoft Visual Studio/2022/BuildTools/VC/Tools/Llvm/x64/bin',
    [string]$Go = $(if (Test-Path "$PSScriptRoot/tools/go/bin/go.exe") { "$PSScriptRoot/tools/go/bin/go.exe" } else { 'go' }),
    # Link address of the Go image. Kept clear of Go's heap hints
    # (0x00c000000000, 0x01c000000000, ...). goshim reports if it is taken.
    # Must be a 16 KiB-aligned base plus the 0x1000 ELF header, or the
    # first segment gets a negative file offset.
    [string]$TextAddr = '0x4000001000',
    [string]$Package = './probe',
    # goshim's dead-man switch: the payload process exits after this many
    # seconds whatever the Go program does. 0 disables it.
    [int]$DeadlineSeconds = 90
)
$ErrorActionPreference = 'Stop'
$sdkPath = (Resolve-Path -LiteralPath $Sdk).Path
$llvmPath = (Resolve-Path -LiteralPath $Llvm).Path
$build = Join-Path $PSScriptRoot 'build'
$name = Split-Path -Leaf $Package
New-Item -ItemType Directory -Force -Path $build | Out-Null

# goroot-patches/<path> replaces GOROOT/src/<path> through -overlay, leaving
# the toolchain unmodified.
$replace = [ordered]@{}
$gorootPatches = (Resolve-Path (Join-Path $PSScriptRoot 'goroot-patches')).Path
$gorootSrc = Join-Path (& $Go env GOROOT).Trim() 'src'
Get-ChildItem -Recurse -File $gorootPatches | ForEach-Object {
    $replace[(Join-Path $gorootSrc $_.FullName.Substring($gorootPatches.Length + 1))] = $_.FullName
}
$overlay = Join-Path $build 'overlay.json'
@{ Replace = $replace } | ConvertTo-Json | Set-Content -Encoding ascii $overlay

# module-patches/<module>@<version>/...: Go refuses overlays inside the
# module cache, so each patched module is copied fresh from the cache to
# build/modules/<module>@<version> and patched there. The program's go.mod
# points at that copy with a replace directive.
$modPatches = Join-Path $PSScriptRoot 'module-patches'
$modCount = 0
if (Test-Path $modPatches) {
    $modPatches = (Resolve-Path $modPatches).Path
    $modcache = (& $Go env GOMODCACHE).Trim()
    Get-ChildItem -Recurse -Directory $modPatches | Where-Object { $_.Name -like '*@*' } | ForEach-Object {
        $rel = $_.FullName.Substring($modPatches.Length + 1)
        $src = Join-Path $modcache $rel
        $dst = Join-Path $build "modules/$rel"
        if (-not (Test-Path $src)) { throw "module $rel is not in the module cache; run go mod download" }
        if (Test-Path $dst) { Remove-Item -Recurse -Force $dst }
        New-Item -ItemType Directory -Force -Path (Split-Path $dst) | Out-Null
        Copy-Item -Recurse $src $dst
        Get-ChildItem -Recurse -File $dst | ForEach-Object { $_.IsReadOnly = $false }
        $modRoot = $_.FullName
        Get-ChildItem -Recurse -File $modRoot | ForEach-Object {
            $fileRel = $_.FullName.Substring($modRoot.Length + 1)
            $target = Join-Path $dst $fileRel
            # New files (ps5_*.go) have no original; any other patch must.
            if ($_.Name -notlike 'ps5_*' -and -not (Test-Path $target)) {
                throw "patch $rel/$fileRel has no original; was the module version bumped?"
            }
            Copy-Item -Force $_.FullName $target
            $script:modCount++
        }
    }
}

Write-Output "== go build $Package (freebsd/amd64, text at $TextAddr, $($replace.Count) GOROOT patches, $modCount module patches)"
$env:GOOS = 'freebsd'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
Push-Location (Join-Path $PSScriptRoot ($Package -replace '^\./', ''))
try {
    # -R 0x4000 rounds segments to the PS5's 16 KiB pages.
    & $Go build -trimpath -overlay $overlay -ldflags "-T $TextAddr -R 0x4000" -o (Join-Path $build 'go.elf') .
    if ($LASTEXITCODE) { throw 'go build failed' }
} finally {
    Pop-Location
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
}

Write-Output "== compile goshim (deadline $DeadlineSeconds s)"
$cflags = @('--target=x86_64-sie-ps5', '-isysroot', $sdkPath, '-isystem', "$sdkPath/target/include",
            '-fPIC', '-fno-stack-protector', '-fno-plt', '-femulated-tls', '-Wall', '-Wextra', '-Werror', '-O2',
            "-DGOSHIM_DEADLINE_SECONDS=$DeadlineSeconds")
& "$llvmPath/clang.exe" @cflags -c "$PSScriptRoot/shim/goshim.c" -o "$build/goshim.o"
if ($LASTEXITCODE) { throw 'goshim.c failed' }
# .incbin resolves go.elf relative to the working directory.
Push-Location $build
try {
    & "$llvmPath/clang.exe" --target=x86_64-sie-ps5 -c "$PSScriptRoot/shim/blob.S" -o "$build/blob.o"
    if ($LASTEXITCODE) { throw 'blob.S failed' }
} finally { Pop-Location }

Write-Output '== link'
$elf = Join-Path $build "$name.elf"
& "$llvmPath/ld.lld.exe" -m elf_x86_64 -pie --eh-frame-hdr -z max-page-size=0x4000 -T "$sdkPath/ldscripts/elf_x86_64.x" `
    --hash-style=gnu -L "$sdkPath/target/lib" -o $elf "$sdkPath/target/lib/crt1.o" "$build/goshim.o" "$build/blob.o" `
    -lc -lkernel_web -lSceLibcInternal
if ($LASTEXITCODE) { throw 'link failed' }
$size = (Get-Item $elf).Length
Write-Output "== built $elf ($size bytes)"
