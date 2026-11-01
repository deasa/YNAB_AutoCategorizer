#!/usr/bin/env python3
"""Read-only: conversations.list + conversations.history only. Never posts, updates or deletes.
Prints one line per bot status message: ts, edited ts, text length. --text also prints the text,
which is safe to show only after the caller has counted secrets in the output.
Token comes from SLACK_BOT_TOKEN; it goes in a header, never on argv."""
import json, os, subprocess, sys, time, urllib.parse

def call(method, **params):
    url = f"https://slack.com/api/{method}?" + urllib.parse.urlencode(params)
    # curl, for its CA bundle; the header goes through stdin so the token stays off argv.
    cfg = f'header = "Authorization: Bearer {os.environ["SLACK_BOT_TOKEN"]}"\n'
    r = subprocess.run(["curl", "-sS", "-m", "20", "-K", "-", url], input=cfg, capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit(f"{method}: curl exit {r.returncode}")
    out = json.loads(r.stdout)
    if not out.get("ok"):
        sys.exit(f"{method}: {out.get('error')}")
    return out

chan = next(c for c in call("conversations.list", types="private_channel,public_channel", limit=200)["channels"]
            if c["name"] == "finbot-finances")
msgs = call("conversations.history", channel=chan["id"], limit=50)["messages"]
now = time.time()
n = 0
for m in msgs:
    if m.get("bot_id") is None:
        continue
    n += 1
    edited = m.get("edited", {}).get("ts")
    row = {"ts": m["ts"], "age_min": round((now - float(m["ts"])) / 60, 1),
           "edited_ts": edited, "edited_age_min": round((now - float(edited)) / 60, 1) if edited else None,
           "len": len(m.get("text", "")), "pinned": bool(m.get("pinned_to"))}
    if "--text" in sys.argv:
        row["text"] = m.get("text", "")
    print(json.dumps(row))
print(f"bot_messages={n} total_seen={len(msgs)}")
