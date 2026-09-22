"""模拟 javbus 站点，用于端到端验证「连通性诊断」功能。

- GET /                -> 首页，含 movie-box 结构标记
- GET /searchstar/<kw>  -> 演员搜索接口，返回 JSON
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import unquote

HOME = """<!DOCTYPE html>
<html><head><title>JavBus</title></head><body>
<div class="container">
  <div class="movie-box"><a href="/SSIS-001"><div class="photo-frame">
    <img src="/pics/cover/8q5t_b.jpg" title="SSIS-001"></div>
    <div class="photo-info"><span>SSIS-001<br><date>2021-01-01</date></span></div></a></div>
  <div class="movie-box"><a href="/SSIS-002"><div class="photo-frame">
    <img src="/pics/cover/aaaa_b.jpg" title="SSIS-002"></div>
    <div class="photo-info"><span>SSIS-002<br><date>2021-02-01</date></span></div></a></div>
</div>
<a href="/star/1v">三上悠亜</a>
</body></html>"""


class H(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        sys.stderr.write("[mock-javbus] %s %s\n" % (self.command, self.path))
        sys.stderr.flush()

    def do_GET(self):
        path = unquote(self.path.split("?")[0])
        if path.startswith("/searchstar/"):
            kw = path[len("/searchstar/"):]
            body = json.dumps([{"star_id": "1v", "name": kw}]).encode()
            ctype = "application/json"
        elif path == "/" or path.startswith("/star/"):
            body = HOME.encode()
            ctype = "text/html; charset=utf-8"
        else:
            body = HOME.encode()
            ctype = "text/html; charset=utf-8"
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9500
    srv = ThreadingHTTPServer(("127.0.0.1", port), H)
    sys.stderr.write("[mock-javbus] listening on 127.0.0.1:%d\n" % port)
    sys.stderr.flush()
    srv.serve_forever()
