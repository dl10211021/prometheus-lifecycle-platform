#!/usr/bin/env python3
"""Encode test host registration without interpreting credential characters."""
import json
import os
print(json.dumps({
    "name": os.environ["TEST_HOST_NAME"],
    "ip": os.environ["REMOTE_HOST"],
    "os": "linux",
    "connection_type": "ssh",
    "username": os.environ.get("REMOTE_USER", "root"),
    "password": os.environ["REMOTE_PASS"],
    "port": 22,
}))
