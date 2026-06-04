import hashlib

# Constants must stay in sync with pkg/servers/e2b/keys/compat.go.
_E2B_SDK_PREFIX = "e2b_"
_E2B_SDK_COMPAT_MAGIC = "6f6b6167"
_E2B_SDK_COMPAT_VERSION = "01"
_E2B_SDK_COMPAT_CHECKSUM_SALT = "openkruise-agents/e2b-key-compat/v1"


def _e2b_sdk_compat_checksum(raw_bytes: bytes) -> str:
    payload = _E2B_SDK_COMPAT_CHECKSUM_SALT.encode("utf-8") + raw_bytes
    digest = hashlib.sha256(payload).digest()
    return digest[:8].hex()


def encode_for_e2b_sdk(raw: str) -> str:
    """Wrap a raw OpenKruise Agents API key in an E2B SDK-compatible form.

    This is the Python counterpart to ``EncodeForE2BSDK`` in
    ``pkg/servers/e2b/keys/compat.go``. Callers must pass a regular API key
    (UUID or admin key, well under 4 GB); inputs larger than 4 GB would
    overflow the 8-hex-character length field and are not supported.
    """
    raw_bytes = raw.encode("utf-8")
    return "{prefix}{magic}{version}{length:08x}{body}{checksum}".format(
        prefix=_E2B_SDK_PREFIX,
        magic=_E2B_SDK_COMPAT_MAGIC,
        version=_E2B_SDK_COMPAT_VERSION,
        length=len(raw_bytes),
        body=raw_bytes.hex(),
        checksum=_e2b_sdk_compat_checksum(raw_bytes),
    )
