"""Entry point for `python -m bench`.

Delegates to cli.main(); see bench/cli.py for the subcommand surface.
"""

from __future__ import annotations

import sys

from bench.cli import main


if __name__ == "__main__":
    sys.exit(main())
