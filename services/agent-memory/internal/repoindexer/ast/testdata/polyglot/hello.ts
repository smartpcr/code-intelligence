// Polyglot smoke fixture: TypeScript.
// Must declare a class/type, a free function, a same-file call,
// and one import so the dispatcher emits >=1 class node,
// >=1 method node, and >=1 static_calls edge per language.
import { something } from "external-pkg";

class Greeter {
  greet(name: string): string {
    return formatGreeting(name);
  }
}

function formatGreeting(name: string): string {
  return "hi " + name;
}
