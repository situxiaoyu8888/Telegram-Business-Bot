"""Minimal fake Telegram Bot API for end-to-end tests.

POST /inject {update}  queues an update for getUpdates
GET  /sent             returns every message the bot sent
"""
import json, sys, threading, time
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler

lock = threading.Lock()
updates, sent = [], []
next_update, next_message = [1], [100]

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def reply(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try: return json.loads(raw or b"{}")
        except Exception: return {}
    def do_GET(self):
        if self.path == "/rss.xml":
            body = ("<?xml version='1.0' encoding='UTF-8'?><rss version='2.0'><channel><title>E2E</title>"
                    "<item><title>RSS 旧闻一</title><link>https://example.com/1</link><guid>1</guid><description>one</description></item>"
                    "<item><title>RSS 旧闻二</title><link>https://example.com/2</link><guid>2</guid><description>two</description></item>"
                    "</channel></rss>").encode()
            self.send_response(200); self.send_header("Content-Type", "application/rss+xml"); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
            return
        if self.path == "/sent":
            with lock: return self.reply(sent)
        self.reply({"ok": False}, 404)
    def do_POST(self):
        data = self.body()
        if self.path == "/inject":
            with lock:
                data["update_id"] = next_update[0]; next_update[0] += 1; updates.append(data)
            return self.reply({"ok": True})
        parts = self.path.strip("/").split("/")
        if len(parts) != 2 or not parts[0].startswith("bot"):
            return self.reply({"ok": False}, 404)
        token, method = parts[0][3:], parts[1]
        bot_id = int(token.split(":")[0])
        if method == "getMe":
            return self.reply({"ok": True, "result": {"id": bot_id, "is_bot": True, "first_name": "E2E", "username": f"e2e_bot_{bot_id}"}})
        if method == "getUpdates":
            offset = int(data.get("offset") or 0)
            deadline = time.time() + 1.0
            while time.time() < deadline:
                with lock:
                    ready = [u for u in updates if u["update_id"] >= offset and u.get("_bot", bot_id) == bot_id]
                if ready:
                    return self.reply({"ok": True, "result": [{k: v for k, v in u.items() if k != "_bot"} for u in ready]})
                time.sleep(0.05)
            return self.reply({"ok": True, "result": []})
        if method in ("sendMessage", "sendPhoto", "sendVideo", "editMessageText", "editMessageCaption", "copyMessage"):
            with lock:
                next_message[0] += 1
                sent.append({"bot": bot_id, "method": method, **data, "mid": next_message[0]})
                mid = next_message[0]
            result = {"message_id": mid, "chat": {"id": data.get("chat_id")}, "date": int(time.time())}
            if method == "sendPhoto":
                result["photo"] = [{"file_id": f"fid-small-{mid}"}, {"file_id": f"fid-{mid}"}]
            return self.reply({"ok": True, "result": result})
        if method == "sendMediaGroup":
            with lock:
                next_message[0] += 1
                sent.append({"bot": bot_id, "method": method, **data, "mid": next_message[0]})
            return self.reply({"ok": True, "result": []})
        if method in ("answerCallbackQuery", "answerInlineQuery", "setMyCommands", "deleteMessage"):
            with lock:
                sent.append({"bot": bot_id, "method": method, **data})
        return self.reply({"ok": True, "result": True})

ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
