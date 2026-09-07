"""Read-only GHCR manifest resolution. HTTP/auth failures never mean an absent tag."""
import base64
import hashlib
import json
import os
import re
from urllib.error import HTTPError
from urllib.parse import quote, urlencode
from urllib.request import HTTPRedirectHandler, Request, build_opener

DIGEST_PATTERN = re.compile(r"sha256:[0-9a-f]{64}")
MEDIA_TYPES = (
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
)


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # Credentials are only sent to the fixed GHCR token endpoint.
        raise HTTPError(req.full_url, code, "Unexpected registry redirect", headers, fp)


OPENER = build_opener(NoRedirect)


def image_for_repo(repo):
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9_.-]+", repo):
        raise ValueError("Invalid repository identity")
    if repo.split('/')[1] in ('.', '..'):
        raise ValueError("Invalid repository identity")
    return "ghcr.io/" + repo.lower()


def require_digest(value):
    if not isinstance(value, str) or not DIGEST_PATTERN.fullmatch(value):
        raise ValueError("Invalid image digest")
    return value


def registry_digest(image, reference, *, allow_missing=False):
    if image != image_for_repo(os.environ["GITHUB_REPOSITORY"]):
        raise ValueError("Registry image does not match repository identity")
    if not re.fullmatch(r"[A-Za-z0-9_.:-]+", reference):
        raise ValueError("Invalid registry reference")
    repository = image.removeprefix("ghcr.io/")
    headers = {}
    if os.environ.get("GH_TOKEN"):
        credential = os.environ.get("GITHUB_ACTOR", "x-access-token") + ":" + os.environ["GH_TOKEN"]
        headers["Authorization"] = "Basic " + base64.b64encode(credential.encode()).decode()
    token_url = "https://ghcr.io/token?" + urlencode({"service": "ghcr.io", "scope": f"repository:{repository}:pull"})
    with OPENER.open(Request(token_url, headers=headers), timeout=30) as response:
        token = json.loads(response.read(65537))["token"]
    if not isinstance(token, str) or not token or len(token) > 65536:
        raise ValueError("Invalid registry read token")
    request = Request(
        f"https://ghcr.io/v2/{repository}/manifests/{quote(reference, safe=':')}",
        headers={"Authorization": "Bearer " + token, "Accept": ", ".join(MEDIA_TYPES)},
    )
    try:
        with OPENER.open(request, timeout=30) as response:
            raw = response.read(8 * 1024 * 1024 + 1)
            digest = require_digest(response.headers.get("Docker-Content-Digest"))
    except HTTPError as error:
        if allow_missing and error.code == 404:
            return None
        raise
    if len(raw) > 8 * 1024 * 1024 or digest != "sha256:" + hashlib.sha256(raw).hexdigest():
        raise ValueError("Registry manifest content does not match its digest")
    manifest = json.loads(raw)
    if manifest.get("schemaVersion") != 2 or manifest.get("mediaType") not in MEDIA_TYPES:
        raise ValueError("Unsupported registry manifest")
    if reference.startswith("sha256:") and reference != digest:
        raise ValueError("Registry returned a different digest than requested")
    return digest
