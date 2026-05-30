// Polyglot smoke fixture: C++.
// Must declare a class/type, a free function, a same-file call,
// and one import so the dispatcher emits >=1 class node,
// >=1 method node, and >=1 static_calls edge per language.
// The class counts as the type; the same-file call flows from
// greet -> format_greeting (both translation-unit free funcs).
#include <string>

class Greeter {
public:
  int n;
};

int format_greeting(int n) {
  return n + 1;
}

int greet(int n) {
  return format_greeting(n);
}
