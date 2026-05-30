# Polyglot smoke fixture: PowerShell.
# Must declare a class/type, a free function, a same-file call,
# and one import so the dispatcher emits >=1 class node,
# >=1 method node, and >=1 static_calls edge per language.
# Greet calls $this.Format, which Pass 2b resolves as a
# receiver-qualified static_calls edge to Greeter.Format.
Import-Module Foo

class Greeter {
    [string] $Prefix
    [string] Format([string]$name) {
        return "$($this.Prefix) $name"
    }
    [string] Greet([string]$name) {
        return $this.Format($name)
    }
}

function Format-Hello {
    param([string]$Name)
    return "hi $Name"
}
