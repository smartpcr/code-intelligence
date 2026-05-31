# SPDX-License-Identifier: CC0-1.0
"""A trivial python module so the language sniffer picks Python up."""


def hello() -> str:
    return "hello from python"


if __name__ == "__main__":
    print(hello())
