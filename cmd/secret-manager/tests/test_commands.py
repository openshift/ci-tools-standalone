"""Test the commands that write to Google Secret Manager directly."""

import json
import sys
import unittest
from datetime import datetime, timezone
from pathlib import Path
from unittest.mock import MagicMock, patch

from click.testing import CliRunner
from google.api_core.exceptions import DeadlineExceeded, InvalidArgument, NotFound, PermissionDenied, RetryError, ServiceUnavailable
from google.cloud import secretmanager

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from commands.create import create as create_command
from commands.delete import delete as delete_command
from commands.describe import describe as describe_command
from commands.get_service_account import get_service_account as get_sa_command
from commands.update import update as update_command

SECRET_ID = "my-collection__aws__key"
# EXPIRED_LOGIN is the text Google reports for an expired or revoked login.
EXPIRED_LOGIN = "Getting metadata from plugin failed with error: ('invalid_grant: Token has been expired or revoked.')"


def fake_client():
    """fake_client returns a mocked Secret Manager client that builds resource paths."""
    client = MagicMock()
    client.secret_path.side_effect = lambda project, secret: f"projects/{project}/secrets/{secret}"
    return client


class CommandsTest(unittest.TestCase):
    """Exercise create, update, and delete without Google credentials."""

    def run_command(self, command, module, args, client, **patches):
        """run_command invokes a command with a fake Secret Manager client."""
        with patch(f"commands.{module}.secretmanager.SecretManagerServiceClient", return_value=client):
            for name, value in patches.items():
                stack = patch(f"commands.{module}.{name}", return_value=value)
                stack.start()
                self.addCleanup(stack.stop)
            return CliRunner().invoke(command, args)

    def assert_index_untouched(self, client):
        """assert_index_untouched fails if the command read or wrote an index secret."""
        for call in client.mock_calls:
            self.assertNotIn("____index", str(call))
        client.access_secret_version.assert_not_called()

    def test_create(self):
        for name, args, get_error, add_error, delete_error, want_exit, want_output, want_created, want_deleted in [
            ("new secret is created", ["-l", "value"], NotFound("missing"), None, None,
             0, "created successfully", True, False),
            ("existing secret is rejected", ["-l", "value"], None, None, None,
             1, "already exists", False, False),
            ("missing permission is reported", ["-l", "value"], PermissionDenied("denied"), None, None,
             1, "Insufficient permissions", False, False),
            ("missing file is rejected before creating the secret", ["-f", "/nonexistent/secret.txt"],
             NotFound("missing"), None, None, 2, "does not exist", False, False),
            ("failed version add removes the empty secret", ["-l", "value"], NotFound("missing"),
             InvalidArgument("too large"), None, 1, "Please try again", True, True),
            ("failed cleanup asks the user to delete the secret", ["-l", "value"], NotFound("missing"),
             InvalidArgument("too large"), PermissionDenied("denied"), 1,
             "Run 'delete -c my-collection aws/key'", True, True),
        ]:
            with self.subTest(name=name):
                client = fake_client()
                client.get_secret.side_effect = get_error
                client.create_secret.return_value.name = f"projects/openshift-ci-secrets/secrets/{SECRET_ID}"
                client.add_secret_version.side_effect = add_error
                client.delete_secret.side_effect = delete_error
                result = self.run_command(
                    create_command,
                    "create",
                    ["-c", "my-collection", "aws/key", *args],
                    client,
                    check_if_collection_exists=True,
                    prompt_for_labels={},
                    prompt_for_annotations={},
                )
                self.assertEqual(result.exit_code, want_exit, result.output)
                self.assertIn(want_output, result.output)
                self.assertEqual(client.create_secret.called, want_created)
                self.assertEqual(client.add_secret_version.call_count, int(want_created))
                if want_created:
                    self.assertEqual(client.create_secret.call_args.kwargs["request"]["secret_id"], SECRET_ID)
                if want_deleted:
                    client.delete_secret.assert_called_once_with(
                        name=f"projects/openshift-ci-secrets/secrets/{SECRET_ID}"
                    )
                else:
                    client.delete_secret.assert_not_called()
                self.assert_index_untouched(client)

    def test_update(self):
        for name, get_error, want_exit, want_output in [
            ("existing secret gets a new version", None, 0, "updated successfully"),
            ("missing secret is reported", NotFound("missing"), 1, "does not exist in collection"),
            ("missing permission is reported", PermissionDenied("denied"), 1, "don't have permission"),
            ("other Google error is reported", DeadlineExceeded("slow"), 1, "Failed to access secret 'aws/key'"),
            ("Google outage is not reported as a login problem", ServiceUnavailable("backend unavailable"), 1, "Failed to access secret 'aws/key'"),
            ("expired login is passed on to main", ServiceUnavailable(EXPIRED_LOGIN), 1, ""),
        ]:
            with self.subTest(name=name):
                client = fake_client()
                client.get_secret.side_effect = get_error
                result = self.run_command(
                    update_command,
                    "update",
                    ["-c", "my-collection", "aws/key", "-l", "value"],
                    client,
                    destroy_previous_versions=None,
                )
                self.assertEqual(result.exit_code, want_exit, result.output)
                self.assertIn(want_output, result.output)
                self.assertEqual(client.add_secret_version.called, want_exit == 0)
                self.assertEqual(isinstance(result.exception, ServiceUnavailable), want_output == "")
                if want_exit == 0:
                    self.assertEqual(
                        client.add_secret_version.call_args.kwargs["parent"],
                        f"projects/openshift-ci-secrets/secrets/{SECRET_ID}",
                    )
                self.assert_index_untouched(client)

    def test_delete(self):
        for name, delete_error, want_exit, want_output in [
            ("existing secret is deleted", None, 0, "successfully deleted"),
            ("missing secret is reported", NotFound("missing"), 1, "does not exist"),
            ("missing permission is reported", PermissionDenied("denied"), 1, "don't have permission to delete"),
            ("expired login is passed on to main", ServiceUnavailable(EXPIRED_LOGIN), 1, ""),
        ]:
            with self.subTest(name=name):
                client = fake_client()
                client.delete_secret.side_effect = delete_error
                result = self.run_command(delete_command, "delete", ["-c", "my-collection", "aws/key"], client)
                self.assertEqual(result.exit_code, want_exit, result.output)
                self.assertIn(want_output, result.output)
                self.assertEqual(isinstance(result.exception, ServiceUnavailable), isinstance(delete_error, ServiceUnavailable))
                client.delete_secret.assert_called_once_with(
                    name=f"projects/openshift-ci-secrets/secrets/{SECRET_ID}"
                )
                self.assert_index_untouched(client)

    def test_describe(self):
        for name, get_error, want_exit in [
            ("existing secret is described", None, 0),
            ("expired login is passed on to main", ServiceUnavailable(EXPIRED_LOGIN), 1),
        ]:
            with self.subTest(name=name):
                client = fake_client()
                client.get_secret.side_effect = get_error
                client.get_secret.return_value.labels = {}
                client.get_secret.return_value.annotations = {}
                result = self.run_command(describe_command, "describe", ["-c", "my-collection", "aws/key"], client)
                self.assertEqual(result.exit_code, want_exit, result.output)
                self.assertEqual(isinstance(result.exception, ServiceUnavailable), get_error is not None)
                if get_error is not None:
                    client.get_secret_version.assert_not_called()
                self.assert_index_untouched(client)

    def test_describe_latest_version(self):
        created = datetime(2025, 1, 2, 3, 4, 5, tzinfo=timezone.utc)
        updated = datetime(2025, 6, 7, 8, 9, 10, tzinfo=timezone.utc)
        for name, version_time, version_error, want_exit, want_time, want_error in [
            ("latest version creation time", updated, None, 0, "2025-06-07 08:09:10 UTC", ""),
            ("missing version timestamp", None, None, 0, "(not set)", ""),
            ("no versions", None, NotFound("missing"), 0, "(not set)", ""),
            ("version metadata permission denied", None, PermissionDenied("denied"),
             0, "(unavailable)", "Warning: you don't have permission"),
            ("Google error", None, DeadlineExceeded("slow"),
             1, None, "Failed to describe the latest version of secret 'aws/key'"),
            ("Google outage", None, ServiceUnavailable("backend unavailable"),
             1, None, "Failed to describe the latest version of secret 'aws/key'"),
            ("expired login", None, ServiceUnavailable(EXPIRED_LOGIN), 1, None, ""),
            ("expired login retry timeout", None, RetryError("timeout", ServiceUnavailable(EXPIRED_LOGIN)),
             1, None, ""),
        ]:
            for output in ["text", "json"]:
                with self.subTest(name=name, output=output):
                    client = fake_client()
                    client.get_secret.return_value = secretmanager.Secret(
                        create_time=created,
                        labels={"jira-project": "TEST"},
                        annotations={
                            "rotation-instructions": "Rotate regularly",
                            "request-information": "Ask the owner",
                        },
                    )
                    client.get_secret_version.return_value = secretmanager.SecretVersion(
                        create_time=version_time
                    )
                    client.get_secret_version.side_effect = version_error
                    result = self.run_command(
                        describe_command, "describe",
                        ["-c", "my-collection", "aws/key", "-o", output], client,
                    )
                    self.assertEqual(result.exit_code, want_exit, result.output)
                    self.assertIn(want_error, result.stderr)
                    if want_exit == 0:
                        if output == "json":
                            self.assertEqual(json.loads(result.stdout), {
                                "create-time": "2025-01-02 03:04:05 UTC",
                                "latest-version-create-time": want_time,
                                "jira-project": "TEST",
                                "rotation-instructions": "Rotate regularly",
                                "request-information": "Ask the owner",
                            })
                        else:
                            self.assertIn("Created:                2025-01-02 03:04:05 UTC", result.stdout)
                            self.assertIn(f"Latest version created: {want_time}", result.stdout)
                    elif not want_error:
                        self.assertIs(result.exception, version_error)
                    client.get_secret_version.assert_called_once_with(request={
                        "name": f"projects/openshift-ci-secrets/secrets/{SECRET_ID}/versions/latest"
                    })
                    self.assert_index_untouched(client)

    def test_get_service_account(self):
        for name, access_error, want_exit in [
            ("key is printed", None, 0),
            ("expired login is passed on to main", ServiceUnavailable(EXPIRED_LOGIN), 1),
            ("retry timeout after an expired login is passed on", RetryError("timeout", ServiceUnavailable(EXPIRED_LOGIN)), 1),
        ]:
            with self.subTest(name=name):
                client = fake_client()
                client.access_secret_version.side_effect = access_error
                client.access_secret_version.return_value.payload.data = b'{"type": "service_account"}'
                result = self.run_command(get_sa_command, "get_service_account", ["-c", "my-collection"], client)
                self.assertEqual(result.exit_code, want_exit, result.output)
                if access_error is None:
                    self.assertIn('"service_account"', result.output)
                else:
                    self.assertIs(result.exception, access_error)


if __name__ == "__main__":
    unittest.main()
