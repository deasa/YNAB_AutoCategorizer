#!/usr/bin/env python3
"""Stub MCP server (stdio, newline-delimited JSON-RPC) for the M0-4b spike.

Usage: stub_mcp.py <tag> <logfile>
Tools: read_balance (read), write_note (a "write" that only logs), and
extra_<tag> (only exists on this tag, to tell configs apart on --resume).
Every request is appended to <logfile> with the tag, so the log shows which
config served a call.
"""
import json
import sys
import time

TAG = sys.argv[1]
LOG = sys.argv[2]


def log(event):
    with open(LOG, "a") as f:
        f.write(json.dumps({"t": time.time(), "tag": TAG, **event}) + "\n")


TOOLS = [
    {"name": "read_balance", "description": "Read the current balance of the Fun Money category.",
     "inputSchema": {"type": "object", "properties": {}}},
    {"name": "write_note", "description": "Write a note onto a transaction.",
     "inputSchema": {"type": "object", "properties": {"note": {"type": "string"}}, "required": ["note"]}},
    {"name": "extra_" + TAG.lower(), "description": "Return the server tag.",
     "inputSchema": {"type": "object", "properties": {}}},
]


def reply(id_, result):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": id_, "result": result}) + "\n")
    sys.stdout.flush()


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    method = msg.get("method")
    log({"method": method, "params": msg.get("params") if method == "tools/call" else None})
    if "id" not in msg:
        continue
    if method == "initialize":
        reply(msg["id"], {"protocolVersion": msg["params"].get("protocolVersion", "2025-06-18"),
                          "capabilities": {"tools": {}},
                          "serverInfo": {"name": "stub-" + TAG, "version": "0"}})
    elif method == "tools/list":
        reply(msg["id"], {"tools": TOOLS})
    elif method == "tools/call":
        name = msg["params"]["name"]
        text = {"read_balance": f"Fun Money balance: $42.00 (served by {TAG})",
                "write_note": "note saved"}.get(name, f"tag={TAG}")
        reply(msg["id"], {"content": [{"type": "text", "text": text}]})
    else:
        reply(msg["id"], {})
