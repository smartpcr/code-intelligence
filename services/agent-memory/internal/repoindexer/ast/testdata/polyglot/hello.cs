// Polyglot smoke fixture: C#.
// Must declare a class/type, a free function, a same-file call,
// and one import so the dispatcher emits >=1 class node,
// >=1 method node, and >=1 static_calls edge per language.
// C# has no free functions, so FormatGreeting is a static
// class member; the bare-name resolver in Pass 2b stitches
// Greet -> FormatGreeting via the simple-name multimap.
using System;

class Greeter
{
    public string Greet(string name)
    {
        return FormatGreeting(name);
    }

    public static string FormatGreeting(string name)
    {
        return "hi " + name;
    }
}
