# Ignore dynamic imports
# pylint: disable=E0401, C0413

import json

import click
from google.api_core.exceptions import NotFound, PermissionDenied
from google.cloud import secretmanager
from util import (
    JIRA_LABEL,
    PROJECT_ID,
    REQUEST_INFO,
    ROTATION_INSTRUCTIONS,
    get_secret_name,
    is_login_error,
    validate_collection,
    validate_path,
)

NOT_SET = "(not set)"


@click.command("describe")
@click.option(
    "-o",
    "--output",
    type=click.Choice(["json", "text"], case_sensitive=False),
    default="text",
    help="Output format, defaults to plain text but can be set to 'json'.",
)
@click.option(
    "-c",
    "--collection",
    required=True,
    help="The collection the secret belongs to.",
    type=str,
    callback=validate_collection,
)
@click.argument(
    "secret_path", required=True, callback=validate_path, metavar="SECRET_PATH"
)
def describe(output: str, collection: str, secret_path: str):
    """Show the metadata associated with a secret.

    Displays creation times for the secret and its latest version, plus the
    JIRA project, rotation instructions, and request information. The secret
    value is never shown.

    The SECRET_PATH should be in the format 'group/field' (e.g., 'aws/password').
    """

    client = secretmanager.SecretManagerServiceClient()

    full_secret_path = client.secret_path(
        PROJECT_ID, get_secret_name(collection, secret_path)
    )

    try:
        gcp_secret = client.get_secret(request={"name": full_secret_path})
    except PermissionDenied as e:
        raise click.ClickException(
            f"You don't have permission to access secrets in collection '{collection}'."
        ) from e
    except NotFound as e:
        raise click.ClickException(
            f"Secret '{secret_path}' does not exist in collection '{collection}'."
        ) from e
    except Exception as e:
        if is_login_error(e):
            raise  # reported by main
        raise click.ClickException(
            f"Failed to describe secret '{secret_path}': {e}."
        ) from e

    labels = dict(gcp_secret.labels)
    annotations = dict(gcp_secret.annotations)

    create_time = NOT_SET
    if gcp_secret.create_time:
        create_time = gcp_secret.create_time.strftime("%Y-%m-%d %H:%M:%S UTC")

    latest_version_create_time = NOT_SET
    try:
        latest_version = client.get_secret_version(
            request={"name": f"{full_secret_path}/versions/latest"}
        )
        if latest_version.create_time:
            latest_version_create_time = latest_version.create_time.strftime(
                "%Y-%m-%d %H:%M:%S UTC"
            )
    except NotFound:
        pass  # Secrets can exist without any versions.
    except PermissionDenied:
        latest_version_create_time = "(unavailable)"
        click.echo(
            "Warning: you don't have permission to view the latest version's metadata.",
            err=True,
        )
    except Exception as e:
        if is_login_error(e):
            raise  # reported by main
        raise click.ClickException(
            f"Failed to describe the latest version of secret '{secret_path}': {e}."
        ) from e

    metadata = {
        "create-time": create_time,
        "latest-version-create-time": latest_version_create_time,
        JIRA_LABEL: labels.get(JIRA_LABEL, NOT_SET),
        ROTATION_INSTRUCTIONS: annotations.get(ROTATION_INSTRUCTIONS, NOT_SET),
        REQUEST_INFO: annotations.get(REQUEST_INFO, NOT_SET),
    }

    if output == "json":
        click.echo(json.dumps(metadata, indent=2))
        return

    click.echo(f"Created:                {metadata['create-time']}")
    click.echo(f"Latest version created: {metadata['latest-version-create-time']}")
    click.echo(f"JIRA project:           {metadata[JIRA_LABEL]}")
    click.echo(f"Rotation instructions:  {metadata[ROTATION_INSTRUCTIONS]}")
    click.echo(f"Request information:    {metadata[REQUEST_INFO]}")
