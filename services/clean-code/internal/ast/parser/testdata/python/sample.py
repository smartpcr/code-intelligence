"""Fixture module for the clean-code v1 Python parser adapter.

Stage 2.1 tests assert the parser extracts the canonical AST
scopes from this file (file, two functions, one class with two
methods, one ABC interface).
"""

from abc import ABC, abstractmethod
from typing import List, Optional


class Sampler(ABC):
    """ABC base every concrete sampler implements."""

    @abstractmethod
    def sample(self, seed: int) -> str:
        ...

    @abstractmethod
    def close(self) -> None:
        ...


class MemorySampler:
    """Concrete sampler backed by an in-memory list."""

    def __init__(self, initial: Optional[List[str]] = None) -> None:
        self._values: List[str] = list(initial or [])
        self._closed: bool = False

    def sample(self, seed: int) -> str:
        if self._closed:
            raise RuntimeError("sample: already closed")
        if not self._values:
            raise RuntimeError("sample: empty buffer")
        return self._values[seed % len(self._values)]

    def close(self) -> None:
        self._closed = True


def make_sampler(initial: Optional[List[str]] = None) -> MemorySampler:
    return MemorySampler(initial)


async def async_make_sampler(initial: Optional[List[str]] = None) -> MemorySampler:
    return MemorySampler(initial)
