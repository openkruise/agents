import ssl

import httpx

# 强制 patch httpx 客户端
def disable_ssl_verification():
    # 彻底禁用 SSL 验证
    try:
        _create_unverified_https_context = ssl._create_unverified_context
    except AttributeError:
        # Legacy Python that doesn't verify HTTPS certificates by default
        pass
    else:
        # Handle target environment that doesn't support HTTPS verification
        ssl._create_default_https_context = _create_unverified_https_context
    # Patch HTTPTransport
    original_http_transport_init = httpx.HTTPTransport.__init__

    def patched_http_transport_init(self, *args, **kwargs):
        kwargs['verify'] = False
        return original_http_transport_init(self, *args, **kwargs)

    httpx.HTTPTransport.__init__ = patched_http_transport_init

    # Patch AsyncHTTPTransport
    original_async_http_transport_init = httpx.AsyncHTTPTransport.__init__

    def patched_async_http_transport_init(self, *args, **kwargs):
        kwargs['verify'] = False
        return original_async_http_transport_init(self, *args, **kwargs)

    httpx.AsyncHTTPTransport.__init__ = patched_async_http_transport_init

    # Patch Client
    original_client_init = httpx.Client.__init__

    def patched_client_init(self, *args, **kwargs):
        kwargs['verify'] = False
        return original_client_init(self, *args, **kwargs)

    httpx.Client.__init__ = patched_client_init

    # Patch AsyncClient
    original_async_client_init = httpx.AsyncClient.__init__

    def patched_async_client_init(self, *args, **kwargs):
        kwargs['verify'] = False
        return original_async_client_init(self, *args, **kwargs)

    httpx.AsyncClient.__init__ = patched_async_client_init
