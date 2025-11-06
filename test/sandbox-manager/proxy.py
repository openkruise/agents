from mitmproxy import http


def request(flow: http.HTTPFlow) -> None:
    original_host = flow.request.headers.get("Host")
    print(f"URL:     {flow.request.url}")
    print(f"Headers: {flow.request.headers}")
    flow.request.host = "localhost"
    flow.request.port = 8080
    flow.request.scheme = "http"
    # 保留原始 Host 信息
    flow.request.headers["X-Original-Host"] = original_host
    flow.request.headers["X-Forwarded-Host"] = original_host
