import os
from typing import Optional

class Greeter:
    def greet(self, name):
        return f"Hello, {name}"

def main():
    g = Greeter()
    print(g.greet("world"))
