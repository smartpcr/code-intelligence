// Fixture module for the clean-code v1 Java parser adapter.
// Stage 2.1 tests assert the parser extracts the canonical AST
// scopes from this file (file, package, one interface, one
// class with two methods).

package com.example.sample;

import java.util.ArrayList;
import java.util.List;

public interface Sample {
    String sample(int seed);

    void close();
}

class MemorySample implements Sample {
    private final List<String> values;
    private boolean closed;

    public MemorySample(List<String> initial) {
        this.values = new ArrayList<>(initial);
        this.closed = false;
    }

    public String sample(int seed) {
        if (closed) {
            throw new IllegalStateException("sample: already closed");
        }
        if (values.isEmpty()) {
            throw new IllegalStateException("sample: empty buffer");
        }
        return values.get(seed % values.size());
    }

    public void close() {
        closed = true;
    }
}
