import os

from e2b import ConnectionConfig
from e2b.sandbox.main import SandboxBase
from e2b_code_interpreter.code_interpreter_sync import Sandbox as SandboxSync
from e2b_code_interpreter.code_interpreter_sync import JUPYTER_PORT


TRAFFIC_ACCESS_TOKEN_HEADER = "E2B-Traffic-Access-Token"
_sandbox_base_init = SandboxBase.__init__


def __sandbox_base_init(self, *args, **kwargs):
    _sandbox_base_init(self, *args, **kwargs)

    traffic_access_token = getattr(self, "traffic_access_token", None)
    if not traffic_access_token:
        return

    extra_headers = getattr(
        self.connection_config,
        "_ConnectionConfig__extra_sandbox_headers",
        None,
    )
    if not isinstance(extra_headers, dict):
        raise RuntimeError(
            "installed e2b SDK does not expose mutable extra sandbox headers"
        )
    extra_headers[TRAFFIC_ACCESS_TOKEN_HEADER] = traffic_access_token


def __sandbox_get_host(self, port: int) -> str:
    return f"{self.sandbox_domain}/kruise/{self.sandbox_id}/{port}"


def __connection_config_get_host(_, sandbox_id: str, sandbox_domain: str, port: int) -> str:
    return f"{sandbox_domain}/kruise/{sandbox_id}/{port}"


def __get_api_url(https: bool):
    return f"{'https' if https else 'http'}://{os.environ['E2B_DOMAIN']}/kruise/api"


def __connection_config_get_sandbox_url_http(self, sandbox_id: str, sandbox_domain: str) -> str:
    return f"http://{__connection_config_get_host(self, sandbox_id, sandbox_domain, ConnectionConfig.envd_port)}"


def __jupyter_url_http(self) -> str:
    return f"http://{__sandbox_get_host(self, JUPYTER_PORT)}"


def patch_e2b(https: bool = True, validate_key: bool = True):
    """
    patch e2b sdk to use kruise private protocol
    :param https: Use https to connect to sandbox-manager
    :param validate_key: Set to false to disable api key validation. Only works for e2b>=2.25.0, other versions may cause an error
    :return: None
    """
    os.environ["E2B_API_URL"] = __get_api_url(https)
    SandboxBase.__init__ = __sandbox_base_init
    SandboxBase.get_host = __sandbox_get_host
    ConnectionConfig.get_host = __connection_config_get_host
    if not https:
        ConnectionConfig.get_sandbox_url = __connection_config_get_sandbox_url_http
        setattr(SandboxSync, '_jupyter_url', property(__jupyter_url_http))
    if not validate_key:
        try:
            import e2b.api as e2b_api
        except ImportError as exc:
            raise RuntimeError(
                "validate_key=False requires an e2b version exposing e2b.api.validate_api_key"
            ) from exc
        e2b_api.validate_api_key = lambda _api_key: None
