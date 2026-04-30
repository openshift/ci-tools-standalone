# Ignore dynamic imports
# pylint: disable=E0401, C0413

import os
import subprocess

import click


@click.command()
def login():
    """Authenticate to Google Cloud."""

    click.echo("Authenticating to Google Cloud...\n")

    try:
        # Run gcloud interactively with error-level verbosity to suppress warnings
        # (we set the quota project immediately after, so the warning is not relevant)
        subprocess.run(
            ["gcloud", "auth", "application-default", "login", "--verbosity=error"],
            check=True
        )

        creds_path = os.path.expanduser(os.environ.get("CLOUDSDK_CONFIG", "~/.config/gcloud"))
        creds_file = os.path.join(creds_path, "application_default_credentials.json")

        click.echo(
            f"\nLogin successful. Credentials are stored locally ({creds_file}) for this CLI only"
            " and will persist while you use the CLI."
            "\nTo reset the CLI environment and log out, run this script with the `clean` command."
        )
    except FileNotFoundError:
        click.echo("\n'gcloud' was not found. Please install Google Cloud CLI and retry.", err=True)
        raise click.Abort()
    except subprocess.CalledProcessError:
        click.echo("\nLogin failed.", err=True)
        raise click.Abort()
