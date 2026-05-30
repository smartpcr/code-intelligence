# Polyglot smoke fixture: Python.
# Must declare a class/type, a free function, a same-file call,
# and one import so the dispatcher emits >=1 class node,
# >=1 method node, and >=1 static_calls edge per language.
from utils import helper_external


class Greeter:
    def greet(self, name):
        return format_greeting(name)


def format_greeting(name):
    return "hi " + name
