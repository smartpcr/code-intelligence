// Polyglot smoke fixture: Rust.
// Must declare a class/type, a free function, a same-file call,
// and one import so the dispatcher emits >=1 class node,
// >=1 method node, and >=1 static_calls edge per language.
// The struct counts as the type; the same-file call flows
// from greet -> format_greeting (both module-level free fns).
use std::fmt::Display;

pub struct Greeter {
    prefix: String,
}

pub fn format_greeting(name: &str) -> String {
    String::from(name)
}

pub fn greet(name: &str) -> String {
    format_greeting(name)
}
