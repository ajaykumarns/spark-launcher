"""Check discovery and authentication boundaries without model inference."""
import json
from urllib.error import HTTPError
from urllib.request import Request, build_opener, HTTPRedirectHandler
from spark_launcher.catalog import fetch_catalog, load_config, state_dir, write_json

class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None

config = load_config()
fetch_catalog(config)
results = {"discovery": "passed", "inference": False, "routes": {}}
for path, method, payload, expected in [
    ("/anthropic/v1/messages", "POST", {}, (400, 422)),
    ("/anthropic/v1/messages/count_tokens", "POST", {}, (400, 422)),
    ("/v1/responses", "POST", {}, (400, 422)),
    ("/api/providers", "GET", None, (401, 403, 302, 307)),
    ("/anthropic/api/providers", "GET", None, (401, 403, 404, 302, 307, 200)),
    ("/anthropic/v1/messages", "GET", None, (401, 403, 404, 405, 200)),
]:
    data = json.dumps(payload).encode() if payload is not None else None
    request = Request(config["gateway"] + path, data=data, method=method, headers={"Content-Type": "application/json"})
    try:
        with build_opener(NoRedirect).open(request, timeout=30) as response:
            code = response.status
            if method == "GET" and code == 200:
                assert "text/html" in response.headers.get("Content-Type", ""), "Unexpected admin data response"
                assert b"<html" in response.read(4096).lower(), "Unexpected admin data response"
    except HTTPError as error:
        code = error.code
    results["routes"][method + " " + path] = code
    assert code in expected, (method, path, code)
write_json(state_dir() / "verification/gateway-boundaries.json", results)
print(json.dumps(results, indent=2))
