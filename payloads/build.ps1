# Build the small C payloads in this folder (hello.c, stop_ps5ts.c) with the
# PS5 payload SDK. Output: build/<name>.elf, ready for tools/send.py.
param(
    [string]$Sdk = "$PSScriptRoot/../../ps5_emulator/KytyPS5/_Build/ps5-diagnostics/ps5-payload-sdk",
    [string]$Llvm = 'C:/Program Files (x86)/Microsoft Visual Studio/2022/BuildTools/VC/Tools/Llvm/x64/bin'
)
$ErrorActionPreference = 'Stop'
$sdkPath = (Resolve-Path -LiteralPath $Sdk).Path
$llvmPath = (Resolve-Path -LiteralPath $Llvm).Path
$build = Join-Path $PSScriptRoot '../build'
New-Item -ItemType Directory -Force -Path $build | Out-Null

foreach ($source in Get-ChildItem "$PSScriptRoot/*.c") {
    $name = $source.BaseName
    $object = Join-Path $build "$name.o"
    $elf = Join-Path $build "$name.elf"
    & "$llvmPath/clang.exe" --target=x86_64-sie-ps5 -isysroot $sdkPath -isystem "$sdkPath/target/include" `
        -fPIC -fno-stack-protector -fno-plt -femulated-tls -Wall -Wextra -Werror -O2 -c $source.FullName -o $object
    if ($LASTEXITCODE) { throw "compile failed: $name" }
    & "$llvmPath/ld.lld.exe" -m elf_x86_64 -pie --eh-frame-hdr -z max-page-size=0x4000 -T "$sdkPath/ldscripts/elf_x86_64.x" `
        --hash-style=gnu -L "$sdkPath/target/lib" -o $elf "$sdkPath/target/lib/crt1.o" $object -lc -lkernel_web -lSceLibcInternal
    if ($LASTEXITCODE) { throw "link failed: $name" }
    Write-Output "built $elf"
}
