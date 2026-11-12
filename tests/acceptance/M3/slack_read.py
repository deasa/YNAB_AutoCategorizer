#!/usr/bin/env python3
"""Read-only Slack reader for hoid_check.sh: conversations.history only; never posts, edits or deletes.
stdin: lines "<qid> <channel_id> <message_ts>". Writes one JSON line per line read to argv[1]
({qid, found, text, blocks}) and the last 200 channel messages to argv[2], both for the caller's
secret grep. Prints counts only. The token is read from .env, sent in a header via curl's stdin config."""
import json, subprocess, sys, urllib.parse

def env(key):
    for line in open(".env"):
        k, _, v = line.strip().partition("=")
        if k == key:
            v = v.split(" #")[0].strip().strip('"').strip("'")
            return v
    sys.exit(f"{key} not in .env")

TOKEN = env("SLACK_BOT_TOKEN")

def call(method, **params):
    url = f"https://slack.com/api/{method}?" + urllib.parse.urlencode(params)
    cfg = f'header = "Authorization: Bearer {TOKEN}"\n'
    r = subprocess.run(["curl", "-sS", "-m", "20", "-K", "-", url], input=cfg, capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit(f"{method}: curl exit {r.returncode}")
    out = json.loads(r.stdout)
    if not out.get("ok"):
        sys.exit(f"{method}: {out.get('error')}")
    return out

chan, n, found = None, 0, 0
with open(sys.argv[1], "w") as qf:
    for line in sys.stdin:
        qid, chan, ts = line.split()
        msgs = call("conversations.history", channel=chan, latest=ts, oldest=ts, inclusive="true", limit=1)["messages"]
        m = msgs[0] if msgs else {}
        found += bool(msgs)
        n += 1
        qf.write(json.dumps({"qid": int(qid), "found": bool(msgs), "text": m.get("text", ""),
                             "blocks": m.get("blocks", [])}) + "\n")
if chan:
    with open(sys.argv[2], "w") as df:
        for m in call("conversations.history", channel=chan, limit=200)["messages"]:
            df.write(json.dumps({"text": m.get("text", ""), "blocks": m.get("blocks", [])}) + "\n")
print(f"question_messages_requested={n} found={found}")
