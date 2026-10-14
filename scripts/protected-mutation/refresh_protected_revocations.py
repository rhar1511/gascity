#!/usr/bin/env python3
"""Refresh one root-managed Beads revocation snapshot; never reset its denylist."""

import argparse
import base64
import datetime
import fcntl
import json
import os
from pathlib import Path
import re
import stat
import tempfile
import unicodedata

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

DOMAIN = b"beads.protected-mutation-revocations.v1\n"
PURPOSE = "protected_mutation_revocation"


def canonical_json(value):
    encoded = json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    for char, escaped in [("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), ("\u2028", "\\u2028"), ("\u2029", "\\u2029")]:
        encoded = encoded.replace(char, escaped)
    return encoded.encode("utf-8")


def signing_bytes(payload):
    return DOMAIN + canonical_json(payload)


def atom(value, limit):
    if not isinstance(value, str) or not value or value.strip() != value or len(value.encode()) > limit or any(unicodedata.category(c) == "Cc" for c in value):
        raise ValueError("invalid authority binding")
    return value


def signed_snapshot(authority, keyring, revoked_ids, private_key, now):
    if not isinstance(private_key, Ed25519PrivateKey):
        raise ValueError("revocation key must be Ed25519")
    generation = keyring.get("generation")
    if keyring.get("schema_version") != 1 or type(generation) is not int or not 0 < generation < 2**64:
        raise ValueError("invalid keyring generation")
    public = base64.b64encode(private_key.public_key().public_bytes_raw()).decode()
    matches = [key for key in keyring["keys"] if key.get("purpose") == PURPOSE and key.get("public_key") == public]
    if len(matches) != 1 or sum(key.get("public_key") == public for key in keyring["keys"]) != 1:
        raise ValueError("private key must match exactly one trusted revocation authority")
    key = matches[0]
    if not isinstance(revoked_ids, list) or len(revoked_ids) > 65536 or any(not isinstance(value, str) or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:-]{15,199}", value) is None for value in revoked_ids):
        raise ValueError("invalid revoked replay IDs")
    if len(set(revoked_ids)) != len(revoked_ids):
        raise ValueError("duplicate revoked replay IDs")
    if now.tzinfo is None or now.utcoffset() is None:
        raise ValueError("snapshot clock must have a timezone")
    now = now.astimezone(datetime.timezone.utc).replace(microsecond=0)
    stamp = lambda moment: moment.strftime("%Y-%m-%dT%H:%M:%SZ")
    payload = {
        "schema_version": "beads.protected-mutation-revocations.v1",
        "purpose": PURPOSE,
        "key_id": atom(key["key_id"], 200),
        "issuer": atom(key["issuer"], 256),
        "audience": atom(authority["audience"], 255),
        "project_id": atom(authority["project_id"], 255),
        "database": atom(authority["database"], 255),
        "generation": generation,
        "issued_at": stamp(now),
        "expires_at": stamp(now + datetime.timedelta(seconds=240)),
        "revoked_replay_ids": sorted(revoked_ids),
    }
    encoded = signing_bytes(payload)
    if len(encoded) - len(DOMAIN) > 4 << 20:
        raise ValueError("revocation snapshot exceeds backend limit")
    signature = base64.urlsafe_b64encode(private_key.sign(encoded)).rstrip(b"=").decode()
    return {"payload": payload, "signature": signature}


def trusted_path(path):
    path = Path(path)
    if not path.is_absolute() or path != path.resolve(strict=True):
        raise ValueError("authority paths must be absolute and contain no symlinks")
    for current in [path, *path.parents]:
        info = current.lstat()
        if info.st_uid != 0 or info.st_mode & 0o022:
            raise ValueError("authority paths must be root-owned and not group/world writable")
    return path


def read_root_file(path, private=False):
    path = trusted_path(path)
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022 or (private and stat.S_IMODE(info.st_mode) != 0o600):
            raise ValueError("invalid authority file ownership or mode")
        data = source.read((4 << 20) + 1)
        if len(data) > 4 << 20:
            raise ValueError("authority file too large")
        return data


def strict_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate JSON key")
        value[key] = item
    return value


def refresh(authority_path, private_key_path):
    if os.geteuid() != 0:
        raise PermissionError("revocation refresh must run as root")
    authority_path = trusted_path(authority_path)
    directory = authority_path.parent
    lock_fd = os.open(directory / ".refresh.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lock_fd, "rb") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        load = lambda path: json.loads(read_root_file(path), object_pairs_hook=strict_object)
        authority = load(authority_path)
        if set(authority) != {"schema_version", "audience", "project_id", "database", "keyring_file", "revocations_file"} or authority["schema_version"] != 1:
            raise ValueError("invalid authority document")
        for field in ("keyring_file", "revocations_file"):
            name = authority[field]
            if not isinstance(name, str) or name in ("", ".", "..") or Path(name).name != name:
                raise ValueError("authority references must be plain file names")
        keyring = load(directory / authority["keyring_file"])
        revoked_ids = load(directory / "revoked-replay-ids.json")
        key = serialization.load_pem_private_key(read_root_file(private_key_path, private=True), password=None)
        snapshot = signed_snapshot(authority, keyring, revoked_ids, key, datetime.datetime.now(datetime.timezone.utc))
        fd, temporary = tempfile.mkstemp(prefix=".revocations-", dir=directory)
        try:
            with os.fdopen(fd, "wb") as target:
                os.fchmod(target.fileno(), 0o644)
                target.write(canonical_json(snapshot) + b"\n")
                target.flush()
                os.fsync(target.fileno())
            os.replace(temporary, directory / authority["revocations_file"])
            directory_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--authority", required=True)
    parser.add_argument("--private-key", required=True)
    arguments = parser.parse_args()
    refresh(arguments.authority, arguments.private_key)
