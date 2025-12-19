import os
from typing import Optional, Dict, Unpack, Self

from e2b.connection_config import ApiParams
from e2b.sandbox.sandbox_api import McpServer, SandboxNetworkOpts
from e2b_code_interpreter import Sandbox as E2bSandbox
from e2b.sandbox.utils import class_method_variant


class Sandbox(E2bSandbox):
    def get_host(self, port: int) -> str:
        return f"sandbox.{self.sandbox_domain}/kruise/{self.sandbox_id}/{port}"

    @classmethod
    def create(
            cls,
            template: Optional[str] = None,
            timeout: Optional[int] = None,
            metadata: Optional[Dict[str, str]] = None,
            envs: Optional[Dict[str, str]] = None,
            secure: bool = True,
            allow_internet_access: bool = True,
            mcp: Optional[McpServer] = None,
            network: Optional[SandboxNetworkOpts] = None,
            https: bool = True,
            **opts: Unpack[ApiParams],
    ) -> Self:
        sbx: E2bSandbox = super().create(
            template=template,
            timeout=timeout,
            metadata=metadata,
            envs=envs,
            secure=secure,
            allow_internet_access=allow_internet_access,
            mcp=mcp,
            network=network,
            api_url=get_api_url(https),
            **opts,
        )
        if not https:
            sbx.connection_config.debug = True
        return sbx

    @class_method_variant("_cls_connect")
    def connect(
            self,
            timeout: Optional[int] = None,
            https: bool = True,
            **opts: Unpack[ApiParams],
    ) -> Self:
        return super().connect(
            timeout,
            api_url=get_api_url(https),
            **opts,
        )

    @classmethod
    def _cls_connect(
            cls,
            sandbox_id: str,
            timeout: Optional[int] = None,
            https: bool = True,
            **opts: Unpack[ApiParams],
    ) -> Self:
        return super()._cls_connect(sandbox_id, timeout, api_url=get_api_url(https), **opts)


def get_api_url(https: bool):
    return f"{'https' if https else 'http'}://{os.environ['E2B_DOMAIN']}/kruise/api"
