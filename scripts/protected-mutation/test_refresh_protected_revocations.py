import base64
import datetime
import json
import unittest

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from refresh_protected_revocations import signed_snapshot, signing_bytes


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.key = Ed25519PrivateKey.generate()
        self.authority = {"audience": "pilot<&>", "project_id": "project-1", "database": "hq"}
        self.keyring = {"schema_version": 1, "generation": 7, "keys": [{
            "key_id": "revoke-1", "issuer": "pilot", "purpose": "protected_mutation_revocation",
            "public_key": base64.b64encode(self.key.public_key().public_bytes_raw()).decode(),
        }]}
        self.now = datetime.datetime(2026, 10, 13, tzinfo=datetime.timezone.utc)

    def snapshot(self, ids):
        return signed_snapshot(self.authority, self.keyring, ids, self.key, self.now)

    def test_signature_binding_and_revocations_are_preserved(self):
        ids = ["permit-use-00000002", "permit-use-00000001"]
        envelope = self.snapshot(ids)
        payload = envelope["payload"]
        self.assertEqual(payload["revoked_replay_ids"], sorted(ids))
        self.assertEqual(payload["generation"], 7)
        self.assertEqual(payload["audience"], "pilot<&>")
        self.assertEqual(payload["expires_at"], "2026-10-13T00:04:00Z")
        signature = base64.urlsafe_b64decode(envelope["signature"] + "==")
        self.key.public_key().verify(signature, signing_bytes(payload))
        self.assertNotIn("=", envelope["signature"])

    def test_go_json_escaping_and_order(self):
        payload = self.snapshot([])["payload"]
        encoded = signing_bytes(payload)
        self.assertTrue(encoded.startswith(b'beads.protected-mutation-revocations.v1\n{"schema_version":'))
        self.assertIn(b'pilot\\u003c\\u0026\\u003e', encoded)
        self.assertEqual(list(payload), ["schema_version", "purpose", "key_id", "issuer", "audience", "project_id", "database", "generation", "issued_at", "expires_at", "revoked_replay_ids"])
        self.assertEqual(json.loads(encoded.split(b"\n", 1)[1])["revoked_replay_ids"], [])

    def test_invalid_ids_refused(self):
        for ids in [None, ["short"], ["permit-use-00000001"] * 2, ["bad replay id000000"], [1]]:
            with self.subTest(ids=ids), self.assertRaises(ValueError):
                self.snapshot(ids)

    def test_wrong_purpose_or_private_key_refused(self):
        self.keyring["keys"][0]["purpose"] = "protected_mutation_permit"
        with self.assertRaises(ValueError):
            self.snapshot([])
        self.keyring["keys"][0]["purpose"] = "protected_mutation_revocation"
        self.key = Ed25519PrivateKey.generate()
        with self.assertRaises(ValueError):
            self.snapshot([])

    def test_shared_permit_and_revocation_key_refused(self):
        shared = dict(self.keyring["keys"][0])
        shared.update(key_id="permit-1", purpose="protected_mutation_permit")
        self.keyring["keys"].append(shared)
        with self.assertRaises(ValueError):
            self.snapshot([])

    def test_missing_binding_and_invalid_generation_refused(self):
        for generation in [0, -1, True, "7"]:
            self.keyring["generation"] = generation
            with self.subTest(generation=generation), self.assertRaises(ValueError):
                self.snapshot([])
        self.keyring["generation"] = 7
        self.authority["project_id"] = ""
        with self.assertRaises(ValueError):
            self.snapshot([])


if __name__ == "__main__":
    unittest.main()
