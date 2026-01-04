import os

from e2b import ConnectionConfig
from e2b.sandbox.main import SandboxBase


def __sandbox_get_host(self, port: int) -> str:
    return f"{self.sandbox_domain}/kruise/{self.sandbox_id}/{port}"


def __connection_config_get_host(_, sandbox_id: str, sandbox_domain: str, port: int) -> str:
    return f"{sandbox_domain}/kruise/{sandbox_id}/{port}"


def __get_api_url(https: bool):
    return f"{'https' if https else 'http'}://{os.environ['E2B_DOMAIN']}/kruise/api"


def patch_e2b(https: bool = True):
    os.environ["E2B_API_URL"] = __get_api_url(https)
    if not https:
        os.environ["E2B_DEBUG"] = "true"
    SandboxBase.get_host = __sandbox_get_host
    ConnectionConfig.get_host = __connection_config_get_host
