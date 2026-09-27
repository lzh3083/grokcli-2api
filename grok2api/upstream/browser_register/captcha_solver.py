"""YesCaptcha 打码平台客户端（Cloudflare Turnstile 协议接口）。

背景
----
住宅 IP 下注册时，xAI 的登录/注册页可能弹出 Cloudflare Turnstile。
现有流程只是**被动等待**浏览器自己通过（见 cpa_xai/browser_confirm.py
的 _wait_turnstile）；遇到必须交互的 Turnstile 就会卡到超时。

本模块把 Turnstile 交给 YesCaptcha 云端解题，拿回 token 后注入页面。

两个关键细节（容易踩坑）
------------------------
1. **token 与解题时的 UA 绑定**。YesCaptcha 在 solution 里回传
   userAgent，注入 token 的浏览器必须用同一个 UA，否则校验不过。
2. **token 一次性、有效期约 120 秒**。拿到后要尽快注入，不要缓存复用。

注意：本模块只解决「页面上出现 Turnstile」的情况。若整站被 Cloudflare
托管挑战（Just a moment...）挡住，页面根本加载不出来、拿不到 sitekey，
打码平台也无能为力 —— 那属于 IP 信誉问题。
"""
from __future__ import annotations

import os
import json
import time

DEFAULT_API_BASE = "https://api.yescaptcha.com"
CREATE_PATH = "/createTask"
RESULT_PATH = "/getTaskResult"
# 官方文档：轮询间隔 3 秒；10~80 秒出结果
POLL_INTERVAL_SEC = 3.0
DEFAULT_TIMEOUT_SEC = 120.0
TASK_TYPE = "TurnstileTaskProxyless"


class CaptchaError(RuntimeError):
    """打码失败。message 已是可读文本，调用方直接记录即可。"""


class YesCaptchaClient:
    """YesCaptcha 客户端。

    http_post: 可选注入的 HTTP 客户端（签名与项目一致：
        http_post(url, json=..., timeout=...) -> 带 .status_code / .text 的响应）。
        不传则回退到 curl_cffi。
    """

    def __init__(self, client_key, api_base="", http_post=None, log=None,
                 timeout_sec=DEFAULT_TIMEOUT_SEC, sleep=None):
        key = str(client_key or "").strip()
        if not key:
            raise CaptchaError("YesCaptcha clientKey 未配置")
        self.client_key = key
        base = str(api_base or "").strip() or DEFAULT_API_BASE
        self.api_base = base.rstrip("/")
        self.http_post = http_post
        self._log = log or (lambda _m: None)
        self.timeout_sec = max(10.0, float(timeout_sec or DEFAULT_TIMEOUT_SEC))
        self._sleep = sleep or time.sleep

    # ---- HTTP ----
    def _post(self, path, payload):
        url = self.api_base + path
        body = dict(payload)
        body["clientKey"] = self.client_key
        text = ""
        if self.http_post is not None:
            try:
                response = self.http_post(url, json=body, timeout=30)
            except Exception as exc:
                raise CaptchaError("请求打码平台失败: %s" % exc)
            status = getattr(response, "status_code", 200)
            text = getattr(response, "text", "") or ""
            if status != 200:
                raise CaptchaError("打码平台返回 HTTP %s" % status)
        else:
            try:
                from curl_cffi import requests as curl_requests
            except Exception as exc:  # pragma: no cover - 依赖缺失
                raise CaptchaError("缺少 curl_cffi，无法请求打码平台: %s" % exc)
            try:
                response = curl_requests.post(url, json=body, timeout=30)
            except Exception as exc:
                raise CaptchaError("请求打码平台失败: %s" % exc)
            text = getattr(response, "text", "") or ""
            if getattr(response, "status_code", 200) != 200:
                raise CaptchaError("打码平台返回 HTTP %s" % getattr(response, "status_code", "?"))
        try:
            data = json.loads(text)
        except Exception:
            raise CaptchaError("打码平台响应无法解析: %s" % text[:160])
        if not isinstance(data, dict):
            raise CaptchaError("打码平台响应格式异常")
        return data

    # ---- 主流程 ----
    def solve_turnstile(self, website_url, website_key, timeout_sec=None):
        """解 Turnstile，返回 {"token": str, "user_agent": str}。

        失败抛 CaptchaError。任何情况下都不会返回空 token。
        """
        url = str(website_url or "").strip()
        key = str(website_key or "").strip()
        if not url or not key:
            raise CaptchaError("缺少 websiteURL 或 websiteKey")
        budget = max(10.0, float(timeout_sec or self.timeout_sec))

        created = self._post(CREATE_PATH, {
            "task": {"type": TASK_TYPE, "websiteURL": url, "websiteKey": key}
        })
        if int(created.get("errorId") or 0) != 0:
            raise CaptchaError("创建任务失败: %s" % _describe(created))
        task_id = str(created.get("taskId") or "").strip()
        if not task_id:
            raise CaptchaError("创建任务未返回 taskId")
        self._log("Turnstile 任务已创建: %s" % task_id)

        deadline = time.time() + budget
        attempt = 0
        while time.time() < deadline:
            self._sleep(POLL_INTERVAL_SEC)
            attempt += 1
            result = self._post(RESULT_PATH, {"taskId": task_id})
            if int(result.get("errorId") or 0) != 0:
                raise CaptchaError("取结果失败: %s" % _describe(result))
            status = str(result.get("status") or "").lower()
            if status == "ready":
                solution = result.get("solution") or {}
                token = str(solution.get("token") or "").strip()
                if not token:
                    raise CaptchaError("任务完成但未返回 token")
                user_agent = str(solution.get("userAgent") or "").strip()
                self._log("Turnstile 已解出（第 %d 次轮询，token %d 字符）" % (attempt, len(token)))
                return {"token": token, "user_agent": user_agent}
            # processing：继续等
        raise CaptchaError("Turnstile 解题超时（%.0f 秒）" % budget)

    def balance(self):
        """查询余额（点数）。失败抛 CaptchaError。"""
        data = self._post("/getBalance", {})
        if int(data.get("errorId") or 0) != 0:
            raise CaptchaError("查询余额失败: %s" % _describe(data))
        return data.get("balance")


def _describe(payload):
    code = payload.get("errorCode")
    desc = payload.get("errorDescription")
    parts = [str(p) for p in (code, desc) if p]
    return " / ".join(parts) or "未知错误"


def extract_turnstile_key(page):
    """从页面提取 Turnstile 的 sitekey。

    依次尝试：显式 data-sitekey 属性、cf-turnstile-response 输入框所在的
    表单、以及页面脚本里的 turnstile.render 调用。
    """
    probes = (
        "const el=document.querySelector('[data-sitekey]');return el?el.getAttribute('data-sitekey'):'';",
        "const el=document.querySelector('.cf-turnstile');return el?el.getAttribute('data-sitekey'):'';",
        # 隐式渲染时 sitekey 只出现在 iframe 的 src 路径里，形如
        # https://challenges.cloudflare.com/.../0x4AAAAAAAhr9JGVDZbrZOo0/light/normal
        # 这是 x.ai 注册页的实际情况 —— 只查 DOM 属性会全部落空。
        ("const f=document.querySelector('iframe[src*=\"turnstile\"],iframe[src*=\"challenges.cloudflare.com\"]');"
         "if(!f)return '';const m=String(f.src||'').match(/(0x[0-9A-Za-z_-]{10,})/);return m?m[1]:'';"),
        # 兜底：任何 iframe 的 src 里出现 0x 开头的 sitekey
        ("const fs=document.querySelectorAll('iframe');"
         "for(const f of fs){const m=String(f.src||'').match(/(0x[0-9A-Za-z_-]{10,})/);if(m)return m[1];}return '';"),
        # Turnstile 渲染时若挂了全局回调，参数里会带 sitekey
        ("try{if(window.turnstile&&window.turnstile._widgets)return '';}catch(e){}return '';"),
        "const m=document.documentElement.innerHTML.match(/data-sitekey=[\"']([^\"']+)[\"']/);return m?m[1]:'';",
        ("const m=document.documentElement.innerHTML.match(/(0x4[A-Za-z0-9_-]{10,})/);return m?m[1]:'';"),
        "const m=document.documentElement.innerHTML.match(/sitekey[\"']?\\s*[:=]\\s*[\"']([0-9A-Za-z_-]{10,})[\"']/);return m?m[1]:'';",
    )
    for script in probes:
        try:
            value = page.run_js(script)
        except Exception:
            continue
        text = str(value or "").strip()
        if text:
            return text
    return ""


def inject_turnstile_token(page, token):
    """把 token 写进 cf-turnstile-response 并触发回调。

    Turnstile 的校验既看表单字段，也依赖渲染时的回调；只填 input 有时
    不会被识别，所以这里同时尝试调用常见的全局回调。
    """
    script = """
    const token = arguments[0];
    let filled = 0;
    document.querySelectorAll('input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"]').forEach(el => {
        el.value = token;
        el.dispatchEvent(new Event('input', { bubbles: true }));
        el.dispatchEvent(new Event('change', { bubbles: true }));
        filled += 1;
    });
    let called = 0;
    for (const key of Object.keys(window)) {
        try {
            if (typeof window[key] === 'function' && /turnstile|onSuccess|callback/i.test(key)) {
                window[key](token);
                called += 1;
            }
        } catch (_) {}
    }
    return JSON.stringify({ filled, called });
    """
    try:
        raw = page.run_js(script, token)
    except Exception as exc:
        return {"filled": 0, "called": 0, "error": str(exc)[:120]}
    try:
        data = json.loads(raw) if isinstance(raw, str) else (raw or {})
    except Exception:
        data = {"filled": 0, "called": 0}
    return data if isinstance(data, dict) else {"filled": 0, "called": 0}


def is_local_endpoint(api_base):
    """端点是否指向本机（本地 solver 通常不校验 clientKey）。"""
    text = str(api_base or "").strip().lower()
    if not text:
        return False
    for marker in ("127.0.0.1", "localhost", "0.0.0.0", "[::1]"):
        if marker in text:
            return True
    return False


def settings_from_config():
    """从 app_config 读打码配置。未启用或缺 key 时返回 None。

    任何异常都当作「未启用」—— 打码是可选增强，不能因为读配置失败
    就把注册主流程带崩。CPA 流程与注册流程共用这一份读取逻辑。
    """
    try:
        import app_config
        cfg = getattr(app_config, "config", None)
        if not isinstance(cfg, dict):
            return None
        if not cfg.get("captcha_solver_enabled"):
            return None
        key = str(cfg.get("captcha_solver_api_key") or "").strip()
        base = str(cfg.get("captcha_solver_api_base") or "").strip()
        # 本地 solver 默认不校验 clientKey（未设 API_KEY 时），所以本机端点
        # 允许留空，免得用户为了过校验去填一个无意义的占位串。
        if not key and not is_local_endpoint(base):
            return None
        return {
            "api_key": key or "local",
            "api_base": base,
            "timeout_sec": float(cfg.get("captcha_solver_timeout_sec") or 120),
            # 留空则从页面自动提取；提取不到时可用它兜底。
            "sitekey": str(cfg.get("captcha_solver_sitekey") or "").strip(),
        }
    except Exception:
        return None


def auto_wait_sec(default=10.0):
    """先给 Cloudflare 多少秒自动完成的机会，之后才交给打码。

    自动完成只要 5 秒左右，过早打码是浪费（本地 solver 也要占浏览器）。
    读不到配置就用默认值。
    """
    try:
        import app_config
        cfg = getattr(app_config, "config", None)
        if isinstance(cfg, dict):
            # 注意不能用 `raw or default`：0 是合法值（表示立即打码），
            # 但它是 falsy，会被静默换成默认值 —— 这个坑真踩过。
            raw = cfg.get("captcha_solver_auto_wait_sec")
            value = float(default if raw is None else raw)
            return max(0.0, min(value, 120.0))
    except Exception:
        pass
    return float(default)


def solve_and_inject(page, client_key, log=None, api_base="", timeout_sec=None,
                     http_post=None, url="", sitekey=""):
    """一站式：检测 Turnstile → 提取 sitekey → 解题 → 注入。

    返回 dict：{"ok": bool, "reason": str, "token_len": int, "ua_matched": bool}
    永不抛异常 —— 打码失败不应中断注册，交由调用方决定是否重试。
    """
    logger = log or (lambda _m: None)
    key = str(sitekey or "").strip()
    if key:
        logger("使用配置指定的 Turnstile sitekey: %s" % key)
    else:
        try:
            key = extract_turnstile_key(page)
        except Exception as exc:
            return {"ok": False, "reason": "提取 sitekey 失败: %s" % str(exc)[:100],
                    "token_len": 0, "ua_matched": False}
    if not key:
        return {"ok": False, "reason": "页面未找到 Turnstile sitekey",
                "token_len": 0, "ua_matched": False}
    page_url = str(url or "").strip()
    if not page_url:
        try:
            page_url = str(getattr(page, "url", "") or "")
        except Exception:
            page_url = ""
    try:
        client = YesCaptchaClient(client_key, api_base=api_base,
                                  http_post=http_post, log=logger, timeout_sec=timeout_sec)
        solved = client.solve_turnstile(page_url, key, timeout_sec=timeout_sec)
    except CaptchaError as exc:
        return {"ok": False, "reason": str(exc)[:160], "token_len": 0, "ua_matched": False}

    token = solved["token"]
    ua_matched = True
    expected_ua = solved.get("user_agent") or ""
    if expected_ua:
        try:
            actual = str(page.run_js("return navigator.userAgent;") or "")
            ua_matched = actual.strip() == expected_ua.strip()
        except Exception:
            ua_matched = False
        if not ua_matched:
            # token 与解题 UA 绑定，UA 不一致时注入大概率无效，提前告知
            logger("Turnstile UA 不一致：解题=%s 浏览器=%s" % (expected_ua[:50], actual[:50]))

    result = inject_turnstile_token(page, token)
    ok = int(result.get("filled") or 0) > 0 or int(result.get("called") or 0) > 0
    return {
        "ok": ok,
        "reason": "" if ok else "token 已取得但未找到可注入的目标元素",
        "token_len": len(token),
        "ua_matched": ua_matched,
    }

def current_registration_proxy():
    """取当前线程正在使用的注册代理 URL，取不到返回空串。"""
    try:
        import proxy_pool_v3
        url = proxy_pool_v3.current_proxy_url()
        if url:
            return str(url).strip()
    except Exception:
        pass
    try:
        import browser_runtime
        return str(browser_runtime.get_configured_proxy() or "").strip()
    except Exception:
        return ""


def solver_proxy_line(proxy_url):
    """把注册侧的代理 URL 转成 solver 的 proxies.txt 行格式。

    Playwright/Camoufox 认 socks5://，不认 socks5h://（h 只是 curl 的
    本地 DNS 解析标记），这里统一降级。
    """
    url = str(proxy_url or "").strip()
    if not url:
        return ""
    if url.startswith("socks5h://"):
        return "socks5://" + url[len("socks5h://"):]
    return url


def sync_solver_proxy(log=None, path=""):
    """把当前注册代理写进 solver 的 proxies.txt。

    Cloudflare 会核对 Turnstile token 与提交请求是否来自同一出口 IP：
    实测 solver 走 NAS 本机 IP、注册走住宅代理时，token 能注入、页面也
    显示验证完成，但服务端拒绝，最终页卡在反复重试且拿不到 sso cookie；
    让 solver 走同一个住宅代理后立刻成功。所以每次打码前必须同步代理。

    注意：solver 用的是 random.choice(proxies)，因此这里清空后只写一行，
    保证它别无选择。多线程并发时该文件会互相覆盖，故仅在单线程注册下
    可靠。
    """
    logger = log or (lambda _m: None)
    target = str(path or "").strip() or default_proxies_file()
    proxy = current_registration_proxy()
    if not proxy:
        # 直连场景：清空文件，避免 solver 继续用上一批的旧代理。
        try:
            if target and os.path.exists(target):
                with open(target, "w", encoding="utf-8") as handle:
                    handle.write("")
        except Exception:
            pass
        return ""

    line = solver_proxy_line(proxy)
    if not target:
        return line
    try:
        directory = os.path.dirname(target)
        if directory:
            os.makedirs(directory, exist_ok=True)
        tmp = target + ".tmp"
        with open(tmp, "w", encoding="utf-8") as handle:
            handle.write(line + "\n")
        os.replace(tmp, target)
        logger("已同步打码代理: %s" % line)
    except Exception as exc:
        logger("同步打码代理失败（不影响注册）: %s" % str(exc)[:80])
    return line


def default_proxies_file():
    """solver 的 proxies.txt 默认位置。"""
    try:
        import app_config
        cfg = app_config.config if isinstance(app_config.config, dict) else {}
        configured = str(cfg.get("captcha_solver_proxies_file") or "").strip()
        if configured:
            return configured
    except Exception:
        pass
    here = os.path.dirname(os.path.abspath(__file__))
    return os.path.join(here, "turnstile-solver", "proxies.txt")
