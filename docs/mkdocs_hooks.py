"""Build-time helpers for the Atteler MkDocs site."""

from pathlib import Path
from shutil import copyfile


def on_post_build(config, **_kwargs):
    """Publish generated LLM docs next to each built documentation version."""

    config_file_path = getattr(config, "config_file_path", None)
    if config_file_path is None:
        config_file_path = config.get("config_file_path")

    root = Path(config_file_path).resolve().parent if config_file_path else Path.cwd()
    site_dir = Path(config["site_dir"])

    for name in ("llms.txt", "llms-full.txt"):
        source = root / name
        if not source.is_file():
            raise FileNotFoundError(f"{name} must exist; run `make generate` first")

        copyfile(source, site_dir / name)
