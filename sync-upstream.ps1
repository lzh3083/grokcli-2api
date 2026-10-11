<#
.SYNOPSIS
    同步原作者 (upstream) 的更新到你自己的 fork (origin) 并 rebase 开发分支。
.DESCRIPTION
    工作流：
      1. fetch upstream
      2. checkout main，merge upstream/main（默认不自动推送 origin/main，仅在显式传入 -PushMain 时才推送到 origin/main）
      3. checkout dev，rebase main（如果有冲突需手动解决后 git rebase --continue）
      4. push dev 到 origin/dev
.NOTES
    在仓库根目录运行： ./sync-upstream.ps1
    如需同步推送 main 分支到 origin/main，可加参数： ./sync-upstream.ps1 -PushMain
    如需跳过 dev 分支 rebase，可加参数： ./sync-upstream.ps1 -SkipDev
#>

param(
    [switch]$SkipDev,
    [switch]$PushMain
)

$ErrorActionPreference = "Stop"
Set-Location -Path (Split-Path -Parent $MyInvocation.MyCommand.Definition)

function Write-Step($msg) { Write-Host "`n[*] $msg" -ForegroundColor Cyan }
function Write-Ok($msg)   { Write-Host "[OK] $msg" -ForegroundColor Green }
function Write-Warn($msg) { Write-Host "[!]  $msg" -ForegroundColor Yellow }
function Die($msg)        { Write-Host "[X]  $msg" -ForegroundColor Red; exit 1 }

# 0. 前置检查
if (-not (Test-Path ".git")) { Die "当前目录不是 git 仓库" }

$upstream = git remote get-url upstream 2>$null
if (-not $upstream) { Die "未配置 upstream 远程。请先执行： git remote add upstream https://github.com/HM2899/grokcli-2api.git" }
Write-Ok "upstream -> $upstream"

# 1. 拉取 upstream 最新代码
Write-Step "fetch upstream ..."
git fetch upstream
if ($LASTEXITCODE -ne 0) { Die "git fetch upstream 失败" }

# 2. 同步 main
Write-Step "checkout main & merge upstream/main"
git checkout main
if ($LASTEXITCODE -ne 0) { Die "切换 main 失败（请先提交或 stash dev 上的改动）" }

$behind = [int](git rev-list --count main..upstream/main)
if ($behind -eq 0) {
    Write-Ok "main 已是最新，无需合并"
} else {
    Write-Host "    upstream 领先 $behind 个提交，近期变更预览："
    git log --oneline main..upstream/main -n 20
    git merge --ff-only upstream/main
    if ($LASTEXITCODE -ne 0) { Die "fast-forward 合并失败，请人工审查 upstream/main 变更后手动合并" }
    Write-Ok "merge 完成"
}

if ($PushMain) {
    Write-Step "push main -> origin (-PushMain 已启用)"
    git push origin main
    if ($LASTEXITCODE -ne 0) { Die "push origin main 失败" }
    Write-Ok "main 已推送"
} else {
    Write-Warn "默认未自动推送 main（防止未经审查触发镜像发布）。审阅无误后请运行：git push origin main 或加 -PushMain 参数"
}

# 3. rebase dev 分支
if ($SkipDev) {
    Write-Warn "已跳过 dev 分支 rebase (-SkipDev)"
    exit 0
}

$devExists = git show-ref --verify --quiet refs/heads/dev
if (-not $devExists) {
    Write-Warn "未找到 dev 分支，跳过 rebase"
    exit 0
}

Write-Step "checkout dev & rebase main"
git checkout dev
if ($LASTEXITCODE -ne 0) { Die "切换 dev 失败" }

git rebase main
if ($LASTEXITCODE -ne 0) {
    Write-Warn "rebase 出现冲突！请按以下步骤处理后继续："
    Write-Host "    1. 手动解决冲突文件"
    Write-Host "    2. git add <已解决的文件>"
    Write-Host "    3. git rebase --continue"
    Write-Host "    4. 重新运行本脚本（或 git push origin dev --force-with-lease）"
    Die "rebase 未完成"
}
Write-Ok "rebase 完成"

Write-Step "push dev -> origin (--force-with-lease)"
git push origin dev --force-with-lease
if ($LASTEXITCODE -ne 0) { Die "push origin dev 失败" }
Write-Ok "dev 已推送"

Write-Host "`n========================================" -ForegroundColor Green
Write-Ok "同步完成！当前在 dev 分支，已基于最新 upstream/main"
Write-Host "========================================`n" -ForegroundColor Green
