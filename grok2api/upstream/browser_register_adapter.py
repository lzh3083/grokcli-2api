"""Browser registration adapter bridging grok-register engine with grokcli-2api.

Executes real-browser registration (Chromium via DrissionPage + Camoufox Solver),
extracts the resulting SSO cookie, and passes it back to grok2api for OIDC Device
Flow token conversion and PostgreSQL persistence.
"""
from __future__ import annotations

import os
import sys
import time
from pathlib import Path
from typing import Any, Callable

# Ensure browser_register package is in sys.path
_PKG_DIR = Path(__file__).resolve().parent / "browser_register"
if str(_PKG_DIR) not in sys.path:
    sys.path.insert(0, str(_PKG_DIR))

from grok2api.upstream.browser_register import (
    app_config,
    browser_runtime,
    captcha_solver,
    mail_service,
    registration_browser,
    sso_risk,
    us_consistency,
)
from curl_cffi import requests


def _preflight_registration_path(proxy_url: str = "", log_callback: Callable[[str], None] = None) -> bool:
    """非破坏性预检 accounts.x.ai / grok.com 的连通性与 Cloudflare 状态。"""
    targets = ("https://accounts.x.ai/", "https://grok.com/")
    req_proxies = {"http": proxy_url, "https": proxy_url} if proxy_url else None
    for url in targets:
        try:
            started = time.monotonic()
            resp = requests.get(
                url,
                proxies=req_proxies,
                impersonate="chrome124",
                timeout=15,
                allow_redirects=False,
            )
            latency = int((time.monotonic() - started) * 1000)
            status_code = int(resp.status_code)
            headers = {str(k).lower(): str(v).lower() for k, v in dict(getattr(resp, "headers", {}) or {}).items()}
            text = str(getattr(resp, "text", "") or "")[:4096].lower()
            cf_blocked = ("cloudflare" in headers.get("server", "") or "cf-error" in text) and status_code in (403, 429, 503)
            if cf_blocked:
                if log_callback:
                    log_callback(f"[!] 路径预检告警: {url} 遭遇 Cloudflare 阻断 (HTTP {status_code})")
                return False
            if log_callback:
                log_callback(f"[+] 路径预检正常: {url} (HTTP {status_code}, 延迟 {latency}ms)")
        except Exception as exc:
            if log_callback:
                log_callback(f"[!] 路径预检异常: {url} 连接失败: {exc}")
            return False
    return True


def _detect_chromium_path() -> str:
    candidates = [
        os.environ.get("GROK_BROWSER_PATH", ""),
        os.environ.get("CHROME_PATH", ""),
        "/usr/bin/chromium",
        "/usr/bin/chromium-browser",
        "/usr/bin/google-chrome",
        "/usr/bin/chrome",
    ]
    for c in candidates:
        if c and os.path.isfile(c) and os.access(c, os.X_OK):
            return c
    return ""


def run_browser_registration(
    *,
    sid: str,
    sess: dict[str, Any],
    update: Callable[[str, str], None],
    check_cancel: Callable[[], None],
    proxy: str = "",
    receiver: Any = None,
) -> dict[str, Any]:
    """Execute one browser registration flow and return credentials dict."""
    check_cancel()
    update("registering", "preparing browser environment")

    # 1. Load active config
    cfg = app_config.load_config()

    # Overlay user preferences from session/web form
    for k in (
        "proxy_pool_preflight_enabled",
        "us_consistency_enabled",
        "us_consistency_timezone",
        "us_consistency_locale",
        "enable_nsfw",
        "sso_risk_gate_enabled",
        "sso_risk_rejected_file",
    ):
        if k in sess and sess[k] is not None:
            cfg[k] = sess[k]

    # Detect browser executable
    chrome_bin = _detect_chromium_path()
    if chrome_bin:
        cfg["browser_path"] = chrome_bin
    else:
        # Let DrissionPage auto-detect if not found in standard paths
        cfg["browser_path"] = cfg.get("browser_path") or ""

    # 2. Configure proxy mode
    active_proxy = str(proxy or sess.get("proxy") or "").strip()
    is_direct = not active_proxy or active_proxy.lower() in ("direct", "none", "off", "0")
    if is_direct:
        cfg["proxy_mode"] = "direct"
        cfg["proxy"] = ""
        # Direct mode: clear proxies.txt so Camoufox solver also runs direct
        solver_proxy_file = captcha_solver.default_proxies_file()
        if solver_proxy_file:
            try:
                Path(solver_proxy_file).write_text("", encoding="utf-8")
            except Exception:
                pass
    else:
        cfg["proxy_mode"] = "single"
        cfg["proxy"] = active_proxy
        # Proxy mode: sync solver proxies.txt to exit via identical IP
        try:
            captcha_solver.sync_solver_proxy(
                proxy=active_proxy,
                proxies_file=cfg.get("captcha_solver_proxies_file"),
                log=lambda m: print(f"[{sid}] {m}"),
            )
        except Exception as exc:
            print(f"[{sid}] solver proxy sync warning: {exc}")

    # 3. Configure local Turnstile solver
    local_solver_url = (
        os.environ.get("GROK2API_LOCAL_SOLVER_URL")
        or os.environ.get("LOCAL_SOLVER_URL")
        or "http://127.0.0.1:5072"
    ).rstrip("/")
    cfg["captcha_solver_enabled"] = True
    cfg["captcha_solver_provider"] = "yescaptcha"
    cfg["captcha_solver_api_base"] = local_solver_url

    # 4. Bind runtime configuration
    registration_browser.bind_runtime(cfg)
    browser_runtime.configure_runtime(cfg)
    mail_service.bind_runtime(cfg)
    captcha_solver.bind_runtime(cfg)
    sso_risk.configure_risk_runtime(cfg, browser_runtime.http_get)

    # 5. Define status update callbacks
    def _log_cb(msg: str) -> None:
        check_cancel()
        text = str(msg or "").strip()
        print(f"[{sid}] {text}")
        # Map browser stages to grok2api UI phases
        if "打开注册页" in text or "visiting" in text.lower():
            update("registering", text)
        elif "验证码" in text or "邮箱" in text:
            update("registering", text)
        elif "Turnstile" in text or "打码" in text or "过盾" in text:
            update("solving_turnstile", text)
        elif "资料" in text or "profile" in text.lower():
            update("registering", text)
        elif "sso" in text.lower():
            update("registering", text)
        else:
            update("registering", text)

    def _cancel_cb() -> None:
        check_cancel()

    # 6. Run the browser registration steps
    email = ""
    dev_token = ""
    sso = ""
    profile: dict[str, Any] = {}
    try:
        # A. 执行注册路径非破坏性预检
        if cfg.get("proxy_pool_preflight_enabled", True):
            _log_cb("[*] 正在执行注册路径预检 (accounts.x.ai / grok.com)...")
            preflight_ok = _preflight_registration_path(proxy_url=active_proxy, log_callback=_log_cb)
            if not preflight_ok:
                raise RuntimeError("注册路径预检失败：目标站点不可达或遭遇 Cloudflare 阻断，已终止本次尝试")

        # B. 出口国家与网络环境自适应对齐（多国支持）
        if us_consistency.enabled():
            expect_c = str(cfg.get("us_consistency_expect_country") or "").strip()
            detected_zone = us_consistency.align_timezone_with_proxy(active_proxy, expect_country=expect_c)
            if detected_zone:
                _log_cb(f"[*] {us_consistency.describe()}")

        _log_cb("[*] 正在启动 Chromium 浏览器实例...")
        use_proxy = not is_direct
        registration_browser.start_browser(log_callback=_log_cb, use_proxy=use_proxy)
        check_cancel()

        _log_cb("[*] 打开 xAI 注册页面: https://accounts.x.ai/sign-up?redirect=grok-com")
        registration_browser.open_signup_page(log_callback=_log_cb, cancel_callback=_cancel_cb)
        check_cancel()

        _log_cb("[*] 创建邮箱并提交注册表单...")
        email, dev_token = registration_browser.fill_email_and_submit(
            timeout=45,
            log_callback=_log_cb,
            cancel_callback=_cancel_cb,
        )
        sess["email"] = email
        _log_cb(f"[+] 邮箱已提交: {email}")
        check_cancel()

        _log_cb("[*] 正在等待接收并提交验证码...")
        code = registration_browser.fill_code_and_submit(
            email=email,
            dev_token=dev_token,
            timeout=180,
            log_callback=_log_cb,
            cancel_callback=_cancel_cb,
        )
        _log_cb("[+] 验证码验证成功")
        check_cancel()

        _log_cb("[*] 填写个人资料并提交...")
        profile = registration_browser.fill_profile_and_submit(
            timeout=120,
            log_callback=_log_cb,
            cancel_callback=_cancel_cb,
        )
        _log_cb(f"[+] 资料提交完成: {profile.get('given_name', '')} {profile.get('family_name', '')}")
        check_cancel()

        _log_cb("[*] 正在提取 SSO Cookie...")
        sso = registration_browser.wait_for_sso_cookie(
            timeout=120,
            log_callback=_log_cb,
            cancel_callback=_cancel_cb,
        )
        if not sso:
            raise RuntimeError("未能从浏览器会话中提取到 sso cookie")
        _log_cb(f"[+] 成功获取 SSO Cookie (长度={len(sso)})")

        # C. SSO 风控早停检查 (读取 grok.com botFlagSource / policy=deny)
        if cfg.get("sso_risk_gate_enabled", True):
            _log_cb("[*] 正在执行 SSO 风控早停安全检查 (botFlagSource / policy)...")
            sso_risk.ensure_sso_eligible(
                raw_token=sso,
                email=email,
                proxy=active_proxy,
                log_callback=_log_cb,
                http_get=browser_runtime.http_get,
            )

        # D. 开启 NSFW 敏感内容权限 (后处理开关)
        if cfg.get("enable_nsfw", True):
            _log_cb("[*] 正在开启账号 NSFW 权限 (gRPC-web)...")
            try:
                nsfw_ok, nsfw_msg = registration_browser.enable_nsfw_for_token(sso, log_callback=_log_cb)
                if nsfw_ok:
                    _log_cb(f"[+] NSFW 权限开启成功: {nsfw_msg}")
                else:
                    _log_cb(f"[!] NSFW 权限开启失败 (不影响账号正常入库): {nsfw_msg}")
            except Exception as nsfw_exc:
                _log_cb(f"[!] NSFW 开启异常: {nsfw_exc}")

    finally:
        # Always reclaim browser process and memory after each registration
        try:
            registration_browser.cleanup_runtime_memory(
                log_callback=_log_cb,
                reason="单次注册结束",
            )
        except Exception as clean_exc:
            print(f"[{sid}] cleanup error: {clean_exc}")

    password = str(profile.get("password") or sess.get("password") or "")
    return {
        "ok": True,
        "sso": sso,
        "email": email,
        "password": password,
        "profile": profile,
        "session_cookies": {
            "sso": sso,
            "sso-rw": sso,
        },
    }
